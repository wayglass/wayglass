// Forkable agent sessions (golden path step 7): share freezes an intent's
// context; fork continues it in a NEW intent with its own fork repo and a
// write token for that repo only. The original fork's token is never handed on.

import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import { FORK_TOKEN_TTL_SECONDS } from "../src/artifacts";
import type { Project } from "../src/project";
import { projectStub } from "../src/stub";
import type { FakeRepoHost } from "./fake-artifacts";
import { call, createProject, KEY, useFakeArtifacts } from "./helpers";

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

const as = (agent: string) => ({ authorization: `Bearer ${KEY}`, "x-wayglass-agent": agent });

/** A project with a trunk commit, an intent by agent-a, and one commit on its fork. */
async function setup() {
  const project = await createProject();
  const base = fake.commit(project, { "README.md": "# demo\n", "web/login.ts": "x\n" });
  const d = await call("POST", "/v1/intents", { project, goal: "Rate-limit login", reason: "Brute force in the log", paths: ["web/**"] }, as("agent-a"));
  expect(d.status).toBe(201);
  const id: string = d.json.data.intent.id;
  const forkRepo: string = d.json.data.fork.name;
  const head = fake.commit(forkRepo, { "README.md": "# demo\n", "web/login.ts": "limited\n" });
  await call("POST", `/v1/intents/${id}/push`, { project, paths: ["web/login.ts"], commit: head }, as("agent-a"));
  return { project, id, forkRepo, base, head };
}

const share = (project: string, id: string, body: Record<string, unknown> = {}, agent = "agent-a") =>
  call("POST", `/v1/intents/${id}/sessions`, { project, ...body }, as(agent));

describe("session share", () => {
  it("snapshots the intent, the fork head read from Artifacts, and the notes", async () => {
    const { project, id, head } = await setup();
    const r = await share(project, id, { notes: "  Limiter done; tests for lockout still missing.\n", commit: head.slice(0, 12) });
    expect(r.status).toBe(201);
    const { session, url } = r.json.data;
    expect(session.id).toMatch(/^s[0-9a-f]{10}$/);
    expect(url).toBe(`https://wayglass.test/v1/projects/${project}/sessions/${session.id}`);
    expect(session).toMatchObject({ intent_id: id, shared_by: "agent-a", head_commit: head, notes: "Limiter done; tests for lockout still missing." });
    expect(session.snapshot.intent).toMatchObject({ id, goal: "Rate-limit login", actual_paths: ["web/login.ts"] });
    expect(session.snapshot.history.map((e: { kind: string }) => e.kind)).toEqual(["intent.declared", "intent.pushed"]);
    expect(session.snapshot.decisions).toEqual([]);
  });

  it("refuses a snapshot of a commit that is not on the fork", async () => {
    const { project, id } = await setup();
    const r = await share(project, id, { commit: "deadbeef" });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/push before sharing/);
  });

  it.each([
    ["notes not a string", { notes: 5 }, /notes must be a string/],
    ["notes too long", { notes: "x".repeat(32 * 1024 + 1) }, /longer than/],
    ["bad commit", { commit: "zz" }, /hex SHA/],
  ])("rejects %s", async (_name, body, msg) => {
    const { project, id } = await setup();
    const r = await share(project, id, body);
    expect(r.status).toBe(400);
    expect(r.json.error?.message).toMatch(msg);
  });

  it("is audited and append-only", async () => {
    const { project, id } = await setup();
    const sid = (await share(project, id, { notes: "n" })).json.data.session.id;
    const audit = await call("GET", `/v1/audit?project=${project}`);
    const ev = audit.json.data.events.at(-1);
    expect(ev).toMatchObject({ kind: "session.shared", actor: "agent-a", intent_id: id, data: { session_id: sid, notes_chars: 1 } });
    await runInDurableObject(await projectStub(env, project), async (_i: Project, state) => {
      const sql = state.storage.sql;
      expect(() => sql.exec("UPDATE sessions SET notes = 'rewritten'")).toThrow(/append-only/);
      expect(() => sql.exec("DELETE FROM sessions")).toThrow(/append-only/);
    });
  });

  it("returns the frozen snapshot by URL even after the intent moves on", async () => {
    const { project, id, forkRepo, head } = await setup();
    const sid = (await share(project, id, { notes: "first" })).json.data.session.id;
    const later = fake.commit(forkRepo, { "web/login.ts": "later\n" });
    await call("POST", `/v1/intents/${id}/push`, { project, paths: ["web/x.ts"], commit: later }, as("agent-a"));
    const r = await call("GET", `/v1/projects/${project}/sessions/${sid}`);
    expect(r.status).toBe(200);
    expect(r.json.data.session.head_commit).toBe(head);
    expect(r.json.data.session.snapshot.intent.actual_paths).toEqual(["web/login.ts"]);
    expect(r.json.data.forks).toEqual([]);
  });

  it.each([
    ["unknown session", (p: string) => `/v1/projects/${p}/sessions/s0000000000`, 404],
    ["bad session id", (p: string) => `/v1/projects/${p}/sessions/nope`, 400],
    ["bad project", () => `/v1/projects/Bad--Name/sessions/s0000000000`, 400],
  ])("GET with %s fails", async (_n, path, status) => {
    const { project } = await setup();
    const r = await call("GET", path(project));
    expect(r.status).toBe(status);
  });
});

describe("session fork", () => {
  it("stacks a new intent on the source, with a token for the new fork only", async () => {
    const { project, id, forkRepo, base, head } = await setup();
    const sid = (await share(project, id, { notes: "over to you" })).json.data.session.id;
    const tokensBefore = fake.tokens.length;

    const r = await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"));
    expect(r.status).toBe(201);
    const { intent, fork, token, parent, overlaps, session } = r.json.data;
    expect(intent.id).not.toBe(id);
    expect(intent).toMatchObject({
      agent: "agent-b",
      goal: "Rate-limit login",
      reason: "Brute force in the log",
      declared_paths: ["web/**"],
      // Stacked like `declare --on`: the diff starts at the source's head,
      // policy still comes from the trunk commit the source started on.
      parent_id: id,
      base_commit: head,
      trunk_base: base,
      parent_intent_id: id,
      session_id: sid,
      status: "open",
    });
    expect(fork.name).toBe(`${project}--${intent.id}`);
    expect(fake.repos.get(fork.name)).toEqual({ source: forkRepo });
    expect(fake.heads.get(fork.name)).toBe(head);
    expect(parent).toMatchObject({ id, agent: "agent-a", fork_repo: forkRepo });
    expect(session.notes).toBe("over to you");

    // Zero Trust: exactly one new token, write, short-lived, scoped to the new fork.
    const minted = fake.tokens.slice(tokensBefore);
    expect(minted).toEqual([{ id: expect.any(String), repo: fork.name, scope: "write", ttl: FORK_TOKEN_TTL_SECONDS }]);
    expect(token.value).toBe(`fake_token_for_${fork.name}`);

    // A stack is not an overlap: the child builds on the parent's code on purpose.
    expect(overlaps).toEqual([]);
  });

  it("stacks on a stacked source too, keeping the stack's trunk base", async () => {
    const { project, id, base, head } = await setup();
    // A child stacked on the first intent, with one commit of its own.
    const c = await call("POST", "/v1/intents", { project, goal: "Lockout page", reason: "Users need to know", paths: ["web/**"], on: id }, as("agent-a"));
    expect(c.status).toBe(201);
    const child = c.json.data.intent;
    const childHead = fake.commit(c.json.data.fork.name, { "README.md": "# demo\n", "web/login.ts": "limited\n", "web/lockout.ts": "x\n" });
    await call("POST", `/v1/intents/${child.id}/push`, { project, paths: ["web/lockout.ts"], commit: childHead }, as("agent-a"));
    const sid = (await share(project, child.id)).json.data.session.id;

    const r = await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"));
    expect(r.status).toBe(201);
    expect(r.json.data.intent).toMatchObject({ parent_id: child.id, trunk_base: base, base_commit: childHead, parent_intent_id: child.id });
    expect(head).not.toBe(childHead);
    // Same stack at every depth, so neither the source nor its parent is an overlap.
    expect(r.json.data.overlaps).toEqual([]);
  });

  it("lets the forking agent refresh its own token, not the parent's", async () => {
    const { project, id } = await setup();
    const sid = (await share(project, id)).json.data.session.id;
    const child = (await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"))).json.data.intent.id;
    expect((await call("POST", `/v1/intents/${child}/token`, { project }, as("agent-b"))).status).toBe(201);
    const denied = await call("POST", `/v1/intents/${id}/token`, { project }, as("agent-b"));
    expect(denied.status).toBe(403);
  });

  it("records the fork on both intents and lists it under the session", async () => {
    const { project, id } = await setup();
    const sid = (await share(project, id)).json.data.session.id;
    const child = (await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"))).json.data.intent.id;
    const events = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as { kind: string; intent_id: string; actor: string; data: any }[];
    expect(events.find((e) => e.kind === "session.forked")).toMatchObject({ intent_id: id, actor: "agent-b", data: { session_id: sid, child_intent_id: child } });
    expect(events.filter((e) => e.kind === "intent.declared").at(-1)).toMatchObject({ intent_id: child, data: { parent_intent_id: id, session_id: sid } });
    const view = await call("GET", `/v1/projects/${project}/sessions/${sid}`);
    expect(view.json.data.forks.map((i: { id: string }) => i.id)).toEqual([child]);
    // The parent's push does not flag its own stacked child.
    const push = await call("POST", `/v1/intents/${id}/push`, { project, paths: ["web/login.ts"] }, as("agent-a"));
    expect(push.json.data.overlaps).toEqual([]);
  });

  it("forks a session shared before any push alongside its source, flagged as family", async () => {
    const project = await createProject();
    const base = fake.commit(project, { "web/login.ts": "x\n" });
    const d = await call("POST", "/v1/intents", { project, goal: "Rate-limit login", reason: "Brute force", paths: ["web/**"] }, as("agent-a"));
    const id = d.json.data.intent.id;
    const sid = (await share(project, id, { notes: "not started" })).json.data.session.id;
    const r = await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"));
    expect(r.status).toBe(201);
    // Nothing to stack on: same base as the source, no parent.
    expect(r.json.data.intent).toMatchObject({ parent_id: null, base_commit: base, trunk_base: base, parent_intent_id: id });
    expect(r.json.data.overlaps).toEqual([expect.objectContaining({ intent_id: id, relation: "parent" })]);
  });

  it("proposes only the child's own change and lands after its source", async () => {
    const { project, id, head } = await setup();
    const sid = (await share(project, id)).json.data.session.id;
    const f = await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"));
    const child = f.json.data.intent.id;
    const childHead = fake.commit(f.json.data.fork.name, { ...fake.filesAt(head), "web/lockout.ts": "locked\n" });
    const p = await call("POST", `/v1/intents/${child}/propose`, { project }, as("agent-b"));
    expect(p.status).toBe(201);
    expect(p.json.data.decision).toMatchObject({ base_commit: head, commit_sha: childHead, paths: ["web/lockout.ts"] });
    const ok = await call("POST", `/v1/intents/${child}/decide`, { project, verdict: "approve", reason: "Lockout page reviewed" }, as("agent-c"));
    expect(ok.json.data.decision.status).toBe("approved");
    const early = await call("POST", `/v1/intents/${child}/land`, { project }, as("agent-b"));
    expect(early.status).toBe(409);
    expect(early.json.error?.message).toMatch(new RegExp(`stacked on ${id}`));
  });

  it("refuses to fork a session whose intent has landed or is unknown", async () => {
    const { project, id } = await setup();
    const sid = (await share(project, id)).json.data.session.id;
    await runInDurableObject(await projectStub(env, project), async (_i: Project, state) => {
      state.storage.sql.exec("UPDATE intents SET status = 'landed' WHERE id = ?", id);
    });
    const r = await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"));
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/landed/);
    expect((await call("POST", `/v1/projects/${project}/sessions/s0123456789/fork`, {}, as("agent-b"))).status).toBe(404);
    expect((await share(project, id)).status).toBe(409);
  });

  it("surfaces an Artifacts fork failure without creating an intent", async () => {
    const { project, id } = await setup();
    const sid = (await share(project, id)).json.data.session.id;
    fake.failNextFork = true;
    const r = await call("POST", `/v1/projects/${project}/sessions/${sid}/fork`, {}, as("agent-b"));
    expect(r.status).toBe(502);
    const list = await call("GET", `/v1/intents?project=${project}`);
    expect(list.json.data.intents).toHaveLength(1);
  });

  it("requires the admin key", async () => {
    const { project, id } = await setup();
    const sid = (await share(project, id)).json.data.session.id;
    const r = await call("GET", `/v1/projects/${project}/sessions/${sid}`, undefined, { authorization: "Bearer wrong" });
    expect(r.status).toBe(401);
  });
});
