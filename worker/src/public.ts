// Public, read-only view of a project's intents, decisions and audit log, for
// the web/ audit page (golden path step 8). No admin key: instead two gates
// that both default to off.
//
//   1. PUBLIC_PROJECTS (Worker var, comma-separated names or prefix patterns
//      with one trailing `*`, e.g. "demo-*,eu-demo-*"). Changing it is a code
//      change. Names it does not match get a 404 without touching a Durable
//      Object, so random names cannot create empty objects.
//   2. The project's own `public` flag, set by the admin with
//      PATCH /v1/projects/:name {"public": true}. Audited, and can be turned
//      off at once without a deploy (cached responses live CACHE_SECONDS).
//
// Responses are built from an allowlist of fields, never by stripping a
// denylist: a field added to an intent, decision or audit event later stays
// private until someone decides it is safe here. Remotes, token ids, land ids,
// subscription ids and client previews never leave; token-shaped strings and
// Artifacts remote URLs inside free text are masked as well.

import { fail, ok, type Result } from "./envelope";
import type { Approval, AuditEvent, Decision, Intent } from "./project";
import { projectStub, type ProjectStub } from "./stub";
import { movedError } from "./residency";
import type { CostLine, CostSummary } from "./cost";

export const PUBLIC_PREFIX = "/v1/public/";
export const CACHE_SECONDS = 15;

const NAME = /^[a-z0-9](?:[a-z0-9._]|-(?!-)){0,62}$/;
const DECISION_ID = /^d[0-9a-f]{10}$/;

type PublicEnv = Env & { PUBLIC_PROJECTS?: string };

/**
 * Whether the deployment allows a project to be public at all. Each entry of
 * PUBLIC_PROJECTS is an exact name or a prefix ending in a single `*`
 * ("demo-*"). A `*` anywhere else, or a bare `*`, matches nothing: opening
 * every project must not be one typo away.
 */
export function projectAllowed(env: PublicEnv, name: string): boolean {
  for (const raw of (env.PUBLIC_PROJECTS ?? "").split(",")) {
    const entry = raw.trim();
    if (!entry) continue;
    const star = entry.indexOf("*");
    if (star === -1) {
      if (entry === name) return true;
    } else if (star === entry.length - 1 && star > 0 && name.startsWith(entry.slice(0, -1))) {
      return true;
    }
  }
  return false;
}

// ---- redaction ------------------------------------------------------------

const TOKEN = /art_v\d+_[A-Za-z0-9_]+(?:\?expires=\d+)?/g;
const REMOTE = /https:\/\/[0-9a-f]{32}\.artifacts\.cloudflare\.net\/[^\s"'<>]*/g;

/** Masks token-shaped strings and Artifacts remotes (which carry the account id) in any JSON value. */
export function scrub<T>(v: T): T {
  if (typeof v === "string") return v.replace(TOKEN, "[token]").replace(REMOTE, "[artifacts remote]") as T;
  if (Array.isArray(v)) return v.map(scrub) as T;
  if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, scrub(x)])) as T;
  return v;
}

function pick(o: Record<string, unknown>, keys: readonly string[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const k of keys) if (o[k] !== undefined) out[k] = o[k];
  return out;
}

export function publicIntent(i: Intent) {
  return scrub({
    id: i.id,
    goal: i.goal,
    reason: i.reason,
    agent: i.agent,
    agent_family: i.agent_family ?? null,
    declared_paths: i.declared_paths,
    actual_paths: i.actual_paths,
    base_commit: i.base_commit,
    head_commit: i.head_commit,
    status: i.status,
    parent_id: i.parent_id,
    stale: i.stale === true,
    abandoned_reason: i.abandoned_reason ?? null,
    created_at: i.created_at,
    updated_at: i.updated_at,
  });
}

export function publicDecision(d: Decision & { approvals?: Approval[] }) {
  return scrub({
    id: d.id,
    intent_id: d.intent_id,
    commit_sha: d.commit_sha,
    base_commit: d.base_commit,
    tags: d.tags,
    matched_rule: d.matched_rule,
    action: d.action,
    reason: d.reason,
    status: d.status,
    approvals_needed: d.approvals_needed,
    paths: d.paths,
    files: Array.isArray(d.detail?.files) ? d.detail.files : [],
    // Policy wire v2 records shadow_matches; decisions from v1 Workers kept
    // them under `shadow`. Each match is rebuilt from an allowlist too.
    shadow_matches: shadowMatches(d.detail?.shadow_matches ?? d.detail?.shadow),
    would_differ: d.detail?.would_differ === true,
    reviewer: (d.detail?.reviewer as { different_family?: boolean } | null)?.different_family ? { different_family: true } : null,
    landed_commit: d.landed_commit,
    decided_by: d.decided_by,
    decided_at: d.decided_at,
    created_at: d.created_at,
    approvals: (d.approvals ?? []).map((a) => ({ by: a.by, claimed_by: a.claimed_by ?? null, family: a.family ?? null, verdict: a.verdict, reason: a.reason, at: a.at })),
  });
}

function shadowMatches(v: unknown) {
  if (!Array.isArray(v)) return [];
  return v.map((m: Record<string, unknown>) => pick(m ?? {}, ["rule", "index", "action", "approvers", "reason", "preempts"]));
}

/** Data fields each audit kind may show. Unknown kinds show only who, what and when. */
const AUDIT_FIELDS: Record<string, readonly string[]> = {
  "project.created": ["name", "jurisdiction"],
  "intent.declared": ["goal", "reason", "declared_paths", "base_commit", "agent_family"],
  "intent.pushed": ["paths", "commit"],
  "intent.overlap": ["overlaps"],
  "decision.proposed": ["commit", "base_commit", "tags", "matched_rule", "action", "reason", "status", "shadow", "shadow_matches", "would_differ", "files", "diff_base"],
  "decision.approve": ["reason", "status", "family", "claimed_by"],
  "decision.reject": ["reason", "status", "family", "claimed_by"],
  "decision.escalated": ["from_action", "reason", "concerns", "family", "action", "status", "rule_reason"],
  "decision.ai_advice": ["summary", "matches_goal", "risks", "questions", "warnings", "source", "family", "action", "status"],
  "land.reserved": ["commit"],
  "land.token_issued": ["scope", "expires_at"],
  "land.cancelled": ["reason"],
  "intent.landed": ["commit", "token_revoked"],
  "intent.abandoned": ["reason", "restacked"],
  "intent.restacked": ["reason", "from_parent", "to_parent", "base_commit", "superseded_decision"],
  "decision.superseded": ["reason", "previous_status", "previous_decided_by", "approved_by"],
  "land.mismatch": ["commit", "problems"],
  "token.refreshed": ["scope", "expires_at"],
  "token.read_minted": ["scope", "expires_at"],
  "artifacts.repo.pushed": ["ref", "before", "after", "total_commits"],
};

export function publicEvent(e: AuditEvent) {
  return scrub({
    seq: e.seq,
    at: e.at,
    kind: e.kind,
    actor: e.actor,
    intent_id: e.intent_id,
    decision_id: e.decision_id,
    data: pick(e.data, AUDIT_FIELDS[e.kind] ?? []),
  });
}

/** Operation counts and the estimate; never per-request detail. */
export function publicCost(c: CostSummary) {
  const line = (l: CostLine) => ({ ops: l.ops, by_op: { ...l.by_op }, tokens_minted: l.tokens_minted, est_usd: l.est_usd });
  return {
    scope: c.scope,
    pricing: { ...c.pricing },
    total: line(c.total),
    per_landed_change: c.per_landed_change && { ...c.per_landed_change },
    events: { ...c.events },
    // AI calls and outcomes, no budget detail.
    ai: c.ai && {
      calls: c.ai.calls,
      reviews: c.ai.reviews,
      approvals: c.ai.approvals,
      escalations: c.ai.escalations,
      advice: c.ai.advice,
      tokens: c.ai.tokens,
      est_usd: c.ai.est_usd,
      total: { ...c.ai.total },
      today: { ...c.ai.today },
      basis: c.ai.basis,
    },
  };
}

// ---- routes ---------------------------------------------------------------

class NotFound extends Error {}
class BadRequest extends Error {}

function intParam(url: URL, name: string, def: number, min: number, max: number): number {
  const raw = url.searchParams.get(name);
  const n = raw === null ? def : Number(raw);
  if (!Number.isInteger(n) || n < min || n > max) throw new BadRequest(`${name} must be an integer from ${min} to ${max}`);
  return n;
}

function unwrap<T>(r: { ok: true; data: T } | { ok: false; code: string; message: string }): T {
  if (!r.ok) throw r.code === "not_found" ? new NotFound() : new BadRequest(r.message);
  return r.data;
}

type Stub = ProjectStub;
type DecisionRow = Decision & { approvals: Approval[] };

// DO RPC typing turns `unknown` fields (tags, detail) into `never`; the values
// are plain JSON, so read them as the method's declared return type.
async function decisionsOf(stub: Stub, limit: number): Promise<DecisionRow[]> {
  const r = (await stub.listDecisions(undefined, limit)) as Result<{ decisions: DecisionRow[] }>;
  return unwrap(r).decisions;
}

/**
 * The canonical cache key for a public path: the route plus validated
 * parameters only, so stray query strings cannot bypass the cache.
 */
function canonical(rest: string[], url: URL): string {
  if (rest.length === 0) return "overview";
  if (rest.length === 1 && rest[0] === "intents") return "intents";
  if (rest.length === 1 && rest[0] === "decisions") return `decisions?limit=${intParam(url, "limit", 200, 1, 500)}`;
  if (rest.length === 2 && rest[0] === "decisions") {
    if (!DECISION_ID.test(rest[1]!)) throw new BadRequest("invalid decision id");
    return `decisions/${rest[1]}`;
  }
  if (rest.length === 1 && rest[0] === "audit") {
    const since = intParam(url, "since", 0, 0, Number.MAX_SAFE_INTEGER);
    return `audit?since=${since}&limit=${intParam(url, "limit", 100, 1, 500)}`;
  }
  throw new NotFound();
}

/** Builds the response data for a public path, or throws NotFound / BadRequest. */
async function build(stub: Stub, rest: string[], url: URL, defaultJurisdiction: string): Promise<{ data: unknown }> {
  const info = await stub.info();
  if (!info) throw new NotFound();
  // A project made before jurisdictions were recorded lives in the Worker's
  // main namespace, whatever that one is (src/browse.ts says the same).
  const project = { name: info.name, jurisdiction: info.jurisdiction || defaultJurisdiction, created_at: info.created_at };

  if (rest.length === 0) {
    const intents = unwrap(await stub.listIntents()).intents as Intent[];
    const decisions = await decisionsOf(stub, 500);
    const tally = (xs: string[]) => xs.reduce<Record<string, number>>((m, x) => ((m[x] = (m[x] ?? 0) + 1), m), {});
    return {
      data: {
        project,
        counts: {
          agents: new Set(intents.map((i) => i.agent)).size,
          intents: intents.length,
          decisions: decisions.length,
          intents_by_status: tally(intents.map((i) => i.status)),
          decisions_by_action: tally(decisions.map((d) => d.action)),
          decisions_by_status: tally(decisions.map((d) => d.status)),
          landed: decisions.filter((d) => d.landed_commit).length,
        },
        cost: publicCost(unwrap((await stub.costSummary()) as Result<CostSummary>)),
      },
    };
  }
  if (rest.length === 1 && rest[0] === "intents") {
    const intents = unwrap(await stub.listIntents()).intents as Intent[];
    return { data: { project, intents: intents.map(publicIntent) } };
  }
  if (rest.length === 1 && rest[0] === "decisions") {
    const limit = intParam(url, "limit", 200, 1, 500);
    const decisions = await decisionsOf(stub, limit);
    return { data: { project, decisions: decisions.map(publicDecision) } };
  }
  if (rest.length === 2 && rest[0] === "decisions") {
    const id = rest[1]!;
    if (!DECISION_ID.test(id)) throw new BadRequest("invalid decision id");
    // Read from the list (newest 500): no new Durable Object method for a public view.
    const decisions = await decisionsOf(stub, 500);
    const d = decisions.find((x) => x.id === id);
    if (!d) throw new NotFound();
    const intent = (unwrap(await stub.listIntents()).intents as Intent[]).find((i) => i.id === d.intent_id) ?? null;
    return { data: { project, decision: publicDecision(d), intent: intent && publicIntent(intent) } };
  }
  if (rest.length === 1 && rest[0] === "audit") {
    const since = intParam(url, "since", 0, 0, Number.MAX_SAFE_INTEGER);
    const limit = intParam(url, "limit", 100, 1, 500);
    const r = unwrap(await stub.listAudit(since, limit)) as { events: AuditEvent[]; next: number };
    return { data: { project, events: r.events.map(publicEvent), next: r.next } };
  }
  throw new NotFound();
}

const CORS = { "access-control-allow-origin": "*", vary: "origin" };

function withHeaders(res: Response, extra: Record<string, string>): Response {
  const out = new Response(res.body, res);
  for (const [k, v] of Object.entries(extra)) out.headers.set(k, v);
  return out;
}

/**
 * GET /v1/public/projects/:name[/intents|/decisions|/decisions/:id|/audit]
 * No auth by design; see the gates at the top of this file. Successful
 * answers are cached for CACHE_SECONDS under a canonical key, so a busy page
 * costs one Durable Object read per path per colo every few seconds.
 */
export async function handlePublic(req: Request, env: Env): Promise<Response> {
  const url = new URL(req.url);
  if (req.method !== "GET" && req.method !== "HEAD") return withHeaders(fail("method_not_allowed", "the public view is read-only"), CORS);
  const parts = url.pathname.slice(PUBLIC_PREFIX.length).split("/").filter(Boolean);
  const notFound = () => withHeaders(fail("not_found", "no public project here"), CORS);
  if (parts[0] !== "projects" || !parts[1] || !NAME.test(parts[1])) return notFound();
  const name = parts[1];
  // Same answer for "does not exist" and "not public": existence is not public either.
  if (!projectAllowed(env as PublicEnv, name)) return notFound();

  const rest = parts.slice(2);
  let key: string;
  try {
    key = canonical(rest, url);
  } catch (err) {
    if (err instanceof BadRequest) return withHeaders(fail("bad_request", err.message), CORS);
    return notFound();
  }
  // Only successful answers are cached, so a project made public shows up at
  // once; one made private again stops answering within CACHE_SECONDS.
  const cacheKey = new Request(`${url.origin}${PUBLIC_PREFIX}projects/${name}/${key}`);
  const cache = (globalThis as { caches?: { default?: Cache } }).caches?.default;
  if (cache) {
    const hit = await cache.match(cacheKey);
    if (hit) return withHeaders(hit, { ...CORS, "x-wayglass-cache": "hit" });
  }

  let built: { data: unknown };
  try {
    const stub = await projectStub(env, name);
    if (!(await stub.isPublic())) return notFound();
    built = await build(stub, rest, url, env.ARTIFACTS_JURISDICTION ?? "");
  } catch (err) {
    if (err instanceof NotFound) return notFound();
    if (err instanceof BadRequest) return withHeaders(fail("bad_request", err.message), CORS);
    const moved = movedError(err);
    if (moved) return withHeaders(fail(moved.code, moved.message), CORS);
    console.error("public view", err);
    return withHeaders(fail("internal", "internal error"), CORS);
  }

  const res = withHeaders(ok(built.data), { ...CORS, "cache-control": `public, max-age=${CACHE_SECONDS}` });
  if (cache) await cache.put(cacheKey, res.clone());
  return withHeaders(res, { "x-wayglass-cache": "miss" });
}
