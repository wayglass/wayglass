// Intent lifecycle holes: who may report a push, propose racing a restack or
// a land, a child of a parent that finished in between, what a merge may
// hold in an approved path, a late `landed` after the token expired, and
// refused verdicts in the audit log.

import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import type { Project } from "../src/project";
import { projectStub } from "../src/stub";
import type { FakeRepoHost, Files } from "./fake-artifacts";
import { call, createProject, KEY, useFakeArtifacts } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "README.md": "# demo\n", "docs/usage.md": "Usage\n", "web/login.ts": "export function login() {}\n" };

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

const as = (agent: string, key = KEY) => ({ authorization: `Bearer ${key}`, "x-wayglass-agent": agent });

async function mint(agent: string, kind?: "human" | "agent"): Promise<string> {
  const r = await call("POST", "/v1/keys", { agent, ...(kind ? { kind } : {}) });
  expect(r.status).toBe(201);
  return r.json.data.key;
}

async function declared(project: string, agent: string, paths = ["docs/**"], headers = as(agent), on?: string) {
  const r = await call("POST", "/v1/intents", { project, goal: `work by ${agent}`, reason: "because", paths, ...(on ? { on } : {}) }, headers);
  expect(r.status).toBe(201);
  return { id: r.json.data.intent.id as string, fork: r.json.data.fork.name as string };
}

function commitOn(fork: string, edits: Files): string {
  const head = fake.heads.get(fork);
  return fake.commit(fork, { ...(head ? fake.filesAt(head) : {}), ...edits });
}

const audit = async (project: string) => (await call("GET", `/v1/audit?project=${project}&limit=500`)).json.data.events as { kind: string; actor: string; data: any }[];

/** A project on BASE with an auto-merged docs change by agent-a, approved and ready to land. */
async function approvedDocsChange() {
  const project = await createProject();
  const base = fake.commit(project, BASE);
  const a = await declared(project, "agent-a");
  const head = commitOn(a.fork, { "docs/usage.md": "Usage\nMore.\n" });
  const p = await call("POST", `/v1/intents/${a.id}/propose`, { project }, as("agent-a"));
  expect(p.json.data.decision.status).toBe("approved");
  return { project, base, id: a.id, fork: a.fork, head };
}

describe("push: only the author or the admin records one", () => {
  it("refuses another agent's bound key, and takes the author's key and the admin", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const author = await mint("agent-a");
    const other = await mint("agent-b");
    const a = await declared(project, "agent-a", ["docs/**"], as("agent-a", author));
    const c = commitOn(a.fork, { "docs/usage.md": "Usage\nMore.\n" });

    const denied = await call("POST", `/v1/intents/${a.id}/push`, { project, paths: ["web/login.ts"], commit: c }, as("agent-b", other));
    expect(denied.status).toBe(403);
    expect(denied.json.error?.code).toBe("forbidden");
    // A bound key cannot claim the author's name through the header.
    expect((await call("POST", `/v1/intents/${a.id}/push`, { project, paths: ["web/login.ts"] }, as("agent-a", other))).status).toBe(403);
    let intent = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents[0];
    expect(intent).toMatchObject({ actual_paths: [], head_commit: null, last_push_at: null });
    expect((await audit(project)).map((e) => e.kind)).not.toContain("intent.pushed");

    expect((await call("POST", `/v1/intents/${a.id}/push`, { project, paths: ["docs/usage.md"], commit: c }, as("agent-a", author))).status).toBe(200);
    expect((await call("POST", `/v1/intents/${a.id}/push`, { project, paths: ["docs/more.md"] }, as("someone@laptop"))).status).toBe(200);
    intent = (await call("GET", `/v1/intents?project=${project}`)).json.data.intents[0];
    expect(intent).toMatchObject({ actual_paths: ["docs/more.md", "docs/usage.md"], head_commit: c });
  });
});

describe("propose: races with restack and land", () => {
  it("refuses a decision diffed on a fork the intent has since left", async () => {
    const project = await createProject();
    const base = fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const head = commitOn(a.fork, { "docs/usage.md": "Usage\nMore.\n" });
    // What the route would record after reading the intent, if a restack
    // moved the intent to another fork while it was reading Artifacts.
    const r = await runInDurableObject(await projectStub(env, project), (i: Project) =>
      i.propose(a.id, {
        decision_id: "d0000000001",
        head_commit: head,
        fork_repo: `${a.fork}--r1`,
        base_commit: base,
        decision: { v: 3, tags: { areas: ["docs"], data_classes: [], risk: "low", jurisdiction: "us", lines_changed: 1, files_changed: 1, evidence: [] }, matched_rule: "rules[4]", action: "auto_merge", approvers: 1, reason: "docs", default: false, shadow_matches: [], would_differ: false },
        policy_commit: base,
        policy_sha256: "sha256:x",
        paths: ["docs/usage.md"],
        truncated_reasons: [],
        client_preview: null,
        actor: "agent-a",
      }),
    );
    expect(r).toMatchObject({ ok: false, code: "conflict" });
    expect((r as { message: string }).message).toMatch(/re-stacked while it was being proposed/);
    expect((await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions).toEqual([]);
    expect((await call("GET", `/v1/intents?project=${project}`)).json.data.intents[0].status).toBe("open");
  });

  it("refuses while a trunk token for the earlier decision is out", async () => {
    const { project, id, fork } = await approvedDocsChange();
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as("agent-a"))).status).toBe(201);
    commitOn(fork, { "docs/usage.md": "Usage\nMore.\nAnd more.\n" });
    const r = await call("POST", `/v1/intents/${id}/propose`, { project }, as("agent-a"));
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/is landing .*; finish or abort the land before proposing again/);
    expect((await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions).toHaveLength(1);
    // Abort frees it; the new head can then be proposed.
    expect((await call("POST", `/v1/intents/${id}/land/abort`, { project, reason: "changed my mind" }, as("agent-a"))).status).toBe(200);
    expect((await call("POST", `/v1/intents/${id}/propose`, { project }, as("agent-a"))).status).toBe(201);
  });
});

describe("declare --on: the parent must still be unlanded when the intent is recorded", () => {
  it("refuses a child of a parent that was abandoned after the route checked it", async () => {
    const project = await createProject();
    const base = fake.commit(project, BASE);
    const a = await declared(project, "agent-a");
    const head = commitOn(a.fork, { "docs/usage.md": "Usage\nMore.\n" });
    expect((await call("POST", `/v1/intents/${a.id}/abandon`, { project, reason: "superseded by a simpler approach" }, as("agent-a"))).status).toBe(200);
    const r = await runInDurableObject(await projectStub(env, project), (i: Project) =>
      i.createIntent({
        id: "i0000000002",
        goal: "child",
        reason: "because",
        agent: "agent-b",
        declared_paths: ["docs/**"],
        fork_repo: `${project}--i0000000002`,
        fork_remote: "https://fake.artifacts.invalid/git/test/x.git",
        base_commit: head,
        parent_id: a.id,
        trunk_base: base,
        fork_token: { id: "tokx", expires_at: new Date(Date.now() + 60_000).toISOString() },
      }),
    );
    expect(r).toMatchObject({ ok: false, code: "conflict" });
    expect((r as { message: string }).message).toMatch(/is abandoned; stack only on an unlanded intent/);
    expect((await call("GET", `/v1/intents?project=${project}`)).json.data.intents).toHaveLength(1);
  });
});

describe("landed: what a merge may hold in an approved path", () => {
  it("rejects a merge whose approved file is not the approved version when the trunk never touched it", async () => {
    const { project, base, id, head } = await approvedDocsChange();
    // Trunk moved in another file, so the client has to merge rather than fast-forward.
    const moved = fake.commit(project, { ...fake.filesAt(base), "README.md": "# demo v2\n" });
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as("agent-a"))).status).toBe(201);
    // A hand-made "merge": right parents, decided path only, but not the approved content.
    const merge = fake.commit(project, { ...fake.filesAt(moved), "docs/usage.md": "Usage\nMore.\nexec(evil)\n" }, [moved, head]);
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge }, as("agent-a"));
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/docs\/usage.md .*neither the approved version nor a merge with a trunk edit/);
    expect((await call("GET", `/v1/intents?project=${project}`)).json.data.intents[0].status).toBe("proposed");
    const ev = (await audit(project)).find((e) => e.kind === "land.mismatch")!;
    expect(ev.data.problems).toEqual([expect.stringMatching(/neither the approved version/)]);
    expect((await call("GET", `/v1/why?project=${project}&commit=${merge}`)).status).toBe(404);
  });

  it("accepts the approved version, and a different one where the trunk edited the same file", async () => {
    const { project, base, id, head } = await approvedDocsChange();
    const moved = fake.commit(project, { ...fake.filesAt(base), "README.md": "# demo v2\n", "docs/usage.md": "Intro\nUsage\n" });
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as("agent-a"))).status).toBe(201);
    const merge = fake.commit(project, { ...fake.filesAt(moved), "docs/usage.md": "Intro\nUsage\nMore.\n" }, [moved, head]);
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge }, as("agent-a"))).status).toBe(200);
  });

  it("accepts a merge that takes the approved file as is", async () => {
    const { project, base, id, head } = await approvedDocsChange();
    const moved = fake.commit(project, { ...fake.filesAt(base), "README.md": "# demo v2\n" });
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as("agent-a"))).status).toBe(201);
    const merge = fake.commit(project, { ...fake.filesAt(moved), ...fake.filesAt(head), "README.md": "# demo v2\n" }, [moved, head]);
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge }, as("agent-a"));
    expect(r.status).toBe(200);
    expect(r.json.data.intent.status).toBe("landed");
  });
});

describe("landed after the trunk token expired", () => {
  /** Moves the newest land of `id` into the past, as if its token had expired. */
  async function expireLand(project: string, id: string) {
    await runInDurableObject(await projectStub(env, project), async (_i: Project, state) => {
      state.storage.sql.exec("UPDATE lands SET expires_at = ? WHERE intent_id = ?", new Date(Date.now() - 1000).toISOString(), id);
    });
  }

  it("still verifies and records the trunk commit once another land marked it expired", async () => {
    const { project, id, head } = await approvedDocsChange();
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as("agent-a"))).status).toBe(201);
    fake.setHead(project, head); // pushed while the token lived
    await expireLand(project, id);
    // Another intent lands next: the slot is free and the stale land is marked expired.
    const b = await declared(project, "agent-b", ["docs/b.md"]);
    commitOn(b.fork, { "docs/b.md": "b\n" });
    expect((await call("POST", `/v1/intents/${b.id}/propose`, { project }, as("agent-b"))).json.data.decision.status).toBe("approved");
    expect((await call("POST", `/v1/intents/${b.id}/land`, { project }, as("agent-b"))).status).toBe(201);
    await runInDurableObject(await projectStub(env, project), async (_i: Project, state) => {
      expect(state.storage.sql.exec("SELECT status FROM lands WHERE intent_id = ?", id).one().status).toBe("expired");
    });

    const late = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as("agent-a"));
    expect(late.status).toBe(200);
    expect(late.json.data).toMatchObject({ intent: { status: "landed" }, land: { status: "landed", landed_commit: head } });
    expect((await call("GET", `/v1/why?project=${project}&commit=${head}`)).json.data.decision.landed_commit).toBe(head);
    // Closed for good: no second report, no abort.
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as("agent-a"))).status).toBe(409);
    expect((await call("POST", `/v1/intents/${id}/land/abort`, { project, reason: "late" }, as("agent-a"))).status).toBe(409);
  });

  it("can be aborted late too, and an abandoned intent never becomes landed", async () => {
    const first = await approvedDocsChange();
    expect((await call("POST", `/v1/intents/${first.id}/land`, { project: first.project }, as("agent-a"))).status).toBe(201);
    await expireLand(first.project, first.id);
    const aborted = await call("POST", `/v1/intents/${first.id}/land/abort`, { project: first.project, reason: "merge conflict" }, as("agent-a"));
    expect(aborted.status).toBe(200);
    expect(aborted.json.data.land.status).toBe("cancelled");

    const { project, id, head } = await approvedDocsChange();
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as("agent-a"))).status).toBe(201);
    await expireLand(project, id);
    expect((await call("POST", `/v1/intents/${id}/abandon`, { project, reason: "superseded by a simpler approach" }, as("agent-a"))).status).toBe(200);
    fake.setHead(project, head);
    const late = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as("agent-a"));
    expect(late.status).toBe(409);
    expect(late.json.error?.message).toMatch(/is abandoned/);
    expect((await call("GET", `/v1/intents?project=${project}`)).json.data.intents[0].status).toBe("abandoned");
    const ev = (await audit(project)).find((e) => e.kind === "land.mismatch")!;
    expect(ev.data).toMatchObject({ commit: head, problems: [expect.stringMatching(/is abandoned/)] });
  });
});

describe("refused verdicts are audited", () => {
  it("records an agent key trying to decide a change routed to a person, and an author deciding its own", async () => {
    const project = await createProject();
    fake.commit(project, BASE);
    const author = await mint("agent-a");
    const other = await mint("agent-b");
    const a = await declared(project, "agent-a", ["web/**"], as("agent-a", author));
    commitOn(a.fork, { "web/login.ts": "export function login() { return token }\n" });
    const p = await call("POST", `/v1/intents/${a.id}/propose`, { project }, as("agent-a", author));
    expect(p.json.data.decision.status).toBe("pending_human");
    const did = p.json.data.decision.id as string;

    const agentTry = await call("POST", `/v1/decisions/${did}`, { project, verdict: "approve", reason: "checked the token handling" }, as("agent-b", other));
    expect(agentTry.status).toBe(403);
    expect(agentTry.json.error?.code).toBe("needs_human");
    const selfTry = await call("POST", `/v1/decisions/${did}`, { project, verdict: "approve", reason: "checked my own token handling" }, as("agent-a"));
    expect(selfTry.status).toBe(409);

    const refused = (await audit(project)).filter((e) => e.kind === "decision.refused");
    expect(refused).toEqual([
      expect.objectContaining({ actor: "agent-b", intent_id: a.id, decision_id: did, data: expect.objectContaining({ code: "needs_human", verdict: "approve", action: "human_approve", identity_source: "key" }) }),
      expect.objectContaining({ actor: "admin", decision_id: did, data: expect.objectContaining({ code: "conflict", verdict: "approve", identity_source: "admin", claimed_by: "agent-a" }) }),
    ]);
    // The decision itself is untouched: no approval rows, still pending.
    const d = (await call("GET", `/v1/decisions?project=${project}`)).json.data.decisions[0];
    expect(d).toMatchObject({ status: "pending_human", approvals: [] });
  });
});
