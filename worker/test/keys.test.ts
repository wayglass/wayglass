// Per-agent keys: minted by the admin, bound to one agent name, stored only as
// a hash. A bound key cannot claim another name through X-Wayglass-Agent,
// body.agent or body.by.

import { runInDurableObject } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import type { FakeRepoHost } from "./fake-artifacts";
import { hashKey, Registry, registryStub as registry } from "../src/keys";
import { call, createProject, createProjectIn, KEY, useFakeArtifacts } from "./helpers";

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});

const as = (key: string, agent?: string): Record<string, string> => ({
  authorization: `Bearer ${key}`,
  ...(agent ? { "x-wayglass-agent": agent } : {}),
});

async function mint(agent: string, kind?: "human" | "agent"): Promise<{ key: string; id: string }> {
  const r = await call("POST", "/v1/keys", { agent, note: "test", ...(kind ? { kind } : {}) });
  expect(r.status).toBe(201);
  return { key: r.json.data.key, id: r.json.data.info.id };
}

describe("whoami", () => {
  it("reports the admin key and its self-reported name", async () => {
    const r = await call("GET", "/v1/whoami", undefined, as(KEY, "alice@laptop"));
    expect(r.status).toBe(200);
    expect(r.json.data).toEqual({ role: "admin", agent: "alice@laptop", bound: false, key_id: null, kind: "human", source: "admin", expires_at: null });
  });

  it("reports an agent key's bound name, ignoring the header", async () => {
    const { key, id } = await mint("ci-bot");
    const r = await call("GET", "/v1/whoami", undefined, as(key, "admin-pretender"));
    expect(r.json.data).toEqual({ role: "agent", agent: "ci-bot", bound: true, key_id: id, kind: "agent", source: "key", expires_at: null });
  });

  it("needs a key", async () => {
    expect((await call("GET", "/v1/whoami", undefined, {})).status).toBe(401);
  });

  it("answers 401, not 404 or 405, to anyone without a key: the API has no routes to enumerate", async () => {
    for (const [m, p] of [
      ["GET", "/v1/no-such-route"],
      ["DELETE", "/v1/whoami"],
      ["GET", "/v1/keys"],
    ] as const) {
      const r = await call(m, p, undefined, {});
      expect(r.status, `${m} ${p}`).toBe(401);
    }
    expect((await call("GET", "/v1/no-such-route")).status).toBe(404);
    expect((await call("DELETE", "/v1/whoami")).status).toBe(405);
  });

  it.each([
    ["no scheme", (k: string) => k],
    ["another scheme", (k: string) => `Basic ${k}`],
    ["a lower-case scheme", (k: string) => `bearer ${k}`],
    ["a padded token", (k: string) => `Bearer  ${k}`],
    ["a token with a suffix", (k: string) => `Bearer ${k} extra`],
    ["a prefix of the admin key", (k: string) => `Bearer ${k.slice(0, -1)}`],
  ])("refuses the admin key sent with %s", async (_name, wrap) => {
    const r = await call("GET", "/v1/whoami", undefined, { authorization: wrap(KEY) });
    expect(r.status).toBe(401);
    expect(r.json.error?.code).toBe("unauthorized");
  });

  it("refuses a bound key with a suffix or a missing character", async () => {
    const { key } = await mint("exact-bot");
    for (const bad of [`${key}0`, key.slice(0, -1), key.toUpperCase()]) {
      expect((await call("GET", "/v1/whoami", undefined, as(bad))).status).toBe(401);
    }
  });
});

describe("keys", () => {
  it("mints a wgk_ key once and stores only its hash", async () => {
    const r = await call("POST", "/v1/keys", { agent: "claude-1" }, as(KEY, "john@mac"));
    expect(r.status).toBe(201);
    expect(r.json.data.key).toMatch(/^wgk_[0-9a-f]{64}$/);
    expect(r.json.data.info).toMatchObject({ agent: "claude-1", created_by: "john@mac", revoked_at: null });
    const stub = registry(env);
    await runInDurableObject(stub, async (_obj, state) => {
      const rows = state.storage.sql.exec("SELECT * FROM keys WHERE id = ?", r.json.data.info.id).toArray();
      expect(rows).toHaveLength(1);
      expect(JSON.stringify(rows)).not.toContain(r.json.data.key);
      expect(rows[0]!.hash).toMatch(/^[0-9a-f]{64}$/);
    });
    const list = await call("GET", "/v1/keys");
    expect(JSON.stringify(list.json)).not.toContain(r.json.data.key);
    expect(list.json.data.keys.map((k: { id: string }) => k.id)).toContain(r.json.data.info.id);
    // Neither listing nor the events carry the hash: a leaked response is not a replayable row.
    const hash = await hashKey(r.json.data.key);
    expect(JSON.stringify(list.json)).not.toContain(hash);
    expect(JSON.stringify((await call("GET", "/v1/keys?all=1")).json)).not.toContain(hash);
    expect(JSON.stringify((await call("GET", "/v1/keys/events")).json)).not.toContain(hash);
  });

  it.each([[""], ["has space"], ["-lead"], ["x".repeat(129)]])("refuses agent name %j", async (agent) => {
    expect((await call("POST", "/v1/keys", { agent })).status).toBe(400);
  });

  it("is admin-only: neither an agent key nor a human key manages keys", async () => {
    const agent = await mint("worker-a");
    const human = await mint("alice@laptop", "human");
    for (const { key, id } of [agent, human]) {
      for (const [m, p, b] of [
        ["POST", "/v1/keys", { agent: "worker-b" }],
        ["POST", "/v1/keys", { agent: "worker-b", kind: "human" }],
        ["GET", "/v1/keys", undefined],
        ["GET", "/v1/keys/events", undefined],
        ["DELETE", `/v1/keys/${id}`, undefined],
      ] as const) {
        const r = await call(m, p, b, as(key));
        expect(r.status).toBe(403);
        expect(r.json.error?.code).toBe("forbidden");
      }
      // Still live: a refused call revokes nothing.
      expect((await call("GET", "/v1/whoami", undefined, as(key))).status).toBe(200);
    }
    // "self" is a route, not a key id: DELETE /v1/keys/self revokes nothing.
    expect((await call("DELETE", "/v1/keys/self")).status).toBe(400);
  });

  it("stops working once revoked", async () => {
    const { key, id } = await mint("short-lived");
    expect((await call("GET", "/v1/whoami", undefined, as(key))).status).toBe(200);
    const r = await call("DELETE", `/v1/keys/${id}`);
    expect(r.json.data.revoked_at).not.toBeNull();
    const after = await call("GET", "/v1/whoami", undefined, as(key));
    expect(after.status).toBe(401);
    expect(after.json.error?.message).toMatch(/revoked/);
    expect((await call("GET", "/v1/keys")).json.data.keys.map((k: { id: string }) => k.id)).not.toContain(id);
    expect((await call("GET", "/v1/keys?all=1")).json.data.keys.map((k: { id: string }) => k.id)).toContain(id);
  });

  it("lets a bound key revoke itself (sign-out), audited as key.revoked by its own name", async () => {
    const { key, id } = await mint("signing-out");
    const r = await call("POST", "/v1/keys/self/revoke", {}, as(key));
    expect(r.status).toBe(200);
    expect(r.json.data).toMatchObject({ id, agent: "signing-out", revoked_at: expect.any(String) });
    expect((await call("GET", "/v1/whoami", undefined, as(key))).status).toBe(401);
    const events = (await call("GET", "/v1/keys/events")).json.data.events as { kind: string; key_id: string; actor: string }[];
    expect(events.find((e) => e.key_id === id && e.kind === "key.revoked")).toMatchObject({ actor: "signing-out" });
    // The admin key is a Worker secret, not a registry key.
    const admin = await call("POST", "/v1/keys/self/revoke", {});
    expect(admin.status).toBe(400);
    expect(admin.json.error?.message).toMatch(/admin key/);
  });

  it("refuses an unknown wgk_ key and an unknown key id", async () => {
    expect((await call("GET", "/v1/whoami", undefined, as("wgk_" + "0".repeat(64)))).status).toBe(401);
    expect((await call("DELETE", "/v1/keys/k0000000000")).status).toBe(404);
    expect((await call("DELETE", "/v1/keys/nope")).status).toBe(400);
  });
});

describe("a bound key cannot speak for anyone else", () => {
  it("declares as its own name whatever the header and body say", async () => {
    const project = await createProject();
    const { key } = await mint("agent-honest");
    const r = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["docs/**"], agent: "someone-else" }, as(key, "someone-else"));
    expect(r.status).toBe(201);
    expect(r.json.data.intent.agent).toBe("agent-honest");
    const audit = await call("GET", `/v1/audit?project=${project}`);
    const declared = audit.json.data.events.find((e: { kind: string }) => e.kind === "intent.declared");
    expect(declared.actor).toBe("agent-honest");
  });

  it("cannot approve its own change by naming another approver", async () => {
    const policy = (vectors.policies as Record<string, string>).example!;
    const project = await createProject();
    fake.commit(project, { ".wayglass/policy.yaml": policy, "web/login.ts": "export function login() {}\n" });
    const { key } = await mint("author-bot");
    const d = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["**"] }, as(key));
    const id = d.json.data.intent.id;
    const fork = d.json.data.fork.name;
    fake.commit(fork, { ...fake.filesAt(fake.heads.get(fork)!), "web/login.ts": "export function login() { return true; }\n" });
    const p = await call("POST", `/v1/intents/${id}/propose`, { project }, as(key));
    expect(p.json.data.decision.status).toBe("pending_human");
    // An agent key never decides a change routed to a person, whatever name it reports.
    const self = await call("POST", `/v1/intents/${id}/decide`, { project, verdict: "approve", reason: "looks fine", by: "a-person" }, as(key, "a-person"));
    expect(self.status).toBe(403);
    expect(self.json.error?.code).toBe("needs_human");
    // The admin key may name a person, but the verdict counts as the admin key's; the name is kept as claimed_by.
    const ok = await call("POST", `/v1/intents/${id}/decide`, { project, verdict: "approve", reason: "Checked the login change", by: "a-person" });
    expect(ok.status).toBe(200);
    expect(ok.json.data.decision.decided_by).toBe("admin");
    expect(ok.json.data.approvals).toEqual([expect.objectContaining({ by: "admin", claimed_by: "a-person", identity_source: "admin" })]);
  });
});

describe("trunk read token", () => {
  it("mints a 60 s read token for the trunk only, and audits it", async () => {
    const project = await createProject();
    const { key } = await mint("doctor-check");
    const r = await call("POST", `/v1/projects/${project}/trunk/read-token`, {}, as(key));
    expect(r.status).toBe(201);
    expect(r.json.data.token.scope).toBe("read");
    expect(r.json.data.trunk.name).toBe(project);
    expect(fake.tokens.at(-1)).toMatchObject({ repo: project, scope: "read", ttl: 60 });
    const audit = await call("GET", `/v1/audit?project=${project}`);
    expect(audit.json.data.events.at(-1)).toMatchObject({ kind: "trunk.read_token", actor: "doctor-check" });
  });

  it("is not_found for an unknown project", async () => {
    expect((await call("POST", "/v1/projects/no-such-project/trunk/read-token", {})).status).toBe(404);
  });
});

describe("only a person decides a change routed to a person", () => {
  const policy = (vectors.policies as Record<string, string>).example!;

  /** A proposed change by author-bot in a fresh project; returns ids and the decision status. */
  async function proposed(files: Record<string, string>, jurisdiction?: string) {
    const project = jurisdiction ? await createProjectIn(jurisdiction) : await createProject();
    fake.commit(project, { ".wayglass/policy.yaml": policy, "web/login.ts": "export function login() {}\n" });
    const d = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["**"], agent: "author-bot" });
    const id = d.json.data.intent.id as string;
    const fork = d.json.data.fork.name as string;
    fake.commit(fork, { ...fake.filesAt(fake.heads.get(fork)!), ...files });
    const p = await call("POST", `/v1/intents/${id}/propose`, { project });
    return { project, id, decision: p.json.data.decision as { id: string; status: string } };
  }
  const login = { "web/login.ts": "export function login() { return true; }\n" };

  it("mints human keys, lists the kind and refuses unknown kinds", async () => {
    const r = await call("POST", "/v1/keys", { agent: "alice@laptop", kind: "human" });
    expect(r.status).toBe(201);
    expect(r.json.data.info.kind).toBe("human");
    expect((await call("GET", "/v1/whoami", undefined, as(r.json.data.key))).json.data).toMatchObject({ role: "agent", kind: "human", bound: true });
    const listed = (await call("GET", "/v1/keys")).json.data.keys.find((k: { id: string }) => k.id === r.json.data.info.id);
    expect(listed.kind).toBe("human");
    expect((await call("POST", "/v1/keys", { agent: "x", kind: "robot" })).status).toBe(400);
    expect((await mint("default-kind")).key).toMatch(/^wgk_/);
    const def = (await call("GET", "/v1/keys")).json.data.keys.find((k: { agent: string }) => k.agent === "default-kind");
    expect(def.kind).toBe("agent");
  });

  it.each(["intent", "decision"])("refuses an agent key on human_approve (by %s id), and records nothing", async (by) => {
    const { project, id, decision } = await proposed(login);
    expect(decision.status).toBe("pending_human");
    const { key } = await mint("reviewer-bot");
    for (const verdict of ["approve", "reject"]) {
      const path = by === "intent" ? `/v1/intents/${id}/decide` : `/v1/decisions/${decision.id}`;
      const r = await call("POST", path, { project, verdict, reason: "Checked the login change", by: "a-person" }, as(key, "a-person"));
      expect(r.status).toBe(403);
      expect(r.json.error?.code).toBe("needs_human");
      expect(r.json.error?.message).toMatch(/to a person.*keys create --human/);
    }
    const after = await call("GET", `/v1/decisions?project=${project}&status=pending_human`);
    expect(after.json.data.decisions).toEqual([expect.objectContaining({ id: decision.id, approvals: [] })]);
    const audit = await call("GET", `/v1/audit?project=${project}`);
    expect(audit.json.data.events.some((e: { kind: string }) => e.kind.startsWith("decision.approve") || e.kind.startsWith("decision.reject"))).toBe(false);
  });

  it("lets a human key and the admin key decide it", async () => {
    const human = await mint("alice@laptop", "human");
    const a = await proposed(login);
    const r = await call("POST", `/v1/intents/${a.id}/decide`, { project: a.project, verdict: "approve", reason: "Checked the login change" }, as(human.key));
    expect(r.status).toBe(200);
    expect(r.json.data.decision).toMatchObject({ status: "approved", decided_by: "alice@laptop" });
    const b = await proposed(login);
    const admin = await call("POST", `/v1/intents/${b.id}/decide`, { project: b.project, verdict: "approve", reason: "Checked the login change", by: "john" });
    expect(admin.json.data.decision).toMatchObject({ status: "approved", decided_by: "admin" });
    expect(admin.json.data.approvals).toEqual([expect.objectContaining({ by: "admin", claimed_by: "john" })]);
  });

  it("lets a person approve, but only the author or the admin key land it", async () => {
    const human = await mint("bob@laptop", "human");
    const { project, id } = await proposed(login);
    expect((await call("POST", `/v1/intents/${id}/decide`, { project, verdict: "approve", reason: "Checked the login change" }, as(human.key))).status).toBe(200);
    const tokensBefore = fake.tokens.length;
    // Approving says the change may go in; it does not let the approver push it in the author's name.
    const land = await call("POST", `/v1/intents/${id}/land`, { project }, as(human.key, "author-bot"));
    expect(land.status).toBe(403);
    expect(land.json.error?.code).toBe("not_author");
    expect(fake.tokens.length).toBe(tokensBefore);
    const author = await mint("author-bot");
    expect((await call("POST", `/v1/intents/${id}/land`, { project }, as(author.key))).status).toBe(201);
  });

  it("still refuses a human key deciding its own change", async () => {
    const human = await mint("alice-author", "human");
    const project = await createProject();
    fake.commit(project, { ".wayglass/policy.yaml": policy, "web/login.ts": "export function login() {}\n" });
    const d = await call("POST", "/v1/intents", { project, goal: "g", reason: "r", paths: ["**"] }, as(human.key));
    const id = d.json.data.intent.id;
    const fork = d.json.data.fork.name;
    fake.commit(fork, { ...fake.filesAt(fake.heads.get(fork)!), ...login });
    await call("POST", `/v1/intents/${id}/propose`, { project }, as(human.key));
    const r = await call("POST", `/v1/intents/${id}/decide`, { project, verdict: "approve", reason: "Checked my own change" }, as(human.key));
    expect(r.status).toBe(409);
    expect(r.json.error?.message).toMatch(/wrote a change cannot decide/);
  });

  it("refuses an agent key on multi_approve and on quarantine", async () => {
    const { key } = await mint("reviewer-bot-2");
    const infra = await proposed({ "worker/wrangler.jsonc": "{}\n" });
    expect(infra.decision.status).toBe("pending_human");
    const pii = await proposed({ "web/profile.ts": "save({ date_of_birth: dob })\n" }, "eu");
    expect(pii.decision.status).toBe("quarantined");
    for (const c of [infra, pii]) {
      const r = await call("POST", `/v1/intents/${c.id}/decide`, { project: c.project, verdict: "approve", reason: "Read the whole diff" }, as(key));
      expect(r.json.error?.code).toBe("needs_human");
    }
  });

  it("leaves agent_review to agent keys", async () => {
    const { key } = await mint("reviewer-bot-3");
    const c = await proposed({ "internal/x.go": "package x\n\nfunc X() {}\n" });
    expect(c.decision.status).toBe("pending_agent");
    const r = await call("POST", `/v1/intents/${c.id}/decide`, { project: c.project, verdict: "approve", reason: "Read the diff; it matches the goal" }, as(key));
    expect(r.status).toBe(200);
    expect(r.json.data.decision).toMatchObject({ status: "approved", decided_by: "reviewer-bot-3" });
  });

  it("treats keys minted before kinds existed as agent keys", async () => {
    const { key, id } = await mint("old-key");
    const stub = registry(env);
    await runInDurableObject(stub, async (_obj, state) => {
      state.storage.sql.exec("ALTER TABLE keys DROP COLUMN kind");
      // A fresh instance runs the migration, as after a deploy.
      const fresh = new Registry(state, env);
      expect(fresh.list().find((k) => k.id === id)?.kind).toBe("agent");
    });
    expect((await call("GET", "/v1/whoami", undefined, as(key))).json.data.kind).toBe("agent");
  });
});
