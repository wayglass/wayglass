// A decision holds only under the policy it was made under. Propose reads the
// trunk's current .wayglass/policy.yaml and records its digest; land compares
// it with the trunk's policy at the commit the land starts from and refuses
// with policy_changed, before minting a trunk token, when they differ or the
// decided policy cannot be told. Proposing again decides under the new rules.

import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import type { Project } from "../src/project";
import { projectStub } from "../src/stub";
import type { FakeRepoHost, Files } from "./fake-artifacts";
import { call, createProject, declare, useFakeArtifacts } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const STRICTER = POLICY.replace('action: auto_merge, reason: "Routine docs change; every file is docs."', 'action: agent_review, reason: "Docs now get a second look."');
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "README.md": "# demo\n", "docs/usage.md": "Usage\n" };

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

/** An approved (auto-merged) docs change on a fresh project. */
async function approvedDocsChange() {
  const project = await createProject();
  const baseCommit = fake.commit(project, BASE);
  const r = await declare(project, ["docs/**"], "document usage", "agent-a");
  expect(r.status).toBe(201);
  const id: string = r.json.data.intent.id;
  const fork: string = r.json.data.fork.name;
  const head = fake.commit(fork, { ...BASE, "docs/usage.md": "Usage\nMore.\n" });
  const d = (await call("POST", `/v1/intents/${id}/propose`, { project })).json.data.decision;
  expect(d).toMatchObject({ status: "approved", action: "auto_merge", policy_commit: baseCommit });
  expect(d.policy_sha256).toMatch(/^sha256:[0-9a-f]{64}$/);
  return { project, id, fork, head, baseCommit, decision: d };
}

const land = (project: string, id: string) => call("POST", `/v1/intents/${id}/land`, { project });
const trunkTokens = (project: string) => fake.tokens.filter((t) => t.repo === project && t.scope === "write").length;
const auditKinds = async (project: string) =>
  ((await call("GET", `/v1/audit?project=${project}&limit=500`)).json.data.events as { kind: string; data: any }[]).map((e) => [e.kind, e.data?.reason]);

/** Pretends the decision predates Oct 14, when no policy digest was recorded. */
async function forgetPolicy(project: string, decisionId: string) {
  await runInDurableObject(await projectStub(env, project), async (_p: Project, state) => {
    state.storage.sql.exec("UPDATE decisions SET policy_commit = NULL, policy_sha256 = NULL WHERE id = ?", decisionId);
  });
}

describe("land under a changed trunk policy", () => {
  it("lands when the trunk moved but its policy did not", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    fake.commit(project, { ...fake.filesAt(baseCommit), "README.md": "# demo v2\n" });
    expect((await land(project, id)).status).toBe(201);
  });

  it("refuses with policy_changed and mints no trunk token when the policy changed", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    fake.commit(project, { ...fake.filesAt(baseCommit), ".wayglass/policy.yaml": STRICTER });
    const before = trunkTokens(project);
    const r = await land(project, id);
    expect(r.status).toBe(409);
    expect(r.json.error).toMatchObject({ code: "policy_changed" });
    expect(r.json.error!.message).toMatch(/wayglass sync .* wayglass propose again/);
    expect(trunkTokens(project)).toBe(before);
    // Like a moved fork, the reserved land is cancelled (audited) and the slot freed.
    expect(await auditKinds(project)).toContainEqual(["land.cancelled", "trunk policy changed after the decision"]);
    expect((await land(project, id)).json.error?.code).toBe("policy_changed");
  });

  it("treats any byte change to the policy as a change, even a comment", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    fake.commit(project, { ...fake.filesAt(baseCommit), ".wayglass/policy.yaml": POLICY + "# a comment\n" });
    expect((await land(project, id)).json.error?.code).toBe("policy_changed");
  });

  it("refuses when the trunk policy was removed", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    const files = fake.filesAt(baseCommit);
    delete files[".wayglass/policy.yaml"];
    fake.commit(project, files);
    expect((await land(project, id)).json.error?.code).toBe("policy_changed");
  });

  it("re-proposing decides under the trunk's new policy, and then lands", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    fake.commit(project, { ...fake.filesAt(baseCommit), ".wayglass/policy.yaml": STRICTER });
    expect((await land(project, id)).json.error?.code).toBe("policy_changed");
    const d = (await call("POST", `/v1/intents/${id}/propose`, { project })).json.data.decision;
    // The new docs rule decided, not the old auto_merge.
    expect(d).toMatchObject({ action: "agent_review", status: "pending_agent", reason: "Docs now get a second look." });
    const ok = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "agent-z", reason: "Checked the docs read correctly" });
    expect(ok.json.data.decision.status).toBe("approved");
    expect((await land(project, id)).status).toBe(201);
  });

  it("re-proposing after merging the trunk into the intent also lands", async () => {
    const { project, id, fork, head, baseCommit } = await approvedDocsChange();
    const moved = fake.commit(project, { ...fake.filesAt(baseCommit), ".wayglass/policy.yaml": STRICTER });
    expect((await land(project, id)).json.error?.code).toBe("policy_changed");
    fake.commit(fork, { ...fake.filesAt(moved), "docs/usage.md": "Usage\nMore.\n" }, [head, moved]);
    const d = (await call("POST", `/v1/intents/${id}/propose`, { project })).json.data.decision;
    // The merged trunk policy edit is the trunk's, not the intent's: landing
    // the merge adds only the docs change, judged by the new docs rule.
    expect(d).toMatchObject({ action: "agent_review", reason: "Docs now get a second look.", policy_commit: moved, paths: ["docs/usage.md"] });
    expect(d.detail.diff_base).toMatchObject({ method: "trunk_head", commits: [moved] });
    await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "agent-z", reason: "Checked the docs read correctly" });
    expect((await land(project, id)).status).toBe(201);
  });

  it("fails closed when the trunk policy cannot be read", async () => {
    const { project, id, baseCommit } = await approvedDocsChange();
    const moved = fake.commit(project, { ...fake.filesAt(baseCommit), "README.md": "# demo v2\n" });
    fake.failReadFile.add(moved);
    const before = trunkTokens(project);
    const r = await land(project, id);
    expect(r.status).toBeGreaterThanOrEqual(500);
    expect(trunkTokens(project)).toBe(before);
    fake.failReadFile.clear();
    expect((await land(project, id)).status).toBe(201); // the failed attempt freed the slot
  });
});

describe("decisions from before the policy digest", () => {
  it("land compares the policy at the intent's trunk base: unchanged lands", async () => {
    const { project, id, decision, baseCommit } = await approvedDocsChange();
    await forgetPolicy(project, decision.id);
    fake.commit(project, { ...fake.filesAt(baseCommit), "README.md": "# demo v2\n" });
    expect((await land(project, id)).status).toBe(201);
  });

  it("land compares the policy at the intent's trunk base: changed is refused", async () => {
    const { project, id, decision, baseCommit } = await approvedDocsChange();
    await forgetPolicy(project, decision.id);
    fake.commit(project, { ...fake.filesAt(baseCommit), ".wayglass/policy.yaml": STRICTER });
    expect((await land(project, id)).json.error?.code).toBe("policy_changed");
  });

  it("refuses when the policy at the base cannot be read", async () => {
    const { project, id, decision, baseCommit } = await approvedDocsChange();
    await forgetPolicy(project, decision.id);
    fake.commit(project, { ...fake.filesAt(baseCommit), "README.md": "# demo v2\n" });
    fake.failReadFile.add(baseCommit);
    const before = trunkTokens(project);
    const r = await land(project, id);
    expect(r.json.error?.code).toBe("policy_changed");
    expect(r.json.error!.message).toMatch(/could not tell which policy/);
    expect(trunkTokens(project)).toBe(before);
  });
});
