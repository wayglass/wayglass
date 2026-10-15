import { beforeEach, describe, expect, it } from "vitest";
import { FORK_TOKEN_TTL_SECONDS, forkWithRetry } from "../src/artifacts";
import type { FakeRepoHost } from "./fake-artifacts";
import { call, createProject, declare, uniqueProject, useFakeArtifacts } from "./helpers";

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

describe("projects", () => {
  it("creates the trunk repo and a short-lived trunk token", async () => {
    const name = uniqueProject();
    const r = await call("POST", "/v1/projects", { name });
    expect(r.status).toBe(201);
    expect(r.json.data.project.name).toBe(name);
    expect(r.json.data.trunk.name).toBe(name);
    expect(r.json.data.token.scope).toBe("write");
    expect(fake.repos.get(name)).toEqual({ source: null });
  });

  it("refuses to create the same project twice", async () => {
    const name = await createProject();
    const r = await call("POST", "/v1/projects", { name });
    expect(r.status).toBe(409);
    expect(r.json.error?.code).toBe("conflict");
  });
});

describe("intents", () => {
  it("forks per intent and mints a write token scoped to that fork", async () => {
    const project = await createProject();
    const r = await declare(project, ["web/login.ts", "web/auth/"]);
    expect(r.status).toBe(201);
    const { intent, fork, token, overlaps } = r.json.data;
    expect(intent.id).toMatch(/^i[0-9a-f]{10}$/);
    expect(intent.status).toBe("open");
    expect(intent.declared_paths).toEqual(["web/auth/**", "web/login.ts"]);
    expect(intent.actual_paths).toEqual([]);
    expect(fork.name).toBe(`${project}--${intent.id}`);
    expect(intent.fork_repo).toBe(fork.name);
    expect(fake.repos.get(fork.name)).toEqual({ source: project });
    expect(token.scope).toBe("write");
    expect(fake.tokens.at(-1)).toMatchObject({ repo: fork.name, scope: "write", ttl: FORK_TOKEN_TTL_SECONDS });
    expect(overlaps).toEqual([]);
  });

  it("never stores the token in the intent record", async () => {
    const project = await createProject();
    await declare(project, ["a.txt"]);
    const list = await call("GET", `/v1/intents?project=${project}`);
    expect(JSON.stringify(list.json)).not.toMatch(/fake_token/);
  });

  it("lists intents in creation order and filters by status", async () => {
    const project = await createProject();
    const a = await declare(project, ["a.txt"], "first");
    const b = await declare(project, ["b.txt"], "second");
    const r = await call("GET", `/v1/intents?project=${project}`);
    expect(r.status).toBe(200);
    expect(r.json.data.intents.map((i: { id: string }) => i.id)).toEqual([a.json.data.intent.id, b.json.data.intent.id]);
    const landed = await call("GET", `/v1/intents?project=${project}&status=landed`);
    expect(landed.json.data.intents).toEqual([]);
    expect((await call("GET", `/v1/intents?project=${project}&status=bogus`)).status).toBe(400);
  });

  it.each([
    [{ reason: "r", paths: ["a"] }, /goal is required/],
    [{ goal: "g", paths: ["a"] }, /reason is required/],
    [{ goal: "g", reason: "r" }, /paths must be an array/],
    [{ goal: "g", reason: "r", paths: [] }, /at least one/],
    [{ goal: "g", reason: "r", paths: ["/abs"] }, /invalid path/],
    // Agent-written text is shown later in terminals and browsers: no escapes.
    [{ goal: "fix login\u001b[2J\u001b]8;;https://x.example\u0007", reason: "r", paths: ["a"] }, /goal contains control characters/],
    [{ goal: "g", reason: "looks fine\rapproved by a person", paths: ["a"] }, null],
    [{ goal: "g", reason: "r\u202eevil", paths: ["a"] }, /reason contains control characters/],
    [{ goal: "g", reason: "r", paths: ["web/\u001b[31mx.ts"] }, /invalid path/],
    [{ goal: "g", reason: "r", paths: ["web/a\nb.ts"] }, /invalid path/],
  ])("validates %j", async (body, msg) => {
    const project = await createProject();
    const r = await call("POST", "/v1/intents", { project, ...body });
    if (msg === null) {
      // Tabs, newlines and carriage returns stay allowed in prose fields;
      // terminals strip a lone \r on display (internal/textsafe).
      expect(r.status).toBe(201);
      return;
    }
    expect(r.status).toBe(400);
    expect(r.json.error?.message).toMatch(msg);
  });

  it("returns upstream_error when the fork fails, and records nothing", async () => {
    const project = await createProject();
    fake.failNextFork = true;
    const r = await declare(project, ["a.txt"]);
    expect(r.status).toBe(502);
    expect(r.json.error?.code).toBe("upstream_error");
    expect((await call("GET", `/v1/intents?project=${project}`)).json.data.intents).toEqual([]);
  });

  it("retries a fork that fails with INTERNAL_ERROR under load, and meters both tries", async () => {
    const project = await createProject();
    fake.forkFailures = ["INTERNAL_ERROR"];
    const before = fake.forkCalls;
    const r = await declare(project, ["a.txt"]);
    expect(r.status).toBe(201);
    expect(fake.forkCalls - before).toBe(2);
    const id = r.json.data.intent.id;
    // The failed name stays taken at Artifacts, so the retry forks under a fresh one.
    expect(r.json.data.fork.name).toBe(`${project}--${id}--f1`);
    expect(r.json.data.intent.fork_repo).toBe(`${project}--${id}--f1`);
    const cost = await call("GET", `/v1/cost?project=${project}`);
    expect(cost.json.data.intents[id].by_op.fork).toBe(2);
  });

  it("does not retry other fork errors", async () => {
    const project = await createProject();
    fake.forkFailures = ["QUOTA_EXCEEDED"];
    const before = fake.forkCalls;
    const r = await declare(project, ["a.txt"]);
    expect(r.status).toBe(502);
    expect(fake.forkCalls - before).toBe(1);
  });

  it("returns not_found when declaring against an unknown project", async () => {
    const r = await declare("no-such-project", ["a.txt"]);
    expect(r.status).toBe(404);
  });
});

describe("push", () => {
  it("records actual changed paths and the head commit", async () => {
    const project = await createProject();
    const id = (await declare(project, ["web/**"])).json.data.intent.id;
    await call("POST", `/v1/intents/${id}/push`, { project, paths: ["web/a.ts"], commit: "abc1234" });
    const r = await call("POST", `/v1/intents/${id}/push`, { project, paths: ["web/b.ts", "web/a.ts"] });
    expect(r.status).toBe(200);
    expect(r.json.data.intent.actual_paths).toEqual(["web/a.ts", "web/b.ts"]);
    expect(r.json.data.intent.head_commit).toBe("abc1234");
  });

  it("returns not_found for an unknown intent", async () => {
    const project = await createProject();
    const r = await call("POST", `/v1/intents/i0000000000/push`, { project, paths: ["a"] });
    expect(r.status).toBe(404);
  });
});

describe("overlap detection", () => {
  it("reports overlap between declared paths", async () => {
    const project = await createProject();
    const a = await declare(project, ["web/login.ts"], "fix login", "claude");
    const b = await declare(project, ["web/**"], "restyle web", "codex");
    expect(a.json.data.overlaps).toEqual([]);
    expect(b.json.data.overlaps).toEqual([
      {
        intent_id: a.json.data.intent.id,
        goal: "fix login",
        agent: "claude",
        status: "open",
        paths: [{ mine: "web/**", theirs: "web/login.ts" }],
      },
    ]);
  });

  it("does not report disjoint intents", async () => {
    const project = await createProject();
    await declare(project, ["docs/*.md"]);
    const b = await declare(project, ["src/**"]);
    expect(b.json.data.overlaps).toEqual([]);
  });

  it("detects overlap that only appears once actual paths are pushed", async () => {
    const project = await createProject();
    const a = await declare(project, ["docs/guide.md"], "docs", "claude");
    const b = await declare(project, ["web/**"], "web", "codex");
    expect(b.json.data.overlaps).toEqual([]);
    // Agent A strays outside its declared paths.
    const push = await call("POST", `/v1/intents/${a.json.data.intent.id}/push`, { project, paths: ["web/index.html"] });
    expect(push.json.data.overlaps).toHaveLength(1);
    expect(push.json.data.overlaps[0].intent_id).toBe(b.json.data.intent.id);
    expect(push.json.data.overlaps[0].paths).toEqual([{ mine: "web/index.html", theirs: "web/**" }]);
  });

  it("ignores intents in other projects", async () => {
    const p1 = await createProject();
    const p2 = await createProject();
    await declare(p1, ["web/**"]);
    const b = await declare(p2, ["web/**"]);
    expect(b.json.data.overlaps).toEqual([]);
  });
});

describe("forkWithRetry", () => {
  it("gives up after the last delay and throws the INTERNAL_ERROR", async () => {
    fake.forkFailures = ["INTERNAL_ERROR", "INTERNAL_ERROR", "INTERNAL_ERROR"];
    const before = fake.forkCalls;
    await expect(forkWithRetry(fake, "trunk", "f", "d", [0, 0])).rejects.toMatchObject({ code: "INTERNAL_ERROR" });
    expect(fake.forkCalls - before).toBe(3);
  });
});
