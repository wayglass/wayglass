import { env } from "cloudflare:workers";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { setRepoHostForTests } from "../src/artifacts";
import { setSubscriptionFetchForTests, subscribeRepoPushes } from "../src/subscriptions";
import { FakeRepoHost } from "./fake-artifacts";
import { call, uniqueProject, useFakeArtifacts } from "./helpers";

const configured: Partial<Env> = {
  CF_ACCOUNT_ID: "acct123",
  CF_EVENTS_API_TOKEN: "events-token-secret",
};

interface Seen {
  url: string;
  method: string;
  auth: string | null;
  body: any;
}

function fakeFetch(respond: () => Response): { seen: Seen[] } {
  const seen: Seen[] = [];
  setSubscriptionFetchForTests((async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers = new Headers(init?.headers);
    seen.push({ url: String(input), method: init?.method ?? "GET", auth: headers.get("authorization"), body: JSON.parse(String(init?.body)) });
    return respond();
  }) as typeof fetch);
  return { seen };
}

const cfOk = (id: string) => Response.json({ success: true, errors: [], messages: [], result: { id } });

beforeEach(() => {
  useFakeArtifacts();
});
afterEach(() => {
  setSubscriptionFetchForTests(null);
  setRepoHostForTests(null);
});

describe("subscribeRepoPushes", () => {
  it("posts the wrangler-shaped request for one repo's pushed events", async () => {
    const { seen } = fakeFetch(() => cfOk("sub-1"));
    const r = await subscribeRepoPushes({ ...env, ...configured } as Env, "wayglass", "proj");
    expect(r).toEqual({ subscribed: true, id: "sub-1" });
    expect(seen).toHaveLength(1);
    expect(seen[0]!.method).toBe("POST");
    expect(seen[0]!.url).toBe("https://api.cloudflare.com/client/v4/accounts/acct123/event_subscriptions/subscriptions");
    expect(seen[0]!.auth).toBe("Bearer events-token-secret");
    expect(seen[0]!.body).toEqual({
      name: "wayglass proj pushed",
      enabled: true,
      source: { type: "artifacts.repo", namespace: "wayglass", repo_name: "proj" },
      destination: { type: "queues.queue", queue_id: env.ARTIFACTS_EVENTS_QUEUE_ID },
      events: ["pushed"],
    });
  });

  it("does not call the API when secrets are missing", async () => {
    const { seen } = fakeFetch(() => cfOk("never"));
    const r = await subscribeRepoPushes({ ...env, CF_ACCOUNT_ID: undefined, CF_EVENTS_API_TOKEN: "" } as Env, "wayglass", "proj");
    expect(r).toEqual({ subscribed: false, reason: "not_configured", detail: "missing CF_ACCOUNT_ID, CF_EVENTS_API_TOKEN" });
    expect(seen).toHaveLength(0);
  });

  it.each([
    ["an API error", () => Response.json({ success: false, errors: [{ code: 10000, message: "Authentication error" }], result: null }, { status: 403 }), "HTTP 403: 10000 Authentication error"],
    ["a non-JSON body", () => new Response("bad gateway", { status: 502 }), "HTTP 502"],
    ["success without an id", () => Response.json({ success: true, result: {} }), "HTTP 200"],
  ])("reports %s without throwing or echoing the token", async (_name, respond, detail) => {
    fakeFetch(respond);
    const r = await subscribeRepoPushes({ ...env, ...configured } as Env, "wayglass", "proj");
    expect(r).toEqual({ subscribed: false, reason: "api_error", detail });
    expect(JSON.stringify(r)).not.toContain("events-token-secret");
  });

  it("reports a network failure without throwing", async () => {
    setSubscriptionFetchForTests((async () => {
      throw new Error("connection reset");
    }) as typeof fetch);
    const r = await subscribeRepoPushes({ ...env, ...configured } as Env, "wayglass", "proj");
    expect(r).toEqual({ subscribed: false, reason: "api_error", detail: "connection reset" });
  });
});

describe("POST /v1/projects trunk events", () => {
  const auth = { authorization: "Bearer test-admin-key" };

  it("subscribes the new trunk and records it in the audit log", async () => {
    const { seen } = fakeFetch(() => cfOk("sub-42"));
    const name = uniqueProject();
    const r = await call("POST", "/v1/projects", { name }, auth, configured);
    expect(r.status).toBe(201);
    expect(r.json.data.events).toEqual({ subscribed: true, subscription_id: "sub-42" });
    expect(seen[0]!.body.source.repo_name).toBe(r.json.data.trunk.name);
    const audit = await call("GET", `/v1/audit?project=${name}`);
    const ev = audit.json.data.events.find((e: { kind: string }) => e.kind === "trunk.events_subscribed");
    expect(ev?.data).toEqual({ repo: r.json.data.trunk.name, subscription_id: "sub-42" });
  });

  it("still creates the project when the subscription fails, and says why", async () => {
    fakeFetch(() => Response.json({ success: false, errors: [{ code: 10000, message: "Authentication error" }] }, { status: 403 }));
    const name = uniqueProject();
    const r = await call("POST", "/v1/projects", { name }, auth, configured);
    expect(r.status).toBe(201);
    expect(r.json.data.events).toEqual({ subscribed: false, reason: "api_error", detail: "HTTP 403: 10000 Authentication error" });
    const audit = await call("GET", `/v1/audit?project=${name}`);
    expect(audit.json.data.events.map((e: { kind: string }) => e.kind)).toContain("trunk.events_unsubscribed");
  });

  it("creates an eu project's trunk and subscription in the eu namespace only", async () => {
    const us = new FakeRepoHost();
    const eu = new FakeRepoHost();
    setRepoHostForTests(us, "us");
    setRepoHostForTests(eu, "eu");
    const { seen } = fakeFetch(() => cfOk("sub-eu"));
    const name = uniqueProject();
    const r = await call("POST", "/v1/projects", { name, jurisdiction: "eu" }, auth, configured);
    expect(r.status).toBe(201);
    expect(eu.repos.has(name)).toBe(true);
    expect(us.repos.has(name)).toBe(false);
    expect(seen[0]!.body.source).toEqual({ type: "artifacts.repo", namespace: "wayglass-eu", repo_name: name });
    const d = await call("POST", "/v1/intents", { project: name, goal: "g", reason: "r", paths: ["a"] });
    expect(eu.repos.has(d.json.data.fork.name)).toBe(true);
    expect(us.repos.size).toBe(0);
  });

  it("reports not_configured when the Worker has no events token", async () => {
    const r = await call("POST", "/v1/projects", { name: uniqueProject() });
    expect(r.status).toBe(201);
    expect(r.json.data.events.subscribed).toBe(false);
    expect(r.json.data.events.reason).toBe("not_configured");
  });
});
