// Data residency for project state (src/residency.ts): a project in a pinned
// jurisdiction keeps its Durable Object there, every route, the queue
// consumer and the public views find it through the directory, a project
// made before pinning is refused (never served from the unpinned object)
// until POST /v1/projects/:name/migrate-residency moves it, and that move is
// verified, tombstones the source and can be rerun.

import { env } from "cloudflare:workers";
import { createExecutionContext, createMessageBatch, getQueueResult, runInDurableObject } from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { setRepoHostForTests } from "../src/artifacts";
import worker from "../src/index";
import { registryStub } from "../src/keys";
import type { Project } from "../src/project";
import { projectJurisdiction, resetResidencyCacheForTests, setPinnedNamespaceForTests, stubIn } from "../src/residency";
import { FakeRepoHost, type Files } from "./fake-artifacts";
import { call, createProject, createProjectIn, declare, KEY, uniqueProject } from "./helpers";
import { pinnedIds } from "./setup";

let us: FakeRepoHost;
let eu: FakeRepoHost;
beforeEach(() => {
  setRepoHostForTests(null);
  us = new FakeRepoHost();
  eu = new FakeRepoHost();
  setRepoHostForTests(us, "us");
  setRepoHostForTests(eu, "eu");
});
afterEach(() => setRepoHostForTests(null));

const POLICY = `
version: 1
rules:
  - { when: { area: docs }, action: auto_merge, reason: "docs" }
  - { when: { jurisdiction: eu, data_class: pii }, action: quarantine, reason: "EU personal data" }
`;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "docs/usage.md": "Usage\n" };

/** The unpinned object for a name: where every project lived before Oct 14. */
const unpinned = (name: string) => env.PROJECT.get(env.PROJECT.idFromName(name)) as DurableObjectStub<Project>;
const admin = (agent = "john") => ({ authorization: `Bearer ${KEY}`, "x-wayglass-agent": agent });
const migrate = (name: string, headers: Record<string, string> = admin()) => call("POST", `/v1/projects/${name}/migrate-residency`, {}, headers);
const audit = async (project: string) => (await call("GET", `/v1/audit?project=${project}&limit=500`)).json.data.events as { seq: number; kind: string; data: Record<string, unknown> }[];

/**
 * An EU project as it was made before pinning: state in the unpinned object,
 * no directory entry. Made through the normal routes with the pinned
 * namespace pointed at the unpinned one, then struck from the directory.
 */
async function legacyEuProject(name = uniqueProject("eu-legacy-")): Promise<{ name: string; intent: string; fork: string }> {
  setPinnedNamespaceForTests(() => env.PROJECT);
  try {
    await createProjectIn("eu", name);
    eu.commit(name, BASE);
    const d = await declare(name, ["**"], "add a signup form", "agent-eu");
    expect(d.status).toBe(201);
    const intent: string = d.json.data.intent.id;
    const fork: string = d.json.data.fork.name;
    eu.commit(fork, { ...BASE, "web/signup.ts": "const email = form.email;\nconst date_of_birth = form.dob;\n" });
    const p = await call("POST", `/v1/intents/${intent}/propose`, { project: name });
    expect(p.status).toBe(201);
    expect(p.json.data.decision.id).toMatch(/^d/);
    const s = await call("POST", `/v1/intents/${intent}/sessions`, { project: name, notes: "handing over", agent: "agent-eu" });
    expect(s.status).toBe(201);
    await registryStub(env).unregisterProject(name);
    resetResidencyCacheForTests();
    pinnedIds.length = 0;
    return { name, intent, fork };
  } finally {
    // Back to the setup.ts fake: a distinct object per pinned jurisdiction.
    setPinnedNamespaceForTests((jurisdiction) => ({
      idFromName(n: string) {
        pinnedIds.push([jurisdiction, n]);
        return env.PROJECT.idFromName(`pinned:${jurisdiction}:${n}`);
      },
      get(id: DurableObjectId) {
        return env.PROJECT.get(id);
      },
    }));
  }
}

describe("where a project's state lives", () => {
  it("creates an eu project in the eu-pinned namespace and leaves the unpinned object empty", async () => {
    const name = await createProjectIn("eu");
    expect(pinnedIds).toContainEqual(["eu", name]);
    expect(await unpinned(name).info()).toBeNull();
    expect(await stubIn(env, name, "eu").info()).toMatchObject({ name, jurisdiction: "eu" });
    expect(await registryStub(env).projectJurisdiction(name)).toBe("eu");
  });

  it("routes every later request for an eu project through the pinned namespace", async () => {
    const name = await createProjectIn("eu");
    eu.commit(name, BASE);
    for (const [method, path, body] of [
      ["POST", "/v1/intents", { project: name, goal: "g", reason: "because", paths: ["docs/**"] }],
      ["GET", `/v1/intents?project=${name}`],
      ["GET", `/v1/audit?project=${name}`],
      ["GET", `/v1/decisions?project=${name}`],
      ["GET", `/v1/cost?project=${name}`],
      ["POST", `/v1/projects/${name}/trunk/read-token`, {}],
      ["GET", `/v1/projects/${name}/browse`],
    ] as const) {
      pinnedIds.length = 0;
      resetResidencyCacheForTests();
      const r = await call(method, path, body);
      expect(r.status, `${method} ${path}: ${JSON.stringify(r.json)}`).toBeLessThan(300);
      expect(pinnedIds, `${method} ${path}`).toContainEqual(["eu", name]);
    }
    expect(await unpinned(name).info()).toBeNull();
  });

  it("keeps a us project in the unpinned object and never asks for a pinned namespace", async () => {
    const name = await createProject();
    us.commit(name, BASE);
    await declare(name, ["docs/**"]);
    resetResidencyCacheForTests();
    const r = await call("GET", `/v1/intents?project=${name}`);
    expect(r.status).toBe(200);
    expect(pinnedIds.map(([, n]) => n)).not.toContain(name);
    expect(await unpinned(name).info()).toMatchObject({ name, jurisdiction: "us" });
    expect(await registryStub(env).projectJurisdiction(name)).toBe("us");
  });

  it("finds a pre-directory us project in the unpinned object and registers it", async () => {
    const name = await createProject();
    await registryStub(env).unregisterProject(name);
    resetResidencyCacheForTests();
    const r = await call("GET", `/v1/intents?project=${name}`);
    expect(r.status).toBe(200);
    expect(await registryStub(env).projectJurisdiction(name)).toBe("us");
    expect(await projectJurisdiction(env, name)).toBe("us");
  });

  it("answers not_found for a name with no project in any jurisdiction", async () => {
    const r = await call("GET", `/v1/intents?project=${uniqueProject("nothing-")}`);
    expect(r.status).toBe(404);
    expect(await projectJurisdiction(env, uniqueProject("nothing-"))).toBeNull();
  });

  it("lets a deleted eu name be created again as a us project", async () => {
    const name = `eu-demo-test-${crypto.randomUUID().slice(0, 8)}`;
    await createProjectIn("eu", name);
    const del = await call("DELETE", `/v1/projects/${name}`, { confirm: name });
    expect(del.status).toBe(200);
    expect(await registryStub(env).projectJurisdiction(name)).toBeNull();
    const again = await call("POST", "/v1/projects", { name });
    expect(again.status).toBe(201);
    expect(await registryStub(env).projectJurisdiction(name)).toBe("us");
    expect(await unpinned(name).info()).toMatchObject({ name, jurisdiction: "us" });
  });

  it("records a push event for an eu repo in the pinned object", async () => {
    const name = await createProjectIn("eu");
    const batch = createMessageBatch<unknown>("artifacts-events", [
      {
        id: "m-eu-pinned",
        timestamp: new Date(),
        attempts: 1,
        body: {
          type: "cf.artifacts.repo.pushed",
          source: { type: "artifacts.repo", namespace: "wayglass-eu", repoName: name },
          payload: { ref: "refs/heads/main", before: "a".repeat(40), after: "b".repeat(40), commits: [{ id: "b".repeat(40) }], totalCommitsCount: 1 },
        },
      },
    ]);
    const ctx = createExecutionContext();
    pinnedIds.length = 0;
    resetResidencyCacheForTests();
    await worker.queue(batch, env, ctx);
    const r = await getQueueResult(batch, ctx);
    expect(r.explicitAcks).toEqual(["m-eu-pinned"]);
    expect(pinnedIds).toContainEqual(["eu", name]);
    expect((await audit(name)).map((e) => e.kind)).toContain("artifacts.repo.pushed");
    expect(await unpinned(name).info()).toBeNull();
  });
});

describe("a pre-pinning eu project (state in the unpinned object)", () => {
  it("is refused with residency_pending on every route rather than served from the unpinned object", async () => {
    const { name, intent } = await legacyEuProject();
    for (const [method, path, body] of [
      ["GET", `/v1/intents?project=${name}`],
      ["GET", `/v1/audit?project=${name}`],
      ["POST", "/v1/intents", { project: name, goal: "g", reason: "because", paths: ["docs/**"] }],
      ["POST", `/v1/intents/${intent}/token`, { project: name }],
      ["POST", `/v1/decisions/${"d0000000000"}`, { project: name, verdict: "approve", reason: "checked the form" }],
      ["GET", `/v1/projects/${name}/browse`],
      ["POST", "/v1/projects", { name, jurisdiction: "us" }],
    ] as const) {
      resetResidencyCacheForTests();
      const r = await call(method, path, body);
      expect(r.status, `${method} ${path}`).toBe(503);
      expect(r.json.error?.code, `${method} ${path}`).toBe("residency_pending");
      expect(r.json.error?.message).toContain(`migrate-residency`);
    }
    expect(pinnedIds.map(([, n]) => n)).not.toContain(name);
  });

  it("is listed in the directory as pending, with no counts", async () => {
    const { name } = await legacyEuProject();
    const r = await call("GET", "/v1/projects?cursor=" + btoa(JSON.stringify({ index: 1, cursor: "" })));
    expect(r.status).toBe(200);
    const row = (r.json.data.projects as { name: string; residency?: string; counts: unknown }[]).find((p) => p.name === name);
    expect(row).toMatchObject({ jurisdiction: "eu", residency: "residency_pending", counts: null });
  });

  it("holds a queued push until it is migrated", async () => {
    const { name } = await legacyEuProject();
    const batch = createMessageBatch<unknown>("artifacts-events", [
      {
        id: "m-eu-pending",
        timestamp: new Date(),
        attempts: 1,
        body: {
          type: "cf.artifacts.repo.pushed",
          source: { type: "artifacts.repo", namespace: "wayglass-eu", repoName: name },
          payload: { ref: "refs/heads/main", before: "a".repeat(40), after: "b".repeat(40), commits: [{ id: "b".repeat(40) }], totalCommitsCount: 1 },
        },
      },
    ]);
    const ctx = createExecutionContext();
    await worker.queue(batch, env, ctx);
    const r = await getQueueResult(batch, ctx);
    expect(r.retryMessages.map((m) => m.msgId)).toEqual(["m-eu-pending"]);
    expect(r.explicitAcks).toEqual([]);
  });
});

describe("POST /v1/projects/:name/migrate-residency", () => {
  it("copies every table into the pinned object, verifies, tombstones the source and records the move", async () => {
    const { name, intent } = await legacyEuProject();
    const before = await runInDurableObject(unpinned(name), (p: Project) => p.stateDigest());
    expect(before.counts).toMatchObject({ meta: 6, intents: 1, decisions: 1, sessions: 1 });
    expect(before.counts.audit).toBeGreaterThan(3);
    const sourceAudit = (await runInDurableObject(unpinned(name), (p: Project) => p.listAudit(0, 500))) as unknown as { ok: true; data: { events: unknown[] } };
    const sourceIntents = await runInDurableObject(unpinned(name), (p: Project) => p.listIntents());
    const sourceDecisions = await runInDurableObject(unpinned(name), (p: Project) => p.listDecisions());
    const sourceCost = await runInDurableObject(unpinned(name), (p: Project) => p.costSummary());

    const r = await migrate(name);
    expect(r.status, JSON.stringify(r.json)).toBe(200);
    expect(r.json.data).toMatchObject({ project: name, jurisdiction: "eu", migrated: true, counts: before.counts, digest: before.digest, source_tombstoned: true });

    // The pinned object holds everything, verbatim, plus one new audit event.
    expect(pinnedIds).toContainEqual(["eu", name]);
    const target = stubIn(env, name, "eu");
    expect(await target.info()).toMatchObject({ name, jurisdiction: "eu" });
    expect(await target.listIntents()).toEqual(sourceIntents);
    expect(await target.listDecisions()).toEqual(sourceDecisions);
    expect(await target.costSummary()).toEqual(sourceCost);
    const moved = (await target.listAudit(0, 500)) as unknown as { ok: true; data: { events: { seq: number; kind: string; data: Record<string, unknown> }[] } };
    const copied = moved.data.events.slice(0, -1);
    expect(copied).toEqual(sourceAudit.data.events);
    const last = moved.data.events.at(-1)!;
    expect(last).toMatchObject({ kind: "project.residency_migrated", actor: "john", data: { from: "unpinned", to: "eu", counts: before.counts, digest: before.digest } });
    expect(last.seq).toBe(copied.length + 1);
    expect(await target.hasAudit("project.residency_migrated")).toBe(true);

    // The directory now says eu, and the routes follow it.
    expect(await registryStub(env).projectJurisdiction(name)).toBe("eu");
    resetResidencyCacheForTests();
    const list = await call("GET", `/v1/intents?project=${name}`);
    expect(list.status).toBe(200);
    expect(list.json.data.intents[0].id).toBe(intent);
    expect((await audit(name)).map((e) => e.kind)).toContain("project.residency_migrated");

    // The source is a tombstone: no state, and every call refuses.
    expect(await runInDurableObject(unpinned(name), (p: Project) => p.residencyStatus())).toMatchObject({ moved_to: "eu" });
    await expect(runInDurableObject(unpinned(name), (p: Project) => p.stateDigest())).rejects.toThrow(/^project_moved: /);
    await expect(runInDurableObject(unpinned(name), (p: Project) => p.info())).rejects.toThrow(/^project_moved: /);
    await expect(runInDurableObject(unpinned(name), (p: Project) => p.listIntents())).rejects.toThrow(/moved to the eu jurisdiction/);
    await expect(runInDurableObject(unpinned(name), (p: Project) => p.wipe())).rejects.toThrow(/^project_moved: /);
    // Over RPC the same refusal reaches the Worker: a request for the name
    // with no directory entry is repaired from the tombstone, never served here.
    await registryStub(env).unregisterProject(name);
    resetResidencyCacheForTests();
    expect(await projectJurisdiction(env, name)).toBe("eu");
  });

  it("is idempotent: a second run copies nothing and reports migrated: false", async () => {
    const { name } = await legacyEuProject();
    const first = await migrate(name);
    expect(first.status).toBe(200);
    const target = stubIn(env, name, "eu");
    const after = await target.stateDigest();
    const again = await migrate(name);
    expect(again.status, JSON.stringify(again.json)).toBe(200);
    expect(again.json.data).toMatchObject({ project: name, jurisdiction: "eu", migrated: false, counts: after.counts, digest: after.digest, source_tombstoned: true });
    expect(await target.stateDigest()).toEqual(after);
    expect((await audit(name)).filter((e) => e.kind === "project.residency_migrated")).toHaveLength(1);
  });

  it("repairs a lost directory entry from the tombstone", async () => {
    const { name } = await legacyEuProject();
    expect((await migrate(name)).status).toBe(200);
    await registryStub(env).unregisterProject(name);
    resetResidencyCacheForTests();
    pinnedIds.length = 0;
    const r = await call("GET", `/v1/intents?project=${name}`);
    expect(r.status).toBe(200);
    expect(pinnedIds).toContainEqual(["eu", name]);
    expect(await registryStub(env).projectJurisdiction(name)).toBe("eu");
  });

  it("resumes a copy that stopped half way", async () => {
    const { name } = await legacyEuProject();
    const source = unpinned(name);
    const target = stubIn(env, name, "eu");
    // A previous run copied meta and the first audit rows, then died.
    for (const table of ["meta", "audit"]) {
      const page = await source.exportRows(table, 0);
      await target.importRows(table, page.rows.slice(0, 2));
    }
    const r = await migrate(name);
    expect(r.status, JSON.stringify(r.json)).toBe(200);
    expect(r.json.data.migrated).toBe(true);
    const sourceCount = r.json.data.counts as Record<string, number>;
    const copied = await target.stateDigest();
    expect(copied.counts).toEqual({ ...sourceCount, audit: sourceCount.audit! + 1 });
  });

  it("refuses when the pinned object already holds other state, and leaves the source alone", async () => {
    const { name } = await legacyEuProject();
    const target = stubIn(env, name, "eu");
    await target.init({ name: "someone-else", trunk_repo: "someone-else", trunk_remote: "x", jurisdiction: "eu", actor: "t" });
    const r = await migrate(name);
    expect(r.status).toBe(503);
    expect(r.json.error).toMatchObject({ code: "project_moved" });
    expect(r.json.error?.message).toContain("someone-else");
    expect(await unpinned(name).info()).toMatchObject({ name, jurisdiction: "eu" });
    expect(await runInDurableObject(unpinned(name), (p: Project) => p.residencyStatus())).toEqual({ moved_to: null, moved_at: null });
  });

  it("refuses rows that differ from the source without touching it", async () => {
    const { name } = await legacyEuProject();
    const target = stubIn(env, name, "eu");
    const page = await unpinned(name).exportRows("meta", 0);
    const tampered = page.rows.map((r) => (r.key === "trunk_remote" ? { ...r, value: "https://elsewhere.example/git" } : r));
    await target.importRows("meta", tampered);
    const r = await migrate(name);
    expect(r.status).toBe(503);
    expect(r.json.error?.message).toMatch(/did not verify/);
    expect(await unpinned(name).info()).toMatchObject({ name, jurisdiction: "eu" });
    expect(await registryStub(env).projectJurisdiction(name)).toBeNull();
  });

  it("is admin-only and has nothing to do for a us project or an unknown name", async () => {
    const keyed = await call("POST", "/v1/keys", { agent: "agent-x" });
    const agentKey: string = keyed.json.data.key;
    const { name } = await legacyEuProject();
    const denied = await migrate(name, { authorization: `Bearer ${agentKey}` });
    expect(denied.status).toBe(403);
    expect(await unpinned(name).info()).toMatchObject({ name });

    const usName = await createProject();
    const usRun = await migrate(usName);
    expect(usRun.status).toBe(503);
    expect(usRun.json.error).toMatchObject({ code: "residency_pending" });
    expect(usRun.json.error?.message).toContain("nothing to migrate");
    expect(await unpinned(usName).info()).toMatchObject({ name: usName });

    const none = await migrate(uniqueProject("nothing-"));
    expect(none.status).toBe(503);
    expect(none.json.error?.message).toContain("nothing to migrate");
  });

  it("does not move a project already created in the pinned object", async () => {
    const name = await createProjectIn("eu");
    const r = await migrate(name);
    expect(r.status).toBe(503);
    expect(r.json.error?.message).toContain("nothing to migrate");
    expect(await stubIn(env, name, "eu").info()).toMatchObject({ name });
  });
});
