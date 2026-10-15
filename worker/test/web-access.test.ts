// The Worker's own Access check on browser-facing responses (src/access.ts
// webAccessGate): static assets, app routes and /v1/public/* need a valid
// Access token for the web app's audience whenever Access is configured. Key
// routes are unaffected, and with every ACCESS_* var empty nothing changes.

import { env } from "cloudflare:workers";
import { afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import { isWebPath, setJwksFetcherForTests, webAccessConfig } from "../src/access";
import worker from "../src/index";
import { KEY, createProject, useFakeArtifacts } from "./helpers";

const TEAM = "wayglass-test.cloudflareaccess.com";
const WEB_AUD = "0041c5d882498504b6527bfb449438c32918edd244f4a64779d74c4d384f9d1d";
const HUMAN_AUD = "4714c1358e65fe4b408ad6d432a5f878f08194bdb4752441fd56faefa9b2b6f2";
const on = { ACCESS_TEAM_DOMAIN: TEAM, ACCESS_AUD: HUMAN_AUD, ACCESS_WEB_AUD: `other-aud, ${WEB_AUD}` } as unknown as Partial<Env>;

const enc = new TextEncoder();
const b64url = (b: Uint8Array | string) =>
  btoa(typeof b === "string" ? b : String.fromCharCode(...b))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
const part = (o: unknown) => b64url(String.fromCharCode(...enc.encode(JSON.stringify(o))));

let signing: CryptoKeyPair;
let jwk: JsonWebKey;
beforeAll(async () => {
  signing = (await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, [
    "sign",
    "verify",
  ])) as CryptoKeyPair;
  jwk = (await crypto.subtle.exportKey("jwk", signing.publicKey)) as JsonWebKey;
});
beforeEach(() => {
  useFakeArtifacts();
  setJwksFetcherForTests(async () => ({ keys: [{ kid: "kid-1", kty: "RSA", alg: "RS256", n: jwk.n, e: jwk.e }] }));
});
afterEach(() => setJwksFetcherForTests(null));

const now = () => Math.floor(Date.now() / 1000);
async function token(aud: string): Promise<string> {
  const payload = { aud: [aud], email: "person@example.com", exp: now() + 600, iat: now() - 5, iss: `https://${TEAM}`, sub: "u1", type: "app" };
  const input = `${part({ alg: "RS256", kid: "kid-1", typ: "JWT" })}.${part(payload)}`;
  const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", signing.privateKey, enc.encode(input)));
  return `${input}.${b64url(sig)}`;
}

const fetchWith = (path: string, headers: Record<string, string>, over: Partial<Env>) =>
  worker.fetch(new Request(`https://wayglass.test${path}`, { headers }) as Request<unknown, IncomingRequestCfProperties>, { ...env, ...over } as Env);

const WEB = ["/", "/console/", "/app.js", "/app.css", "/?project=demo-1", "/no-such-page", "/v1/public/projects", "/v1/public/projects/demo-1/intents"];

describe("web Access gate", () => {
  it.each(WEB)("refuses %s without a token", async (path) => {
    const res = await fetchWith(path, {}, on);
    expect(res.status).toBe(403);
    const body = await res.text();
    if (path.startsWith("/v1/")) expect(JSON.parse(body)).toMatchObject({ v: 1, ok: false, error: { code: "forbidden" } });
    else {
      expect(res.headers.get("content-type")).toContain("text/html");
      expect(body).toContain("Sign in required");
      expect(body).toContain('href="/v1/human/login?to=console"');
      expect(body).not.toContain("app.js");
    }
    expect(res.headers.get("cache-control")).toBe("no-store");
  });

  it.each(WEB)("refuses %s with a garbage token", async (path) => {
    expect((await fetchWith(path, { "cf-access-jwt-assertion": "garbage" }, on)).status).toBe(403);
    expect((await fetchWith(path, { cookie: "CF_Authorization=garbage" }, on)).status).toBe(403);
  });

  it.each(WEB)("refuses %s with the human sign-in app's audience", async (path) => {
    expect((await fetchWith(path, { "cf-access-jwt-assertion": await token(HUMAN_AUD) }, on)).status).toBe(403);
  });

  it.each(["/", "/console/", "/app.js"])("serves %s with a web-app token (header or cookie)", async (path) => {
    const t = await token(WEB_AUD);
    const a = await fetchWith(path, { "cf-access-jwt-assertion": t }, on);
    expect(a.status).toBe(200);
    await a.arrayBuffer();
    const b = await fetchWith(path, { cookie: `x=1; CF_Authorization=${t}` }, on);
    expect(b.status).toBe(200);
    await b.arrayBuffer();
  });

  it("lets a web-app token reach /v1/public/* (which then applies its own gates)", async () => {
    const res = await fetchWith("/v1/public/projects/nope/intents", { "cf-access-jwt-assertion": await token(WEB_AUD) }, on);
    expect(res.status).toBe(404);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("not_found");
  });

  it("leaves the key API alone", async () => {
    const project = await createProject();
    const ok = await fetchWith("/v1/whoami", { authorization: `Bearer ${KEY}` }, on);
    expect(ok.status).toBe(200);
    await ok.arrayBuffer();
    const listed = await fetchWith("/v1/projects", { authorization: `Bearer ${KEY}` }, on);
    expect(listed.status).toBe(200);
    expect(JSON.stringify(await listed.json())).toContain(project);
    const anon = await fetchWith("/v1/whoami", {}, on);
    expect(anon.status).toBe(401);
    await anon.arrayBuffer();
  });

  it("fails closed when Access is half configured", async () => {
    for (const over of [{ ACCESS_WEB_AUD: WEB_AUD }, { ACCESS_TEAM_DOMAIN: TEAM }, { ACCESS_TEAM_DOMAIN: "not a host", ACCESS_WEB_AUD: WEB_AUD }]) {
      const res = await fetchWith("/", { "cf-access-jwt-assertion": await token(WEB_AUD) }, over as unknown as Partial<Env>);
      expect(res.status).toBe(403);
      expect(await res.text()).toContain("misconfigured");
    }
  });

  it("falls back to ACCESS_AUD when ACCESS_WEB_AUD is empty (one Access app for the whole host)", async () => {
    const one = { ACCESS_TEAM_DOMAIN: TEAM, ACCESS_AUD: HUMAN_AUD, ACCESS_WEB_AUD: "" } as unknown as Partial<Env>;
    const res = await fetchWith("/", { "cf-access-jwt-assertion": await token(HUMAN_AUD) }, one);
    expect(res.status).toBe(200);
    await res.arrayBuffer();
    expect((await fetchWith("/", {}, one)).status).toBe(403);
  });

  it("changes nothing with every ACCESS_* var empty (self-host default)", async () => {
    expect(webAccessConfig(env)).toEqual({ on: false });
    const res = await fetchWith("/", {}, {});
    expect(res.status).toBe(200);
    await res.arrayBuffer();
    const pub = await fetchWith("/v1/public/projects/nope/intents", {}, {});
    expect(pub.status).toBe(404);
    await pub.arrayBuffer();
  });

  it.each([
    ["/", true],
    ["/console/", true],
    ["/v1public", true],
    ["/v1/public", true],
    ["/v1/public/projects", true],
    ["/v1", false],
    ["/v1/projects", false],
    ["/v1/human/login", false],
    ["/v1/publicx", false],
  ])("isWebPath(%s) is %s", (p, want) => expect(isWebPath(p)).toBe(want));
});
