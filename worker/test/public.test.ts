// Public read view (src/public.ts): opt-in twice (PUBLIC_PROJECTS and the
// project's flag), read-only, allowlisted fields only, cached.

import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import { projectAllowed, publicDecision, publicEvent, scrub } from "../src/public";
import type { FakeRepoHost, Files } from "./fake-artifacts";
import { call, createProject, declare, uniqueProject, useFakeArtifacts } from "./helpers";

const POLICY = (vectors.policies as Record<string, string>).example!;
const BASE: Files = { ".wayglass/policy.yaml": POLICY, "docs/usage.md": "Usage\n", "web/login.ts": "export function login() {}\n" };

// Token-shaped fixtures are assembled at runtime: a literal one in the source
// would (rightly) trip the secrets rule in this repo's own policy.
const fakeToken = (v: number, letter = "") => `art_v${v}_${letter}${"0123456789abcdef".repeat(3).slice(0, 40)}`;

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

/** Env override for PUBLIC_PROJECTS (wrangler types gives the var its deployed literal type). */
const allowList = (list: string) => ({ PUBLIC_PROJECTS: list }) as unknown as Partial<Env>;

/** A public GET: no Authorization header, the project allowed by the Worker var. */
const pub = (path: string, allow: string, method = "GET") => call(method, `/v1/public/projects/${path}`, undefined, {}, allowList(`other, ${allow}`));
const makePublic = (project: string, on = true) => call("PATCH", `/v1/projects/${project}`, { public: on, agent: "john" });

/** A project with one auto-merged docs change and one auth change a person approved. */
async function populated() {
  const project = await createProject();
  fake.commit(project, BASE);
  const docs = (await declare(project, ["docs/**"], "fix the docs", "agent-a")).json.data;
  const auth = (await call("POST", "/v1/intents", { project, goal: `tighten login, token ${fakeToken(2, "q_")}?expires=1`, reason: "audit finding", paths: ["web/**"], agent: "agent-b" })).json.data;
  const head = (fork: string, edits: Files) => fake.commit(fork, { ...fake.filesAt(fake.heads.get(fork)!), ...edits });
  head(docs.fork.name, { "docs/usage.md": "Usage\nMore.\n" });
  head(auth.fork.name, { "web/login.ts": "export function login() { return false; }\n" });
  const d1 = (await call("POST", `/v1/intents/${docs.intent.id}/propose`, { project })).json.data.decision;
  const d2 = (await call("POST", `/v1/intents/${auth.intent.id}/propose`, { project, action: "auto_merge" })).json.data.decision;
  const approved = await call("POST", `/v1/decisions/${d2.id}`, { project, verdict: "approve", by: "john", reason: "checked the session handling" });
  expect(approved.status).toBe(200);
  return { project, docs: docs.intent.id as string, auth: auth.intent.id as string, d1: d1.id as string, d2: d2.id as string };
}

describe("public view gates", () => {
  it("is off by default and answers like a missing project", async () => {
    const project = await createProject();
    const missing = await pub(uniqueProject("nope"), "x");
    const offFlag = await pub(project, project);
    expect(offFlag.status).toBe(404);
    expect(offFlag.json.error).toEqual(missing.json.error);
  });

  it("reports the Worker's main jurisdiction for a project that recorded none", async () => {
    // A self-host with an unrestricted namespace: nothing says "us".
    const unrestricted = { ARTIFACTS_JURISDICTION: "" } as unknown as Partial<Env>;
    const project = uniqueProject("open");
    const made = await call("POST", "/v1/projects", { name: project }, undefined, unrestricted);
    expect(made.status).toBe(201);
    await makePublic(project);
    const r = await call("GET", `/v1/public/projects/${project}`, undefined, {}, { ...allowList(project), ...unrestricted });
    expect(r.status).toBe(200);
    expect(r.json.data.project).toMatchObject({ name: project, jurisdiction: "" });
  });

  it("needs the Worker var as well as the project flag", async () => {
    const project = await createProject();
    expect((await makePublic(project)).json.data).toEqual({ public: true });
    expect((await pub(project, "someone-else")).status).toBe(404);
    expect((await pub(project, project)).status).toBe(200);
  });

  it("only the admin can flip the flag, and flipping it is audited", async () => {
    const project = await createProject();
    const anon = await call("PATCH", `/v1/projects/${project}`, { public: true }, {});
    expect(anon.status).toBe(401);
    // A bound key, agent or human, is not the admin: it cannot open a project's trail to everyone.
    for (const kind of ["agent", "human"] as const) {
      const minted = await call("POST", "/v1/keys", { agent: `flipper-${kind}`, kind });
      const bound = { authorization: `Bearer ${minted.json.data.key}` };
      const r = await call("PATCH", `/v1/projects/${project}`, { public: true }, bound);
      expect(r.status).toBe(403);
      expect(r.json.error).toMatchObject({ code: "forbidden", message: expect.stringMatching(/admin key.*public/) });
      const created = await call("POST", "/v1/projects", { name: uniqueProject(), public: true }, bound);
      expect(created.status).toBe(403);
      expect(created.json.error?.code).toBe("forbidden");
      // Without the flag, a bound key still creates projects as before.
      expect((await call("POST", "/v1/projects", { name: uniqueProject() }, bound)).status).toBe(201);
    }
    expect((await pub(project, project)).status).toBe(404);
    expect((await call("GET", `/v1/audit?project=${project}`)).json.data.events.some((e: { kind: string }) => e.kind.startsWith("project.made_"))).toBe(false);
    expect((await call("PATCH", `/v1/projects/${project}`, { public: "yes" })).status).toBe(400);
    await makePublic(project);
    await makePublic(project); // idempotent: no second event
    await makePublic(project, false);
    const kinds = (await call("GET", `/v1/audit?project=${project}`)).json.data.events.map((e: { kind: string }) => e.kind);
    expect(kinds.filter((k: string) => k.startsWith("project.made_"))).toEqual(["project.made_public", "project.made_private"]);
    expect((await pub(project, project)).status).toBe(404);
  });

  it("can be turned on at project creation", async () => {
    const name = uniqueProject();
    const created = await call("POST", "/v1/projects", { name, public: true });
    expect(created.status).toBe(201);
    expect(created.json.data.public).toBe(true);
    expect((await pub(name, name)).status).toBe(200);
  });

  it("is read-only", async () => {
    const project = await createProject();
    await makePublic(project);
    const r = await pub(`${project}/intents`, project, "POST");
    expect(r.status).toBe(405);
  });
});

describe("public view content", () => {
  it("counts agents, intents and decisions by action", async () => {
    const { project } = await populated();
    await makePublic(project);
    const r = await pub(project, project);
    expect(r.status).toBe(200);
    expect(r.json.data.project).toMatchObject({ name: project, jurisdiction: "us" });
    expect(r.json.data.counts).toMatchObject({
      agents: 2,
      intents: 2,
      decisions: 2,
      decisions_by_action: { auto_merge: 1, human_approve: 1 },
      decisions_by_status: { approved: 2 },
      intents_by_status: { proposed: 2 },
    });
    // Worker-side Artifacts cost, labelled as such; counts only, never tokens.
    const cost = r.json.data.cost;
    expect(cost.total.ops).toBeGreaterThan(0);
    expect(cost.total.by_op.fork).toBe(2);
    expect(cost.scope).toMatch(/Worker-side/);
    expect(Object.keys(cost).sort()).toEqual(["ai", "events", "per_landed_change", "pricing", "scope", "total"]);
  });

  it("lists intents without remotes, and masks token-shaped text", async () => {
    const { project, auth } = await populated();
    await makePublic(project);
    const intents = (await pub(`${project}/intents`, project)).json.data.intents;
    expect(intents).toHaveLength(2);
    for (const i of intents) {
      expect(Object.keys(i).sort()).toEqual(
        ["abandoned_reason", "actual_paths", "agent", "agent_family", "base_commit", "created_at", "declared_paths", "goal", "head_commit", "id", "parent_id", "reason", "stale", "status", "updated_at"].sort(),
      );
    }
    const a = intents.find((i: { id: string }) => i.id === auth);
    expect(a.goal).toBe("tighten login, token [token]");
    expect(JSON.stringify(intents)).not.toMatch(/art_v2_|fake\.artifacts/);
  });

  it("shows decisions with the matched rule and the approver's reason, but not the client preview", async () => {
    const { project, auth, d2 } = await populated();
    await makePublic(project);
    const list = (await pub(`${project}/decisions`, project)).json.data.decisions;
    const d = list.find((x: { id: string }) => x.id === d2);
    expect(d).toMatchObject({ intent_id: auth, action: "human_approve", matched_rule: "rules[2]", status: "approved", decided_by: "admin" });
    expect(d.approvals).toEqual([expect.objectContaining({ by: "admin", claimed_by: "john", verdict: "approve", reason: "checked the session handling" })]);
    expect(d.files).toEqual([{ path: "web/login.ts", added: 1, deleted: 1 }]);
    expect(d).not.toHaveProperty("client_preview");
    expect(d).not.toHaveProperty("detail");

    const one = (await pub(`${project}/decisions/${d2}`, project)).json.data;
    expect(one.decision.id).toBe(d2);
    expect(one.intent).toMatchObject({ id: auth, reason: "audit finding" });
    expect((await pub(`${project}/decisions/d0000000000`, project)).status).toBe(404);
    expect((await pub(`${project}/decisions/nope`, project)).status).toBe(400);
  });

  it("shows the audit trail with allowlisted data only", async () => {
    const { project } = await populated();
    await makePublic(project);
    const r = await pub(`${project}/audit?limit=500`, project);
    const events = r.json.data.events as { kind: string; data: Record<string, unknown> }[];
    const kinds = events.map((e) => e.kind);
    expect(kinds).toEqual(expect.arrayContaining(["project.created", "intent.declared", "decision.proposed", "decision.approve", "project.made_public"]));
    const proposed = events.filter((e) => e.kind === "decision.proposed");
    for (const e of proposed) {
      expect(e.data).not.toHaveProperty("client_preview");
      expect(e.data).toHaveProperty("matched_rule");
    }
    expect(events.find((e) => e.kind === "intent.declared")!.data).not.toHaveProperty("fork_repo");
    expect(events.find((e) => e.kind === "project.created")!.data).not.toHaveProperty("trunk_repo");
    expect(JSON.stringify(events)).not.toMatch(/art_v2_|fake\.artifacts/);
    expect((await pub(`${project}/audit?limit=0`, project)).status).toBe(400);
  });

  it("serves repeat reads from the cache under a canonical key", async () => {
    const project = await createProject();
    await makePublic(project);
    const first = await call("GET", `/v1/public/projects/${project}/intents`, undefined, {}, allowList(project));
    expect(first.status).toBe(200);
    // A stray query string must not bypass the cache.
    const req = new Request(`https://wayglass.test/v1/public/projects/${project}/intents?bust=1`);
    const { default: worker } = await import("../src/index");
    const { env } = await import("cloudflare:workers");
    const res = await worker.fetch(req as Request<unknown, IncomingRequestCfProperties>, { ...env, ...allowList(project) } as Env);
    expect(res.headers.get("x-wayglass-cache")).toBe("hit");
    expect(res.headers.get("access-control-allow-origin")).toBe("*");
    expect(res.headers.get("cache-control")).toMatch(/max-age=\d+/);
    await res.arrayBuffer();
  });
});

describe("PUBLIC_PROJECTS patterns", () => {
  const allowed = (list: string, name: string) => projectAllowed({ PUBLIC_PROJECTS: list } as unknown as Env, name);
  it.each([
    ["demo-*,eu-demo-*", "demo-0412", true],
    ["demo-*,eu-demo-*", "eu-demo-1", true],
    ["demo-*,eu-demo-*", "demo-", true],
    ["demo-*,eu-demo-*", "wayglass", false],
    ["demo-*,eu-demo-*", "my-demo-1", false],
    ["demo-*,eu-demo-*", "demo", false],
    [" demo-* , other ", "other", true],
    ["wayglass", "wayglass", true],
    ["wayglass", "wayglass-eu", false],
    ["*", "anything", false],
    ["de*mo", "demo", false],
    ["de*mo", "de*mo", false],
    ["demo-**", "demo-x", false],
    ["", "demo-1", false],
  ])("%j allows %j: %s", (list, name, want) => {
    expect(allowed(list, name)).toBe(want);
  });

  it("still needs the project's own flag", async () => {
    const name = await createProject(uniqueProject("demo-"));
    const get = () => call("GET", `/v1/public/projects/${name}`, undefined, {}, allowList("demo-*,eu-demo-*"));
    expect((await get()).status).toBe(404);
    await makePublic(name);
    expect((await get()).status).toBe(200);
  });
});

describe("redaction helpers", () => {
  it.each([
    [fakeToken(1), "[token]"],
    ["see https://00000000000000000000000000000000.artifacts.cloudflare.net/git/wayglass/x.git now", "see [artifacts remote] now"],
    ["plain text", "plain text"],
  ])("scrub(%j)", (input, want) => {
    expect(scrub({ a: [input] })).toEqual({ a: [want] });
  });

  it("shows shadow matches (wire v2, or v1 `shadow`) from an allowlist", () => {
    const m = { rule: "rules[0]", index: 0, action: "block", reason: "would block", preempts: true, extra: "private" };
    const base = { id: "d0123456789", intent_id: "i0123456789", tags: {}, approvals: [] } as unknown as Parameters<typeof publicDecision>[0];
    const v2 = publicDecision({ ...base, detail: { shadow_matches: [m], would_differ: true } } as typeof base);
    expect(v2).toMatchObject({ shadow_matches: [{ rule: "rules[0]", index: 0, action: "block", reason: "would block", preempts: true }], would_differ: true });
    expect((v2 as { shadow_matches: Record<string, unknown>[] }).shadow_matches[0]).not.toHaveProperty("extra");
    const v1 = publicDecision({ ...base, detail: { shadow: [m] } } as typeof base);
    expect(v1).toMatchObject({ shadow_matches: [{ rule: "rules[0]" }], would_differ: false });
  });

  it("drops data fields of unknown audit kinds", () => {
    const e = publicEvent({ seq: 1, at: "t", kind: "something.new", actor: "x", intent_id: null, decision_id: null, data: { secret: "s" } });
    expect(e.data).toEqual({});
  });
});
