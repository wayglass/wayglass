// Propose classifies only the intent's own change: what landing it would add
// to the trunk. Trunk files merged in by wayglass sync (or a land_conflict
// resolution) are not counted; anything the merge itself changes is. Found
// by the 100-agent scale run, where a one-file docs change that synced about
// 50 landed docs crossed the 40-file high-risk line and went to a person.

import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import type { Project } from "../src/project";
import { projectStub } from "../src/stub";
import type { FakeRepoHost, Files } from "./fake-artifacts";
import { call, createProject, declare, useFakeArtifacts } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "README.md": "# demo\n", "docs/usage.md": "Usage\n" };

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

/** A project, an intent forked from BASE and one own docs commit on the fork. */
async function intentWithDocsChange() {
  const project = await createProject();
  const base = fake.commit(project, BASE);
  const r = await declare(project, ["docs/**"], "document usage", "agent-a");
  expect(r.status).toBe(201);
  const id: string = r.json.data.intent.id;
  const fork: string = r.json.data.fork.name;
  const own = fake.commit(fork, { ...BASE, "docs/usage.md": "Usage\nMore.\n" });
  return { project, base, id, fork, own };
}

/** Lands `n` docs files on the trunk, as other agents would. */
function landDocs(project: string, from: string, n: number, tag = "a"): string {
  const files = { ...fake.filesAt(from) };
  for (let i = 0; i < n; i++) files[`docs/landed-${tag}-${i}.md`] = `Landed ${tag} ${i}\n`;
  return fake.commit(project, files);
}

/** wayglass sync: merges the trunk commit into the fork head, with `edits` on top of the plain merge. */
function syncMerge(fork: string, own: string, trunk: string, edits: Files = {}): string {
  return fake.commit(fork, { ...fake.filesAt(trunk), "docs/usage.md": "Usage\nMore.\n", ...edits }, [own, trunk]);
}

const propose = async (project: string, id: string) => {
  const r = await call("POST", `/v1/intents/${id}/propose`, { project });
  expect(r.status, JSON.stringify(r.json)).toBe(201);
  return r.json.data.decision;
};

describe("propose classifies the intent's own change", () => {
  it("does not count trunk docs a sync merged in", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = landDocs(project, base, 50);
    syncMerge(fork, own, trunk);
    const d = await propose(project, id);
    expect(d).toMatchObject({ action: "auto_merge", status: "approved", paths: ["docs/usage.md"] });
    expect(d.detail.diff_base).toMatchObject({ method: "trunk_head", commits: [trunk] });
  });

  it("still excludes them when the trunk moved again after the sync", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const synced = landDocs(project, base, 50);
    syncMerge(fork, own, synced);
    landDocs(project, synced, 3, "b");
    const d = await propose(project, id);
    expect(d).toMatchObject({ action: "auto_merge", paths: ["docs/usage.md"] });
    expect(d.detail.diff_base.method).toBe("merge_bases");
    expect(d.detail.diff_base.commits).toEqual([base, synced].sort());
  });

  it("diffs an intent that never synced from its merge base, not the moved trunk", async () => {
    const { project, base, id } = await intentWithDocsChange();
    landDocs(project, base, 50);
    const d = await propose(project, id);
    expect(d).toMatchObject({ action: "auto_merge", paths: ["docs/usage.md"] });
    expect(d.detail.diff_base).toMatchObject({ method: "merge_base", commits: [base] });
  });

  it("counts a conflict resolution that edits a trunk file", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = landDocs(project, base, 50);
    syncMerge(fork, own, trunk, { "docs/landed-a-7.md": "Resolved by the agent\n", "README.md": "# demo, resolved\n" });
    const d = await propose(project, id);
    expect(d.paths).toEqual(["README.md", "docs/landed-a-7.md", "docs/usage.md"]);
  });

  it("counts a change hidden in the merge commit itself", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = landDocs(project, base, 50);
    syncMerge(fork, own, trunk, { "src/auth/login.ts": "export const bypass = true;\n" });
    const d = await propose(project, id);
    expect(d).toMatchObject({ action: "human_approve", reason: "Auth is a security boundary." });
    expect(d.paths).toEqual(["docs/usage.md", "src/auth/login.ts"]);
  });

  it("counts a merged trunk change the intent then reverts", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = fake.commit(project, { ...BASE, "src/auth/login.ts": "export const checked = true;\n" });
    const merged = syncMerge(fork, own, trunk);
    // Back to the fork base's content: diffing from the fork base would miss it.
    fake.commit(fork, { ...BASE, "docs/usage.md": "Usage\nMore.\n" }, [merged]);
    const d = await propose(project, id);
    expect(d.paths).toEqual(["docs/usage.md", "src/auth/login.ts"]);
    expect(d).toMatchObject({ action: "human_approve", reason: "Auth is a security boundary." });
    expect(d.detail.diff_base).toMatchObject({ method: "trunk_head", commits: [trunk] });
    expect(base).not.toBe(trunk);
  });

  it("still sends the intent's own policy edit to a person", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = landDocs(project, base, 5);
    syncMerge(fork, own, trunk, { ".wayglass/policy.yaml": POLICY + "# loosened\n" });
    const d = await propose(project, id);
    expect(d).toMatchObject({ action: "human_approve", matched_rule: "builtin:protect-policy" });
  });

  it("falls back to the fork base when a trunk-store commit is not on the trunk", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    // Pushed to the trunk under another ref: present in its store, never landed.
    const side = fake.commit(fork, { ...BASE, "docs/usage.md": "Usage\nMore.\n", "docs/side.md": "Side\n" }, [own]);
    fake.pushObjects(project, side);
    const trunk = landDocs(project, base, 3);
    fake.commit(fork, { ...fake.filesAt(trunk), "docs/usage.md": "Usage\nMore.\n", "docs/side.md": "Side\n" }, [side, trunk]);
    const d = await propose(project, id);
    expect(d.detail.diff_base).toMatchObject({ method: "fork_base", commits: [base] });
    expect(d.detail.diff_base.fallback).toMatch(/could not be proven/);
    // The old diff: everything since the fork base, merged docs included.
    expect(d.paths).toContain("docs/side.md");
    expect(d.paths).toContain("docs/landed-a-0.md");
  });

  it("refuses a fork that adds nothing to the trunk", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = fake.commit(project, { ...fake.filesAt(base), "docs/usage.md": "Usage\nMore.\n" });
    fake.commit(fork, fake.filesAt(trunk), [own, trunk]);
    const r = await call("POST", `/v1/intents/${id}/propose`, { project });
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/adds nothing to the trunk/);
  });
});

describe("land checks the same path set", () => {
  const land = (project: string, id: string) => call("POST", `/v1/intents/${id}/land`, { project });
  const landed = (project: string, id: string, commit: string) => call("POST", `/v1/intents/${id}/landed`, { project, commit });

  it("a synced intent fast-forwards inside its decision", async () => {
    const { project, base, id, fork, own } = await intentWithDocsChange();
    const trunk = landDocs(project, base, 50);
    const head = syncMerge(fork, own, trunk);
    const d = await propose(project, id);
    expect(d.paths).toEqual(["docs/usage.md"]);
    expect((await land(project, id)).status).toBe(201);
    fake.setHead(project, head);
    const r = await landed(project, id, head);
    expect(r.status, JSON.stringify(r.json)).toBe(200);
  });

  it("flags a fast-forward that changes paths outside the decision", async () => {
    const { project, id, fork, own } = await intentWithDocsChange();
    const trunk = fake.commit(project, { ...BASE, "src/auth/login.ts": "export const checked = true;\n" });
    const merged = syncMerge(fork, own, trunk);
    const head = fake.commit(fork, { ...BASE, "docs/usage.md": "Usage\nMore.\n" }, [merged]);
    const d = await propose(project, id);
    // What the pre-fix diff from the fork base recorded: the revert of the trunk's file is missing.
    await runInDurableObject(await projectStub(env, project), async (_p: Project, state) => {
      state.storage.sql.exec("UPDATE decisions SET paths = ? WHERE id = ?", JSON.stringify(["docs/usage.md"]), d.id);
    });
    const ok = await call("POST", `/v1/decisions/${d.id}`, { project, verdict: "approve", by: "john-tester", reason: "test" });
    expect(ok.json.data.decision.status).toBe("approved");
    expect((await land(project, id)).status).toBe(201);
    fake.setHead(project, head);
    const r = await landed(project, id, head);
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/fast-forward changes paths outside the decision: src\/auth\/login.ts/);
  });
});
