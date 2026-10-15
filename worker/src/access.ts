// Cloudflare Access sign-in for people. Agents keep their keys; a person who
// signs in through Access gets a short-lived human key (src/human.ts). This
// module only verifies the Access application token: RS256, signed by one of
// the team's keys (https://<team>/cdn-cgi/access/certs), issued by the team
// domain, for this application's audience, unexpired, naming an email.
// Every error fails closed.
//
// Sign-in is off unless both ACCESS_TEAM_DOMAIN and ACCESS_AUD are set
// (wrangler.jsonc vars). The web gate (webAccessGate) is on as soon as any
// ACCESS_* var is set.

export interface AccessConfig {
  /** Bare host, e.g. "acme.cloudflareaccess.com". */
  teamDomain: string;
  /** The Access application's audience (AUD) tag. */
  aud: string;
}

/** Settings for verifying a token against one or more allowed audiences. */
export interface VerifyConfig {
  teamDomain: string;
  aud: string | readonly string[];
}

export interface AccessClaims {
  email: string;
  sub: string;
  exp: number;
}

export type Verified = { ok: true; claims: AccessClaims } | { ok: false; reason: string };

const HOST = /^(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/;
const AUD = /^[A-Za-z0-9._-]{1,256}$/;
/** Clock skew allowed on exp, nbf and iat. */
export const SKEW_SECONDS = 60;
/** How long fetched team keys are trusted before a refetch. */
export const JWKS_TTL_MS = 60 * 60 * 1000;
/** An unknown kid refetches the keys at most this often, so junk tokens cannot hammer the team domain. */
const REFETCH_MIN_MS = 30 * 1000;
const MAX_TOKEN = 16 * 1024;

/** The Access settings, or null when the feature is off (either var unset or malformed). */
export function accessConfig(env: Env): AccessConfig | null {
  const rawTeam = (env.ACCESS_TEAM_DOMAIN ?? "").trim().toLowerCase().replace(/^https:\/\//, "").replace(/\/+$/, "");
  const aud = (env.ACCESS_AUD ?? "").trim();
  if (!rawTeam || !aud) return null;
  if (!HOST.test(rawTeam) || !AUD.test(aud)) return null;
  return { teamDomain: rawTeam, aud };
}

/** The Access token on a request: the Cf-Access-Jwt-Assertion header Access adds, else the CF_Authorization cookie. */
export function accessToken(req: Request): string | null {
  const h = req.headers.get("cf-access-jwt-assertion");
  if (h) return h.trim();
  const cookie = req.headers.get("cookie") ?? "";
  for (const part of cookie.split(";")) {
    const i = part.indexOf("=");
    if (i < 0) continue;
    if (part.slice(0, i).trim() === "CF_Authorization") return part.slice(i + 1).trim() || null;
  }
  return null;
}

interface Jwk {
  kid?: unknown;
  kty?: unknown;
  alg?: unknown;
  n?: unknown;
  e?: unknown;
}

type JwksFetcher = (url: string) => Promise<{ keys?: Jwk[] }>;

// workerd accepts only "follow" or "manual" for redirect ("error" throws), so
// redirects are refused by hand: any non-2xx, 3xx included, is a failure.
const defaultFetcher: JwksFetcher = async (url) => {
  let res: Response;
  try {
    res = await fetch(url, { headers: { accept: "application/json" }, redirect: "manual" });
  } catch (err) {
    throw new Error(`certs endpoint fetch failed: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (!res.ok) throw new Error(`certs endpoint returned ${res.status}`);
  try {
    return (await res.json()) as { keys?: Jwk[] };
  } catch {
    throw new Error("certs endpoint returned no JSON");
  }
};

let fetcher: JwksFetcher = defaultFetcher;

// Per isolate: keys by team domain. Holds only public keys.
interface Cached {
  keys: Map<string, CryptoKey>;
  fetchedAt: number;
}
const cache = new Map<string, Cached>();

/** Tests swap in a fake JWKS; null restores the network fetcher. Also clears the key cache. */
export function setJwksFetcherForTests(f: JwksFetcher | null): void {
  fetcher = f ?? defaultFetcher;
  cache.clear();
}

async function loadKeys(teamDomain: string): Promise<Cached> {
  const body = await fetcher(`https://${teamDomain}/cdn-cgi/access/certs`);
  const keys = new Map<string, CryptoKey>();
  for (const k of Array.isArray(body?.keys) ? body.keys : []) {
    if (typeof k.kid !== "string" || k.kty !== "RSA" || typeof k.n !== "string" || typeof k.e !== "string") continue;
    if (k.alg !== undefined && k.alg !== "RS256") continue;
    try {
      const key = await crypto.subtle.importKey("jwk", { kty: "RSA", n: k.n, e: k.e, alg: "RS256", ext: true }, { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" }, false, ["verify"]);
      keys.set(k.kid, key);
    } catch {
      // A malformed key is skipped; tokens signed by it will not verify.
    }
  }
  const c = { keys, fetchedAt: Date.now() };
  cache.set(teamDomain, c);
  return c;
}

async function keyFor(teamDomain: string, kid: string): Promise<CryptoKey | null> {
  let c = cache.get(teamDomain);
  const fresh = c && Date.now() - c.fetchedAt < JWKS_TTL_MS;
  if (!c || !fresh) c = await loadKeys(teamDomain);
  else if (!c.keys.has(kid) && Date.now() - c.fetchedAt >= REFETCH_MIN_MS) c = await loadKeys(teamDomain); // rotated keys
  return c.keys.get(kid) ?? null;
}

function b64urlBytes(s: string): Uint8Array {
  if (!/^[A-Za-z0-9_-]*$/.test(s)) throw new Error("not base64url");
  const pad = s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4);
  const bin = atob(pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function b64urlJson(s: string): Record<string, unknown> {
  const v: unknown = JSON.parse(new TextDecoder().decode(b64urlBytes(s)));
  if (typeof v !== "object" || v === null || Array.isArray(v)) throw new Error("not a JSON object");
  return v as Record<string, unknown>;
}

const EMAIL = /^[^\s@<>"]{1,64}@[^\s@<>"]{1,190}$/;

/** Verifies an Access application token. Never throws: any problem is a refusal. */
export async function verifyAccessJwt(token: string, cfg: VerifyConfig, nowSeconds = Math.floor(Date.now() / 1000)): Promise<Verified> {
  try {
    if (!token || token.length > MAX_TOKEN) return { ok: false, reason: "malformed token" };
    const parts = token.split(".");
    if (parts.length !== 3) return { ok: false, reason: "malformed token" };
    const [h, p, s] = parts as [string, string, string];
    const header = b64urlJson(h);
    // Only RS256: never "none", never HS256 (which would use a public key as an HMAC secret).
    if (header.alg !== "RS256") return { ok: false, reason: `algorithm ${String(header.alg)} is not accepted` };
    if (typeof header.kid !== "string" || !header.kid) return { ok: false, reason: "token has no key id" };
    const key = await keyFor(cfg.teamDomain, header.kid);
    if (!key) return { ok: false, reason: "token was not signed by a key of this team" };
    const signed = new TextEncoder().encode(`${h}.${p}`);
    const valid = await crypto.subtle.verify("RSASSA-PKCS1-v1_5", key, b64urlBytes(s), signed);
    if (!valid) return { ok: false, reason: "bad signature" };

    const c = b64urlJson(p);
    if (c.iss !== `https://${cfg.teamDomain}`) return { ok: false, reason: "wrong issuer" };
    const aud = Array.isArray(c.aud) ? c.aud : [c.aud];
    const allowed: readonly string[] = typeof cfg.aud === "string" ? [cfg.aud] : cfg.aud;
    if (allowed.length === 0 || !aud.some((a) => typeof a === "string" && allowed.includes(a))) return { ok: false, reason: "wrong audience" };
    if (typeof c.exp !== "number" || c.exp + SKEW_SECONDS <= nowSeconds) return { ok: false, reason: "token expired" };
    if (c.nbf !== undefined && (typeof c.nbf !== "number" || c.nbf - SKEW_SECONDS > nowSeconds)) return { ok: false, reason: "token not yet valid" };
    if (c.iat !== undefined && (typeof c.iat !== "number" || c.iat - SKEW_SECONDS > nowSeconds)) return { ok: false, reason: "token issued in the future" };
    if (c.type !== undefined && c.type !== "app") return { ok: false, reason: "not an application token" };
    // Service tokens carry no email: they are machines, and machines use agent keys.
    if (typeof c.email !== "string" || !EMAIL.test(c.email)) return { ok: false, reason: "token names no person (no email claim)" };
    return { ok: true, claims: { email: c.email.toLowerCase(), sub: typeof c.sub === "string" ? c.sub : "", exp: c.exp } };
  } catch (err) {
    if (err instanceof Error && /certs endpoint/.test(err.message)) {
      console.error(`access: ${err.message}`);
      return { ok: false, reason: "could not fetch the team's signing keys" };
    }
    return { ok: false, reason: "malformed token" };
  }
}

// ---- web gate ---------------------------------------------------------------
//
// Defense in depth behind the Access application that covers the web app: the
// Worker itself refuses to serve a browser-facing response (static assets,
// app routes, /v1/public/*) without a valid Access token for the web app's
// audience. Key-authenticated /v1/* routes and /v1/human/* (own audience,
// src/human.ts) are not gated here.
//
//   all ACCESS_* empty            off: today's self-host behaviour
//   ACCESS_WEB_AUD set            allowed audiences (comma list)
//   ACCESS_WEB_AUD empty          falls back to ACCESS_AUD (one app for the whole host)
//   team domain missing or bad,
//   or no valid audience          misconfigured: refuse everything (fail closed)

export type WebGate = { on: false } | { on: true; cfg: VerifyConfig | null };

type WebEnv = Env & { ACCESS_WEB_AUD?: string };

export function webAccessConfig(env: Env): WebGate {
  const e = env as WebEnv;
  const team = (e.ACCESS_TEAM_DOMAIN ?? "").trim().toLowerCase().replace(/^https:\/\//, "").replace(/\/+$/, "");
  const web = (e.ACCESS_WEB_AUD ?? "").trim();
  const human = (e.ACCESS_AUD ?? "").trim();
  if (!team && !web && !human) return { on: false };
  const auds = (web || human).split(",").map((a) => a.trim()).filter(Boolean);
  if (!HOST.test(team) || auds.length === 0 || !auds.every((a) => AUD.test(a))) return { on: true, cfg: null };
  return { on: true, cfg: { teamDomain: team, aud: auds } };
}

/** Whether a path is browser-facing (static assets, app routes, the public view) rather than the key API or sign-in. */
export function isWebPath(pathname: string): boolean {
  if (pathname === "/v1/public" || pathname.startsWith("/v1/public/")) return true;
  return !(pathname === "/v1" || pathname.startsWith("/v1/"));
}

/** null when the request may proceed; otherwise the 403 to send instead. */
export async function webAccessGate(req: Request, env: Env): Promise<Response | null> {
  const gate = webAccessConfig(env);
  if (!gate.on) return null;
  const url = new URL(req.url);
  if (!gate.cfg) return refuse(url, "Cloudflare Access is misconfigured on this Worker (ACCESS_TEAM_DOMAIN, ACCESS_WEB_AUD)", false);
  const signIn = accessConfig(env) !== null;
  const token = accessToken(req);
  if (!token) return refuse(url, "sign in through Cloudflare Access to open this page", signIn);
  const v = await verifyAccessJwt(token, gate.cfg);
  if (!v.ok) return refuse(url, `Cloudflare Access token refused: ${v.reason}`, signIn);
  return null;
}

const NO_STORE = { "cache-control": "no-store", "x-content-type-options": "nosniff", "referrer-policy": "no-referrer" };

function refuse(url: URL, message: string, signIn: boolean): Response {
  if (url.pathname.startsWith("/v1/")) {
    return new Response(JSON.stringify({ v: 1, ok: false, error: { code: "forbidden", message } }), {
      status: 403,
      headers: { ...NO_STORE, "content-type": "application/json; charset=utf-8" },
    });
  }
  const esc = message.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
  return new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Wayglass: sign in</title></head>` +
      `<body><main><h1>Sign in required</h1><p>${esc}</p>` +
      // The sign-in route sits behind the Access application for people (src/human.ts).
      (signIn ? `<p><a href="/v1/human/login?to=console">Sign in with Cloudflare Access</a></p>` : "") +
      `</main></body></html>`,
    {
      status: 403,
      headers: {
        ...NO_STORE,
        "content-type": "text/html; charset=utf-8",
        "content-security-policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
        "x-frame-options": "DENY",
      },
    },
  );
}
