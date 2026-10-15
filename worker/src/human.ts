// People sign in through Cloudflare Access; agents keep their keys.
//
//   POST /v1/human/session   needs a valid Access token; mints a short-lived
//                            human key bound to the token's email.
//   GET  /v1/human/login     a page that calls the session route, then hands
//                            the key to the CLI's loopback listener
//                            (?port=&state=) or to the web console's tab
//                            storage (?to=console).
//
// Both paths sit behind a self-hosted Access application, which does the
// identity-provider login and adds Cf-Access-Jwt-Assertion. The Worker still
// verifies that token itself (src/access.ts): a request that reaches it some
// other way is refused. With ACCESS_TEAM_DOMAIN or ACCESS_AUD unset, every
// route here answers 404 access_not_configured.

import { accessConfig, accessToken, type AccessConfig, type AccessClaims, verifyAccessJwt } from "./access";
import { fail, fromResult } from "./envelope";
import { AGENT_NAME, registryStub } from "./keys";

export const HUMAN_PREFIX = "/v1/human/";
export const SESSION_DEFAULT_HOURS = 12;
export const SESSION_MAX_HOURS = 24;

const STATE = /^[A-Za-z0-9_-]{16,128}$/;
const PROJECT = /^[a-z0-9](?:[a-z0-9._]|-(?!-)){0,62}$/;

async function claimsOf(req: Request, cfg: AccessConfig): Promise<AccessClaims | Response> {
  const token = accessToken(req);
  if (!token) return fail("unauthorized", "no Cloudflare Access token on the request; this path must sit behind the Access application");
  const v = await verifyAccessJwt(token, cfg);
  if (!v.ok) return fail("unauthorized", `Cloudflare Access token refused: ${v.reason}`);
  return v.claims;
}

export async function handleHuman(req: Request, env: Env): Promise<Response> {
  const cfg = accessConfig(env);
  if (!cfg) return fail("access_not_configured", "Cloudflare Access sign-in is not configured on this Worker (ACCESS_TEAM_DOMAIN and ACCESS_AUD); use a key from your administrator");
  const url = new URL(req.url);
  if (url.pathname === "/v1/human/session") {
    if (req.method !== "POST") return fail("method_not_allowed", `${req.method} not allowed on ${url.pathname}`);
    return session(req, env, cfg);
  }
  if (url.pathname === "/v1/human/login") {
    if (req.method !== "GET") return fail("method_not_allowed", `${req.method} not allowed on ${url.pathname}`);
    return login(req, cfg, url);
  }
  return fail("not_found", `no route for ${url.pathname}`);
}

async function session(req: Request, env: Env, cfg: AccessConfig): Promise<Response> {
  // A JSON body forces a CORS preflight from other origins, which this route never answers.
  if (!(req.headers.get("content-type") ?? "").toLowerCase().startsWith("application/json")) {
    return fail("bad_request", "send a JSON body (content-type: application/json)");
  }
  const claims = await claimsOf(req, cfg);
  if (claims instanceof Response) return claims;
  let body: unknown;
  try {
    body = await req.json();
  } catch {
    return fail("bad_request", "request body must be JSON");
  }
  if (typeof body !== "object" || body === null || Array.isArray(body)) return fail("bad_request", "request body must be a JSON object");
  const ttl = (body as { ttl_hours?: unknown }).ttl_hours ?? SESSION_DEFAULT_HOURS;
  if (typeof ttl !== "number" || !Number.isFinite(ttl) || ttl <= 0 || ttl > SESSION_MAX_HOURS) {
    return fail("bad_request", `ttl_hours must be a number above 0 and at most ${SESSION_MAX_HOURS}`);
  }
  if (!AGENT_NAME.test(claims.email)) return fail("bad_request", "this email cannot name a Wayglass identity (letters, digits, '.', '_', '@', '+', '-' only)");
  const expiresAt = new Date(Date.now() + Math.round(ttl * 3600) * 1000).toISOString();
  const r = await registryStub(env).create(claims.email, claims.email, "Cloudflare Access session", "human", { source: "access", expiresAt });
  if (!r.ok) return fromResult(r);
  return fromResult(
    {
      ok: true,
      data: { key: r.data.key, key_id: r.data.info.id, agent: claims.email, kind: "human", source: "access", expires_at: expiresAt },
    },
    201,
  );
}

const esc = (s: string) => s.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
/** JSON safe to embed in a <script type="application/json"> element. */
const embed = (v: unknown) => JSON.stringify(v).replace(/</g, "\\u003c").replace(/>/g, "\\u003e").replace(/&/g, "\\u0026");

function page(status: number, title: string, bodyHtml: string, opts: { nonce: string; formAction?: string }): Response {
  const csp = [
    "default-src 'none'",
    `script-src 'nonce-${opts.nonce}'`,
    `style-src 'nonce-${opts.nonce}'`,
    "connect-src 'self'",
    `form-action ${opts.formAction ?? "'none'"}`,
    "base-uri 'none'",
    "frame-ancestors 'none'",
  ].join("; ");
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer"><title>${esc(title)}</title>
<style nonce="${opts.nonce}">
:root{color-scheme:light dark;--bg:#fafaf9;--fg:#1c1917;--muted:#57534e;--accent:#1c1917;--accent-fg:#fafaf9}
@media (prefers-color-scheme:dark){:root{--bg:#1c1917;--fg:#f5f5f4;--muted:#a8a29e;--accent:#f5f5f4;--accent-fg:#1c1917}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{max-width:32rem;margin:15vh auto;padding:0 16px}
p{color:var(--muted)}button{font:inherit;padding:.6rem 1.2rem;border:0;border-radius:.4rem;background:var(--accent);color:var(--accent-fg);cursor:pointer}
button[disabled]{opacity:.5;cursor:default}code{overflow-wrap:anywhere}
</style></head><body><main>${bodyHtml}</main></body></html>`;
  return new Response(html, {
    status,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "no-store",
      "content-security-policy": csp,
      "referrer-policy": "no-referrer",
      "x-content-type-options": "nosniff",
      "x-frame-options": "DENY",
    },
  });
}

function nonce(): string {
  return btoa(String.fromCharCode(...crypto.getRandomValues(new Uint8Array(16))));
}

// The page script. It reads its settings from #cfg, asks the session route
// for a key (same origin; Access adds its token), then either posts the key to
// the CLI's loopback listener in a form body (never a URL) or stores it in
// this tab's session storage for the web console.
const SCRIPT = `
const cfg = JSON.parse(document.getElementById("cfg").textContent);
const msg = document.getElementById("msg");
const btn = document.getElementById("go");
async function mint() {
  const res = await fetch("/v1/human/session", { method: "POST", headers: { "content-type": "application/json", accept: "application/json" },
    body: "{}", credentials: "same-origin", redirect: "error", cache: "no-store" });
  const env = await res.json();
  if (!res.ok || !env.ok) throw new Error(env.error ? env.error.message : "sign-in failed (" + res.status + ")");
  return env.data;
}
async function run() {
  if (btn) btn.disabled = true;
  msg.textContent = "Signing in...";
  try {
    const s = await mint();
    if (cfg.mode === "cli") {
      const f = document.createElement("form");
      f.method = "POST";
      f.action = "http://127.0.0.1:" + cfg.port + "/callback";
      for (const [k, v] of Object.entries({ state: cfg.state, key: s.key, agent: s.agent, expires_at: s.expires_at })) {
        const i = document.createElement("input");
        i.type = "hidden"; i.name = k; i.value = v; f.appendChild(i);
      }
      document.body.appendChild(f);
      msg.textContent = "Handing the session to the CLI...";
      f.submit();
    } else {
      sessionStorage.setItem("wayglass.console.session", JSON.stringify({ key: s.key, name: s.agent, source: "access", expires_at: s.expires_at }));
      sessionStorage.removeItem("wayglass.console.signed-out");
      location.replace("/" + (cfg.project ? "?project=" + encodeURIComponent(cfg.project) : "") + (cfg.project ? "#/review" : "#/projects"));
    }
  } catch (e) {
    msg.textContent = e.message;
    if (btn) btn.disabled = false;
  }
}
if (btn) btn.addEventListener("click", run); else run();
`;

async function login(req: Request, cfg: AccessConfig, url: URL): Promise<Response> {
  const n = nonce();
  const claims = await claimsOf(req, cfg);
  if (claims instanceof Response) {
    const err = ((await claims.json()) as { error: { message: string } }).error.message;
    return page(401, "Wayglass sign-in", `<h1>Sign-in refused</h1><p>${esc(err)}</p>`, { nonce: n });
  }
  const to = url.searchParams.get("to");
  if (to === "console") {
    const project = url.searchParams.get("project") ?? "";
    if (project && !PROJECT.test(project)) return page(400, "Wayglass sign-in", "<h1>Bad request</h1><p>Invalid project name.</p>", { nonce: n });
    const body =
      `<h1>Wayglass</h1><p>Signing in to the web console as <strong>${esc(claims.email)}</strong>...</p><p id="msg" role="status"></p>` +
      `<script type="application/json" id="cfg">${embed({ mode: "console", project })}</script><script nonce="${n}">${SCRIPT}</script>`;
    return page(200, "Wayglass sign-in", body, { nonce: n });
  }
  const port = Number(url.searchParams.get("port"));
  const state = url.searchParams.get("state") ?? "";
  if (!Number.isInteger(port) || port < 1024 || port > 65535 || !STATE.test(state)) {
    return page(400, "Wayglass sign-in", "<h1>Bad request</h1><p>Start sign-in with <code>wayglass login --human</code>.</p>", { nonce: n });
  }
  // A person confirms: a link alone must not hand a session to whatever listens on that port.
  const body =
    `<h1>Sign in the Wayglass CLI</h1><p>Signed in to Cloudflare Access as <strong>${esc(claims.email)}</strong>. ` +
    `Continue to give the <code>wayglass</code> command waiting on this computer (port ${port}) a human session.</p>` +
    `<p><button id="go" type="button">Continue</button></p><p id="msg" role="status"></p>` +
    `<script type="application/json" id="cfg">${embed({ mode: "cli", port, state })}</script><script nonce="${n}">${SCRIPT}</script>`;
  return page(200, "Wayglass sign-in", body, { nonce: n, formAction: `http://127.0.0.1:${port}` });
}
