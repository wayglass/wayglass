// DELETE /v1/projects/:name: admin-only cleanup of throwaway test projects.
// Removes every repo the project recorded (in its own namespace), revokes
// live tokens, removes trunk push subscriptions and wipes the project's
// Durable Object. Anything that is not demo-test-* / eu-demo-test-* is refused.

import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { setRepoHostForTests } from "../src/artifacts";
import { isDeletableProject } from "../src/index";
import type { Project } from "../src/project";
import { projectStub } from "../src/stub";
import { setSubscriptionFetchForTests } from "../src/subscriptions";
import { FakeRepoHost } from "./fake-artifacts";
import { call, KEY } from "./helpers";

let fake: FakeRepoHost;
beforeEach(() => {
  fake = new FakeRepoHost();
  setRepoHostForTests(fake);
});
afterEach(() => {
  setRepoHostForTests(null);
  setSubscriptionFetchForTests(null);
});

let n = 0;
const throwaway = (prefix = "demo-test-") => `${prefix}${++n}-${crypto.randomUUID().slice(0, 6)}`;
const as = (agent: string, key = KEY) => ({ authorization: `Bearer ${key}`, "x-wayglass-agent": agent });
const del = (name: string, body: unknown = { confirm: name }, headers: Record<string, string> = as("cleaner")) => call("DELETE", `/v1/projects/${name}`, body, headers);

async function create(name: string, jurisdiction?: string) {
  const r = await call("POST", "/v1/projects", { name, ...(jurisdiction ? { jurisdiction } : {}) });
  expect(r.status).toBe(201);
  fake.commit(name, { "README.md": "hi\n" });
}

async function declare(project: string, on?: string) {
  const r = await call("POST", "/v1/intents", { project, goal: "g", reason: "because", paths: ["docs/**"], ...(on ? { on } : {}) }, as("agent-a"));
  expect(r.status).toBe(201);
  return { id: r.json.data.intent.id as string, fork: r.json.data.fork.name as string };
}

describe("isDeletableProject", () => {
  it.each([
    ["demo-test-1", true],
    ["eu-demo-test-x", true],
    ["demo-test-", false],
    ["demo-1791938487", false],
    ["eu-demo-1791797572", false],
    ["wayglass", false],
    ["my-demo-test-1", false],
    ["demo-testing", false],
  ])("%s -> %s", (name, want) => {
    expect(isDeletableProject(name)).toBe(want);
  });
});

describe("DELETE /v1/projects/:name", () => {
  it("deletes the trunk and every fork, revokes live tokens and wipes the project", async () => {
    const name = throwaway();
    await create(name);
    const a = await declare(name);
    fake.commit(a.fork, { "README.md": "hi\n", "docs/a.md": "a\n" });
    const b = await declare(name, a.id); // stacked: a fork of a's fork
    const revokedBefore = fake.revoked.length;

    const r = await del(name);
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({
      project: name,
      existed: true,
      state_wiped: true,
      actor: "cleaner",
      jurisdiction: "us",
      namespace: "wayglass",
      repos_already_gone: [],
      tokens_revoked: 2,
      counts: { intents: 2 },
    });
    // Stacked fork first, the fork it was made from next, trunk last.
    expect(r.json.data.repos_deleted).toEqual([b.fork, a.fork, name]);
    expect(fake.deleted).toEqual([b.fork, a.fork, name]);
    expect(fake.revoked.length).toBe(revokedBefore + 2);
    for (const repo of [name, a.fork, b.fork]) expect(fake.repos.has(repo)).toBe(false);

    // The Durable Object answers like a project that never existed.
    expect((await call("GET", `/v1/intents?project=${name}`)).status).toBe(404);
    expect((await call("GET", `/v1/audit?project=${name}`)).json.data?.events ?? []).toEqual([]);
    // And the name can be used again.
    await create(name);
    expect((await call("GET", `/v1/intents?project=${name}`)).json.data.intents).toEqual([]);
  });

  it("is idempotent: repos already gone are fine, and a second delete is a no-op", async () => {
    const name = throwaway();
    await create(name);
    const a = await declare(name);
    await fake.deleteRepo(a.fork); // removed by hand earlier
    const r = await del(name);
    expect(r.status).toBe(200);
    expect(r.json.data.repos_deleted).toEqual([name]);
    expect(r.json.data.repos_already_gone).toEqual([a.fork]);

    const again = await del(name);
    expect(again.status).toBe(200);
    expect(again.json.data).toMatchObject({ project: name, existed: false, state_wiped: false, repos_deleted: [] });
  });

  it("keeps the state when a repo cannot be deleted, so a retry finishes the job", async () => {
    const name = throwaway();
    await create(name);
    const a = await declare(name);
    fake.failDelete.add(a.fork);
    const r = await del(name);
    expect(r.status).toBe(502);
    expect(r.json.error).toMatchObject({ code: "upstream_error" });
    expect(r.json.error!.message).toContain(a.fork);
    const events = (await call("GET", `/v1/audit?project=${name}`)).json.data.events as { kind: string; data: any }[];
    expect(events.at(-1)).toMatchObject({ kind: "project.delete_failed", data: { repos_failed: [a.fork], repos_deleted: [name] } });

    fake.failDelete.clear();
    const retry = await del(name);
    expect(retry.status).toBe(200);
    expect(retry.json.data).toMatchObject({ existed: true, repos_deleted: [a.fork], repos_already_gone: [name] });
  });

  it("deletes an EU project's repos in the EU namespace only", async () => {
    const eu = new FakeRepoHost();
    setRepoHostForTests(eu, "eu");
    const name = throwaway("eu-demo-test-");
    const r0 = await call("POST", "/v1/projects", { name, jurisdiction: "eu" });
    expect(r0.status).toBe(201);
    const r = await del(name);
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({ jurisdiction: "eu", namespace: "wayglass-eu", repos_deleted: [name] });
    expect(eu.deleted).toEqual([name]);
    expect(fake.deleted).toEqual([]);
  });

  it("refuses to delete when the project records a repo it does not own", async () => {
    const name = throwaway();
    await create(name);
    const a = await declare(name);
    // A corrupt or planted record naming another project's fork.
    const stub = await projectStub(env, name);
    await runInDurableObject(stub, (p: Project) => {
      p.recordExternalEvent("intent.declared", "planted", null, { fork_repo: "demo-1791938487--i0123456789" });
    });
    const r = await del(name);
    expect(r.status).toBe(500);
    expect(r.json.error).toMatchObject({ code: "internal" });
    expect(r.json.error!.message).toContain("demo-1791938487--i0123456789");
    expect(fake.deleted).toEqual([]);
    expect(fake.repos.has(name)).toBe(true);
    expect(fake.repos.has(a.fork)).toBe(true);
    expect((await call("GET", `/v1/audit?project=${name}`)).status).toBe(200);
  });

  it("removes the trunk's push subscription", async () => {
    const seen: { method: string; url: string }[] = [];
    setSubscriptionFetchForTests((async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push({ method: init?.method ?? "GET", url: String(input) });
      return init?.method === "DELETE" ? Response.json({ success: true, result: null }) : Response.json({ success: true, result: { id: "sub-42" } });
    }) as typeof fetch);
    const configured = { CF_ACCOUNT_ID: "acct123", CF_EVENTS_API_TOKEN: "events-token-secret" };
    const name = throwaway();
    expect((await call("POST", "/v1/projects", { name }, undefined, configured)).status).toBe(201);
    const r = await call("DELETE", `/v1/projects/${name}`, { confirm: name }, undefined, configured);
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({ subscriptions_removed: 1, subscriptions_left: [] });
    expect(seen.at(-1)).toEqual({ method: "DELETE", url: "https://api.cloudflare.com/client/v4/accounts/acct123/event_subscriptions/subscriptions/sub-42" });
  });

  it.each(["demo-1791938487", "eu-demo-1791797572", "wayglass", "demo-testing"])("refuses %s and touches nothing", async (name) => {
    const r = await del(name);
    expect(r.status).toBe(403);
    expect(r.json.error!.message).toMatch(/only throwaway projects/);
    expect(fake.deleted).toEqual([]);
  });

  it("requires confirm to repeat the name", async () => {
    const name = throwaway();
    await create(name);
    for (const body of [{}, { confirm: "demo-test-other" }, { confirm: true }]) {
      const r = await del(name, body);
      expect(r.status).toBe(400);
      expect(r.json.error!.message).toMatch(/confirm must repeat the project name/);
    }
    expect(fake.repos.has(name)).toBe(true);
  });

  it("refuses an agent key", async () => {
    const name = throwaway();
    await create(name);
    const minted = await call("POST", "/v1/keys", { agent: "bot" });
    const r = await del(name, { confirm: name }, { authorization: `Bearer ${minted.json.data.key}` });
    expect(r.status).toBe(403);
    expect(r.json.error!.message).toMatch(/only the admin key/);
    expect(fake.repos.has(name)).toBe(true);
  });

  it("is reachable only with a key at all", async () => {
    const r = await call("DELETE", "/v1/projects/demo-test-x", { confirm: "demo-test-x" }, {});
    expect(r.status).toBe(401);
    expect(env.WAYGLASS_ADMIN_KEY).toBe(KEY);
  });
});
