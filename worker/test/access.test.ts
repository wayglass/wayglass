// Cloudflare Access sign-in for people: JWT verification against a fake team
// JWKS, short-lived human session keys, expiry, and the human gate.

import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import { type AccessConfig, setJwksFetcherForTests, verifyAccessJwt } from "../src/access";
import { registryStub } from "../src/keys";
import { call, createProject, useFakeArtifacts } from "./helpers";
import type { FakeRepoHost } from "./fake-artifacts";

const TEAM = "wayglass-test.cloudflareaccess.com";
const AUD = "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2";
const cfg: AccessConfig = { teamDomain: TEAM, aud: AUD };
// The committed vars are empty (feature off), so their generated type is ""; tests turn it on.
const accessEnv = { ACCESS_TEAM_DOMAIN: TEAM as string, ACCESS_AUD: AUD as string } as Partial<Env>;

const enc = new TextEncoder();
const b64url = (b: Uint8Array | string) =>
  btoa(typeof b === "string" ? b : String.fromCharCode(...b))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
const part = (o: unknown) => b64url(String.fromCharCode(...enc.encode(JSON.stringify(o))));

let signing: CryptoKeyPair;
let other: CryptoKeyPair;
let jwk: JsonWebKey;
let fetches = 0;

beforeAll(async () => {
  const gen = () =>
    crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]) as Promise<CryptoKeyPair>;
  signing = await gen();
  other = await gen();
  jwk = (await crypto.subtle.exportKey("jwk", signing.publicKey)) as JsonWebKey;
});

beforeEach(() => {
  fetches = 0;
  setJwksFetcherForTests(async (url) => {
    fetches++;
    expect(url).toBe(`https://${TEAM}/cdn-cgi/access/certs`);
    return { keys: [{ kid: "kid-1", kty: "RSA", alg: "RS256", use: "sig", n: jwk.n, e: jwk.e }] };
  });
});
afterEach(() => setJwksFetcherForTests(null));

const now = () => Math.floor(Date.now() / 1000);

function claims(over: Record<string, unknown> = {}): Record<string, unknown> {
  return { aud: [AUD], email: "Person@Example.com", exp: now() + 600, iat: now() - 5, nbf: now() - 5, iss: `https://${TEAM}`, sub: "user-1", type: "app", ...over };
}

async function sign(payload: Record<string, unknown>, header: Record<string, unknown> = { alg: "RS256", kid: "kid-1", typ: "JWT" }, key = signing.privateKey): Promise<string> {
  const input = `${part(header)}.${part(payload)}`;
  const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", key, enc.encode(input)));
  return `${input}.${b64url(sig)}`;
}

describe("verifyAccessJwt", () => {
  it("accepts a valid application token and lower-cases the email", async () => {
    const v = await verifyAccessJwt(await sign(claims()), cfg);
    expect(v).toEqual({ ok: true, claims: { email: "person@example.com", sub: "user-1", exp: expect.any(Number) } });
  });

  it("accepts aud as a string", async () => {
    expect((await verifyAccessJwt(await sign(claims({ aud: AUD })), cfg)).ok).toBe(true);
  });

  it.each([
    ["wrong aud", { aud: ["someone-else"] }, "wrong audience"],
    ["wrong iss", { iss: "https://evil.cloudflareaccess.com" }, "wrong issuer"],
    ["expired", { exp: now() - 120 }, "token expired"],
    ["no exp", { exp: undefined }, "token expired"],
    ["not yet valid", { nbf: now() + 600 }, "token not yet valid"],
    ["issued in the future", { iat: now() + 600 }, "token issued in the future"],
    ["missing email (service token)", { email: undefined, common_name: "abc.access" }, "no email"],
    ["org token", { type: "org" }, "not an application token"],
  ])("refuses %s", async (_name, over, reason) => {
    const v = await verifyAccessJwt(await sign(claims(over)), cfg);
    expect(v.ok).toBe(false);
    if (!v.ok) expect(v.reason).toContain(reason);
  });

  it("allows small clock skew on exp", async () => {
    expect((await verifyAccessJwt(await sign(claims({ exp: now() - 10 })), cfg)).ok).toBe(true);
  });

  it.each([
    ["no aud", { aud: undefined }],
    ["an empty aud list", { aud: [] }],
    ["an aud that only looks right", { aud: [AUD.slice(0, -1), `${AUD} `] }],
    ["a numeric aud", { aud: 1 }],
  ])("refuses %s", async (_name, over) => {
    expect(await verifyAccessJwt(await sign(claims(over)), cfg)).toEqual({ ok: false, reason: "wrong audience" });
  });

  it("accepts the audience among other entries, ignoring non-strings", async () => {
    expect((await verifyAccessJwt(await sign(claims({ aud: [1, null, "other", AUD] })), cfg)).ok).toBe(true);
  });

  it.each([
    ["a string exp", { exp: String(now() + 600) }, "token expired"],
    ["a string nbf", { nbf: String(now() - 5) }, "token not yet valid"],
    ["a non-string email", { email: ["person@example.com"] }, "no email"],
    ["an email with spaces", { email: "per son@example.com" }, "no email"],
    ["an iss with a path", { iss: `https://${TEAM}/` }, "wrong issuer"],
    ["an http iss", { iss: `http://${TEAM}` }, "wrong issuer"],
  ])("refuses %s", async (_name, over, reason) => {
    const v = await verifyAccessJwt(await sign(claims(over)), cfg);
    expect(v.ok).toBe(false);
    if (!v.ok) expect(v.reason).toContain(reason);
  });

  it("ignores JWKS entries under the right kid that are not RS256 RSA keys", async () => {
    const ec = (await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"])) as CryptoKeyPair;
    const ecJwk = (await crypto.subtle.exportKey("jwk", ec.publicKey)) as JsonWebKey;
    setJwksFetcherForTests(async () => ({
      keys: [
        { kid: "kid-1", kty: "EC", crv: "P-256", x: ecJwk.x, y: ecJwk.y },
        { kid: "kid-1", kty: "RSA", alg: "RS512", n: jwk.n, e: jwk.e },
        { kid: "kid-1", kty: "RSA", n: "not-a-modulus", e: jwk.e },
        { kty: "RSA", n: jwk.n, e: jwk.e }, // no kid
      ],
    }));
    expect(await verifyAccessJwt(await sign(claims()), cfg)).toEqual({ ok: false, reason: "token was not signed by a key of this team" });
  });

  it("refuses a token whose header alg is RS256 but whose kid is not a string", async () => {
    for (const kid of [1, null, "", ["kid-1"]]) {
      const v = await verifyAccessJwt(await sign(claims(), { alg: "RS256", kid }), cfg);
      expect(v).toEqual({ ok: false, reason: "token has no key id" });
    }
  });

  it("refuses a signature over a different payload", async () => {
    const good = await sign(claims());
    const [h, , s] = good.split(".") as [string, string, string];
    const swapped = `${h}.${part(claims({ email: "attacker@example.com" }))}.${s}`;
    expect(await verifyAccessJwt(swapped, cfg)).toEqual({ ok: false, reason: "bad signature" });
    // Standard base64 (with padding or + /) in any part is not a token.
    expect((await verifyAccessJwt(`${h}=.${good.split(".")[1]}.${s}`, cfg)).ok).toBe(false);
  });

  it("refuses alg none and HS256", async () => {
    const none = `${part({ alg: "none", kid: "kid-1" })}.${part(claims())}.`;
    expect(await verifyAccessJwt(none, cfg)).toMatchObject({ ok: false, reason: expect.stringContaining("none") });
    // HS256 keyed with the public modulus: the classic algorithm-confusion forgery.
    const hk = await crypto.subtle.importKey("raw", enc.encode(jwk.n!), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    const input = `${part({ alg: "HS256", kid: "kid-1" })}.${part(claims())}`;
    const mac = new Uint8Array(await crypto.subtle.sign("HMAC", hk, enc.encode(input)));
    expect(await verifyAccessJwt(`${input}.${b64url(mac)}`, cfg)).toMatchObject({ ok: false, reason: expect.stringContaining("HS256") });
  });

  it("refuses a token signed by another key under a known kid", async () => {
    expect(await verifyAccessJwt(await sign(claims(), undefined, other.privateKey), cfg)).toEqual({ ok: false, reason: "bad signature" });
  });

  it("refuses an unknown kid, caching keys between tokens", async () => {
    expect((await verifyAccessJwt(await sign(claims()), cfg)).ok).toBe(true);
    expect((await verifyAccessJwt(await sign(claims()), cfg)).ok).toBe(true);
    expect(fetches).toBe(1);
    const v = await verifyAccessJwt(await sign(claims(), { alg: "RS256", kid: "kid-unknown" }), cfg);
    expect(v).toEqual({ ok: false, reason: "token was not signed by a key of this team" });
  });

  it("refetches the keys for an unknown kid after keys rotate, at most every 30 s", async () => {
    let rotated = false;
    setJwksFetcherForTests(async () => {
      fetches++;
      return { keys: rotated ? [{ kid: "kid-1", kty: "RSA", n: jwk.n, e: jwk.e }] : [] };
    });
    const t0 = Date.now();
    expect((await verifyAccessJwt(await sign(claims()), cfg)).ok).toBe(false);
    rotated = true;
    // Within 30 s an unknown kid does not refetch, so junk tokens cannot hammer the team domain.
    expect((await verifyAccessJwt(await sign(claims()), cfg)).ok).toBe(false);
    expect(fetches).toBe(1);
    const spy = vi.spyOn(Date, "now").mockReturnValue(t0 + 31_000);
    try {
      expect((await verifyAccessJwt(await sign(claims()), cfg)).ok).toBe(true);
      expect(fetches).toBe(2);
    } finally {
      spy.mockRestore();
    }
  });

  it("fails closed when the certs endpoint fails, and on garbage", async () => {
    setJwksFetcherForTests(async () => {
      throw new Error("certs endpoint returned 500");
    });
    expect(await verifyAccessJwt(await sign(claims()), cfg)).toEqual({ ok: false, reason: "could not fetch the team's signing keys" });
    for (const t of ["", "a.b", "a.b.c", "!!.??.**", "x".repeat(20000)]) expect((await verifyAccessJwt(t, cfg)).ok).toBe(false);
  });

  // The default fetcher runs in workerd, which throws on redirect: "error"; the
  // fake fetchers above never reached that code, so real sign-ins all failed as
  // "malformed token" (found on a fresh account).
  describe("default certs fetcher", () => {
    const jwks = () => new Response(JSON.stringify({ keys: [{ kid: "kid-1", kty: "RSA", alg: "RS256", n: jwk.n, e: jwk.e }] }), { headers: { "content-type": "application/json" } });
    let spy: ReturnType<typeof vi.spyOn>;
    beforeEach(() => setJwksFetcherForTests(null));
    afterEach(() => spy?.mockRestore());

    it("fetches the team's keys with a redirect mode workerd accepts", async () => {
      spy = vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
        expect(String(input)).toBe(`https://${TEAM}/cdn-cgi/access/certs`);
        expect(init?.redirect).toBe("manual");
        return jwks();
      });
      expect((await verifyAccessJwt(await sign(claims()), cfg)).ok).toBe(true);
      expect(spy).toHaveBeenCalledTimes(1);
    });

    it.each([
      ["a redirect", () => new Response(null, { status: 302, headers: { location: "https://evil.example/certs" } })],
      ["a server error", () => new Response("no", { status: 500 })],
      ["HTML instead of JSON", () => new Response("<html>", { status: 200 })],
    ])("fails closed on %s from the certs endpoint", async (_n, reply) => {
      spy = vi.spyOn(globalThis, "fetch").mockImplementation(async () => reply());
      expect(await verifyAccessJwt(await sign(claims()), cfg)).toEqual({ ok: false, reason: "could not fetch the team's signing keys" });
    });

    it("fails closed when the fetch itself throws", async () => {
      spy = vi.spyOn(globalThis, "fetch").mockRejectedValue(new TypeError("Network connection lost."));
      expect(await verifyAccessJwt(await sign(claims()), cfg)).toEqual({ ok: false, reason: "could not fetch the team's signing keys" });
    });
  });
});

const session = (token: string | null, body: unknown = {}, extra: Record<string, string> = {}, over: Partial<Env> = accessEnv) =>
  call("POST", "/v1/human/session", body, { ...(token ? { "cf-access-jwt-assertion": token } : {}), ...extra }, over);

describe("routes with Access off", () => {
  it("answers 404 access_not_configured and leaves the rest of the API alone", async () => {
    const off = { ACCESS_TEAM_DOMAIN: "" as string, ACCESS_AUD: "" as string } as Partial<Env>;
    const r = await call("POST", "/v1/human/session", {}, {}, off);
    expect(r.status).toBe(404);
    expect(r.json.error?.code).toBe("access_not_configured");
    expect((await call("GET", "/v1/human/login?port=5000&state=abcdefghijklmnopq", undefined, {}, off)).json.error?.code).toBe("access_not_configured");
    expect((await call("GET", "/v1/whoami")).status).toBe(200);
  });
});

describe("POST /v1/human/session", () => {
  it("mints a 12 h human key bound to the email, audited as source access", async () => {
    const r = await session(await sign(claims()));
    expect(r.status).toBe(201);
    expect(r.json.data).toMatchObject({ key: expect.stringMatching(/^wgk_/), agent: "person@example.com", kind: "human", source: "access" });
    const hours = (Date.parse(r.json.data.expires_at) - Date.now()) / 3600_000;
    expect(hours).toBeGreaterThan(11.9);
    expect(hours).toBeLessThanOrEqual(12);
    const who = await call("GET", "/v1/whoami", undefined, { authorization: `Bearer ${r.json.data.key}`, "x-wayglass-agent": "someone-else" });
    expect(who.json.data).toMatchObject({ role: "agent", agent: "person@example.com", bound: true, kind: "human", source: "access", expires_at: r.json.data.expires_at });
    const events = (await call("GET", "/v1/keys/events")).json.data.events as { kind: string; key_id: string; actor: string; data: Record<string, unknown> }[];
    expect(events.find((e) => e.key_id === r.json.data.key_id)).toMatchObject({
      kind: "key.created",
      actor: "person@example.com",
      data: { agent: "person@example.com", kind: "human", source: "access", expires_at: r.json.data.expires_at },
    });
    // Stored hashed like every other key.
    await runInDurableObject(registryStub(env), async (_o, state) => {
      expect(JSON.stringify(state.storage.sql.exec("SELECT * FROM keys").toArray())).not.toContain(r.json.data.key);
    });
  });

  it("reads the CF_Authorization cookie too", async () => {
    const r = await call("POST", "/v1/human/session", {}, { cookie: `other=1; CF_Authorization=${await sign(claims())}` }, accessEnv);
    expect(r.status).toBe(201);
  });

  it("honours ttl_hours up to 24 and refuses more", async () => {
    const r = await session(await sign(claims()), { ttl_hours: 24 });
    expect(r.status).toBe(201);
    expect((await session(await sign(claims()), { ttl_hours: 25 })).status).toBe(400);
    expect((await session(await sign(claims()), { ttl_hours: 0 })).status).toBe(400);
    expect((await session(await sign(claims()), { ttl_hours: "12" })).status).toBe(400);
  });

  it("refuses without a valid Access token", async () => {
    expect((await session(null)).json.error?.code).toBe("unauthorized");
    expect((await session(await sign(claims({ aud: ["x"] })))).status).toBe(401);
    expect((await session(await sign(claims({ email: undefined })))).status).toBe(401);
    // An admin key is not an Access sign-in.
    expect((await call("POST", "/v1/human/session", {}, undefined, accessEnv)).status).toBe(401);
  });

  it("refuses the web app's own Access token: each application has its audience", async () => {
    // The web gate accepts ACCESS_WEB_AUD; the sign-in route only ACCESS_AUD. A
    // token for the web app must not mint a key, or the two apps' policies collapse into one.
    const WEB_AUD = "0041c5d882498504b6527bfb449438c32918edd244f4a64779d74c4d384f9d1d";
    const both = { ...accessEnv, ACCESS_WEB_AUD: WEB_AUD } as unknown as Partial<Env>;
    const web = await sign(claims({ aud: [WEB_AUD] }));
    const r = await call("POST", "/v1/human/session", {}, { "cf-access-jwt-assertion": web }, both);
    expect(r.status).toBe(401);
    expect(r.json.error?.message).toMatch(/wrong audience/);
    const { default: worker } = await import("../src/index");
    const res = await worker.fetch(
      new Request("https://wayglass.test/v1/human/login?to=console", { headers: { "cf-access-jwt-assertion": web } }) as Request<unknown, IncomingRequestCfProperties>,
      { ...env, ...both } as Env,
    );
    expect(res.status).toBe(401);
    expect(await res.text()).toContain("wrong audience");
    // Both audiences on one token (one Access app for the whole host) is fine.
    expect((await session(await sign(claims({ aud: [WEB_AUD, AUD] })), {}, {}, both)).status).toBe(201);
  });

  it("refuses an email that cannot be a Wayglass name, minting nothing", async () => {
    const before = ((await call("GET", "/v1/keys/events")).json.data.events as unknown[]).length;
    for (const email of ["o'brien@example.com", "-lead@example.com", `${"x".repeat(64)}@${"y".repeat(70)}.example`]) {
      const r = await session(await sign(claims({ email })));
      expect(r.status, email).toBe(400);
      expect(r.json.error?.message).toMatch(/cannot name a Wayglass identity/);
    }
    expect(((await call("GET", "/v1/keys/events")).json.data.events as unknown[]).length).toBe(before);
  });

  it.each([["12"], [0], [-1], [true], [25]])("refuses ttl_hours %j", async (ttl) => {
    const r = await session(await sign(claims()), { ttl_hours: ttl });
    expect(r.status).toBe(400);
    expect(r.json.error?.message).toMatch(/ttl_hours/);
  });

  it("treats a null ttl_hours as absent (the 12 h default), never as no expiry", async () => {
    const r = await session(await sign(claims()), { ttl_hours: null });
    expect(r.status).toBe(201);
    expect((Date.parse(r.json.data.expires_at) - Date.now()) / 3600_000).toBeLessThanOrEqual(12);
  });

  it("lets a signed-in person end the session, audited as key.revoked by their email", async () => {
    const s = (await session(await sign(claims({ email: "leaving@example.com" })))).json.data;
    const r = await call("POST", "/v1/keys/self/revoke", {}, { authorization: `Bearer ${s.key}` });
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({ id: s.key_id, agent: "leaving@example.com", kind: "human", source: "access", revoked_at: expect.any(String) });
    expect((await call("GET", "/v1/whoami", undefined, { authorization: `Bearer ${s.key}` })).status).toBe(401);
    const events = (await call("GET", "/v1/keys/events")).json.data.events as { kind: string; key_id: string; actor: string }[];
    expect(events.filter((e) => e.key_id === s.key_id).map((e) => [e.kind, e.actor])).toEqual([
      ["key.created", "leaving@example.com"],
      ["key.revoked", "leaving@example.com"],
    ]);
  });

  it("requires a JSON body, so other origins cannot post a form to it", async () => {
    const req = new Request("https://wayglass.test/v1/human/session", { method: "POST", body: "a=b", headers: { "content-type": "application/x-www-form-urlencoded", "cf-access-jwt-assertion": await sign(claims()) } });
    const { default: worker } = await import("../src/index");
    const res = await worker.fetch(req as Request<unknown, IncomingRequestCfProperties>, { ...env, ...accessEnv });
    expect(res.status).toBe(400);
  });

  it("is POST only", async () => {
    expect((await call("GET", "/v1/human/session", undefined, {}, accessEnv)).status).toBe(405);
  });
});

describe("session keys expire", () => {
  it("refuses a key past expires_at and hides it from the key list", async () => {
    const r = await session(await sign(claims({ email: "late@example.com" })));
    const key = r.json.data.key as string;
    await runInDurableObject(registryStub(env), async (_o, state) => {
      state.storage.sql.exec("UPDATE keys SET expires_at = ? WHERE id = ?", new Date(Date.now() - 1000).toISOString(), r.json.data.key_id);
    });
    const who = await call("GET", "/v1/whoami", undefined, { authorization: `Bearer ${key}` });
    expect(who.status).toBe(401);
    expect(who.json.error?.message).toMatch(/expired.*login --human/);
    const live = (await call("GET", "/v1/keys")).json.data.keys as { id: string }[];
    expect(live.some((k) => k.id === r.json.data.key_id)).toBe(false);
    const all = (await call("GET", "/v1/keys?all=1")).json.data.keys as { id: string; source: string }[];
    expect(all.find((k) => k.id === r.json.data.key_id)).toMatchObject({ source: "access" });
  });

  it("leaves admin-minted keys without expiry", async () => {
    const r = await call("POST", "/v1/keys", { agent: "forever-bot" });
    expect(r.json.data.info).toMatchObject({ source: "key", expires_at: null });
  });
});

describe("the human gate with an Access session", () => {
  let fake: FakeRepoHost;
  beforeEach(() => {
    fake = useFakeArtifacts();
  });

  async function pendingHuman() {
    const project = await createProject();
    fake.commit(project, { ".wayglass/policy.yaml": (vectors.policies as Record<string, string>).example!, "web/login.ts": "export function login() {}\n" });
    const d = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["**"], agent: "author-bot" });
    const id = d.json.data.intent.id as string;
    const fork = d.json.data.fork.name as string;
    fake.commit(fork, { ...fake.filesAt(fake.heads.get(fork)!), "web/login.ts": "export function login() { return true; }\n" });
    const p = await call("POST", `/v1/intents/${id}/propose`, { project });
    expect(p.json.data.decision.status).toBe("pending_human");
    return { project, id };
  }

  it("accepts an Access-minted human key and records identity_source access", async () => {
    const { project, id } = await pendingHuman();
    const key = (await session(await sign(claims({ email: "reviewer@example.com" })))).json.data.key as string;
    const r = await call("POST", `/v1/intents/${id}/decide`, { project, verdict: "approve", reason: "Checked the login change", by: "someone-else" }, { authorization: `Bearer ${key}` });
    expect(r.status).toBe(200);
    expect(r.json.data.decision).toMatchObject({ status: "approved", decided_by: "reviewer@example.com" });
    expect(r.json.data.approvals[0]).toMatchObject({ by: "reviewer@example.com", identity_source: "access" });
    const audit = await call("GET", `/v1/audit?project=${project}`);
    const ev = audit.json.data.events.find((e: { kind: string }) => e.kind === "decision.approve");
    expect(ev).toMatchObject({ actor: "reviewer@example.com", data: { identity_source: "access" } });
  });

  it("refuses an expired Access session key", async () => {
    const { project, id } = await pendingHuman();
    const s = (await session(await sign(claims({ email: "gone@example.com" })))).json.data;
    await runInDurableObject(registryStub(env), async (_o, state) => {
      state.storage.sql.exec("UPDATE keys SET expires_at = ? WHERE id = ?", new Date(Date.now() - 1000).toISOString(), s.key_id);
    });
    const r = await call("POST", `/v1/intents/${id}/decide`, { project, verdict: "approve", reason: "Checked the login change" }, { authorization: `Bearer ${s.key}` });
    expect(r.status).toBe(401);
    const after = await call("GET", `/v1/decisions?project=${project}&status=pending_human`);
    expect(after.json.data.decisions[0].approvals).toEqual([]);
  });
});

async function page(path: string, token: string | null) {
  const { default: worker } = await import("../src/index");
  const req = new Request(`https://wayglass.test${path}`, { headers: token ? { "cf-access-jwt-assertion": token } : {} });
  return worker.fetch(req as Request<unknown, IncomingRequestCfProperties>, { ...env, ...accessEnv });
}

describe("GET /v1/human/login", () => {
  const state = "s".repeat(32);

  it("serves a confirm page for the CLI that posts only to the loopback port", async () => {
    const res = await page(`/v1/human/login?port=53682&state=${state}`, await sign(claims()));
    expect(res.status).toBe(200);
    const csp = res.headers.get("content-security-policy")!;
    expect(csp).toContain("form-action http://127.0.0.1:53682");
    expect(csp).toContain("connect-src 'self'");
    expect(csp).toContain("frame-ancestors 'none'");
    expect(res.headers.get("cache-control")).toBe("no-store");
    const html = await res.text();
    expect(html).toContain("person@example.com");
    expect(html).toContain('id="go"');
    // The page mints only when the person clicks; no key is in the HTML.
    expect(html).not.toMatch(/wgk_[0-9a-f]{8}/);
  });

  it("serves the console page that stores the key in tab storage", async () => {
    const res = await page(`/v1/human/login?to=console&project=demo-x`, await sign(claims()));
    expect(res.status).toBe(200);
    const html = await res.text();
    expect(html).toContain("wayglass.console.session");
    expect(res.headers.get("content-security-policy")).toContain("form-action 'none'");
  });

  it.each([
    ["no port", `?state=${state}`],
    ["low port", `?port=80&state=${state}`],
    ["short state", "?port=5000&state=abc"],
    ["bad state", `?port=5000&state=${"<".repeat(20)}`],
    ["bad project", "?to=console&project=../x"],
  ])("refuses %s", async (_n, q) => {
    expect((await page(`/v1/human/login${q}`, await sign(claims()))).status).toBe(400);
  });

  it("refuses without an Access token, escaping the reason", async () => {
    const res = await page(`/v1/human/login?port=5000&state=${state}`, null);
    expect(res.status).toBe(401);
    expect(await res.text()).toContain("Sign-in refused");
  });
});
