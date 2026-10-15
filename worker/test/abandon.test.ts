// Abandoning an intent, and stale intents. Abandon is the author's (or the
// admin's) explicit "this is dead": the intent leaves overlap checks and
// review queues, its fork tokens are revoked, and its fork repo is kept.
// Intents stacked on it take over its commits. Stale is only a report.

import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import type { Project } from "../src/project";
import { projectStub } from "../src/stub";
import type { FakeRepoHost, Files } from "./fake-artifacts";
import { call, createProject, KEY, useFakeArtifacts } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "docs/usage.md": "Usage\n", "web/login.ts": "export function login() {}\n" };

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

const as = (agent: string, key = KEY) => ({ authorization: `Bearer ${key}`, "x-wayglass-agent": agent });

async function declared(project: string, agent: string, paths = ["docs/**"], on?: string) {
  const r = await call("POST", "/v1/intents", { project, goal: `work by ${agent}`, reason: "because", paths, ...(on ? { on } : {}) }, as(agent));
  expect(r.status).toBe(201);
  return { id: r.json.data.intent.id as string, fork: r.json.data.fork.name as string };
}

function commitOn(fork: string, edits: Files): string {
  const head = fake.heads.get(fork);
  return fake.commit(fork, { ...(head ? fake.filesAt(head) : {}), ...edits });
}

const abandon = (project: string, id: string, agent: string, reason = "superseded by a simpler approach", key = KEY) =>
  call("POST", `/v1/intents/${id}/abandon`, { project, reason }, as(agent, key));

async function mint(agent: string): Promise<string> {
  const r = await call("POST", "/v1/keys", { agent });
  expect(r.status).toBe(201);
  return r.json.data.key;
}

describe("intent abandon", () => {
  it("lets the author abandon, keeps the fork, revokes its tokens and audits the reason", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const revokedBefore = fake.revoked.length;
    const r = await abandon(project, a.id, "agent-a");
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({
      intent: { id: a.id, status: "abandoned", abandoned_by: "agent-a", abandoned_reason: "superseded by a simpler approach" },
      restacked: [],
      tokens_revoked: 1,
      fork_kept: true,
    });
    expect(fake.revoked.length).toBe(revokedBefore + 1);
    expect(fake.repos.has(a.fork)).toBe(true);
    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as { kind: string; actor: string; data: any }[];
    expect(events.at(-1)).toMatchObject({ kind: "intent.abandoned", actor: "agent-a", data: { reason: "superseded by a simpler approach", fork_kept: true } });
    // No new fork token, no push, no propose, no second abandon.
    expect((await call("POST", `/v1/intents/${a.id}/token`, { project }, as("agent-a"))).status).toBe(409);
    expect((await call("POST", `/v1/intents/${a.id}/push`, { project, paths: ["docs/x.md"] }, as("agent-a"))).status).toBe(409);
    expect((await call("POST", `/v1/intents/${a.id}/propose`, { project }, as("agent-a"))).status).toBe(409);
    expect((await abandon(project, a.id, "agent-a")).status).toBe(409);
  });

  it("refuses another agent's bound key, but allows the admin", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const other = await mint("agent-b");
    const denied = await abandon(project, a.id, "agent-a", "not mine to keep", other);
    expect(denied.status).toBe(403);
    expect(denied.json.error?.message).toMatch(/only agent-a/);
    const byAdmin = await abandon(project, a.id, "john@mac", "agent crashed overnight");
    expect(byAdmin.status).toBe(200);
    const ev = (await call("GET", `/v1/audit?project=${project}`)).json.data.events.at(-1);
    expect(ev).toMatchObject({ kind: "intent.abandoned", actor: "john@mac", data: { by_admin: true } });
  });

  it("works for the author's own bound key", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const key = await mint("agent-k");
    const r = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["docs/**"] }, as("ignored", key));
    const id = r.json.data.intent.id;
    expect((await abandon(project, id, "whoever", "plan changed", key)).status).toBe(200);
  });

  it("requires a real reason", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    expect((await abandon(project, a.id, "agent-a", "")).status).toBe(400);
    expect((await abandon(project, a.id, "agent-a", "<why>")).status).toBe(400);
  });

  it("drops out of overlaps and review queues; its decision can no longer be decided", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a", ["web/**"]);
    commitOn(a.fork, { "web/login.ts": "export function login() { /* auth */ }\n" });
    const p = await call("POST", `/v1/intents/${a.id}/propose`, { project }, as("agent-a"));
    expect(p.json.data.decision.status).toBe("pending_human");
    const b = await declared(project, "agent-b", ["web/**"]);
    expect((await call("POST", `/v1/intents/${b.id}/push`, { project, paths: ["web/login.ts"] }, as("agent-b"))).json.data.overlaps).toHaveLength(1);

    expect((await abandon(project, a.id, "agent-a")).status).toBe(200);
    expect((await call("POST", `/v1/intents/${b.id}/push`, { project, paths: ["web/login.ts"] }, as("agent-b"))).json.data.overlaps).toEqual([]);
    const list = await call("GET", `/v1/decisions?project=${project}`);
    expect(list.json.data.decisions).toEqual([expect.objectContaining({ intent_id: a.id, status: "pending_human", intent_status: "abandoned" })]);
    const decide = await call("POST", `/v1/intents/${a.id}/decide`, { project, verdict: "approve", reason: "looked at it" }, as("john"));
    expect(decide.status).toBe(409);
    expect(decide.json.error?.message).toMatch(/abandoned/);
  });

  it("refuses while the intent is landing", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    commitOn(a.fork, { "docs/usage.md": "Usage\nMore.\n" });
    expect((await call("POST", `/v1/intents/${a.id}/propose`, { project }, as("agent-a"))).json.data.decision.status).toBe("approved");
    expect((await call("POST", `/v1/intents/${a.id}/land`, { project }, as("agent-a"))).status).toBe(201);
    const r = await abandon(project, a.id, "agent-a");
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/landing/);
  });
});

describe("abandoned parent: the stacked child takes over its commits", () => {
  it("re-stacks the child onto the parent's base, and the child must propose again before landing", async () => {
    const project = await createProject();
    const trunk = fake.commit(project, BASE);
    const parent = await declared(project, "agent-a");
    const parentHead = commitOn(parent.fork, { "docs/usage.md": "Usage\nParent.\n" });
    await call("POST", `/v1/intents/${parent.id}/push`, { project, paths: ["docs/usage.md"], commit: parentHead }, as("agent-a"));
    const child = await declared(project, "agent-b", ["docs/**"], parent.id);
    const childHead = commitOn(child.fork, { "docs/child.md": "child\n" });
    const first = await call("POST", `/v1/intents/${child.id}/propose`, { project }, as("agent-b"));
    expect(first.json.data.decision).toMatchObject({ status: "approved", paths: ["docs/child.md"], base_commit: parentHead });

    const r = await abandon(project, parent.id, "agent-a", "agent-b carries this on");
    expect(r.json.data.restacked).toEqual([child.id]);
    const intents = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[];
    expect(intents.find((i) => i.id === child.id)).toMatchObject({ parent_id: null, base_commit: trunk, trunk_base: trunk });
    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as { kind: string; intent_id: string; data: any }[];
    expect(events.find((e) => e.kind === "intent.restacked")).toMatchObject({ intent_id: child.id, data: { from_parent: parent.id, to_parent: null, base_commit: trunk } });

    // The old decision covered only docs/child.md; the land would carry docs/usage.md too.
    const stale = await call("POST", `/v1/intents/${child.id}/land`, { project }, as("agent-b"));
    expect(stale.status).toBe(409);
    expect(stale.json.error?.message).toMatch(/re-stacked.*propose again/);
    const again = await call("POST", `/v1/intents/${child.id}/propose`, { project }, as("agent-b"));
    expect(again.json.data.decision).toMatchObject({ status: "approved", base_commit: trunk, paths: ["docs/child.md", "docs/usage.md"] });
    const land = await call("POST", `/v1/intents/${child.id}/land`, { project }, as("agent-b"));
    expect(land.status).toBe(201);
    fake.setHead(project, childHead);
    expect((await call("POST", `/v1/intents/${child.id}/landed`, { project, commit: childHead }, as("agent-b"))).status).toBe(200);
  });

  it("supersedes the child's pending decision, so nobody can decide it", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const parent = await declared(project, "agent-a");
    const parentHead = commitOn(parent.fork, { "docs/usage.md": "Usage\nParent.\n" });
    await call("POST", `/v1/intents/${parent.id}/push`, { project, paths: ["docs/usage.md"], commit: parentHead }, as("agent-a"));
    const child = await declared(project, "agent-b", ["web/**"], parent.id);
    commitOn(child.fork, { "web/login.ts": "export function login() { /* child */ }\n" });
    const first = (await call("POST", `/v1/intents/${child.id}/propose`, { project }, as("agent-b"))).json.data.decision;
    expect(first.status).toBe("pending_human");

    await abandon(project, parent.id, "agent-a", "agent-b carries this on");
    const d = ((await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions as any[]).find((x) => x.id === first.id);
    expect(d).toMatchObject({ status: "superseded", decided_by: "wayglass" });
    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as any[];
    expect(events.find((e) => e.kind === "intent.restacked")).toMatchObject({ intent_id: child.id, data: { superseded_decision: first.id } });
    expect(events.find((e) => e.kind === "decision.superseded")).toMatchObject({ decision_id: first.id, data: { previous_status: "pending_human" } });
    const r = await call("POST", `/v1/intents/${child.id}/decide`, { project, verdict: "approve", reason: "checked the login flow" }, as("reviewer"));
    expect(r.json.error).toMatchObject({ code: "superseded" });
  });

  it("supersedes the child's approved decision too, keeping who approved it", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const parent = await declared(project, "agent-a");
    const parentHead = commitOn(parent.fork, { "docs/usage.md": "Usage\nParent.\n" });
    await call("POST", `/v1/intents/${parent.id}/push`, { project, paths: ["docs/usage.md"], commit: parentHead }, as("agent-a"));
    const child = await declared(project, "agent-b", ["web/**"], parent.id);
    commitOn(child.fork, { "web/login.ts": "export function login() { /* child */ }\n" });
    const first = (await call("POST", `/v1/intents/${child.id}/propose`, { project }, as("agent-b"))).json.data.decision;
    expect((await call("POST", `/v1/intents/${child.id}/decide`, { project, verdict: "approve", reason: "checked the login flow" }, as("reviewer"))).status).toBe(200);

    await abandon(project, parent.id, "agent-a", "agent-b carries this on");
    const d = ((await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions as any[]).find((x) => x.id === first.id);
    // The admin key decided (as "reviewer"): it is recorded as the one identity admin.
    expect(d).toMatchObject({ status: "superseded", decided_by: "admin" });
    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as any[];
    expect(events.find((e) => e.kind === "intent.restacked")).toMatchObject({ intent_id: child.id, data: { superseded_decision: first.id } });
    expect(events.find((e) => e.kind === "decision.superseded")).toMatchObject({
      actor: "wayglass",
      decision_id: first.id,
      data: { previous_status: "approved", previous_decided_by: "admin", approved_by: ["admin"] },
    });
    const land = await call("POST", `/v1/intents/${child.id}/land`, { project }, as("agent-b"));
    expect(land.json.error?.message).toMatch(/superseded.*propose again/);
  });

  it("re-stacks onto the grandparent when the abandoned parent was itself stacked", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const g = await declared(project, "agent-a");
    const gHead = commitOn(g.fork, { "docs/a.md": "a\n" });
    await call("POST", `/v1/intents/${g.id}/push`, { project, paths: ["docs/a.md"], commit: gHead }, as("agent-a"));
    const p = await declared(project, "agent-a", ["docs/**"], g.id);
    const pHead = commitOn(p.fork, { "docs/b.md": "b\n" });
    await call("POST", `/v1/intents/${p.id}/push`, { project, paths: ["docs/b.md"], commit: pHead }, as("agent-a"));
    const c = await declared(project, "agent-b", ["docs/**"], p.id);
    await abandon(project, p.id, "agent-a");
    const intents = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[];
    expect(intents.find((i) => i.id === c.id)).toMatchObject({ parent_id: g.id, base_commit: gHead });
  });
});

describe("stale intents", () => {
  it("reports open intents with no push for STALE_INTENT_HOURS, in lists and overlaps", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const b = await declared(project, "agent-b");
    let intents = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[];
    expect(intents.map((i) => i.stale)).toEqual([false, false]);

    const old = new Date(Date.now() - 30 * 3_600_000).toISOString();
    await runInDurableObject(await projectStub(env, project), async (_i: Project, state) => {
      state.storage.sql.exec("UPDATE intents SET created_at = ? WHERE id = ?", old, a.id);
    });
    intents = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[];
    expect(intents.find((i) => i.id === a.id)).toMatchObject({ stale: true, last_push_at: null, status: "open" });
    const push = await call("POST", `/v1/intents/${b.id}/push`, { project, paths: ["docs/x.md"] }, as("agent-b"));
    expect(push.json.data.overlaps).toEqual([expect.objectContaining({ intent_id: a.id, stale: true })]);

    // A push clears it; the var moves the cut-off; stale is never acted on.
    await call("POST", `/v1/intents/${a.id}/push`, { project, paths: ["docs/y.md"] }, as("agent-a"));
    intents = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[];
    expect(intents.find((i) => i.id === a.id)).toMatchObject({ stale: false, status: "open" });
    expect(intents.find((i) => i.id === a.id).last_push_at).toEqual(expect.any(String));
  });

  it("never marks landed or abandoned intents stale", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    await runInDurableObject(await projectStub(env, project), async (_i: Project, state) => {
      state.storage.sql.exec("UPDATE intents SET created_at = '2020-01-01T00:00:00.000Z' WHERE id = ?", a.id);
    });
    await abandon(project, a.id, "agent-a");
    const intents = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[];
    expect(intents[0]).toMatchObject({ status: "abandoned", stale: false });
  });
});
