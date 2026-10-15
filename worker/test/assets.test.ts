// The audit view in web/ is served by this Worker as static assets
// (wrangler.jsonc "assets"); /v1/* always reaches the API first.

import { describe, expect, it } from "vitest";
import { env } from "cloudflare:workers";
import worker from "../src/index";

const get = (path: string) => worker.fetch(new Request(`https://wayglass.test${path}`) as Request<unknown, IncomingRequestCfProperties>, env);

describe("static web app", () => {
  it.each([
    ["/", "text/html", 'src="/app.js"'],
    ["/app.js", "javascript", "/v1/public/projects/"],
    ["/app.css", "text/css", "prefers-color-scheme"],
    ["/console/", "text/html", 'src="/console/redirect.js"'],
    ["/console/redirect.js", "javascript", "location.replace"],
    ["/vendor.js", "javascript", "parseYAML"],
    ["/ui.js", "javascript", "createTextNode"],
  ])("serves %s", async (path, type, snippet) => {
    const res = await get(path);
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toContain(type);
    expect(await res.text()).toContain(snippet);
  });

  it.each(["/", "/console/"])("restricts %s to same-origin scripts and connections", async (path) => {
    const res = await get(path);
    const csp = res.headers.get("content-security-policy");
    for (const rule of ["default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'none'"]) expect(csp).toContain(rule);
    expect(csp).not.toContain("unsafe-inline");
    expect(res.headers.get("cache-control")).toContain("no-store");
    expect(res.headers.get("referrer-policy")).toBe("no-referrer");
  });

  it("never serves the API from assets", async () => {
    const res = await get("/v1/public/projects/nope");
    expect(res.headers.get("content-type")).toContain("application/json");
    expect(res.status).toBe(404);
  });
});
