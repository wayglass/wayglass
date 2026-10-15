// POST /v1/intents/:id/restack: the overlap re-plan "stack" in one step. The
// Worker forks the new parent's fork (like declare --on), moves the intent
// onto it and keeps the old fork; the client merges the intent's own commits
// into the new fork. A decision made before no longer covers the change.

import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
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

/** Commits edits on a fork and reports the push, like `wayglass push`. */
async function pushed(project: string, agent: string, fork: string, id: string, edits: Files): Promise<string> {
  const head = fake.heads.get(fork);
  const c = fake.commit(fork, { ...(head ? fake.filesAt(head) : {}), ...edits });
  const r = await call("POST", `/v1/intents/${id}/push`, { project, paths: Object.keys(edits), commit: c }, as(agent));
  expect(r.status).toBe(200);
  return c;
}

const restack = (project: string, id: string, on: string, agent: string, reason = "both edit docs/usage.md; build on the guide instead", key = KEY) =>
  call("POST", `/v1/intents/${id}/restack`, { project, on, reason }, as(agent, key));

const intentOf = async (project: string, id: string) =>
  ((await call("GET", `/v1/intents?project=${project}`)).json.data.intents as any[]).find((i) => i.id === id);

async function mint(agent: string): Promise<string> {
  const r = await call("POST", "/v1/keys", { agent });
  expect(r.status).toBe(201);
  return r.json.data.key;
}

describe("intent restack", () => {
  it("forks the new parent's fork, moves the intent there, keeps the old fork and audits the reason", async () => {
    const project = await createProject();
    const trunk = fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const aHead = await pushed(project, "agent-a", a.fork, a.id, { "docs/usage.md": "Usage\nA.\n" });
    const b = await declared(project, "agent-b");
    await pushed(project, "agent-b", b.fork, b.id, { "docs/usage.md": "Usage\nB.\n" });
    const revokedBefore = fake.revoked.length;

    const r = await restack(project, b.id, a.id, "agent-b");
    expect(r.status).toBe(201);
    const newFork = `${project}--${b.id}--r1`;
    expect(r.json.data).toMatchObject({
      intent: { id: b.id, parent_id: a.id, base_commit: aHead, trunk_base: trunk, fork_repo: newFork, head_commit: null, status: "open" },
      fork: { name: newFork },
      token: { scope: "write" },
      previous: { parent_id: null, base_commit: trunk, fork_repo: b.fork, fork_kept: true, tokens_revoked: 1 },
      superseded_decision: null,
      overlaps: [], // a stack is not an overlap
    });
    expect(fake.repos.get(newFork)).toEqual({ source: a.fork });
    expect(fake.heads.get(newFork)).toBe(aHead);
    expect(fake.repos.has(b.fork)).toBe(true);
    expect(fake.revoked.slice(revokedBefore)).toEqual([expect.objectContaining({ repo: b.fork })]);
    expect(fake.tokens.at(-1)).toMatchObject({ repo: newFork, scope: "write" });

    const ev = (await call("GET", `/v1/audit?project=${project}`)).json.data.events.find((e: any) => e.kind === "intent.restacked");
    expect(ev).toMatchObject({
      actor: "agent-b",
      intent_id: b.id,
      data: {
        reason: "both edit docs/usage.md; build on the guide instead",
        by_admin: false,
        from_parent: null,
        to_parent: a.id,
        base_commit: aHead,
        previous_base_commit: trunk,
        fork_repo: newFork,
        previous_fork_repo: b.fork,
      },
    });

    // The fresh fork token route now serves the new fork.
    const tok = await call("POST", `/v1/intents/${b.id}/token`, { project }, as("agent-b"));
    expect(tok.json.data.fork.repo).toBe(newFork);

    // After the client merges its commits in, propose covers only B's own change.
    const merged = fake.commit(newFork, { ...fake.filesAt(aHead), "docs/usage.md": "Usage\nA.\nB.\n" });
    const p = await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"));
    expect(p.status).toBe(201);
    expect(p.json.data.decision).toMatchObject({ commit_sha: merged, base_commit: aHead, paths: ["docs/usage.md"] });
    // Stacked: it lands after A.
    const land = await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"));
    expect(land.status).toBe(409);
    expect(land.json.error?.message).toMatch(new RegExp(`land ${a.id} first`));
  });

  it("invalidates an earlier decision: land refuses it until the intent proposes again", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const aHead = await pushed(project, "agent-a", a.fork, a.id, { "docs/usage.md": "Usage\nA.\n" });
    const b = await declared(project, "agent-b");
    await pushed(project, "agent-b", b.fork, b.id, { "docs/b.md": "b\n" });
    const first = await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"));
    expect(first.json.data.decision.status).toBe("approved");

    const r = await restack(project, b.id, a.id, "agent-b");
    expect(r.status).toBe(201);
    expect(r.json.data.superseded_decision).toBe(first.json.data.decision.id);
    expect(r.json.data.intent.status).toBe("proposed");
    // An approved decision is superseded too, but keeps who decided it (here, policy).
    const kept = ((await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions as any[]).find((d) => d.id === first.json.data.decision.id);
    expect(kept).toMatchObject({ status: "superseded", decided_by: "policy" });

    // Even once A has landed, the old decision covered another base.
    fake.commit(`${project}--${b.id}--r1`, { ...fake.filesAt(aHead), "docs/b.md": "b\n" });
    expect((await call("POST", `/v1/intents/${a.id}/propose`, { project }, as("agent-a"))).json.data.decision.status).toBe("approved");
    expect((await call("POST", `/v1/intents/${a.id}/land`, { project }, as("agent-a"))).status).toBe(201);
    fake.setHead(project, aHead);
    expect((await call("POST", `/v1/intents/${a.id}/landed`, { project, commit: aHead }, as("agent-a"))).status).toBe(200);
    const stale = await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"));
    expect(stale.status).toBe(409);
    expect(stale.json.error?.message).toMatch(/superseded.*re-stacked.*propose again/);
    const again = await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"));
    expect(again.json.data.decision).toMatchObject({ status: "approved", base_commit: aHead, paths: ["docs/b.md"] });
    expect((await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"))).status).toBe(201);
  });

  it("supersedes a pending decision: nobody can decide it and it leaves the review queues", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a", ["web/**"]);
    await pushed(project, "agent-a", a.fork, a.id, { "web/login.ts": "export function login() { /* A */ }\n" });
    const b = await declared(project, "agent-b", ["web/**"]);
    await pushed(project, "agent-b", b.fork, b.id, { "web/login.ts": "export function login() { /* B */ }\n" });
    const first = (await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"))).json.data.decision;
    expect(first.status).toBe("pending_human");
    expect((await call("GET", `/v1/decisions?project=${project}&status=pending_human`)).json.data.decisions).toHaveLength(1);

    const r = await restack(project, b.id, a.id, "agent-b");
    expect(r.status).toBe(201);
    expect(r.json.data.superseded_decision).toBe(first.id);

    const all = (await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions as any[];
    expect(all.find((d) => d.id === first.id)).toMatchObject({ status: "superseded", decided_by: "agent-b" });
    expect((await call("GET", `/v1/decisions?project=${project}&status=pending_human`)).json.data.decisions).toEqual([]);
    expect((await call("GET", `/v1/decisions?project=${project}&status=superseded`)).json.data.decisions).toHaveLength(1);

    // A person (the admin key) cannot approve it either, by intent or by decision id; nothing is recorded.
    const byIntent = await call("POST", `/v1/intents/${b.id}/decide`, { project, verdict: "approve", reason: "checked the login flow" }, as("reviewer"));
    expect(byIntent.status).toBe(409);
    expect(byIntent.json.error).toMatchObject({ code: "superseded" });
    expect(byIntent.json.error?.message).toMatch(/re-stacked.*propose again/);
    const byId = await call("POST", `/v1/decisions/${first.id}`, { project, verdict: "reject", reason: "checked the login flow" }, as("reviewer"));
    expect(byId.json.error).toMatchObject({ code: "superseded" });

    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as any[];
    expect(events.filter((e) => e.kind.startsWith("decision.approve") || e.kind.startsWith("decision.reject"))).toEqual([]);
    expect(events.find((e) => e.kind === "decision.superseded")).toMatchObject({
      actor: "agent-b",
      intent_id: b.id,
      decision_id: first.id,
      data: { previous_status: "pending_human", reason: expect.stringMatching(/restacked onto .*propose/) },
    });

    const land = await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"));
    expect(land.json.error).toMatchObject({ code: "conflict", message: expect.stringMatching(/superseded.*propose again/) });

    // Proposing again (after the client merged B onto A) gives a fresh decision that a person can decide.
    const rFork = `${project}--${b.id}--r1`;
    fake.commit(rFork, { ...fake.filesAt(fake.heads.get(rFork)!), "web/login.ts": "export function login() { /* A, B */ }\n" });
    const again = (await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"))).json.data.decision;
    expect(again.status).toBe("pending_human");
    const ok = await call("POST", `/v1/intents/${b.id}/decide`, { project, verdict: "approve", reason: "checked the login flow" }, as("reviewer"));
    expect(ok.status).toBe(200);
    expect(ok.json.data.decision).toMatchObject({ id: again.id, status: "approved" });
  });

  it("supersedes an approval by a person, keeping who approved in the decision, its approvals, why and the audit", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a", ["web/**"]);
    await pushed(project, "agent-a", a.fork, a.id, { "web/login.ts": "export function login() { /* A */ }\n" });
    const b = await declared(project, "agent-b", ["web/**"]);
    const bHead = await pushed(project, "agent-b", b.fork, b.id, { "web/login.ts": "export function login() { /* B */ }\n" });
    const first = (await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"))).json.data.decision;
    expect(first.status).toBe("pending_human");
    const approved = await call("POST", `/v1/intents/${b.id}/decide`, { project, verdict: "approve", reason: "checked the login flow" }, as("reviewer"));
    expect(approved.json.data.decision).toMatchObject({ status: "approved", decided_by: "admin" }); // the admin key, naming itself "reviewer"
    const decidedAt = approved.json.data.decision.decided_at;

    const r = await restack(project, b.id, a.id, "agent-b");
    expect(r.status).toBe(201);
    expect(r.json.data.superseded_decision).toBe(first.id);

    // Only the status changes: who approved, when, and the approval itself stay.
    const d = ((await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions as any[]).find((x) => x.id === first.id);
    expect(d).toMatchObject({ status: "superseded", decided_by: "admin", decided_at: decidedAt });
    expect(d.approvals).toMatchObject([{ by: "admin", claimed_by: "reviewer", verdict: "approve", reason: "checked the login flow" }]);
    const why = await call("GET", `/v1/why?project=${project}&commit=${bHead}`);
    expect(why.json.data.decision).toMatchObject({ id: first.id, status: "superseded", decided_by: "admin" });
    expect(why.json.data.approvals).toMatchObject([{ by: "admin", claimed_by: "reviewer", verdict: "approve" }]);

    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as any[];
    expect(events.find((e) => e.kind === "decision.approve")).toMatchObject({ actor: "admin", decision_id: first.id, data: { claimed_by: "reviewer" } });
    expect(events.find((e) => e.kind === "decision.superseded")).toMatchObject({
      actor: "agent-b",
      decision_id: first.id,
      data: { previous_status: "approved", previous_decided_by: "admin", previous_decided_at: decidedAt, approved_by: ["admin"], reason: expect.stringMatching(/restacked onto .*approval.*proposed again/) },
    });

    // Nobody can decide it again or land it; proposing again starts a fresh review.
    const redo = await call("POST", `/v1/intents/${b.id}/decide`, { project, verdict: "approve", reason: "checked the login flow" }, as("reviewer"));
    expect(redo.json.error).toMatchObject({ code: "superseded" });
    const land = await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"));
    expect(land.status).toBe(409);
    expect(land.json.error?.message).toMatch(/superseded.*propose again/);
    const rFork = `${project}--${b.id}--r1`;
    fake.commit(rFork, { ...fake.filesAt(fake.heads.get(rFork)!), "web/login.ts": "export function login() { /* A, B */ }\n" });
    const again = (await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"))).json.data.decision;
    expect(again).toMatchObject({ status: "pending_human", decided_by: null });
  });

  it("re-stacks onto the same parent's newer head, and refuses when there is nothing new", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "1\n" });
    const b = await declared(project, "agent-b", ["docs/**"], a.id);
    const same = await restack(project, b.id, a.id, "agent-b");
    expect(same.status).toBe(409);
    expect(same.json.error?.message).toMatch(/already stacked on .* at its head/);
    const a2 = await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "2\n" });
    const r = await restack(project, b.id, a.id, "agent-b", "A pushed a fix B depends on");
    expect(r.status).toBe(201);
    expect(r.json.data.intent).toMatchObject({ parent_id: a.id, base_commit: a2, fork_repo: `${project}--${b.id}--r1` });
    // A second restack gets the next fork name; both old forks are kept.
    const a3 = await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "3\n" });
    const r2 = await restack(project, b.id, a.id, "agent-b", "A pushed another fix B depends on");
    expect(r2.json.data.intent).toMatchObject({ base_commit: a3, fork_repo: `${project}--${b.id}--r2` });
    expect(r2.json.data.previous.fork_repo).toBe(`${project}--${b.id}--r1`);
    expect(fake.repos.has(b.fork) && fake.repos.has(`${project}--${b.id}--r1`)).toBe(true);
  });

  it("refuses cycles, itself, landed and abandoned parents, and a parent with nothing pushed", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "a\n" });
    const b = await declared(project, "agent-b", ["docs/**"], a.id);
    await pushed(project, "agent-b", b.fork, b.id, { "docs/b.md": "b\n" });
    const c = await declared(project, "agent-c", ["docs/**"], b.id);
    await pushed(project, "agent-c", c.fork, c.id, { "docs/c.md": "c\n" });

    const cycle = await restack(project, a.id, c.id, "agent-a");
    expect(cycle.status).toBe(409);
    expect(cycle.json.error?.message).toMatch(/cycle/);
    expect((await restack(project, a.id, b.id, "agent-a")).json.error?.message).toMatch(/cycle/);
    expect((await restack(project, a.id, a.id, "agent-a")).json.error?.message).toMatch(/itself/);

    const empty = await declared(project, "agent-d");
    const nothing = await restack(project, a.id, empty.id, "agent-a");
    expect(nothing.status).toBe(409);
    expect(nothing.json.error?.message).toMatch(/nothing pushed/);

    const gone = await declared(project, "agent-e");
    await pushed(project, "agent-e", gone.fork, gone.id, { "docs/e.md": "e\n" });
    await call("POST", `/v1/intents/${gone.id}/abandon`, { project, reason: "superseded by a smaller plan" }, as("agent-e"));
    const abandoned = await restack(project, a.id, gone.id, "agent-a");
    expect(abandoned.status).toBe(409);
    expect(abandoned.json.error?.message).toMatch(/abandoned/);

    // Landed parent: its change is already on trunk.
    const l = await declared(project, "agent-l");
    const lHead = await pushed(project, "agent-l", l.fork, l.id, { "docs/l.md": "l\n" });
    await call("POST", `/v1/intents/${l.id}/propose`, { project }, as("agent-l"));
    await call("POST", `/v1/intents/${l.id}/land`, { project }, as("agent-l"));
    fake.setHead(project, lHead);
    expect((await call("POST", `/v1/intents/${l.id}/landed`, { project, commit: lHead }, as("agent-l"))).status).toBe(200);
    const landed = await restack(project, a.id, l.id, "agent-a");
    expect(landed.status).toBe(409);
    expect(landed.json.error?.message).toMatch(/landed.*already on trunk/);

    // A restacked intent cannot be restacked once it is abandoned either.
    const abandonedSelf = await restack(project, gone.id, a.id, "agent-e");
    expect(abandonedSelf.json.error?.message).toMatch(/abandoned/);
    expect((await intentOf(project, a.id)).parent_id).toBeNull();
  });

  it("requires a real reason and an intent id", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "a\n" });
    const b = await declared(project, "agent-b");
    for (const reason of ["", "<why>", "LGTM", "ok!"]) {
      expect((await restack(project, b.id, a.id, "agent-b", reason)).status).toBe(400);
    }
    expect((await call("POST", `/v1/intents/${b.id}/restack`, { project, on: "main", reason: "a real reason" }, as("agent-b"))).status).toBe(400);
    expect((await intentOf(project, b.id)).parent_id).toBeNull();
  });

  it("lets only the author or the admin restack", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "a\n" });
    const b = await declared(project, "agent-b");
    const other = await mint("agent-x");
    const denied = await restack(project, b.id, a.id, "agent-b", "I would like it stacked", other);
    expect(denied.status).toBe(403);
    expect(denied.json.error?.message).toMatch(/only agent-b/);
    expect(fake.repos.has(`${project}--${b.id}--r1`)).toBe(false);
    const byAdmin = await restack(project, b.id, a.id, "john@mac", "agent-b is offline; the overlap blocks the demo");
    expect(byAdmin.status).toBe(201);
    const ev = (await call("GET", `/v1/audit?project=${project}`)).json.data.events.at(-1);
    expect(ev).toMatchObject({ kind: "intent.restacked", actor: "john@mac", data: { by_admin: true } });
  });

  it("refuses while the intent is landing, and gives the new fork back if the record fails", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    await pushed(project, "agent-a", a.fork, a.id, { "docs/a.md": "a\n" });
    const b = await declared(project, "agent-b");
    await pushed(project, "agent-b", b.fork, b.id, { "docs/b.md": "b\n" });
    await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"));
    expect((await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"))).status).toBe(201);
    const r = await restack(project, b.id, a.id, "agent-b");
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/landing/);
    expect(fake.repos.has(`${project}--${b.id}--r1`)).toBe(false);

    // A parent abandoned between the check and the record (the fork call is
    // where the race window is): the new fork is deleted again.
    const c = await declared(project, "agent-c");
    const fork = fake.fork.bind(fake);
    fake.fork = async (source: string, name: string) => {
      const ref = await fork(source, name);
      await call("POST", `/v1/intents/${a.id}/abandon`, { project, reason: "dropped mid restack" }, as("agent-a"));
      return ref;
    };
    const raced = await restack(project, c.id, a.id, "agent-c");
    expect(raced.status).toBe(409);
    expect(fake.deleted).toContain(`${project}--${c.id}--r1`);
    expect((await intentOf(project, c.id)).fork_repo).toBe(c.fork);
  });

  it("is removed with the project: old and new forks, newest first", async () => {
    const name = `demo-test-restack-${crypto.randomUUID().slice(0, 6)}`;
    expect((await call("POST", "/v1/projects", { name })).status).toBe(201);
    fake.commit(name, BASE);
    const a = await declared(name, "agent-a");
    const b = await declared(name, "agent-b");
    await pushed(name, "agent-b", b.fork, b.id, { "docs/b.md": "b\n" });
    // b (newer) first, then a stacks on it: a's new fork is made from b's fork.
    expect((await restack(name, a.id, b.id, "agent-a")).status).toBe(201);
    const r = await call("DELETE", `/v1/projects/${name}`, { confirm: name }, as("cleaner"));
    expect(r.status).toBe(200);
    expect(r.json.data.repos_deleted).toEqual([`${name}--${a.id}--r1`, b.fork, a.fork, name]);
  });
});
