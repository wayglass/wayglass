// Runs testdata/policy-vectors.json through the Wasm build. Go runs the same
// file (internal/policy/vectors_test.go), so a stale or divergent Wasm build
// fails here.

import { describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import { evaluatePolicy, type WireRequest } from "../src/policy";

interface Vector {
  name: string;
  policy: string | null;
  request: Omit<WireRequest, "policy_yaml">;
  expect: {
    action: string;
    matched_rule: string;
    approvers: number;
    areas: string[];
    data_classes: string[];
    risk: string;
    shadow_matches: string[];
    would_differ: boolean;
    overrode?: string;
    reviewer_different_family?: boolean;
    unscanned?: string[];
    reviewer_kind?: string;
    ai?: { min_confidence?: number; model?: string; advise?: boolean };
  };
}

const policies = vectors.policies as Record<string, string>;
const cases = vectors.vectors as Vector[];

describe("policy wasm matches the shared vectors", () => {
  it("has vectors", () => {
    expect(cases.length).toBeGreaterThan(10);
  });

  it.each(cases.map((v) => [v.name, v] as const))("%s", async (_name, v) => {
    const policy_yaml = v.policy === null ? "" : policies[v.policy]!;
    const d = await evaluatePolicy({ ...v.request, policy_yaml });
    expect({
      action: d.action,
      matched_rule: d.matched_rule,
      approvers: d.approvers,
      areas: d.tags.areas,
      data_classes: d.tags.data_classes,
      risk: d.tags.risk,
      shadow_matches: d.shadow_matches.map((s) => s.rule),
      would_differ: d.would_differ,
      overrode: d.overrode?.id,
      reviewer_different_family: d.reviewer?.different_family === true,
      unscanned: d.tags.unscanned,
      reviewer_kind: d.reviewer?.kind,
      ai: d.ai,
    }).toEqual({ ...v.expect, overrode: v.expect.overrode, reviewer_different_family: v.expect.reviewer_different_family ?? false, reviewer_kind: v.expect.reviewer_kind, ai: v.expect.ai, unscanned: v.expect.unscanned ?? [] });
    expect(d.reason).not.toBe("");
  });

  it("routes a binary file outside binary.allow to a person whatever the rules say", async () => {
    const d = await evaluatePolicy({
      policy_yaml: policies.open!,
      changes: [{ path: "docs/notes.txt", added_lines: [], removed_count: 0, binary: true, bytes: 64 }],
      jurisdiction: "us",
    });
    expect(d.action).toBe("human_approve");
    expect(d.matched_rule).toBe("builtin:unscanned-binary");
    expect(d.overrode?.id).toBe("anything-goes");
    expect(d.tags.unscanned).toEqual(["docs/notes.txt"]);
    expect(d.tags.binary_bytes).toBe(64);
    expect(d.reason).toContain("docs/notes.txt");
  });

  it("never echoes matched secret text into evidence", async () => {
    const token = "art_v1_" + "ab".repeat(20);
    const d = await evaluatePolicy({
      policy_yaml: policies.example!,
      changes: [{ path: "a.go", added_lines: [`t := "${token}"`], removed_count: 0 }],
      jurisdiction: "us",
    });
    expect(d.action).toBe("block");
    expect(JSON.stringify(d)).not.toContain(token);
    // Every secret the shared vectors block, whatever the pattern: the
    // record names the pattern and the line number, not the text.
    const secrets = cases.filter((v) => v.expect.data_classes.includes("secrets"));
    expect(secrets.length).toBeGreaterThan(10);
    for (const v of secrets) {
      const r = await evaluatePolicy({ ...v.request, policy_yaml: policies[v.policy!]! });
      expect(r.action).toBe("block");
      for (const c of v.request.changes) {
        for (const line of c.added_lines) {
          if (line.length >= 8) expect(JSON.stringify(r)).not.toContain(line);
        }
      }
    }
  });

  it("survives many calls in one isolate", async () => {
    for (let i = 0; i < 200; i++) {
      const d = await evaluatePolicy({ policy_yaml: policies.example!, changes: [{ path: `docs/${i}.md`, added_lines: ["x".repeat(i)], removed_count: 0 }], jurisdiction: "us" });
      expect(d.action).toBe("auto_merge");
    }
  });
});
