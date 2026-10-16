// Wayglass Worker: thin control plane. Auth, token minting, intent registry,
// overlap check, decision records, audit log, Artifacts event consumer and the
// static site. No git logic lives here (AGENTS.md section 5).

import { FORK_TOKEN_TTL_SECONDS, LAND_TOKEN_TTL_SECONDS, READ_TOKEN_TTL_SECONDS, forkWithRetry, namespaceFor, namespaces, RepoHostError, repoHost, type RepoHost, type RepoReader } from "./artifacts";
import { aiDailyLimit, Meter, metered } from "./cost";
import { FAMILIES, normalizeFamily } from "./family";
import { authenticate, type Identity, isPerson } from "./auth";
import { fail, fromResult, ok } from "./envelope";
import { handleQueue } from "./events";
import { handleBrowse, isBrowseRoute } from "./browse";
import { handlePublic, PUBLIC_PREFIX } from "./public";
import { handleHuman, HUMAN_PREFIX } from "./human";
import { isWebPath, webAccessGate } from "./access";
import { parsePatterns } from "./paths";
import { diffTrees } from "./diff";
import { ownChange, type DiffBase } from "./ownchange";
import { evaluatePolicy, PolicyEngineError } from "./policy";
import { landRefusal, type Decision, type Intent, type IntentStatus, type LandCaller, type Project, type ProjectInfo } from "./project";

// DO RPC types turn fields typed `unknown` (tags, detail) into `never`, which
// breaks narrowing on Result. The values are plain JSON, so read them as the
// method's declared return type.
type Plain<K extends keyof Project> = Project[K] extends (...a: never[]) => infer R ? Awaited<R> : never;
import { projectStub, type ProjectStub } from "./stub";
import { migrateResidency, movedError, registerProject, stubIn, unregisterProject } from "./residency";
import { subscribeRepoPushes, unsubscribeRepoPushes } from "./subscriptions";
import { ADMIN_IDENTITY, AGENT_NAME, registryStub } from "./keys";
import { hasControl } from "./text";
import { PLACEHOLDER, isFiller } from "./reason";
import { aiReviewIntent, reviewsOnPropose } from "./aireview";
import { advisesOnPropose, aiAdviseIntent } from "./aiadvice";

export { Project } from "./project";
export { Registry } from "./keys";

const PROJECT_NAME = /^[a-z0-9](?:[a-z0-9._]|-(?!-)){0,62}$/; // no "--": it separates project and intent in fork names
const INTENT_ID = /^i[0-9a-f]{10}$/;
const DECISION_ID = /^d[0-9a-f]{10}$/;
const COMMIT = /^[0-9a-f]{7,40}$/;
const MAX_TEXT = 4000;
/** Artifacts' shortest token lifetime; enough for one ls-remote. */
const TRUNK_READ_TOKEN_TTL_SECONDS = 60;

type Handler = (req: Request, env: Env, params: Record<string, string>, url: URL) => Promise<Response>;

interface Route {
  method: string;
  pattern: URLPattern;
  handler: Handler;
}

const routes: Route[] = [];
function route(method: string, pathname: string, handler: Handler) {
  routes.push({ method, pattern: new URLPattern({ pathname }), handler });
}

function randomId(prefix: string): string {
  const b = crypto.getRandomValues(new Uint8Array(5));
  return prefix + [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}

class BadRequest extends Error {}
class Forbidden extends Error {}

async function readJson(req: Request): Promise<Record<string, unknown>> {
  let body: unknown;
  try {
    body = await req.json();
  } catch {
    throw new BadRequest("request body must be JSON");
  }
  if (typeof body !== "object" || body === null || Array.isArray(body)) throw new BadRequest("request body must be a JSON object");
  return body as Record<string, unknown>;
}

/** A why: required, not a template placeholder and not a rubber stamp. The audit log keeps it forever. */
function reasonText(body: Record<string, unknown>, field = "reason"): string {
  const v = text(body, field);
  if (PLACEHOLDER.test(v)) throw new BadRequest(`${field} looks like a placeholder (${JSON.stringify(v)}); say what you checked or why`);
  if (isFiller(v)) throw new BadRequest(`${field} ${JSON.stringify(v)} is a rubber stamp, not a why; the audit log keeps it forever, so say what you checked`);
  return v;
}

function text(body: Record<string, unknown>, field: string, required = true): string {
  const v = body[field];
  if (v === undefined || v === null || v === "") {
    if (required) throw new BadRequest(`${field} is required`);
    return "";
  }
  if (typeof v !== "string") throw new BadRequest(`${field} must be a string`);
  if (v.length > MAX_TEXT) throw new BadRequest(`${field} is longer than ${MAX_TEXT} characters`);
  if (hasControl(v)) throw new BadRequest(`${field} contains control characters; send plain text (tabs and newlines are fine)`);
  return v.trim();
}

/** The caller's declared model family (body.family), normalised; null if none. Self-reported. */
function familyOf(body: Record<string, unknown>): string | null {
  const f = normalizeFamily(text(body, "family", false));
  if (f === null) throw new BadRequest(`family must be one of ${FAMILIES.join(", ")} (aliases: anthropic, codex, gpt, google, person)`);
  return f || null;
}

function paths(body: Record<string, unknown>, field: string): string[] {
  const r = parsePatterns(body[field], field);
  if (!r.ok) throw new BadRequest(r.message);
  return r.paths;
}

function commitOf(body: Record<string, unknown>): string | null {
  const c = text(body, "commit", false);
  if (!c) return null;
  if (!COMMIT.test(c)) throw new BadRequest("commit must be a hex SHA of 7 to 40 characters");
  return c;
}

/** Project comes from the X-Wayglass-Project header, ?project=, or the JSON body. */
function projectName(req: Request, url: URL, body?: Record<string, unknown>): string {
  const p = req.headers.get("x-wayglass-project") ?? url.searchParams.get("project") ?? (typeof body?.project === "string" ? body.project : null);
  if (!p) throw new BadRequest("project is required (X-Wayglass-Project header, ?project= or body.project)");
  if (!PROJECT_NAME.test(p)) throw new BadRequest("project must be lowercase letters, digits, '.', '_' or single '-' (max 63)");
  return p;
}

/** Fork repo name for an intent. */
export const forkName = (project: string, intentId: string) => `${project}--${intentId}`;

// The authenticated identity of each request in flight (set in handleApi).
const identities = new WeakMap<Request, Identity>();

function identityOf(req: Request): Identity {
  const id = identities.get(req);
  if (!id) throw new Error("request was not authenticated");
  return id;
}

/** Who is calling. An agent key's bound name wins; only the admin key may name itself (see auth.ts). */
function actor(req: Request, body?: Record<string, unknown>): string {
  const id = identities.get(req);
  if (id?.role === "agent") return id.agent;
  const a = req.headers.get("x-wayglass-agent") ?? (typeof body?.agent === "string" ? body.agent : null);
  return a && a.length <= 128 && !hasControl(a, false) ? a : "unknown";
}

/** The caller of a land route, for landRefusal: the author or the admin key may land. */
function landCaller(req: Request, body: Record<string, unknown>): LandCaller {
  return { actor: actor(req, body), admin: identityOf(req).role === "admin" };
}

/**
 * The identity a verdict is recorded under. A bound key (agent or human)
 * decides as its bound name. The admin key is one identity, ADMIN_IDENTITY,
 * whatever name it reports: that name is only kept as claimed_by, so the
 * admin key cannot stand in for two approvers of a multi_approve change by
 * naming itself twice. Two people are two keys (`wayglass keys create --human`).
 */
function approver(req: Request, body: Record<string, unknown>): { by: string; claimed_by: string | null } {
  const id = identityOf(req);
  if (id.role === "agent") return { by: id.agent, claimed_by: null };
  const claimed = text(body, "by", false) || actor(req, body);
  return { by: ADMIN_IDENTITY, claimed_by: claimed === "unknown" || claimed === ADMIN_IDENTITY ? null : claimed };
}

// Artifacts calls each request makes, and whom to charge them to; recorded
// when the request ends (handleApi), whatever the outcome (src/cost.ts).
interface Metering {
  meter: Meter;
  stub: ProjectStub;
  intentId: string | null;
}
const meterings = new WeakMap<Request, Metering>();

/** The Artifacts host for this request, metered and charged to `intentId` (null: the project). */
function hostFor(req: Request, env: Env, jurisdiction: string, stub: ProjectStub, intentId: string | null): RepoHost {
  let m = meterings.get(req);
  if (!m) {
    m = { meter: new Meter(), stub, intentId };
    meterings.set(req, m);
  }
  m.stub = stub;
  m.intentId = intentId;
  return metered(repoHost(env, jurisdiction), m.meter);
}

async function recordMetering(req: Request): Promise<void> {
  const m = meterings.get(req);
  if (!m || m.meter.total() === 0) return;
  try {
    await m.stub.recordCost(m.intentId, m.meter.ops);
  } catch (err) {
    console.warn("could not record Artifacts cost", err);
  }
}

function requireAdmin(req: Request): void {
  if (identityOf(req).role !== "admin") throw new Forbidden("only the admin key can manage keys");
}

// The public view shows a project's intents, decisions and audit trail to
// anyone (src/public.ts). An agent key must not be able to open it: a person
// decides what leaves the Worker, with the admin key.
function requirePublicAdmin(req: Request): void {
  if (identityOf(req).role !== "admin") throw new Forbidden("only the admin key can change a project's public view");
}

// POST /v1/projects {name, description?}
route("POST", "/v1/projects", async (req, env) => {
  const body = await readJson(req);
  const name = text(body, "name");
  if (!PROJECT_NAME.test(name)) throw new BadRequest("name must be lowercase letters, digits, '.', '_' or single '-' (max 63)");
  if (body.public === true) requirePublicAdmin(req);
  // The jurisdiction picks the namespace for good: Artifacts cannot move a repo.
  const jurisdiction = text(body, "jurisdiction", false).toLowerCase() || (env.ARTIFACTS_JURISDICTION ?? "");
  if (!namespaces(env).has(jurisdiction)) {
    throw new BadRequest(`jurisdiction "${jurisdiction}" is not available here (have: ${[...namespaces(env).keys()].join(", ")})`);
  }
  const ns = namespaceFor(env, jurisdiction);
  // Wherever a project of this name already lives (the directory, or the
  // unpinned object for one from before the directory), it is not replaced.
  if (await (await projectStub(env, name)).info()) return fail("conflict", `project ${name} already exists`);
  // The state goes into the object pinned to the jurisdiction (src/residency.ts),
  // and the directory says so before any state exists, so no request can be
  // routed to an unpinned object meanwhile.
  const stub = stubIn(env, name, jurisdiction);
  if (await stub.info()) return fail("conflict", `project ${name} already exists`);
  await registerProject(env, name, jurisdiction);
  const created = await hostFor(req, env, jurisdiction, stub, null).createTrunk(name, text(body, "description", false) || `Wayglass trunk for ${name}`);
  const r = await stub.init({ name, trunk_repo: created.repo.name, trunk_remote: created.repo.remote, jurisdiction, actor: actor(req, body), default_branch: created.repo.defaultBranch });
  if (!r.ok) return fromResult(r);
  // Trunk pushes must reach the queue consumer so pushes that skip `land` show
  // up in the audit log. A failure here is logged, not fatal: the trunk exists.
  const sub = await subscribeRepoPushes(env, ns.name, created.repo.name);
  await stub.recordExternalEvent(
    sub.subscribed ? "trunk.events_subscribed" : "trunk.events_unsubscribed",
    "wayglass",
    null,
    sub.subscribed ? { repo: created.repo.name, subscription_id: sub.id } : { repo: created.repo.name, reason: sub.reason, detail: sub.detail },
  );
  if (!sub.subscribed) console.warn("trunk push events not subscribed", created.repo.name, sub.reason, sub.detail);
  // Public read view (src/public.ts) is opt-in and off unless asked for.
  // Opening it is the admin's call (checked before anything is created).
  if (body.public === true) await stub.setPublic(true, actor(req, body));
  return ok(
    {
      project: r.data,
      public: body.public === true,
      trunk: created.repo,
      token: { value: created.token.value, scope: created.token.scope, expires_at: created.token.expiresAt },
      events: sub.subscribed
        ? { subscribed: true, subscription_id: sub.id }
        : { subscribed: false, reason: sub.reason, detail: sub.detail },
    },
    201,
  );
});

// PATCH /v1/projects/:name {public: boolean}  (admin only)
// Opts a project in or out of the unauthenticated read view (src/public.ts).
// The Worker var PUBLIC_PROJECTS must also list it.
route("PATCH", "/v1/projects/:name", async (req, env, p) => {
  requirePublicAdmin(req);
  const name = p.name!;
  if (!PROJECT_NAME.test(name)) throw new BadRequest("invalid project name");
  const body = await readJson(req);
  if (typeof body.public !== "boolean") throw new BadRequest("public must be true or false");
  return fromResult(await (await projectStub(env, name)).setPublic(body.public, actor(req, body)));
});

// Only throwaway test projects can be deleted. A delete erases the project's
// append-only audit log, which is the record the product exists to keep, so a
// real project (demo-*, eu-demo-*, wayglass, anyone's own) must never be one
// mistyped name or one leaked admin key away from losing it. Throwaway
// projects are named demo-test-* (US) and eu-demo-test-* (EU) by convention.
// The rule is code, not a Worker var, so widening it takes a reviewed commit.
const DELETABLE_PREFIXES = ["demo-test-", "eu-demo-test-"] as const;

export function isDeletableProject(name: string): boolean {
  return DELETABLE_PREFIXES.some((p) => name.startsWith(p) && name.length > p.length);
}

// DELETE /v1/projects/:name {confirm: "<name>"}  (admin key only)
//
// Revokes live tokens, deletes every fork the project recorded and its trunk
// (in the project's own namespace), removes trunk push subscriptions, then
// wipes the project's Durable Object. Idempotent: repos already gone count as
// gone, and a project with no state left answers ok with existed: false. If a
// repo cannot be deleted the state is kept, so a retry still knows the repos.
// The project's own audit log goes with it, so the deletion is recorded as a
// Worker log line (`wayglass.project.deleted`) instead.
route("DELETE", "/v1/projects/:name", async (req, env, p) => {
  if (identityOf(req).role !== "admin") throw new Forbidden("only the admin key can delete projects");
  const name = p.name!;
  if (!PROJECT_NAME.test(name)) throw new BadRequest("invalid project name");
  if (!isDeletableProject(name)) {
    throw new Forbidden(`only throwaway projects can be deleted (names starting with ${DELETABLE_PREFIXES.join(" or ")}); ${name} keeps its repos and audit log`);
  }
  const body = await readJson(req);
  if (body.confirm !== name) throw new BadRequest(`confirm must repeat the project name: {"confirm": "${name}"}`);
  const who = actor(req, body);
  const stub = await projectStub(env, name);
  const plan = (await stub.deletionPlan()) as Plain<"deletionPlan">;
  if (!plan.ok) {
    if (plan.code !== "not_found") return fromResult(plan);
    console.log(JSON.stringify({ event: "wayglass.project.delete_noop", project: name, actor: who, at: new Date().toISOString() }));
    return ok({ project: name, existed: false, repos_deleted: [], repos_already_gone: [], tokens_revoked: 0, subscriptions_removed: 0, state_wiped: false });
  }
  const { project: info, forks, tokens, subscription_ids, counts } = plan.data;
  // Unmetered: the cost counters live in the state this wipes.
  const host = repoHost(env, info.jurisdiction);

  let revoked = 0;
  for (const t of tokens) {
    try {
      if (await host.revokeToken(t.repo, t.id)) revoked++;
    } catch {
      // The repo delete below removes its tokens as well.
    }
  }
  const deleted: string[] = [];
  const gone: string[] = [];
  const failed: string[] = [];
  // Only this project's repos: its trunk and forks named "<name>--...". The
  // list comes from the project's own records, but a delete is the one call
  // that cannot be undone, so a name from anywhere else is refused here.
  const own = (repo: string) => repo === name || repo.startsWith(`${name}--`);
  const foreign = [...forks, info.trunk_repo].filter((r) => !own(r));
  if (foreign.length > 0) {
    console.error(JSON.stringify({ event: "wayglass.project.delete_refused", project: name, actor: who, repos: foreign }));
    return fail("internal", `project ${name} records repos it does not own (${foreign.join(", ")}); nothing was deleted`);
  }
  for (const repo of [...forks, info.trunk_repo]) {
    try {
      (await host.deleteRepo(repo)) ? deleted.push(repo) : gone.push(repo);
    } catch (err) {
      console.error("project delete: could not delete repo", repo, err);
      failed.push(repo);
    }
  }
  let unsubscribed = 0;
  const subscriptionsLeft: string[] = [];
  for (const id of subscription_ids) {
    const u = await unsubscribeRepoPushes(env, id);
    if (u.removed) unsubscribed++;
    else {
      console.warn("project delete: could not remove push subscription", id, u.detail);
      subscriptionsLeft.push(id);
    }
  }
  const record = {
    project: name,
    actor: who,
    jurisdiction: info.jurisdiction,
    namespace: namespaceFor(env, info.jurisdiction).name,
    repos_deleted: deleted,
    repos_already_gone: gone,
    tokens_revoked: revoked,
    subscriptions_removed: unsubscribed,
    subscriptions_left: subscriptionsLeft,
    counts,
    at: new Date().toISOString(),
  };
  if (failed.length > 0) {
    await stub.recordExternalEvent("project.delete_failed", who, null, { repos_failed: failed, repos_deleted: deleted, repos_already_gone: gone });
    console.error(JSON.stringify({ event: "wayglass.project.delete_failed", ...record, repos_failed: failed }));
    return fail("upstream_error", `could not delete ${failed.join(", ")}; project state kept so the delete can be retried`);
  }
  await stub.wipe();
  await unregisterProject(env, name);
  console.log(JSON.stringify({ event: "wayglass.project.deleted", ...record }));
  return ok({ ...record, existed: true, state_wiped: true });
});

// POST /v1/projects/:name/migrate-residency {}  (admin key only)
//
// Moves a project created before Durable Objects were pinned per jurisdiction
// (Oct 14) from the unpinned object into the object pinned to its
// jurisdiction: every table copied row for row, counts and a digest verified
// on both sides, then the unpinned object wiped and left as a tombstone that
// refuses every call, a `project.residency_migrated` audit event appended,
// and the directory updated (src/residency.ts). Safe to rerun: a finished
// move answers migrated: false. Until it runs, the project is refused with
// `residency_pending` rather than served from the wrong place.
route("POST", "/v1/projects/:name/migrate-residency", async (req, env, p) => {
  if (identityOf(req).role !== "admin") throw new Forbidden("only the admin key can migrate a project's residency");
  const name = p.name!;
  if (!PROJECT_NAME.test(name)) throw new BadRequest("invalid project name");
  const body = await readJson(req);
  const report = await migrateResidency(env, name, actor(req, body));
  console.log(JSON.stringify({ event: "wayglass.project.residency_migrated", ...report, actor: actor(req, body), at: new Date().toISOString() }));
  return ok(report);
});

// POST /v1/projects/:name/trunk/read-token {}
// A read-only trunk token for TRUNK_READ_TOKEN_TTL_SECONDS: lets `wayglass
// doctor` (or anyone with a key) check the trunk is reachable over git
// without ever holding a write token for it. Audited like every other token.
route("POST", "/v1/projects/:name/trunk/read-token", async (req, env, p) => {
  const name = p.name!;
  if (!PROJECT_NAME.test(name)) throw new BadRequest("invalid project name");
  const stub = await projectStub(env, name);
  const info = (await stub.info()) as ProjectInfo | null;
  if (!info) return fail("not_found", `no project ${name}`);
  const token = await hostFor(req, env, info.jurisdiction, stub, null).mintToken(info.trunk_repo, "read", TRUNK_READ_TOKEN_TTL_SECONDS);
  await stub.recordExternalEvent("trunk.read_token", actor(req), null, { repo: info.trunk_repo, expires_at: token.expiresAt });
  return ok(
    {
      trunk: { name: info.trunk_repo, remote: info.trunk_remote },
      token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
    },
    201,
  );
});

// POST /v1/intents {project, goal, reason, paths[], agent?, on?}
//
// With `on` (an unlanded intent id) the new intent is stacked: its fork is a
// fork of the parent's fork, its diff base is the parent's fork head, so its
// classification covers only its own change, and it cannot land before the
// parent has.
route("POST", "/v1/intents", async (req, env, _p, url) => {
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const goal = text(body, "goal");
  const reason = reasonText(body);
  const declared = paths(body, "paths");
  if (declared.length === 0) throw new BadRequest("paths must list at least one expected path or glob");
  const on = text(body, "on", false);
  if (on && !INTENT_ID.test(on)) throw new BadRequest("on must be an intent id");
  const family = familyOf(body);

  const stub = await projectStub(env, project);
  const info = await stub.info();
  if (!info) return fail("not_found", `project ${project} not found`);

  let parent: Intent | null = null;
  if (on) {
    const pc = (await stub.intentContext(on)) as Plain<"intentContext">;
    if (!pc.ok) return fromResult(pc);
    parent = pc.data.intent;
    if (parent.status !== "open" && parent.status !== "proposed") {
      return fail("conflict", `intent ${on} is ${parent.status}; stack only on an unlanded intent (a landed one is already on trunk)`);
    }
  }

  const id = randomId("i");
  const host = hostFor(req, env, info.jurisdiction, stub, id);
  const placed = await forkIntent(host, info, project, id, parent, goal.slice(0, 200), "refuse");
  if (placed instanceof Response) return placed;
  const { fork, token } = placed;

  const r = await stub.createIntent({
    id,
    goal,
    reason,
    agent: actor(req, body),
    declared_paths: declared,
    fork_repo: fork.name,
    fork_remote: fork.remote,
    base_commit: placed.base_commit,
    parent_id: placed.parent_id,
    trunk_base: placed.trunk_base,
    fork_token: { id: token.id, expires_at: token.expiresAt },
    agent_family: family,
  });
  if (!r.ok) return dropUnrecordedFork(host, fork.name, r);
  return ok(
    {
      intent: r.data.intent,
      fork,
      token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
      overlaps: r.data.overlaps,
    },
    201,
  );
});

/**
 * The project refused to record an intent after its fork was made (the
 * parent landed or was abandoned in between): give the fork back, which
 * also drops the write token minted for it, and answer with the refusal.
 */
async function dropUnrecordedFork(host: RepoHost, fork: string, refusal: { ok: false; code: Parameters<typeof fail>[0]; message: string }): Promise<Response> {
  try {
    await host.deleteRepo(fork);
  } catch (err) {
    console.warn("could not delete the unrecorded fork", fork, err);
  }
  return fromResult(refusal);
}

/**
 * Creates a new intent's fork repo and its write token, from trunk or stacked
 * on `parent`. The one stacking path: `intent declare --on` and `session
 * fork` both come through here.
 *
 * Stacked, the fork is a fork of the parent's fork, its diff base is the
 * parent's fork head (read before forking, so if the source moves in between
 * the base is older than the fork point and the diff can only grow, never
 * hide), and policy is read from the trunk commit the stack started on.
 *
 * `intent restack` comes through here too, with a fresh fork name.
 *
 * When the parent has pushed nothing yet there is nothing to stack on:
 * "refuse" fails (declare --on), "alongside" forks the parent's fork anyway
 * and places the new intent where the parent sits, with the parent's base
 * (session fork of a session shared before any push).
 */
async function forkIntent(
  host: RepoHost,
  info: ProjectInfo,
  project: string,
  id: string,
  parent: Intent | null,
  description: string,
  empty: "refuse" | "alongside",
  name = forkName(project, id),
): Promise<
  | Response
  | { fork: Awaited<ReturnType<RepoHost["fork"]>>; token: Awaited<ReturnType<RepoHost["mintToken"]>>; base_commit: string | null; parent_id: string | null; trunk_base: string | null }
> {
  const source = parent ? parent.fork_repo : info.trunk_repo;
  let base: string | null;
  {
    using src = await host.reader(source);
    base = await src.head();
  }
  let placement: { base_commit: string | null; parent_id: string | null; trunk_base: string | null };
  if (!parent) {
    placement = { base_commit: base, parent_id: null, trunk_base: base };
  } else if (base && base !== parent.base_commit) {
    placement = { base_commit: base, parent_id: parent.id, trunk_base: parent.trunk_base };
  } else if (empty === "alongside") {
    placement = { base_commit: parent.base_commit, parent_id: parent.parent_id, trunk_base: parent.trunk_base };
  } else {
    return fail("conflict", `intent ${parent.id} has nothing pushed yet; push it before stacking on it`);
  }
  const fork = await forkWithRetry(host, source, name, description);
  // Write token scoped to this fork only, short-lived. Never stored by the Worker.
  const token = await host.mintToken(fork.name, "write", FORK_TOKEN_TTL_SECONDS);
  return { fork, token, ...placement };
}

// POST /v1/intents/:id/abandon {project, reason}
//
// The author (bound key name or self-named agent) or the admin gives up on an
// intent. Its fork repo is kept; its live fork tokens are revoked and no new
// ones are minted. Intents stacked on it are re-stacked onto its base and
// carry its commits from then on (see Project.abandonIntent).
route("POST", "/v1/intents/:id/abandon", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const reason = reasonText(body);
  const who = actor(req, body);
  const stub = await projectStub(env, project);
  // Look the project up first: its jurisdiction picks the namespace the
  // revokes go to, and an unknown project must not fall back to the main one.
  const info = await stub.info();
  if (!info) return fail("not_found", `project ${project} not found`);
  const r = (await stub.abandonIntent(id, { actor: who, admin: identityOf(req).role === "admin", reason })) as Plain<"abandonIntent">;
  if (!r.ok) return fromResult(r);
  const { intent, restacked, fork_token_ids } = r.data;
  const host = hostFor(req, env, info.jurisdiction, stub, id);
  const revoked: string[] = [];
  for (const t of fork_token_ids) {
    try {
      await host.revokeToken(intent.fork_repo, t);
      revoked.push(t);
    } catch (err) {
      console.warn("could not revoke fork token on abandon", t, err);
    }
  }
  if (revoked.length > 0) await stub.markTokensRevoked(revoked);
  return ok({ intent, restacked, tokens_revoked: revoked.length, fork_kept: true });
});

// POST /v1/intents/:id/restack {project, on, reason}
//
// Re-plans an overlap as a stack in one step (wayglass intent restack): the
// author or the admin moves an open or proposed intent onto another unlanded
// intent `on`. Like declare --on, the Worker forks `on`'s fork (a new repo,
// `<project>--<id>--r<n>`) and the intent diffs from `on`'s head from then on;
// it lands only after `on`. Workers cannot run git, so the client merges the
// intent's own commits into the new fork with the write token returned here.
// The old fork is kept and its live tokens revoked. A decision made before
// no longer covers the change: land refuses it until the intent proposes
// again. Refuses cycles, landed or abandoned parents and filler reasons.
route("POST", "/v1/intents/:id/restack", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const on = text(body, "on");
  if (!INTENT_ID.test(on)) throw new BadRequest("on must be an intent id");
  const reason = reasonText(body);
  const caller = { actor: actor(req, body), admin: identityOf(req).role === "admin" };
  const stub = await projectStub(env, project);
  const check = (await stub.restackCheck(id, on, caller)) as Plain<"restackCheck">;
  if (!check.ok) return fromResult(check);
  const { project: info, intent, parent, retired } = check.data;
  const host = hostFor(req, env, info.jurisdiction, stub, id);
  if (intent.parent_id === parent.id) {
    using src = await host.reader(parent.fork_repo);
    if ((await src.head()) === intent.base_commit) {
      return fail("conflict", `intent ${id} is already stacked on ${parent.id} at its head; nothing to restack`);
    }
  }
  const name = `${forkName(project, id)}--r${retired + 1}`;
  const placed = await forkIntent(host, info, project, id, parent, `Restack of ${id} on ${parent.id}`.slice(0, 200), "refuse", name);
  if (placed instanceof Response) return placed;
  const { fork, token } = placed;
  const r = (await stub.restackIntent(id, {
    on,
    ...caller,
    reason,
    fork_repo: fork.name,
    fork_remote: fork.remote,
    base_commit: placed.base_commit,
    parent_id: placed.parent_id,
    trunk_base: placed.trunk_base,
    fork_token: { id: token.id, expires_at: token.expiresAt },
  })) as Plain<"restackIntent">;
  if (!r.ok) {
    // Something changed between the check and the record: give the new fork back.
    try {
      await host.deleteRepo(fork.name);
    } catch (err) {
      console.warn("could not delete the unused restack fork", fork.name, err);
    }
    return fromResult(r);
  }
  const { previous, old_token_ids } = r.data;
  const revoked: string[] = [];
  for (const t of old_token_ids) {
    try {
      await host.revokeToken(previous.fork_repo, t);
      revoked.push(t);
    } catch (err) {
      console.warn("could not revoke old fork token on restack", t, err);
    }
  }
  if (revoked.length > 0) await stub.markTokensRevoked(revoked);
  return ok(
    {
      intent: r.data.intent,
      fork,
      token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
      previous: { ...previous, fork_kept: true, tokens_revoked: revoked.length },
      superseded_decision: r.data.superseded_decision,
      overlaps: r.data.overlaps,
    },
    201,
  );
});

// GET /v1/intents?project=&status=
route("GET", "/v1/intents", async (req, env, _p, url) => {
  const project = projectName(req, url);
  const status = url.searchParams.get("status");
  if (status && !["open", "proposed", "landed", "abandoned"].includes(status)) throw new BadRequest("unknown status");
  return fromResult(await (await projectStub(env, project)).listIntents((status ?? undefined) as IntentStatus | undefined));
});

// POST /v1/intents/:id/push {project, paths[], commit?}
route("POST", "/v1/intents/:id/push", async (req, env, p, url) => {
  const id = p.id!;
  if (!INTENT_ID.test(id)) throw new BadRequest("invalid intent id");
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const changed = paths(body, "paths");
  return fromResult(
    await (await projectStub(env, project)).recordPush(id, { paths: changed, commit: commitOf(body), actor: actor(req, body), admin: identityOf(req).role === "admin" }),
  );
});

const FULL_COMMIT = /^[0-9a-f]{40}$/;
const POLICY_PATH = ".wayglass/policy.yaml";
const MAX_POLICY_BYTES = 256 * 1024;

/** "sha256:<hex>" of the policy file's exact bytes, or "absent" when the commit has none. */
async function policyDigest(bytes: Uint8Array | null): Promise<string> {
  if (!bytes) return "absent";
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  return "sha256:" + [...d].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** The trunk policy at `commit` (null: an empty trunk, so no policy). Read errors propagate: callers fail closed. */
async function trunkPolicy(trunk: RepoReader, commit: string | null): Promise<{ commit: string | null; bytes: Uint8Array | null; sha256: string }> {
  const bytes = commit ? await trunk.readFile(commit, POLICY_PATH) : null;
  return { commit, bytes, sha256: await policyDigest(bytes) };
}

/**
 * The digest of the policy a decision was made under. Decisions from before
 * Oct 14 did not record it; they were evaluated with the trunk policy at the
 * intent's trunk_base, so read that. Null when it cannot be determined.
 */
async function decidedPolicy(trunk: RepoReader, intent: Intent, decision: Decision): Promise<string | null> {
  if (decision.policy_sha256) return decision.policy_sha256;
  const at = intent.trunk_base ?? (intent.parent_id ? undefined : intent.base_commit);
  if (at === undefined) return null;
  try {
    return (await trunkPolicy(trunk, at)).sha256;
  } catch (err) {
    console.error("could not read the policy a legacy decision was made under", decision.id, err);
    return null;
  }
}

/** What the client says its change is. Kept for comparison; never decides anything. */
function clientPreview(body: Record<string, unknown>): Record<string, unknown> | null {
  const preview: Record<string, unknown> = {};
  for (const k of ["tags", "matched_rule", "action", "reason", "approvals_needed"]) {
    if (body[k] !== undefined) preview[k] = body[k];
  }
  if (Object.keys(preview).length === 0) return null;
  const s = JSON.stringify(preview);
  if (s.length > 16 * 1024) throw new BadRequest("client preview (tags, action, ...) is larger than 16 KiB");
  return preview;
}

function intentId(p: Record<string, string>): string {
  const id = p.id!;
  if (!INTENT_ID.test(id)) throw new BadRequest("invalid intent id");
  return id;
}

// POST /v1/intents/:id/propose {project, commit?, tags?, matched_rule?, action?, reason?}
//
// The Worker derives the decision itself (ARCHITECTURE D3): it reads the
// fork head and the trunk base commit from Artifacts, diffs them, and runs
// the Go policy evaluator (Wasm) with the policy file from the trunk's
// current head. Anything the client sends about tags or actions is stored as
// client_preview only. `commit`, if given, must be the fork head.
route("POST", "/v1/intents/:id/propose", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const claimed = commitOf(body);
  const preview = clientPreview(body);
  const who = actor(req, body);

  const stub = await projectStub(env, project);
  const ctx = (await stub.intentContext(id)) as Plain<"intentContext">;
  if (!ctx.ok) return fromResult(ctx);
  const { project: info, intent } = ctx.data;
  if (intent.status !== "open" && intent.status !== "proposed") return fail("conflict", `intent ${id} is ${intent.status}`);

  const host = hostFor(req, env, info.jurisdiction, stub, id);
  using fork = await host.reader(intent.fork_repo);
  using trunk = await host.reader(info.trunk_repo);

  const head = await fork.head();
  if (!head) return fail("conflict", `fork ${intent.fork_repo} has no commits; push first`);
  if (claimed && !head.startsWith(claimed)) return fail("conflict", `fork head is ${head}, not ${claimed}; push before proposing`);
  if (head === intent.base_commit) return fail("conflict", "the fork has no changes against its base commit");
  const headCommit = await fork.readCommit(head);
  if (!headCommit) throw new RepoHostError("NOT_FOUND", `fork head ${head} could not be read`);

  // Policy comes from the trunk, never from the fork: an agent must not be
  // able to edit the rules its own change is judged by. It is the trunk's
  // current policy, not the one at the intent's base, so a change is always
  // judged by the rules it will land under; land refuses with policy_changed
  // when the trunk policy moves after the decision, and proposing again
  // re-decides under the new rules. The decision records which policy it used.
  let baseTree: string | null = null;
  if (intent.base_commit) {
    const baseCommit = intent.parent_id ? await fork.readCommit(intent.base_commit) : await trunk.readCommit(intent.base_commit);
    if (!baseCommit) throw new RepoHostError("NOT_FOUND", `base commit ${intent.base_commit} is missing from ${intent.parent_id ? "the fork" : "trunk"}`);
    baseTree = baseCommit.tree;
  }
  const trunkHead = await trunk.head();
  const policy = await trunkPolicy(trunk, trunkHead);
  if (policy.bytes && policy.bytes.length > MAX_POLICY_BYTES) throw new BadRequest(`${POLICY_PATH} on trunk is larger than ${MAX_POLICY_BYTES} bytes`);
  // A file that exists but is empty or undecodable must not read as "no policy".
  const policyYaml = policy.bytes ? new TextDecoder().decode(policy.bytes) || "# empty\n" : "";

  // Classify only the intent's own change: what landing it would add to the
  // trunk, not trunk files a sync merged in (see ownchange.ts). A stacked
  // intent keeps the diff from its parent's fork, as before.
  const forkBaseDiff = () => diffTrees({ repo: intent.parent_id ? fork : trunk, tree: baseTree }, { repo: fork, tree: headCommit.tree });
  const own = intent.parent_id
    ? { diff: await forkBaseDiff(), base: { method: "fork_base", commits: intent.base_commit ? [intent.base_commit] : [], fallback: "stacked intent" } satisfies DiffBase }
    : await ownChange(fork, trunk, { commit: head, tree: headCommit.tree }, trunkHead, intent.base_commit, forkBaseDiff);
  const diff = own.diff;
  if (diff.changes.length === 0) {
    return fail("conflict", own.base.method === "fork_base" ? "the fork has no changes against its base commit" : "the fork adds nothing to the trunk; there is nothing to land");
  }
  const decision = await evaluatePolicy({
    policy_yaml: policyYaml,
    changes: diff.changes,
    // The project's namespace decides, never the client: tags come from where the data lives.
    jurisdiction: namespaceFor(env, info.jurisdiction).jurisdiction,
    truncated: diff.truncated,
  });

  const r = (await stub.propose(id, {
    decision_id: randomId("d"),
    head_commit: head,
    fork_repo: intent.fork_repo,
    base_commit: intent.base_commit,
    decision,
    policy_commit: policy.commit,
    policy_sha256: policy.sha256,
    paths: diff.paths,
    files: diff.changes.map((c) => ({ path: c.path, added: c.added_lines.length, deleted: c.removed_count })),
    truncated_reasons: diff.truncated_reasons,
    diff_base: own.base,
    client_preview: preview,
    actor: who,
  })) as Plain<"propose">;
  if (!r.ok) return fromResult(r);
  // AI_REVIEW=auto, or a rule that said `reviewer: ai`: the AI reviewer
  // takes the agent_review change at once (src/aireview.ts). Its outcome
  // rides along as ai_review; a refusal or an unavailable model leaves the
  // decision waiting for a reviewer agent.
  if (reviewsOnPropose(env, r.data.decision)) {
    const ctx2 = (await stub.intentContext(id)) as Plain<"intentContext">;
    const reviewed = ctx2.ok ? await aiReviewIntent(env, stub, host, ctx2.data, who) : ctx2;
    return ok({ ...r.data, ...(reviewed.ok ? { decision: reviewed.data.decision, ai_review: reviewed.data.review } : { ai_review: { error: { code: reviewed.code, message: reviewed.message } } }) }, 201);
  }
  // AI_REVIEW=auto, or a rule that said `ai: { advise: true }`: the change went
  // to a person, and the AI writes them advice (src/aiadvice.ts). It rides
  // along as ai_advice and changes nothing about the decision.
  if (advisesOnPropose(env, r.data.decision)) {
    const ctx2 = (await stub.intentContext(id)) as Plain<"intentContext">;
    const advised = ctx2.ok ? await aiAdviseIntent(env, stub, host, ctx2.data, who) : ctx2;
    return ok({ ...r.data, ...(advised.ok ? { decision: advised.data.decision, ai_advice: advised.data.advice } : { ai_advice: { error: { code: advised.code, message: advised.message } } }) }, 201);
  }
  return fromResult(r, 201);
});

// POST /v1/intents/:id/ai-advice {project}
//
// Asks the AI for advice on the intent's latest decision when that decision
// waits for a person (src/aiadvice.ts): what the change does against its
// goal, risks, questions. Stored on the decision and audited; never a
// verdict. With AI_REVIEW=off or no AI binding: ai_unavailable (503).
route("POST", "/v1/intents/:id/ai-advice", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const stub = await projectStub(env, project);
  const ctx = (await stub.intentContext(id)) as Plain<"intentContext">;
  if (!ctx.ok) return fromResult(ctx);
  const host = hostFor(req, env, ctx.data.project.jurisdiction, stub, id);
  return fromResult(await aiAdviseIntent(env, stub, host, ctx.data, actor(req, body)));
});

// POST /v1/intents/:id/ai-review {project}
//
// Asks the Workers AI reviewer to decide the intent's latest agent_review
// decision (src/aireview.ts). Any key may ask; the model decides as
// `workers-ai:<model>` under the same rules as a reviewer agent, and can
// only approve or hand the change to a person. With AI_REVIEW=off or no AI
// binding: ai_unavailable (503).
route("POST", "/v1/intents/:id/ai-review", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const stub = await projectStub(env, project);
  const ctx = (await stub.intentContext(id)) as Plain<"intentContext">;
  if (!ctx.ok) return fromResult(ctx);
  const host = hostFor(req, env, ctx.data.project.jurisdiction, stub, id);
  return fromResult(await aiReviewIntent(env, stub, host, ctx.data, actor(req, body)));
});

// POST /v1/intents/:id/land {project}
//
// Hands out a trunk write token (LAND_TOKEN_TTL_SECONDS) once the intent's
// latest server decision is approved, and only while the fork is still at the
// commit that was decided. The token is returned here once and never stored.
// Only the intent's author or the admin key may land it: land, landed and
// land/abort all refuse anyone else with not_author (403).
route("POST", "/v1/intents/:id/land", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const who = actor(req, body);
  const stub = await projectStub(env, project);
  // A trunk token an earlier land could not revoke keeps the slot busy; try
  // the revocation again first, so an Artifacts hiccup costs a retry, not
  // the token's whole lifetime.
  const lingering = (await stub.unrevokedLandTokens()) as Plain<"unrevokedLandTokens">;
  if (lingering.length > 0) {
    const ctx = (await stub.intentContext(id)) as Plain<"intentContext">;
    if (!ctx.ok) return fromResult(ctx);
    const host = hostFor(req, env, ctx.data.project.jurisdiction, stub, id);
    for (const l of lingering) {
      try {
        await host.revokeToken(ctx.data.project.trunk_repo, l.token_id!);
        await stub.landTokenRevoked(l.id, who);
      } catch (err) {
        console.error("could not revoke lingering land token", l.id, err);
      }
    }
  }
  const landId = randomId("l");
  const begun = (await stub.beginLand(id, landId, landCaller(req, body))) as Plain<"beginLand">;
  if (!begun.ok) return fromResult(begun);
  const { project: info, intent, decision } = begun.data;
  const host = hostFor(req, env, info.jurisdiction, stub, id);
  try {
    {
      using fork = await host.reader(intent.fork_repo);
      const head = await fork.head();
      if (head !== decision.commit_sha) {
        await stub.cancelLand(landId, "fork moved after the decision", who);
        return fail("conflict", `fork head is ${head}, but decision ${decision.id} covers ${decision.commit_sha}; propose again`);
      }
    }
    // What trunk held when the token was issued: the landed commit must be
    // the approved change on top of exactly this, so nothing slips in between.
    let trunkBefore: string | null;
    {
      using trunk = await host.reader(info.trunk_repo);
      trunkBefore = await trunk.head();
      // The decision holds only under the policy it was made under. If the
      // trunk's policy changed since (or that cannot be told), refuse before
      // any token exists; proposing again decides under the trunk's policy.
      const current = (await trunkPolicy(trunk, trunkBefore)).sha256;
      const decided = await decidedPolicy(trunk, intent, decision);
      if (decided !== current) {
        await stub.cancelLand(landId, decided ? "trunk policy changed after the decision" : "policy the decision was made under is unknown", who);
        return fail(
          "policy_changed",
          decided
            ? `the trunk's ${POLICY_PATH} changed after decision ${decision.id} was made, so it no longer covers the change; nothing was issued. Next: in intent ${id}'s clone run wayglass sync ${id} (merges the trunk in), then wayglass push and wayglass propose again so the change is decided under the current policy`
            : `could not tell which policy decision ${decision.id} was made under, so it cannot be checked against the trunk's ${POLICY_PATH}; nothing was issued. Next: in intent ${id}'s clone run wayglass sync ${id} (merges the trunk in), then wayglass push and wayglass propose again`,
        );
      }
    }
    const token = await host.mintToken(info.trunk_repo, "write", LAND_TOKEN_TTL_SECONDS);
    const issued = await stub.issueLand(landId, { id: token.id, expires_at: token.expiresAt }, trunkBefore, who);
    if (!issued.ok) {
      await host.revokeToken(info.trunk_repo, token.id);
      return fromResult(issued);
    }
    return ok(
      {
        land: issued.data.land,
        decision,
        trunk: { repo: info.trunk_repo, remote: info.trunk_remote },
        fork: { repo: intent.fork_repo, remote: intent.fork_remote, commit: decision.commit_sha },
        trunk_before: trunkBefore,
        token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
      },
      201,
    );
  } catch (err) {
    await stub.cancelLand(landId, "error while issuing the trunk token", who);
    throw err;
  }
});

// POST /v1/intents/:id/landed {project, commit}
//
// Closes a land: revokes the trunk token, then checks that trunk now points
// at `commit` and that it is the approved change (the fork head itself, or a
// merge of it that touches only the decided paths). Records the commit so
// /v1/why answers for trunk commits. Artifacts has no ref protection, so a
// mismatch is detected and logged, not prevented.
route("POST", "/v1/intents/:id/landed", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const commit = text(body, "commit").toLowerCase();
  if (!FULL_COMMIT.test(commit)) throw new BadRequest("commit must be a full 40-character hex SHA");
  const who = actor(req, body);
  const stub = await projectStub(env, project);
  const ctx = (await stub.landContext(id)) as Plain<"landContext">;
  if (!ctx.ok) return fromResult(ctx);
  const { project: info, intent, decision, land } = ctx.data;
  const refused = landRefusal(intent, landCaller(req, body));
  if (refused) return fromResult(refused);
  const host = hostFor(req, env, info.jurisdiction, stub, id);

  let revoked = false;
  try {
    // false means already gone (expired); either way it can no longer push.
    await host.revokeToken(info.trunk_repo, land.token_id!);
    revoked = true;
  } catch (err) {
    console.error("could not revoke land token", land.id, err);
  }

  const truncated = Array.isArray(decision.detail?.truncated) && decision.detail.truncated.length > 0;
  const problems = await verifyLanded(host, info.trunk_repo, commit, decision.commit_sha!, decision.paths, land.trunk_before, truncated, intent.trunk_base);
  const r = (await stub.finishLand(land.id, { commit, ok: problems.length === 0, problems, token_revoked: revoked, actor: who })) as Plain<"finishLand">;
  if (!r.ok) return fromResult(r);
  if (problems.length > 0) return fail("conflict", `trunk commit ${commit} is not the approved change: ${problems.join("; ")}`);
  return ok({ ...r.data, token_revoked: revoked });
});

// POST /v1/intents/:id/land/abort {project, reason}
//
// The client could not finish a land (merge conflict, push refused, paths
// outside the decision): revoke the trunk token now and free the project's
// land slot, instead of making every other agent wait for the token to expire.
route("POST", "/v1/intents/:id/land/abort", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const reason = text(body, "reason").slice(0, 500);
  const who = actor(req, body);
  const stub = await projectStub(env, project);
  const ctx = (await stub.landContext(id)) as Plain<"landContext">;
  if (!ctx.ok) return fromResult(ctx);
  const { project: info, intent, land } = ctx.data;
  const refused = landRefusal(intent, landCaller(req, body));
  if (refused) return fromResult(refused);
  let revoked = false;
  try {
    await hostFor(req, env, info.jurisdiction, stub, id).revokeToken(info.trunk_repo, land.token_id!);
    revoked = true;
  } catch (err) {
    console.error("could not revoke land token on abort", land.id, err);
  }
  const r = (await stub.abortLand(id, reason, revoked, who)) as Plain<"abortLand">;
  if (!r.ok) return fromResult(r);
  return ok({ land: r.data.land, token_revoked: revoked });
});

/** How far back a fast-forward is searched for the trunk it started from. */
const MAX_FF_WALK = 200;

/**
 * Checks that trunk now holds exactly the approved change on top of what it
 * held when the land began (`trunkBefore`). Two shapes pass:
 *  - fast-forward: trunk is the approved fork head, that head descends
 *    from trunkBefore (so nothing on trunk was dropped), and it changes only
 *    paths the decision covered;
 *  - merge: a two-parent commit whose first parent is trunkBefore and second
 *    is the approved head, changing only paths the decision covered, and in
 *    each of them holding the approved head's version of the file. A merge
 *    may differ from that only where the trunk also edited the file since
 *    the intent's trunk base (`trunkBase`; both sides edited it without
 *    conflict, or a resolution). Without that rule a trunk token could land
 *    any content in an approved path as a hand-made "merge"; a path outside
 *    the decision means the change is no longer what was approved.
 */
async function verifyLanded(
  host: RepoHost,
  trunkRepo: string,
  commit: string,
  approvedHead: string,
  approvedPaths: string[],
  trunkBefore: string | null,
  decisionTruncated = false,
  trunkBase: string | null = null,
): Promise<string[]> {
  using trunk = await host.reader(trunkRepo);
  const problems: string[] = [];
  const head = await trunk.head();
  if (head !== commit) problems.push(`trunk head is ${head ?? "empty"}`);
  if (commit === approvedHead) {
    if (trunkBefore && !(await descendsFrom(trunk, approvedHead, trunkBefore))) {
      problems.push(`fast-forward to ${approvedHead} drops trunk commit ${trunkBefore}`);
      return problems;
    }
    // Same rule as a merge: what the trunk gained must be inside the
    // decision. A truncated decision has partial paths (a person approved
    // it as too large to list), so there is nothing complete to check against.
    if (trunkBefore && !decisionTruncated) {
      const before = await trunk.readCommit(trunkBefore);
      const after = await trunk.readCommit(approvedHead);
      if (!before || !after) return [...problems, "fast-forward commits not found on trunk"];
      const d = await diffTrees({ repo: trunk, tree: before.tree }, { repo: trunk, tree: after.tree }, { withContent: false });
      if (d.truncated) problems.push("fast-forward too large to verify");
      problems.push(...outsideDecision("fast-forward", d.paths, approvedPaths));
    }
    return problems;
  }
  const c = await trunk.readCommit(commit);
  if (!c) return [...problems, "commit not found on trunk"];
  if (c.parents.length !== 2 || c.parents[1] !== approvedHead) {
    return [...problems, `commit is neither ${approvedHead} nor a merge of it into trunk`];
  }
  if (trunkBefore && c.parents[0] !== trunkBefore) {
    return [...problems, `merge is not on the trunk the land started from (${trunkBefore}); trunk moved during the land`];
  }
  const parent = await trunk.readCommit(c.parents[0]!);
  if (!parent) return [...problems, "first parent not found on trunk"];
  const d = await diffTrees({ repo: trunk, tree: parent.tree }, { repo: trunk, tree: c.tree }, { withContent: false });
  if (d.truncated) problems.push("merge too large to verify");
  problems.push(...outsideDecision("merge", d.paths, approvedPaths));
  if (problems.length > 0 || decisionTruncated) return problems;
  // What the merge put in each decided path must be the approved head's
  // version, unless the trunk edited that path too since the intent's base.
  const approved = await trunk.readCommit(approvedHead);
  if (!approved) return ["approved head not found on trunk"];
  const wanted = await diffTrees({ repo: trunk, tree: parent.tree }, { repo: trunk, tree: approved.tree }, { withContent: false });
  if (wanted.truncated) return ["merge too large to verify against the approved head"];
  let trunkEdited: Set<string> | null = null;
  const notApproved: string[] = [];
  for (const p of d.paths) {
    const want = wanted.entries[p];
    const got = d.entries[p]!;
    // Same bytes in a symlink or with another mode is another change.
    if (want && want.after === got.after && want.after_mode === got.after_mode) continue;
    if (!trunkEdited) {
      const base = trunkBase ? await trunk.readCommit(trunkBase) : null;
      if (trunkBase && !base) return [`trunk base ${trunkBase} not found on trunk`];
      const since = await diffTrees({ repo: trunk, tree: base?.tree ?? null }, { repo: trunk, tree: parent.tree }, { withContent: false });
      if (since.truncated) return ["trunk history since the intent's base too large to verify the merge"];
      trunkEdited = new Set(since.paths);
    }
    if (!trunkEdited.has(p)) notApproved.push(p);
  }
  if (notApproved.length > 0) {
    problems.push(`merge holds content in ${notApproved.slice(0, 10).join(", ")} that is neither the approved version nor a merge with a trunk edit`);
  }
  return problems;
}

function outsideDecision(shape: string, changed: string[], approvedPaths: string[]): string[] {
  const allowed = new Set(approvedPaths);
  const extra = changed.filter((p) => !allowed.has(p));
  return extra.length > 0 ? [`${shape} changes paths outside the decision: ${extra.slice(0, 10).join(", ")}`] : [];
}

/** Whether `from` is `ancestor` or reaches it through parents (bounded walk). */
async function descendsFrom(repo: RepoReader, from: string, ancestor: string): Promise<boolean> {
  const seen = new Set<string>();
  const queue = [from];
  while (queue.length > 0 && seen.size < MAX_FF_WALK) {
    const h = queue.shift()!;
    if (h === ancestor) return true;
    if (seen.has(h)) continue;
    seen.add(h);
    const c = await repo.readCommit(h);
    if (c) queue.push(...c.parents);
  }
  return false;
}

// POST /v1/intents/:id/token {project}
//
// A fresh short-lived write token for the intent's own fork. Only the agent
// that declared the intent may ask; earlier fork tokens are revoked.
route("POST", "/v1/intents/:id/token", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const who = actor(req, body);
  const stub = await projectStub(env, project);
  // Fail closed: with no project record there is no jurisdiction, and minting
  // in the Worker's main namespace could hand out a token in the wrong one.
  const info = await stub.info();
  if (!info) return fail("not_found", `project ${project} not found`);
  const check = await stub.forkTokenRequest(id, who);
  if (!check.ok) return fromResult(check);
  const { intent, active_token_ids } = check.data;
  const host = hostFor(req, env, info.jurisdiction, stub, id);
  const token = await host.mintToken(intent.fork_repo, "write", FORK_TOKEN_TTL_SECONDS);
  const revoked: string[] = [];
  for (const old of active_token_ids) {
    try {
      await host.revokeToken(intent.fork_repo, old);
      revoked.push(old);
    } catch (err) {
      console.warn("could not revoke old fork token", old, err);
    }
  }
  const r = await stub.recordForkToken(id, { id: token.id, expires_at: token.expiresAt }, revoked, who);
  if (!r.ok) return fromResult(r);
  return ok(
    {
      fork: { repo: intent.fork_repo, remote: intent.fork_remote },
      token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
      revoked: revoked.length,
    },
    201,
  );
});

// POST /v1/intents/:id/read-token {project}
//
// A short-lived READ token for an intent's fork, so a reviewer (an agent
// doing agent_review, or a person) can see the code it decides on. Any
// authenticated caller may ask: reading is what reviewing is, and the token
// cannot push. Every mint is audited with who asked.
route("POST", "/v1/intents/:id/read-token", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const who = actor(req, body);
  const stub = await projectStub(env, project);
  const c = (await stub.intentContext(id)) as Plain<"intentContext">;
  if (!c.ok) return fromResult(c);
  const { intent, project: info } = c.data;
  const token = await hostFor(req, env, info.jurisdiction, stub, id).mintToken(intent.fork_repo, "read", READ_TOKEN_TTL_SECONDS);
  await stub.recordExternalEvent("token.read_minted", who, id, { repo: intent.fork_repo, scope: "read", expires_at: token.expiresAt });
  return ok(
    {
      intent,
      fork: { repo: intent.fork_repo, remote: intent.fork_remote },
      token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
    },
    201,
  );
});

// POST /v1/decisions/:id {project, verdict: approve|reject, by, reason}
route("POST", "/v1/decisions/:id", async (req, env, p, url) => {
  const id = p.id!;
  if (!DECISION_ID.test(id)) throw new BadRequest("invalid decision id");
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const verdict = text(body, "verdict");
  if (verdict !== "approve" && verdict !== "reject") throw new BadRequest("verdict must be approve or reject");
  // A decision without a reason defeats the point of the product.
  const reason = reasonText(body);
  const { by, claimed_by } = approver(req, body);
  const family = familyOf(body);
  return fromResult(await (await projectStub(env, project)).decide(id, { verdict, by, claimed_by, reason, family, person: isPerson(identityOf(req)), identity_source: identityOf(req).source }));
});

// POST /v1/intents/:id/decide {project, verdict: approve|reject, reason, by?, decision_id?, commit?}
//
// People think in intents, not decision ids: this decides the intent's latest
// decision. The verdict must still be about what the person read, so a
// client says which decision (decision_id) or which fork commit (commit, a
// prefix will do) it showed; a replacement proposed since is refused
// (decision_replaced, 409) instead of approved unseen. wayglass approve and
// the desktop app always bind; a bare request decides whatever is latest.
route("POST", "/v1/intents/:id/decide", async (req, env, p, url) => {
  const id = p.id!;
  if (!INTENT_ID.test(id)) throw new BadRequest("invalid intent id");
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const verdict = text(body, "verdict");
  if (verdict !== "approve" && verdict !== "reject") throw new BadRequest("verdict must be approve or reject");
  const reason = reasonText(body);
  const { by, claimed_by } = approver(req, body);
  const family = familyOf(body);
  const stub = await projectStub(env, project);
  const ctx = (await stub.intentContext(id)) as Plain<"intentContext">;
  if (!ctx.ok) return fromResult(ctx);
  const latest = ctx.data.latest_decision;
  if (!latest) return fail("conflict", `intent ${id} has no proposed change to decide; run wayglass propose first`);
  const decisionId = text(body, "decision_id", false);
  const commit = text(body, "commit", false).toLowerCase();
  if (decisionId && !DECISION_ID.test(decisionId)) throw new BadRequest("invalid decision_id");
  if (commit && !/^[0-9a-f]{7,40}$/.test(commit)) throw new BadRequest("commit must be 7 to 40 hex characters");
  const latestCommit = latest.commit_sha ?? "";
  if ((decisionId && decisionId !== latest.id) || (commit && !latestCommit.startsWith(commit))) {
    return fail(
      "decision_replaced",
      `intent ${id} was proposed again after ${decisionId ? `decision ${decisionId}` : `commit ${commit}`}: its latest decision is ${latest.id} on commit ${latestCommit.slice(0, 7)}; nothing was recorded. Review the new proposal, then decide it`,
    );
  }
  return fromResult(await stub.decide(latest.id, { verdict, by, claimed_by, reason, family, person: isPerson(identityOf(req)), identity_source: identityOf(req).source }));
});

// GET /v1/cost?project=
// Artifacts operations the Worker made, per intent and in total, with an
// estimate from published pricing. Worker-side only; see src/cost.ts.
route("GET", "/v1/cost", async (req, env, _p, url) => {
  const project = projectName(req, url);
  return fromResult(await (await projectStub(env, project)).costSummary(aiDailyLimit(env)));
});

// GET /v1/decisions?project=&status=&limit=
route("GET", "/v1/decisions", async (req, env, _p, url) => {
  const project = projectName(req, url);
  const status = url.searchParams.get("status") ?? undefined;
  const limit = Number(url.searchParams.get("limit") ?? "200");
  if (!Number.isInteger(limit) || limit < 1 || limit > 500) throw new BadRequest("limit must be an integer from 1 to 500");
  return fromResult(await (await projectStub(env, project)).listDecisions(status, limit));
});

// GET /v1/audit?project=&since=&limit=
route("GET", "/v1/audit", async (req, env, _p, url) => {
  const project = projectName(req, url);
  const since = Number(url.searchParams.get("since") ?? "0");
  const limit = Number(url.searchParams.get("limit") ?? "100");
  if (!Number.isInteger(since) || since < 0) throw new BadRequest("since must be a non-negative integer");
  if (!Number.isInteger(limit) || limit < 1 || limit > 500) throw new BadRequest("limit must be an integer from 1 to 500");
  return fromResult(await (await projectStub(env, project)).listAudit(since, limit));
});

// GET /v1/why?project=&commit=
route("GET", "/v1/why", async (req, env, _p, url) => {
  const project = projectName(req, url);
  const commit = (url.searchParams.get("commit") ?? "").toLowerCase();
  if (!COMMIT.test(commit)) throw new BadRequest("commit must be a hex SHA of 7 to 40 characters");
  return fromResult(await (await projectStub(env, project)).why(commit));
});

// ---- sessions (golden path step 7) ----------------------------------------
//
// A session is a frozen, append-only snapshot of an intent: goal, reason,
// paths, decisions, overlaps, history and the agent's handoff notes, plus the
// fork head the Worker read from Artifacts. Its URL is
// <worker>/v1/projects/<project>/sessions/<id>: plain https, no shell
// metacharacters, and it names the Worker, so the CLI only ever sends its key
// back to the Worker it is logged in to.
//
// Forking a session never hands out the original fork's token. The Worker
// forks the original fork into a NEW intent's repo and mints a short-lived
// write token for that repo only; the source agent keeps working undisturbed.

const SESSION_ID = /^s[0-9a-f]{10}$/;
const MAX_NOTES = 32 * 1024;

function sessionUrl(url: URL, project: string, id: string): string {
  return `${url.origin}/v1/projects/${project}/sessions/${id}`;
}

function sessionParams(p: Record<string, string>): { project: string; sid: string } {
  const project = p.project!;
  const sid = p.sid!;
  if (!PROJECT_NAME.test(project)) throw new BadRequest("invalid project name");
  if (!SESSION_ID.test(sid)) throw new BadRequest("invalid session id");
  return { project, sid };
}

// POST /v1/intents/:id/sessions {project, notes?, commit?}
route("POST", "/v1/intents/:id/sessions", async (req, env, p, url) => {
  const id = intentId(p);
  const body = await readJson(req);
  const project = projectName(req, url, body);
  const claimed = commitOf(body);
  const raw = body.notes ?? "";
  if (typeof raw !== "string") throw new BadRequest("notes must be a string");
  if (raw.length > MAX_NOTES) throw new BadRequest(`notes are longer than ${MAX_NOTES} characters`);
  if (hasControl(raw)) throw new BadRequest("notes contain control characters; send plain text (tabs and newlines are fine)");
  const notes = raw.trim();
  const who = actor(req, body);

  const stub = await projectStub(env, project);
  const ctx = (await stub.intentContext(id)) as Plain<"intentContext">;
  if (!ctx.ok) return fromResult(ctx);
  const { project: info, intent } = ctx.data;
  // The snapshot points at what is really in Artifacts, never at a claim.
  let head: string | null;
  {
    using fork = await hostFor(req, env, info.jurisdiction, stub, id).reader(intent.fork_repo);
    head = await fork.head();
  }
  if (claimed && !head?.startsWith(claimed)) {
    return fail("conflict", `fork head is ${head ?? "empty"}, not ${claimed}; run wayglass push before sharing`);
  }
  const sid = randomId("s");
  const r = (await stub.shareSession({ id: sid, intent_id: id, head_commit: head, notes, actor: who })) as Plain<"shareSession">;
  if (!r.ok) return fromResult(r);
  return ok({ session: r.data.session, url: sessionUrl(url, project, sid) }, 201);
});

// GET /v1/projects/:project/sessions/:sid
route("GET", "/v1/projects/:project/sessions/:sid", async (_req, env, p, url) => {
  const { project, sid } = sessionParams(p);
  const r = (await (await projectStub(env, project)).sessionView(sid)) as Plain<"sessionView">;
  if (!r.ok) return fromResult(r);
  return ok({ project: r.data.project.name, session: r.data.session, forks: r.data.forks, url: sessionUrl(url, project, sid) });
});

// POST /v1/projects/:project/sessions/:sid/fork {}
route("POST", "/v1/projects/:project/sessions/:sid/fork", async (req, env, p, url) => {
  const { project, sid } = sessionParams(p);
  const body = await readJson(req);
  const who = actor(req, body);
  const family = familyOf(body);
  const stub = await projectStub(env, project);
  const ctx = (await stub.sessionForkContext(sid)) as Plain<"sessionForkContext">;
  if (!ctx.ok) return fromResult(ctx);
  const { project: info, session, parent } = ctx.data;

  const id = randomId("i");
  const host = hostFor(req, env, info.jurisdiction, stub, id);
  // Stacked on the source intent as it is now (it may have moved past the
  // snapshot; the client reports both commits), exactly like `declare --on`:
  // the new intent's diff covers only its own work, policy comes from the
  // stack's trunk commit, and it lands after its source. A sibling would
  // re-propose the source's commits and conflict or duplicate them on land.
  const placed = await forkIntent(host, info, project, id, parent, `Continues ${parent.id} (session ${sid})`, "alongside");
  if (placed instanceof Response) return placed;
  const { fork, token } = placed;
  const r = await stub.createIntent({
    id,
    goal: parent.goal,
    reason: parent.reason,
    agent: who,
    declared_paths: parent.declared_paths,
    fork_repo: fork.name,
    fork_remote: fork.remote,
    base_commit: placed.base_commit,
    parent_id: placed.parent_id,
    trunk_base: placed.trunk_base,
    fork_token: { id: token.id, expires_at: token.expiresAt },
    parent_intent_id: parent.id,
    session_id: sid,
    // Whoever continues the work is its author from here on.
    agent_family: family,
  });
  if (!r.ok) return dropUnrecordedFork(host, fork.name, r);
  return ok(
    {
      session,
      url: sessionUrl(url, project, sid),
      intent: r.data.intent,
      parent: { id: parent.id, agent: parent.agent, status: parent.status, fork_repo: parent.fork_repo },
      fork,
      token: { value: token.value, scope: token.scope, expires_at: token.expiresAt },
      overlaps: r.data.overlaps,
    },
    201,
  );
});

// GET /v1/whoami: who this key is. Lets a client check a login without touching a project.
route("GET", "/v1/whoami", async (req) => {
  const id = identityOf(req);
  return ok({ role: id.role, agent: actor(req), bound: id.role === "agent", key_id: id.key_id, kind: id.kind, source: id.source, expires_at: id.expires_at });
});

// POST /v1/keys {agent, note?, kind?: human|agent}  (admin only): mint a key
// bound to agent. kind defaults to agent; only a human key may decide a change
// routed to a person. The plaintext is shown once.
route("POST", "/v1/keys", async (req, env) => {
  requireAdmin(req);
  const body = await readJson(req);
  const agent = text(body, "agent");
  if (!AGENT_NAME.test(agent)) throw new BadRequest("agent must be 1-128 letters, digits, '.', '_', '@', '+' or '-'");
  const kind = text(body, "kind", false) || "agent";
  if (kind !== "human" && kind !== "agent") throw new BadRequest("kind must be human or agent");
  return fromResult(await registryStub(env).create(agent, actor(req, body), text(body, "note", false), kind), 201);
});

// GET /v1/keys?all=1  (admin only): key ids, agents and dates; never a key or its hash.
route("GET", "/v1/keys", async (req, env, _p, url) => {
  requireAdmin(req);
  return ok({ keys: await registryStub(env).list(url.searchParams.get("all") === "1") });
});

// DELETE /v1/keys/:id  (admin only): revoke; takes effect on the next request.
route("DELETE", "/v1/keys/:id", async (req, env, p) => {
  requireAdmin(req);
  return fromResult(await registryStub(env).revoke(p.id!, actor(req)));
});

// POST /v1/keys/self/revoke {}  (any bound key): revoke the calling key, so
// signing out of the web app or CLI ends a session instead of leaving it valid
// until it expires. The admin key is a Worker secret and cannot revoke itself.
route("POST", "/v1/keys/self/revoke", async (req, env) => {
  const id = identityOf(req);
  if (id.role !== "agent") throw new BadRequest("the admin key is a Worker secret and cannot be revoked here; rotate WAYGLASS_ADMIN_KEY instead");
  return fromResult(await registryStub(env).revoke(id.key_id, id.agent));
});

// GET /v1/keys/events?after=<seq>  (admin only): key.created and key.revoked,
// oldest first, including sessions minted through Cloudflare Access.
route("GET", "/v1/keys/events", async (req, env, _p, url) => {
  requireAdmin(req);
  const after = Number(url.searchParams.get("after") ?? 0);
  if (!Number.isInteger(after) || after < 0) throw new BadRequest("after must be a non-negative integer");
  return ok({ events: await registryStub(env).events(after) });
});

export async function handleApi(req: Request, env: Env): Promise<Response> {
  const url = new URL(req.url);
  // Authenticate before routing: without a key the API has no routes to
  // enumerate, only 401.
  const who = await authenticate(req, env);
  if (who instanceof Response) return who;
  identities.set(req, who);

  const matching = routes.filter((r) => r.pattern.test({ pathname: url.pathname }));
  if (matching.length === 0) return fail("not_found", `no route for ${url.pathname}`);
  const r = matching.find((m) => m.method === req.method);
  if (!r) return fail("method_not_allowed", `${req.method} not allowed on ${url.pathname}`);

  const params = (r.pattern.exec({ pathname: url.pathname })?.pathname.groups ?? {}) as Record<string, string>;
  try {
    return await r.handler(req, env, params, url);
  } catch (err) {
    if (err instanceof BadRequest) return fail("bad_request", err.message);
    if (err instanceof Forbidden) return fail("forbidden", err.message);
    // Fail closed: no decision is recorded when the evaluator cannot run.
    if (err instanceof PolicyEngineError) {
      console.error("policy engine", err);
      return fail("internal", "policy engine failed; no decision was recorded");
    }
    if (err instanceof RepoHostError) return fail("upstream_error", err.message);
    const moved = movedError(err);
    if (moved) return fail(moved.code, moved.message);
    console.error("unhandled", err);
    return fail("internal", "internal error");
  } finally {
    await recordMetering(req);
  }
}

export default {
  async fetch(req, env): Promise<Response> {
    const url = new URL(req.url);
    // Browser-facing responses need a valid Access token for the web app when Access is configured.
    if (isWebPath(url.pathname)) {
      const refused = await webAccessGate(req, env);
      if (refused) return refused;
    }
    if (isBrowseRoute(req)) return handleBrowse(req, env);
    if (url.pathname.startsWith(PUBLIC_PREFIX)) return handlePublic(req, env);
    // Cloudflare Access sign-in for people: authenticated by the Access token, not a key.
    if (url.pathname.startsWith(HUMAN_PREFIX)) return handleHuman(req, env);
    if (url.pathname === "/v1" || url.pathname.startsWith("/v1/")) return handleApi(req, env);
    return env.ASSETS.fetch(req);
  },
  queue: handleQueue,
} satisfies ExportedHandler<Env>;

