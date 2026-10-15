// Each project's repos and tokens stay in the namespace its jurisdiction
// picked at creation. With separate us and eu fakes, every route that touches
// Artifacts (token, propose, land, landed) must reach only its own namespace,
// and an unknown project is refused rather than served from the main one.

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import { setRepoHostForTests } from "../src/artifacts";
import { FakeRepoHost, type Files } from "./fake-artifacts";
import { call, createProjectIn, declare, uniqueProject } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "docs/usage.md": "Usage\n" };

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

const fakes = (j: "us" | "eu") => (j === "us" ? { own: us, other: eu } : { own: eu, other: us });

describe.each(["us", "eu"] as const)("a %s project", (j) => {
  it("mints, proposes, lands and verifies only in its own namespace", async () => {
    const { own, other } = fakes(j);
    const project = await createProjectIn(j);
    own.commit(project, BASE);
    const d = await declare(project, ["docs/**"]);
    expect(d.status).toBe(201);
    const id: string = d.json.data.intent.id;
    const fork: string = d.json.data.fork.name;
    expect(own.repos.has(fork)).toBe(true);

    const tok = await call("POST", `/v1/intents/${id}/token`, { project, agent: "agent-a" });
    expect(tok.status).toBe(201);
    expect(own.tokens.at(-1)).toMatchObject({ repo: fork, scope: "write" });

    const head = own.commit(fork, { ...BASE, "docs/usage.md": "Usage\nMore.\n" });
    const p = await call("POST", `/v1/intents/${id}/propose`, { project });
    expect(p.status).toBe(201);
    expect(p.json.data.decision).toMatchObject({ status: "approved", tags: { jurisdiction: j } });

    const land = await call("POST", `/v1/intents/${id}/land`, { project });
    expect(land.status).toBe(201);
    const landToken = own.tokens.at(-1)!;
    expect(landToken).toMatchObject({ repo: project, scope: "write" });

    own.setHead(project, head);
    const landed = await call("POST", `/v1/intents/${id}/landed`, { project, commit: head });
    expect(landed.status).toBe(200);
    expect(own.revoked).toContainEqual({ repo: project, id: landToken.id });

    expect(other.repos.size).toBe(0);
    expect(other.tokens).toEqual([]);
    expect(other.revoked).toEqual([]);
    expect(other.reads).toEqual({ tree: 0, blob: 0, commit: 0, file: 0 });
  });
});

describe("an unknown project", () => {
  const routes = ["token", "abandon", "propose", "land", "landed"] as const;
  it.each(routes)("is refused by %s without touching any namespace", async (route) => {
    const project = uniqueProject("nope-");
    const body: Record<string, unknown> = { project, agent: "agent-a", reason: "no longer needed here", commit: "a".repeat(40) };
    const r = await call("POST", `/v1/intents/i0123456789/${route}`, body);
    expect(r.status).toBe(404);
    expect(r.json.ok).toBe(false);
    expect(r.json.error?.code).toBe("not_found");
    for (const f of [us, eu]) {
      expect(f.tokens).toEqual([]);
      expect(f.revoked).toEqual([]);
      expect(f.repos.size).toBe(0);
    }
  });
});
