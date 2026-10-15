import { beforeEach, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import { call, createProject, declare, KEY, useFakeArtifacts } from "./helpers";
import type { FakeRepoHost, Files } from "./fake-artifacts";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "README.md": "base\n", "web/login.ts": "export const limit = 5;\n", "internal/x.go": "package x\nfunc X() int { return 1 }\n" };
let fake: FakeRepoHost;
beforeEach(() => { fake = useFakeArtifacts(); });
const admin = { authorization: `Bearer ${KEY}`, "x-wayglass-agent": "person-reviewer" };
async function setup(edits: Files) {
  const project = await createProject();
  const base = fake.commit(project, BASE);
  const a = await declare(project, Object.keys(edits));
  const id = a.json.data.intent.id;
  const fork = a.json.data.fork.name;
  const head = fake.commit(fork, { ...BASE, ...edits });
  const p = await call("POST", `/v1/intents/${id}/propose`, { project });
  expect(p.status).toBe(201);
  return { project, base, id, fork, head, decision: p.json.data.decision };
}

it("F1: a decision-bound approval must not approve a replacement proposal", async () => {
  const s = await setup({ "web/login.ts": "export const limit = 4;\n" });
  expect(s.decision.status).toBe("pending_human");
  const replacement = fake.commit(s.fork, { ...BASE, "web/login.ts": "export const limit = Infinity;\n" });
  const newer = await call("POST", `/v1/intents/${s.id}/propose`, { project: s.project });
  expect(newer.json.data.decision.commit_sha).toBe(replacement);
  // A CLI or desktop form still showing the earlier proposal names it.
  const approval = await call("POST", `/v1/intents/${s.id}/decide`, {
    project: s.project, verdict: "approve", reason: "Verified the limit decreases from five to four.", decision_id: s.decision.id,
  }, admin);
  expect(approval.status, `approved unseen commit ${approval.json.data?.decision?.commit_sha}`).toBe(409);
  expect(approval.json.error).toMatchObject({ code: "decision_replaced" });
  // Binding by the commit that was read works the same (the fake's commit
  // ids share a long prefix, so the whole sha is used here).
  const byCommit = await call("POST", `/v1/intents/${s.id}/decide`, {
    project: s.project, verdict: "approve", reason: "Verified the limit decreases from five to four.", commit: s.head,
  }, admin);
  expect(byCommit.json.error).toMatchObject({ code: "decision_replaced" });
  // The earlier decision cannot be decided by its own id either.
  const stale = await call("POST", `/v1/decisions/${s.decision.id}`, { project: s.project, verdict: "approve", reason: "Verified the limit decreases from five to four." }, admin);
  expect(stale.status).toBe(409);
  expect(stale.json.error).toMatchObject({ code: "decision_replaced" });
  // Nothing was recorded: the replacement still waits for a person who read it.
  const all = (await call("GET", `/v1/decisions?project=${s.project}`)).json.data.decisions;
  expect(all.find((d: { id: string }) => d.id === newer.json.data.decision.id).status).toBe("pending_human");
  expect(all.find((d: { id: string }) => d.id === s.decision.id).status).toBe("pending_human");
  // Naming the replacement decides it.
  const bound = await call("POST", `/v1/intents/${s.id}/decide`, {
    project: s.project, verdict: "approve", reason: "Verified the limit is now unbounded, which is wrong, but the review is of this commit.", decision_id: newer.json.data.decision.id,
  }, admin);
  expect(bound.status).toBe(200);
  expect(bound.json.data.decision.commit_sha).toBe(replacement);
});

it("F2: reproposing identical bytes must preserve an escalation to a person", async () => {
  const s = await setup({ "internal/x.go": "package x\nfunc X() int { return 2 }\n" });
  expect(s.decision.status).toBe("pending_agent");
  const escalated = await call("POST", `/v1/intents/${s.id}/ai-review`, { project: s.project }, undefined, {
    AI_REVIEW: "manual", AI: { run: async () => ({ response: { verdict: "escalate", reason: "The changed return value needs a person to check the callers.", concerns: ["callers unverified"], confidence: 0.8, instructions_in_diff: false } }) } as unknown as Ai,
  });
  expect(escalated.json.data.decision.status).toBe("pending_human");
  const again = await call("POST", `/v1/intents/${s.id}/propose`, { project: s.project });
  expect(again.json.data.decision.commit_sha).toBe(s.head);
  // The retry carries the escalation: still a person's decision, and the record says where it came from.
  expect(again.json.data.decision).toMatchObject({ action: "human_approve", status: "pending_human" });
  expect(again.json.data.decision.detail.escalation).toMatchObject({ carried_from: escalated.json.data.decision.id, by: escalated.json.data.decision.detail.escalation.by });
  const minted = await call("POST", "/v1/keys", { agent: "independent-agent" });
  const approved = await call(
    "POST",
    `/v1/intents/${s.id}/decide`,
    { project: s.project, verdict: "approve", reason: "Checked the callers and the changed return value.", decision_id: again.json.data.decision.id },
    { authorization: `Bearer ${minted.json.data.key}` },
  );
  expect(approved.status, "an agent approved the same commit after escalation").toBe(403);
  expect(approved.json.error?.code).toBe("needs_human");
  // A revised change (a new commit) is decided afresh by policy.
  fake.commit(s.fork, { ...BASE, "internal/x.go": "package x\nfunc X() int { return 3 }\n" });
  const revised = await call("POST", `/v1/intents/${s.id}/propose`, { project: s.project });
  expect(revised.json.data.decision).toMatchObject({ action: "agent_review", status: "pending_agent" });
  expect(revised.json.data.decision.detail.escalation).toBeUndefined();
});

it("F3: a failed trunk-token revocation must keep the project land slot reserved", async () => {
  const s = await setup({ "README.md": "reviewed docs\n" });
  const a = await call("POST", `/v1/intents/${s.id}/land`, { project: s.project });
  expect(a.status).toBe(201);
  const second = await declare(s.project, ["docs/other.md"], "another docs change", "agent-b");
  const id = second.json.data.intent.id;
  fake.commit(second.json.data.fork.name, { ...BASE, "docs/other.md": "another doc\n" });
  expect((await call("POST", `/v1/intents/${id}/propose`, { project: s.project })).status).toBe(201);
  fake.revokeToken = async () => { throw new Error("simulated Artifacts outage"); };
  const aborted = await call("POST", `/v1/intents/${s.id}/land/abort`, { project: s.project, reason: "push failed, retry later" });
  expect(aborted.json.data.token_revoked).toBe(false);
  const next = await call("POST", `/v1/intents/${id}/land`, { project: s.project });
  expect(next.status, "a second live trunk write token was issued").toBe(409);
});

it("F4: merge verification must compare git modes as well as blob hashes", async () => {
  const s = await setup({ "README.md": "../../outside-file" });
  expect(s.decision.status).toBe("approved");
  const moved = fake.commit(s.project, { ...BASE, "docs/other.md": "trunk advanced\n" });
  expect((await call("POST", `/v1/intents/${s.id}/land`, { project: s.project })).status).toBe(201);
  const merge = fake.commit(s.project, { ...fake.filesAt(moved), "README.md": "../../outside-file" }, [moved, s.head]);
  // The stock fake always writes mode 100644. Model a real git symlink by
  // giving this merge its own tree with mode 120000 and the SAME blob bytes.
  const objects = (fake as unknown as { objects: Map<string, any> }).objects;
  const commit = objects.get(merge);
  const tree = structuredClone(objects.get(commit.info.tree));
  tree.entries.find((e: any) => e.name === "README.md").mode = "120000";
  const newTree = "f".repeat(40);
  objects.set(newTree, tree);
  commit.info.tree = newTree;
  const result = await call("POST", `/v1/intents/${s.id}/landed`, { project: s.project, commit: merge });
  expect(result.status, "a symlink was accepted as the approved regular file").toBe(409);
});
