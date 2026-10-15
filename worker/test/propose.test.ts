// Server-side decisions (ARCHITECTURE D3): the Worker re-derives every change
// from Artifacts objects and decides with the Go evaluator. Client-reported
// tags are stored as client_preview and never decide anything.

import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import { FORK_TOKEN_TTL_SECONDS, LAND_TOKEN_TTL_SECONDS, READ_TOKEN_TTL_SECONDS } from "../src/artifacts";
import type { FakeRepoHost, Files } from "./fake-artifacts";
import { call, createProject, createProjectIn, declare, KEY, uniqueProject, useFakeArtifacts } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = {
  ".wayglass/policy.yaml": POLICY,
  "README.md": "# demo\n",
  "docs/usage.md": "Usage\n",
  "web/login.ts": "export function login() {}\n",
  "internal/x.go": "package x\n",
};

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

/** A project whose trunk holds `base`, and one declared intent forked from it. */
async function setup(base: Files | null = BASE, agent = "agent-a", jurisdiction?: string) {
  const project = jurisdiction ? await createProjectIn(jurisdiction) : await createProject();
  const baseCommit = base ? fake.commit(project, base) : null;
  const r = await declare(project, ["**"], "do a thing", agent);
  expect(r.status).toBe(201);
  const id: string = r.json.data.intent.id;
  const fork: string = r.json.data.fork.name;
  return { project, id, fork, baseCommit };
}

/** Commits base + edits to the fork and returns the new head. */
function push(fork: string, edits: Files, drop: string[] = []): string {
  const head = fake.heads.get(fork);
  const files: Files = head ? fake.filesAt(head) : {};
  for (const p of drop) delete files[p];
  return fake.commit(fork, { ...files, ...edits });
}

const propose = (project: string, id: string, body: Record<string, unknown> = {}, envOverride: Partial<Env> = {}) =>
  call("POST", `/v1/intents/${id}/propose`, { project, ...body }, undefined, envOverride);

const audit = async (project: string) => (await call("GET", `/v1/audit?project=${project}&limit=500`)).json.data.events as { kind: string; data: any }[];

describe("propose: the server decides", () => {
  it("auto-merges a docs change from tags it derived itself", async () => {
    const { project, id, fork, baseCommit } = await setup();
    const head = push(fork, { "docs/usage.md": "Usage\nMore usage.\n" });
    const r = await propose(project, id);
    expect(r.status).toBe(201);
    const d = r.json.data.decision;
    expect(d).toMatchObject({ action: "auto_merge", status: "approved", matched_rule: "rules[4]", decided_by: "policy", commit_sha: head, base_commit: baseCommit });
    expect(d.tags).toMatchObject({ areas: ["docs"], data_classes: [], risk: "low", jurisdiction: "us", lines_changed: 1, files_changed: 1 });
    expect(d.paths).toEqual(["docs/usage.md"]);
    expect(d.client_preview).toBeNull();
    expect(r.json.data.intent).toMatchObject({ status: "proposed", head_commit: head, actual_paths: ["docs/usage.md"] });
  });

  it("ignores a client that claims auto_merge for an auth change", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "web/login.ts": "export function login() { return true; }\n" });
    const r = await propose(project, id, { action: "auto_merge", matched_rule: "rules[4]", tags: { areas: ["docs"] }, reason: "trust me" });
    expect(r.status).toBe(201);
    const d = r.json.data.decision;
    expect(d).toMatchObject({ action: "human_approve", status: "pending_human", matched_rule: "rules[2]", reason: "Auth is a security boundary." });
    expect(d.tags.areas).toEqual(["auth", "web"]);
    expect(d.client_preview).toEqual({ action: "auto_merge", matched_rule: "rules[4]", tags: { areas: ["docs"] }, reason: "trust me" });
    const ev = (await audit(project)).find((e) => e.kind === "decision.proposed")!;
    expect(ev.data).toMatchObject({ decided_by: "server", action: "human_approve", preview_agrees: false });
  });

  it("does not need the client to send an action at all", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "internal/x.go": "package x\n\nfunc X() {}\n" });
    const r = await propose(project, id);
    expect(r.json.data.decision).toMatchObject({ action: "agent_review", status: "pending_agent" });
  });

  it("quarantines EU personal data in a project created in the eu namespace", async () => {
    const { project, id, fork } = await setup(BASE, "agent-a", "eu");
    push(fork, { "web/profile.ts": "save({ date_of_birth: dob })\n" });
    const r = await propose(project, id);
    expect(r.json.data.decision).toMatchObject({ action: "quarantine", status: "quarantined", matched_rule: "rules[3]" });
    expect(r.json.data.decision.tags).toMatchObject({ data_classes: ["pii"], jurisdiction: "eu" });
  });

  it("tags by the project's namespace, so the same PII change in a us project is not quarantined", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "web/profile.ts": "save({ date_of_birth: dob })\n" });
    const r = await propose(project, id, { jurisdiction: "eu" }); // a client cannot claim another jurisdiction
    expect(r.json.data.decision.tags).toMatchObject({ data_classes: ["pii"], jurisdiction: "us" });
    expect(r.json.data.decision.action).not.toBe("quarantine");
  });

  it("rejects a jurisdiction the Worker has no namespace for, and keeps the choice on the project", async () => {
    const bad = await call("POST", "/v1/projects", { name: uniqueProject(), jurisdiction: "fedramp" });
    expect(bad.status).toBe(400);
    expect(bad.json.error?.message).toContain("not available");
    const name = await createProjectIn("eu");
    const audit = await call("GET", `/v1/audit?project=${name}`);
    expect(audit.json.data.events[0].data).toMatchObject({ jurisdiction: "eu" });
  });

  it("blocks a secret, and a block cannot be approved or landed", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "docs/setup.md": `export TOKEN=art_v1_${"ab".repeat(20)}\n` });
    const r = await propose(project, id);
    const d = r.json.data.decision;
    expect(d).toMatchObject({ action: "block", status: "blocked", matched_rule: "rules[0]" });
    expect(JSON.stringify(r.json)).not.toContain("ab".repeat(20));
    expect((await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "x", reason: "please" })).status).toBe(409);
    expect((await call("POST", `/v1/intents/${id}/land`, { project })).status).toBe(409);
  });

  describe("multi_approve counts distinct keys, not distinct names", () => {
    const human = async (agent: string) => {
      const r = await call("POST", "/v1/keys", { agent, kind: "human", note: "test" });
      expect(r.status).toBe(201);
      return { authorization: `Bearer ${r.json.data.key}` };
    };
    const infra = async () => {
      const { project, id, fork } = await setup();
      push(fork, { ".github/workflows/ci.yml": "on: push\n" });
      const d = (await propose(project, id)).json.data.decision;
      expect(d).toMatchObject({ action: "multi_approve", approvals_needed: 2, status: "pending_human" });
      return { project, d };
    };

    it("the admin key is one approver whatever names it gives", async () => {
      const { project, d } = await infra();
      const one = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "a", reason: "Checked the schema migration" });
      expect(one.json.data.decision.status).toBe("pending_human");
      expect(one.json.data.waiting_for).toMatch(/^needs another approver: 1 of 2/);
      const dup = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "b", reason: "Checked the rollback path" });
      expect(dup.json.data.decision.status).toBe("pending_human");
      expect(dup.json.data.waiting_for).toMatch(/^needs another approver: 1 of 2.*admin key counts once/);
      expect(dup.json.data.approvals).toEqual([
        expect.objectContaining({ by: "admin", claimed_by: "a", verdict: "approve" }),
        expect.objectContaining({ by: "admin", claimed_by: "b", verdict: "approve" }),
      ]);
      const audit = (await call("GET", `/v1/audit?project=${project}`)).json.data.events as any[];
      expect(audit.filter((e) => e.kind === "decision.approve").map((e) => [e.actor, e.data.claimed_by])).toEqual([["admin", "a"], ["admin", "b"]]);
    });

    it("the admin key plus a human key approve", async () => {
      const { project, d } = await infra();
      const one = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "john", reason: "Checked the schema migration" });
      expect(one.json.data.decision.status).toBe("pending_human");
      const two = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "john", reason: "Checked the rollback path" }, await human("alice@laptop"));
      expect(two.json.data.decision).toMatchObject({ status: "approved", decided_by: "alice@laptop" });
      expect(two.json.data.waiting_for).toBeNull();
      expect(two.json.data.approvals).toEqual([
        expect.objectContaining({ by: "admin", claimed_by: "john" }),
        expect.objectContaining({ by: "alice@laptop", claimed_by: null, identity_source: "key" }),
      ]);
    });

    it("two human keys approve, and a human key cannot count twice", async () => {
      const { project, d } = await infra();
      const alice = await human("alice@laptop");
      const one = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", reason: "Checked the schema migration" }, alice);
      expect(one.json.data.decision.status).toBe("pending_human");
      const again = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "someone-else", reason: "Checked it twice" }, alice);
      expect(again.json.data.decision.status).toBe("pending_human");
      expect(again.json.data.waiting_for).toMatch(/^needs another approver: 1 of 2/);
      const two = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", reason: "Checked the rollback path" }, await human("bob@laptop"));
      expect(two.json.data.decision).toMatchObject({ status: "approved", decided_by: "bob@laptop" });
    });

    it("no key can be bound to the admin identity", async () => {
      const r = await call("POST", "/v1/keys", { agent: "admin", kind: "human", note: "test" });
      expect(r.status).toBe(400);
      expect(r.json.error?.message).toContain("admin key");
    });
  });

  it("reads policy from the trunk base, so a fork cannot relax its own rules", async () => {
    const { project, id, fork } = await setup();
    // The fork replaces the policy with "everything auto-merges" and edits auth code.
    push(fork, {
      ".wayglass/policy.yaml": "version: 1\ndefault: { action: auto_merge, reason: mine }\n",
      "web/login.ts": "export function login() { return true; }\n",
    });
    const d = (await propose(project, id)).json.data.decision;
    // The trunk's auth rule decided; the fork's "default: auto_merge" was never read.
    expect(d).toMatchObject({ action: "human_approve", matched_rule: "rules[2]", reason: "Auth is a security boundary." });
    expect(d.tags.areas).toEqual(["auth", "web"]);
  });

  it("routes any .wayglass/ change to a person even when policy would auto-merge it", async () => {
    const { project, id, fork } = await setup();
    push(fork, { ".wayglass/notes.md": "hello\n" }); // matches the docs area (**/*.md)
    const d = (await propose(project, id)).json.data.decision;
    expect(d).toMatchObject({ action: "human_approve", matched_rule: "builtin:protect-policy" });
    expect(d.detail.overrode.id).toBe("rules[4]");
  });

  it("falls back to a person when the trunk has no policy", async () => {
    const { project, id, fork } = await setup({ "README.md": "hi\n" });
    push(fork, { "docs/a.md": "x\n" });
    const d = (await propose(project, id)).json.data.decision;
    expect(d).toMatchObject({ action: "human_approve", matched_rule: "builtin:no-policy" });
  });

  it("falls back to a person when the trunk policy is invalid", async () => {
    const { project, id, fork } = await setup({ ".wayglass/policy.yaml": "version: 7\n" });
    push(fork, { "docs/a.md": "x\n" });
    const d = (await propose(project, id)).json.data.decision;
    expect(d).toMatchObject({ action: "human_approve", matched_rule: "builtin:invalid-policy" });
    expect(d.detail.policy_error).toMatch(/version is 7/);
  });

  it("records shadow rules on the decision and in the audit log without enforcing them", async () => {
    // The example policy plus one trial rule in shadow mode, ahead of the docs rule.
    const trial = '  - { name: trial-docs-review, when: { area: docs }, action: human_approve, reason: "Trial: docs need a person.", mode: shadow }\n';
    const policy = POLICY.replace("rules:\n", "rules:\n" + trial);
    expect(policy).toContain("trial-docs-review");
    const { project, id, fork } = await setup({ ...BASE, ".wayglass/policy.yaml": policy });
    push(fork, { "docs/usage.md": "Usage\nMore usage.\n" });
    const r = await propose(project, id);
    expect(r.status).toBe(201);
    const d = r.json.data.decision;
    // The enforced docs rule still decides.
    expect(d).toMatchObject({ action: "auto_merge", status: "approved", matched_rule: "rules[5]" });
    expect(d.detail.would_differ).toBe(true);
    expect(d.detail.shadow_matches).toEqual([
      { rule: "trial-docs-review", index: 0, action: "human_approve", reason: "Trial: docs need a person.", preempts: true },
    ]);
    const ev = (await audit(project)).find((e) => e.kind === "decision.proposed");
    expect(ev?.data.shadow_matches).toEqual(d.detail.shadow_matches);
    expect(ev?.data.would_differ).toBe(true);
  });

  it("records an empty shadow_matches list when no shadow rule matched", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "docs/usage.md": "Usage\nMore usage.\n" });
    const d = (await propose(project, id)).json.data.decision;
    expect(d.detail).toMatchObject({ shadow_matches: [], would_differ: false });
  });

  it("treats an empty trunk (no base commit) as no policy", async () => {
    const { project, id, fork } = await setup(null);
    push(fork, { "docs/a.md": "x\n" });
    const d = (await propose(project, id)).json.data.decision;
    expect(d).toMatchObject({ action: "human_approve", matched_rule: "builtin:no-policy", base_commit: null });
  });

  it("refuses a proposal with nothing pushed, no changes, or a stale commit", async () => {
    const { project, id, fork } = await setup(null);
    expect((await propose(project, id)).json.error?.message).toMatch(/no commits/);
    const s = await setup();
    expect((await propose(s.project, s.id)).json.error?.message).toMatch(/no changes/);
    push(s.fork, { "docs/a.md": "x\n" });
    const stale = await propose(s.project, s.id, { commit: "deadbeef" });
    expect(stale.status).toBe(409);
    expect(stale.json.error?.message).toMatch(/fork head is/);
    void fork;
  });

  it("re-checks overlaps with the paths it derived", async () => {
    const { project, id, fork } = await setup();
    const other = await declare(project, ["web/login.ts"], "other", "agent-b");
    expect(other.status).toBe(201);
    push(fork, { "web/login.ts": "changed\n" });
    const r = await propose(project, id);
    expect(r.json.data.overlaps.map((o: { intent_id: string }) => o.intent_id)).toContain(other.json.data.intent.id);
  });
});

describe("land", () => {
  async function approvedDocsChange() {
    const s = await setup();
    const head = push(s.fork, { "docs/usage.md": "Usage\nMore.\n" });
    const d = (await propose(s.project, s.id)).json.data.decision;
    expect(d.status).toBe("approved");
    return { ...s, head, decision: d };
  }

  it("issues a 300 s trunk token once the decision is approved and records the land", async () => {
    const { project, id, decision } = await approvedDocsChange();
    const r = await call("POST", `/v1/intents/${id}/land`, { project });
    expect(r.status).toBe(201);
    expect(r.json.data.token.scope).toBe("write");
    expect(r.json.data.trunk.repo).toBe(project);
    expect(r.json.data.fork.commit).toBe(decision.commit_sha);
    expect(fake.tokens.at(-1)).toMatchObject({ repo: project, scope: "write", ttl: LAND_TOKEN_TTL_SECONDS });
    const events = await audit(project);
    expect(events.map((e) => e.kind)).toEqual(expect.arrayContaining(["land.reserved", "land.token_issued"]));
    expect(JSON.stringify(events)).not.toContain("fake_token");
  });

  it("refuses until a human approves, then allows", async () => {
    const s = await setup();
    push(s.fork, { "web/login.ts": "x\n" });
    const d = (await propose(s.project, s.id)).json.data.decision;
    expect((await call("POST", `/v1/intents/${s.id}/land`, { project: s.project })).status).toBe(409);
    await call("POST", `/v1/decisions/${d.id}`, { project: s.project, verdict: "approve", by: "john", reason: "Reviewed." });
    expect((await call("POST", `/v1/intents/${s.id}/land`, { project: s.project })).status).toBe(201);
  });

  it("decides by intent id: approve the latest decision with a reason", async () => {
    const s = await setup();
    const early = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", reason: "Checked the diff" });
    expect(early.status).toBe(409);
    expect(early.json.error?.message).toContain("wayglass propose");
    push(s.fork, { "web/login.ts": "x\n" });
    const d = (await propose(s.project, s.id)).json.data.decision;
    expect((await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve" })).status).toBe(400);
    const r = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", by: "john", reason: "Reviewed the login flow." });
    expect(r.status).toBe(200);
    expect(r.json.data.decision).toMatchObject({ id: d.id, status: "approved", decided_by: "admin" });
    expect(r.json.data.approvals).toEqual([expect.objectContaining({ by: "admin", claimed_by: "john", verdict: "approve", reason: "Reviewed the login flow." })]);
    expect((await call("POST", `/v1/intents/${s.id}/land`, { project: s.project })).status).toBe(201);
  });

  it("decides by intent id: reject is final", async () => {
    const s = await setup();
    push(s.fork, { "web/login.ts": "x\n" });
    await propose(s.project, s.id);
    const r = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "reject", by: "john", reason: "Breaks SSO." });
    expect(r.json.data.decision.status).toBe("rejected");
    expect((await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", reason: "changed my mind" })).status).toBe(409);
    expect((await call("POST", `/v1/intents/i0000000000/decide`, { project: s.project, verdict: "approve", reason: "x" })).status).toBe(404);
  });

  it("refuses placeholder reasons, which the append-only log would keep forever", async () => {
    const s = await setup();
    push(s.fork, { "web/login.ts": "x\n" });
    await propose(s.project, s.id);
    for (const reason of ["<what you checked>", "TODO", "..."]) {
      const r = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", by: "john", reason });
      expect(r.status).toBe(400);
      expect(r.json.error?.message).toContain("placeholder");
    }
    expect((await declare(s.project, ["a"], "goal")).status).toBe(201); // ordinary reasons still pass
  });

  it("refuses rubber-stamp reasons like LGTM, so every approval says what was checked", async () => {
    const s = await setup();
    push(s.fork, { "web/login.ts": "x\n" });
    await propose(s.project, s.id);
    for (const reason of ["LGTM", "lgtm!", "Looks good to me.", "ok", "+1", "👍", "Approved", "ship it 🚀", "yes, lgtm"]) {
      const r = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", by: "john", reason });
      expect(r.status, reason).toBe(400);
      expect(r.json.error?.message).toContain("rubber stamp");
    }
    const r = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", by: "john", reason: "LGTM: checked the lockout path against the intent" });
    expect(r.status).toBe(200);
  });

  it("refuses to let the authoring agent decide its own change", async () => {
    const s = await setup(BASE, "agent-a");
    push(s.fork, { "web/login.ts": "x\n" });
    await propose(s.project, s.id);
    const r = await call("POST", `/v1/intents/${s.id}/decide`, { project: s.project, verdict: "approve", by: "agent-a", reason: "checked my own work" });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toContain("cannot decide");
  });

  it("refuses when the fork moved after the decision", async () => {
    const { project, id, fork } = await approvedDocsChange();
    push(fork, { "web/login.ts": "sneaky\n" });
    const r = await call("POST", `/v1/intents/${id}/land`, { project });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/propose again/);
    expect((await audit(project)).map((e) => e.kind)).toContain("land.cancelled");
    // Re-proposing re-decides on the new head (auth, so a person now).
    expect((await propose(project, id)).json.data.decision.action).toBe("human_approve");
  });

  it("allows one land at a time per project", async () => {
    const a = await approvedDocsChange();
    const b = await declare(a.project, ["docs/**"], "second", "agent-b");
    const bid = b.json.data.intent.id;
    push(b.json.data.fork.name, { "docs/b.md": "b\n" });
    expect((await propose(a.project, bid)).json.data.decision.status).toBe("approved");
    expect((await call("POST", `/v1/intents/${a.id}/land`, { project: a.project })).status).toBe(201);
    const second = await call("POST", `/v1/intents/${bid}/land`, { project: a.project });
    expect(second.status).toBe(409);
    expect(second.json.error?.message).toMatch(/is landing/);
  });

  it("landed: a fast-forward is verified, the token revoked and /v1/why answers for it", async () => {
    const { project, id, head } = await approvedDocsChange();
    await call("POST", `/v1/intents/${id}/land`, { project });
    const tokenId = fake.tokens.at(-1)!.id;
    fake.setHead(project, head); // the client pushed the fork head to trunk
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head });
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({ token_revoked: true, intent: { status: "landed" }, land: { status: "landed", landed_commit: head } });
    expect(fake.revoked).toContainEqual({ repo: project, id: tokenId });
    const why = await call("GET", `/v1/why?project=${project}&commit=${head.slice(0, 8)}`);
    expect(why.json.data).toMatchObject({ commit: head, intent: { id }, decision: { action: "auto_merge", landed_commit: head } });
  });

  it("landed: a merge commit touching only the decided paths is accepted", async () => {
    const { project, id, head, baseCommit } = await approvedDocsChange();
    // Trunk moved on before the land (another intent landed), then the client merged the fork in.
    const moved = fake.commit(project, { ...fake.filesAt(baseCommit!), "README.md": "# demo v2\n" });
    await call("POST", `/v1/intents/${id}/land`, { project });
    const merge = fake.commit(project, { ...fake.filesAt(moved), "docs/usage.md": "Usage\nMore.\n" }, [moved, head]);
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge });
    expect(r.status).toBe(200);
    const why = await call("GET", `/v1/why?project=${project}&commit=${merge}`);
    expect(why.json.data.commit).toBe(merge);
    expect(why.json.data.intent.id).toBe(id);
  });

  it("landed: a merge that sneaks in other paths is rejected and logged", async () => {
    const { project, id, head, baseCommit } = await approvedDocsChange();
    await call("POST", `/v1/intents/${id}/land`, { project });
    const merge = fake.commit(project, { ...fake.filesAt(baseCommit!), "docs/usage.md": "Usage\nMore.\n", "web/login.ts": "backdoor\n" }, [baseCommit!, head]);
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/web\/login\.ts/);
    const ev = (await audit(project)).find((e) => e.kind === "land.mismatch")!;
    expect(ev.data).toMatchObject({ commit: merge, token_revoked: true });
    expect(fake.revoked.length).toBeGreaterThan(0);
    // The land is closed; the intent can try again.
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge })).status).toBe(409);
  });

  it("landed: an unrelated commit is rejected", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    await call("POST", `/v1/intents/${id}/land`, { project });
    const other = fake.commit(project, { ...fake.filesAt(baseCommit!), "docs/usage.md": "Usage\nMore.\n" });
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: other });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/neither/);
  });

  it("landed: a textual merge in a decided path is accepted when trunk also edited that file", async () => {
    const { project, id, head, baseCommit } = await approvedDocsChange();
    // Another intent landed an edit to the same file; git merged both hunks.
    const moved = fake.commit(project, { ...fake.filesAt(baseCommit!), "docs/usage.md": "Intro\nUsage\n" });
    const land = await call("POST", `/v1/intents/${id}/land`, { project });
    expect(land.json.data.trunk_before).toBe(moved);
    const merge = fake.commit(project, { ...fake.filesAt(moved), "docs/usage.md": "Intro\nUsage\nMore.\n" }, [moved, head]);
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge });
    expect(r.status).toBe(200);
  });

  it("landed: a merge onto a trunk that moved during the land is rejected", async () => {
    const { project, id, head, baseCommit } = await approvedDocsChange();
    await call("POST", `/v1/intents/${id}/land`, { project });
    // Someone pushed to trunk between the token and the merge.
    const sneaked = fake.commit(project, { ...fake.filesAt(baseCommit!), "README.md": "# sneaked\n" });
    const merge = fake.commit(project, { ...fake.filesAt(sneaked), "docs/usage.md": "Usage\nMore.\n" }, [sneaked, head]);
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/trunk moved during the land/);
  });

  it("landed: a fast-forward that drops trunk commits is rejected", async () => {
    const { project, id, head, baseCommit } = await approvedDocsChange();
    fake.commit(project, { ...fake.filesAt(baseCommit!), "README.md": "# demo v2\n" });
    await call("POST", `/v1/intents/${id}/land`, { project });
    fake.setHead(project, head); // a force-push of the fork head over the moved trunk
    const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/drops trunk commit/);
  });

  it("landed: an octopus merge is not a merge of the approved change", async () => {
    const { project, id, head, baseCommit } = await approvedDocsChange();
    await call("POST", `/v1/intents/${id}/land`, { project });
    const other = fake.commit(project, { ...fake.filesAt(baseCommit!), "README.md": "x\n" }, [baseCommit!]);
    fake.setHead(project, baseCommit!);
    const merge = fake.commit(project, { ...fake.filesAt(baseCommit!), "docs/usage.md": "Usage\nMore.\n" }, [baseCommit!, head, other]);
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: merge })).status).toBe(409);
  });

  it("abort: revokes the trunk token and frees the land slot for the next intent", async () => {
    const a = await approvedDocsChange();
    const b = await declare(a.project, ["docs/**"], "second", "agent-b");
    const bid = b.json.data.intent.id;
    push(b.json.data.fork.name, { "docs/b.md": "b\n" });
    await propose(a.project, bid);
    expect((await call("POST", `/v1/intents/${a.id}/land`, { project: a.project })).status).toBe(201);
    const tokenId = fake.tokens.at(-1)!.id;
    expect((await call("POST", `/v1/intents/${a.id}/land/abort`, { project: a.project })).status).toBe(400); // reason required
    const r = await call("POST", `/v1/intents/${a.id}/land/abort`, { project: a.project, reason: "merge conflict in docs/usage.md" });
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({ token_revoked: true, land: { status: "cancelled" } });
    expect(fake.revoked).toContainEqual({ repo: a.project, id: tokenId });
    const ev = (await audit(a.project)).find((e) => e.kind === "land.aborted")!;
    expect(ev.data).toMatchObject({ reason: "merge conflict in docs/usage.md", token_revoked: true });
    expect((await call("POST", `/v1/intents/${bid}/land`, { project: a.project })).status).toBe(201);
    // The aborted intent is still proposed and can try again later; nothing to abort now.
    expect((await call("POST", `/v1/intents/${a.id}/land/abort`, { project: a.project, reason: "again" })).status).toBe(409);
  });

  describe("only the author or the admin key may land", () => {
    const as = (key: string, agent?: string) => ({ authorization: `Bearer ${key}`, ...(agent ? { "x-wayglass-agent": agent } : {}) });
    async function mint(agent: string): Promise<string> {
      const r = await call("POST", "/v1/keys", { agent });
      expect(r.status).toBe(201);
      return r.json.data.key;
    }

    it("refuses another agent's key before reserving anything, and lets the author's own key land", async () => {
      const { project, id, head } = await approvedDocsChange();
      const other = await mint(`agent-b-${id}`);
      const tokensBefore = fake.tokens.length;
      const denied = await call("POST", `/v1/intents/${id}/land`, { project }, as(other));
      expect(denied.status).toBe(403);
      expect(denied.json.error?.code).toBe("not_author");
      expect(denied.json.error?.message).toMatch(/only agent-a .*or the admin key may land it/);
      expect(fake.tokens.length).toBe(tokensBefore); // no trunk token minted
      expect((await audit(project)).map((e) => e.kind)).not.toContain("land.reserved");

      // A bound key cannot claim to be the author through the header.
      expect((await call("POST", `/v1/intents/${id}/land`, { project }, as(other, "agent-a"))).json.error?.code).toBe("not_author");

      const author = await mint("agent-a");
      expect((await call("POST", `/v1/intents/${id}/land`, { project }, as(author))).status).toBe(201);
      fake.setHead(project, head);
      const done = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as(author));
      expect(done.status).toBe(200);
      expect(done.json.data.intent.status).toBe("landed");
    });

    it("refuses landed and land/abort from another agent while the author's land is in progress", async () => {
      const { project, id, head } = await approvedDocsChange();
      const author = await mint("agent-a");
      const other = await mint(`agent-b-${id}`);
      expect((await call("POST", `/v1/intents/${id}/land`, { project }, as(author))).status).toBe(201);
      const revokedBefore = fake.revoked.length;
      fake.setHead(project, head);

      const landed = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as(other));
      expect(landed.status).toBe(403);
      expect(landed.json.error?.code).toBe("not_author");
      const aborted = await call("POST", `/v1/intents/${id}/land/abort`, { project, reason: "not mine" }, as(other));
      expect(aborted.status).toBe(403);
      expect(aborted.json.error?.code).toBe("not_author");
      expect(fake.revoked.length).toBe(revokedBefore); // the author's trunk token is untouched

      expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as(author))).status).toBe(200);
    });

    it("lets the admin key land any approved change, whatever name it gives", async () => {
      const { project, id, head } = await approvedDocsChange();
      expect((await call("POST", `/v1/intents/${id}/land`, { project }, as(KEY, "someone@laptop"))).status).toBe(201);
      fake.setHead(project, head);
      const r = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head }, as(KEY, "someone@laptop"));
      expect(r.status).toBe(200);
      const ev = (await audit(project)).find((e) => e.kind === "intent.landed") as { actor?: string } | undefined;
      expect(ev?.actor).toBe("someone@laptop");
    });

    it("compares names exactly: a key bound to Agent-A cannot land agent-a's change", async () => {
      const { project, id } = await approvedDocsChange();
      const other = await mint("Agent-A");
      expect((await call("POST", `/v1/intents/${id}/land`, { project }, as(other))).json.error?.code).toBe("not_author");
    });
  });

  it("landed needs a land in progress and a full sha", async () => {
    const { project, id, head } = await approvedDocsChange();
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: head })).status).toBe(409);
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: head.slice(0, 7) })).status).toBe(400);
  });
});

describe("fork token refresh", () => {
  it("mints a short-lived token for the owner and revokes the old one", async () => {
    const { project, id, fork } = await setup(BASE, "agent-a");
    const first = fake.tokens.find((t) => t.repo === fork)!;
    const r = await call("POST", `/v1/intents/${id}/token`, { project, agent: "agent-a" });
    expect(r.status).toBe(201);
    expect(r.json.data).toMatchObject({ fork: { repo: fork }, token: { scope: "write" }, revoked: 1 });
    expect(fake.tokens.at(-1)).toMatchObject({ repo: fork, scope: "write", ttl: FORK_TOKEN_TTL_SECONDS });
    expect(fake.revoked).toContainEqual({ repo: fork, id: first.id });
    const ev = (await audit(project)).find((e) => e.kind === "token.refreshed")!;
    expect(JSON.stringify(ev)).not.toContain("fake_token");
  });

  it("refuses anyone but the intent's agent", async () => {
    const { project, id } = await setup(BASE, "agent-a");
    const r = await call("POST", `/v1/intents/${id}/token`, { project, agent: "agent-b" });
    expect(r.status).toBe(403);
    expect(r.json.error?.code).toBe("forbidden");
  });

  it("refuses for a landed intent", async () => {
    const s = await setup();
    const head = push(s.fork, { "docs/usage.md": "x\n" });
    await propose(s.project, s.id);
    await call("POST", `/v1/intents/${s.id}/land`, { project: s.project });
    fake.setHead(s.project, head);
    await call("POST", `/v1/intents/${s.id}/landed`, { project: s.project, commit: head });
    expect((await call("POST", `/v1/intents/${s.id}/token`, { project: s.project, agent: "agent-a" })).status).toBe(409);
  });
});

describe("read token for reviewers", () => {
  it("mints a short-lived read token for any caller and audits who asked", async () => {
    const { project, id, fork } = await setup(BASE, "agent-a");
    const r = await call("POST", `/v1/intents/${id}/read-token`, { project, agent: "reviewer" });
    expect(r.status).toBe(201);
    expect(r.json.data).toMatchObject({ fork: { repo: fork }, token: { scope: "read" }, intent: { id } });
    expect(fake.tokens.at(-1)).toMatchObject({ repo: fork, scope: "read", ttl: READ_TOKEN_TTL_SECONDS });
    const ev = (await audit(project)).find((e) => e.kind === "token.read_minted")!;
    expect(ev).toMatchObject({ actor: "reviewer", intent_id: id, data: { scope: "read" } });
    expect(JSON.stringify(ev)).not.toContain("fake_token");
  });

  it("404s for an unknown intent", async () => {
    const { project } = await setup();
    expect((await call("POST", `/v1/intents/i0000000000/read-token`, { project })).status).toBe(404);
  });
});

describe("stacked intents", () => {
  async function parentWithPush() {
    const s = await setup();
    const parentHead = push(s.fork, { "docs/usage.md": "Usage\nParent.\n" });
    await call("POST", `/v1/intents/${s.id}/push`, { project: s.project, paths: ["docs/usage.md"], commit: parentHead });
    return { ...s, parentHead };
  }

  it("forks the parent's fork, bases the diff on the parent's head and skips the parent overlap", async () => {
    const p = await parentWithPush();
    const r = await call("POST", "/v1/intents", { project: p.project, goal: "child", reason: "builds on the parent", paths: ["docs/**"], agent: "agent-a", on: p.id });
    expect(r.status).toBe(201);
    const child = r.json.data.intent;
    expect(child).toMatchObject({ parent_id: p.id, base_commit: p.parentHead, trunk_base: p.baseCommit });
    expect(r.json.data.overlaps).toEqual([]);
    expect(fake.repos.get(r.json.data.fork.name)).toEqual({ source: p.fork });
    // The child only adds docs/child.md; the parent's docs/usage.md edit is not its change.
    push(r.json.data.fork.name, { "docs/child.md": "child\n" });
    const d = (await propose(p.project, child.id)).json.data.decision;
    expect(d.paths).toEqual(["docs/child.md"]);
    expect(d.base_commit).toBe(p.parentHead);
    expect(d.action).toBe("auto_merge");
  });

  it("refuses to land a child before its parent landed", async () => {
    const p = await parentWithPush();
    const r = await call("POST", "/v1/intents", { project: p.project, goal: "child", reason: "builds on it", paths: ["docs/**"], on: p.id });
    const childId = r.json.data.intent.id;
    const childHead = push(r.json.data.fork.name, { "docs/child.md": "child\n" });
    expect((await propose(p.project, childId)).json.data.decision.status).toBe("approved");
    const early = await call("POST", `/v1/intents/${childId}/land`, { project: p.project });
    expect(early.status).toBe(409);
    expect(early.json.error?.message).toMatch(new RegExp(`stacked on ${p.id}.*land ${p.id} first`));

    // Parent lands (fast-forward), then the child fast-forwards on top of it.
    expect((await propose(p.project, p.id)).json.data.decision.status).toBe("approved");
    await call("POST", `/v1/intents/${p.id}/land`, { project: p.project });
    fake.setHead(p.project, p.parentHead);
    expect((await call("POST", `/v1/intents/${p.id}/landed`, { project: p.project, commit: p.parentHead })).status).toBe(200);
    const land = await call("POST", `/v1/intents/${childId}/land`, { project: p.project });
    expect(land.status).toBe(201);
    expect(land.json.data.trunk_before).toBe(p.parentHead);
    fake.setHead(p.project, childHead);
    expect((await call("POST", `/v1/intents/${childId}/landed`, { project: p.project, commit: childHead })).status).toBe(200);
  });

  it("refuses to stack on a missing, landed or empty intent", async () => {
    const s = await setup();
    const on = (id: string) => call("POST", "/v1/intents", { project: s.project, goal: "c", reason: "r", paths: ["a"], on: id });
    expect((await on("i0000000000")).status).toBe(404);
    expect((await on("nope")).status).toBe(400);
    const empty = await on(s.id);
    expect(empty.status).toBe(409);
    expect(empty.json.error?.message).toMatch(/nothing pushed/);
  });
});

describe("decisions list", () => {
  it("lists decisions newest last, with approvals and per-file line counts", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "web/login.ts": "export function login() { return true; }\nexport const max = 5;\n" });
    const d = (await propose(project, id)).json.data.decision;
    await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "john", reason: "Checked the login flow" });

    const r = await call("GET", `/v1/decisions?project=${project}`);
    expect(r.status).toBe(200);
    const [got] = r.json.data.decisions;
    expect(got).toMatchObject({ id: d.id, intent_id: id, status: "approved" });
    expect(got.approvals).toEqual([expect.objectContaining({ by: "admin", claimed_by: "john", verdict: "approve", reason: "Checked the login flow" })]);
    expect(got.detail.files).toEqual([{ path: "web/login.ts", added: 2, deleted: 1 }]);
  });

  it("filters by status and validates its query", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "internal/x.go": "package x\n\nfunc X() {}\n" });
    await propose(project, id);
    const pending = await call("GET", `/v1/decisions?project=${project}&status=pending_agent`);
    expect(pending.json.data.decisions).toHaveLength(1);
    const none = await call("GET", `/v1/decisions?project=${project}&status=pending_human`);
    expect(none.json.data.decisions).toEqual([]);
    expect((await call("GET", `/v1/decisions?project=${project}&status=bogus`)).status).toBe(400);
    expect((await call("GET", `/v1/decisions?project=${project}&limit=0`)).status).toBe(400);
    expect((await call("GET", `/v1/decisions?project=${project}`, undefined, {})).status).toBe(401);
  });
});

describe("cost", () => {
  it("meters every Artifacts call per intent, and averages the landed changes", async () => {
    const { project, id, fork } = await setup();
    const head = push(fork, { "docs/usage.md": "Usage\nMore.\n" });
    expect((await propose(project, id)).json.data.decision.status).toBe("approved");
    expect((await call("POST", `/v1/intents/${id}/land`, { project })).status).toBe(201);
    fake.setHead(project, head);
    expect((await call("POST", `/v1/intents/${id}/landed`, { project, commit: head })).status).toBe(200);

    const r = await call("GET", `/v1/cost?project=${project}`);
    expect(r.status).toBe(200);
    const c = r.json.data;
    // Creating the trunk is the project's, not an intent's.
    expect(c.project.by_op).toEqual({ create: 1, get: 1, revokeToken: 1, createToken: 1 });
    const mine = c.intents[id];
    // One fork; tokens: the fork's write token and the land's trunk token. The policy is read at propose and again at land.
    expect(mine.by_op).toMatchObject({ fork: 1, createToken: 2, readFile: 2 });
    expect(mine.tokens_minted).toBe(2);
    expect(mine.ops).toBe(Object.values(mine.by_op as Record<string, number>).reduce((a, b) => a + b, 0));
    expect(mine.est_usd).toBeCloseTo((mine.ops * 0.15) / 1000, 6);
    expect(c.total.ops).toBe(c.project.ops + mine.ops);
    expect(c.per_landed_change).toEqual({ landed: 1, ops: mine.ops, est_usd: mine.est_usd });
    expect(c.scope).toMatch(/Worker-side/);
    expect(c.pricing).toMatchObject({ usd_per_1000_ops: 0.15, included_ops_per_month: 10000 });
  });

  it("charges a failed request's calls too", async () => {
    const { project, id } = await setup();
    // Nothing pushed: propose reads the fork head, then refuses.
    expect((await propose(project, id)).status).toBe(409);
    const c = (await call("GET", `/v1/cost?project=${project}`)).json.data;
    expect(c.intents[id].by_op.log).toBe(2); // declare's base read, propose's head read
  });
});

describe("reviewer from another model family", () => {
  const FAMILY_POLICY = `version: 1
classify:
  area:
    tests: ["**/*_test.go"]
rules:
  - { name: tests-cross-review, when: { only_area: tests }, action: agent_review, reviewer: { different_family: true }, reason: "Tests only; another model family reviews." }
default: { action: human_approve, reason: "A person decides." }
`;

  async function crossReview(authorFamily?: string) {
    const project = await createProject();
    fake.commit(project, { ".wayglass/policy.yaml": FAMILY_POLICY, "x_test.go": "package x\n" });
    const r = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["**"], agent: "author", ...(authorFamily ? { family: authorFamily } : {}) });
    expect(r.status).toBe(201);
    const id: string = r.json.data.intent.id;
    push(r.json.data.fork.name, { "x_test.go": "package x\n// more\n" });
    const d = (await propose(project, id)).json.data.decision;
    expect(d).toMatchObject({ action: "agent_review", status: "pending_agent", matched_rule: "tests-cross-review" });
    expect(d.detail.reviewer).toEqual({ different_family: true });
    return { project, id, intent: r.json.data.intent };
  }

  const decide = (project: string, id: string, by: string, family: string | undefined, verdict = "approve") =>
    call("POST", `/v1/intents/${id}/decide`, { project, verdict, reason: "checked the assertions", by, ...(family ? { family } : {}) });

  it("records the author's family and refuses a reviewer of the same family", async () => {
    const { project, id, intent } = await crossReview("anthropic");
    expect(intent.agent_family).toBe("claude"); // aliases normalise
    const same = await decide(project, id, "reviewer-1", "claude");
    expect(same.status).toBe(403);
    expect(same.json.error?.code).toBe("same_family");
    const none = await decide(project, id, "reviewer-1", undefined);
    expect(none.json.error?.code).toBe("family_required");
    const other = await decide(project, id, "reviewer-2", "codex");
    expect(other.status).toBe(200);
    expect(other.json.data.decision.status).toBe("approved");
    expect(other.json.data.approvals).toEqual([expect.objectContaining({ by: "admin", claimed_by: "reviewer-2", family: "openai", verdict: "approve" })]);
  });

  it("needs the author's family too, but anyone may still reject", async () => {
    const { project, id } = await crossReview();
    const r = await decide(project, id, "reviewer", "gemini");
    expect(r.status).toBe(403);
    expect(r.json.error?.code).toBe("family_required");
    expect(r.json.error?.message).toMatch(/author declared none/);
    const rej = await decide(project, id, "reviewer", undefined, "reject");
    expect(rej.json.data.decision.status).toBe("rejected");
  });

  it("does not let two unknown families pass as different", async () => {
    const { project, id } = await crossReview("other");
    expect((await decide(project, id, "reviewer", "other")).json.error?.code).toBe("same_family");
    expect((await decide(project, id, "a-person", "human")).json.data.decision.status).toBe("approved");
  });

  it("refuses a family it does not know", async () => {
    const project = await createProject();
    const r = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["a"], family: "llama-ish" });
    expect(r.status).toBe(400);
    expect(r.json.error?.message).toMatch(/family must be one of/);
  });

  it("leaves rules without a reviewer constraint alone", async () => {
    const { project, id, fork } = await setup();
    push(fork, { "internal/x.go": "package x\n// tidy\n" });
    const d = (await propose(project, id)).json.data.decision;
    expect(d.detail.reviewer).toBeNull();
  });
});
