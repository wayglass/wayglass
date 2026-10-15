import { env } from "cloudflare:workers";
import { SELF } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import { call, KEY, useFakeArtifacts } from "./helpers";

beforeEach(() => {
  useFakeArtifacts();
});

describe("auth", () => {
  it("rejects a missing bearer token with the error envelope", async () => {
    const r = await call("GET", "/v1/intents?project=x", undefined, {});
    expect(r.status).toBe(401);
    expect(r.json).toEqual({ v: 1, ok: false, error: { code: "unauthorized", message: "missing bearer token" } });
  });

  it("rejects a wrong key", async () => {
    const r = await call("GET", "/v1/intents?project=x", undefined, { authorization: "Bearer nope" });
    expect(r.status).toBe(401);
    expect(r.json.error?.code).toBe("unauthorized");
  });

  it("fails closed when the admin key is not configured", async () => {
    const { default: worker } = await import("../src/index");
    const res = await worker.fetch(
      new Request("https://wayglass.test/v1/intents?project=x", { headers: { authorization: `Bearer ${KEY}` } }) as Request<unknown, IncomingRequestCfProperties>,
      { ...env, WAYGLASS_ADMIN_KEY: undefined },
    );
    expect(res.status).toBe(503);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("not_configured");
  });
});

describe("routing", () => {
  it.each([
    ["GET", "/v1/nope", 404, "not_found"],
    ["PATCH", "/v1/projects", 405, "method_not_allowed"],
    ["DELETE", "/v1/intents", 405, "method_not_allowed"],
    ["GET", "/v1/intents/i0123456789/push", 405, "method_not_allowed"],
  ])("%s %s -> %i %s", async (method, path, status, code) => {
    const r = await call(method, path);
    expect(r.status).toBe(status);
    expect(r.json.v).toBe(1);
    expect(r.json.ok).toBe(false);
    expect(r.json.error?.code).toBe(code);
  });

  it("returns bad_request for invalid JSON", async () => {
    const r = await call("POST", "/v1/projects", "{not json");
    expect(r.status).toBe(400);
    expect(r.json.error?.code).toBe("bad_request");
  });

  it("returns bad_request when project is missing", async () => {
    const r = await call("GET", "/v1/intents");
    expect(r.status).toBe(400);
    expect(r.json.error?.message).toMatch(/project is required/);
  });

  it("rejects project names containing the fork separator", async () => {
    const r = await call("POST", "/v1/projects", { name: "a--b" });
    expect(r.status).toBe(400);
  });

  it("validates ids in the path", async () => {
    expect((await call("POST", "/v1/intents/../push", { project: "x", paths: [] })).status).toBe(404);
    expect((await call("POST", "/v1/intents/bad/push", { project: "x", paths: [] })).json.error?.code).toBe("bad_request");
    expect((await call("POST", "/v1/decisions/bad", { project: "x" })).json.error?.code).toBe("bad_request");
  });

  it("returns not_found for an unknown project", async () => {
    const r = await call("GET", "/v1/intents?project=does-not-exist");
    expect(r.status).toBe(404);
    expect(r.json).toEqual({ v: 1, ok: false, error: { code: "not_found", message: "project not found" } });
  });

  it("serves the static site outside /v1 without auth", async () => {
    const res = await SELF.fetch("https://wayglass.test/");
    expect(res.status).toBe(200);
    expect(await res.text()).toMatch(/Wayglass/);
  });

  it("uses the success envelope with JSON content type", async () => {
    const r = await call("POST", "/v1/projects", { name: `env-${crypto.randomUUID().slice(0, 8)}` });
    expect(r.status).toBe(201);
    expect(r.json.v).toBe(1);
    expect(r.json.ok).toBe(true);
    expect(r.json.error).toBeUndefined();
    expect(Object.keys(r.json).sort()).toEqual(["data", "ok", "v"]);
  });
});
