// One Project Durable Object per project. SQLite-backed.
// Holds intents, decisions, approvals and the append-only audit log, and runs
// the overlap check with strong consistency.

import { DurableObject } from "cloudflare:workers";
import { failResult, okResult, type Failure, type Result } from "./envelope";
import { intersectSets, type PathOverlap } from "./paths";
import type { ServerDecision } from "./policy";
import type { DiffBase } from "./ownchange";
import { familyRefusal } from "./family";
import { ADMIN_IDENTITY } from "./keys";
import { addAiCost, AI_COST_BASIS, aiCostLine, aiDailyLimit, COST_SCOPE, costLine, estimateUsd, PRICING, utcDay, type AiBudget, type AiCost, type AiUsageInput, type CostSummary } from "./cost";

export const ACTIONS = ["auto_merge", "agent_review", "human_approve", "multi_approve", "quarantine", "block"] as const;
export type Action = (typeof ACTIONS)[number];

export type IntentStatus = "open" | "proposed" | "landed" | "abandoned";
/** Intents that still compete for paths. */
const ACTIVE_STATUSES: IntentStatus[] = ["open", "proposed"];
/** Decisions waiting for a person: only a person may decide them (see decide). */
const HUMAN_STATUSES: readonly DecisionStatus[] = ["pending_human", "quarantined"];

export type DecisionStatus = "approved" | "rejected" | "pending_agent" | "pending_human" | "quarantined" | "blocked" | "superseded";
/** Decisions still waiting for a verdict. */
const PENDING_STATUSES: readonly DecisionStatus[] = ["pending_agent", "pending_human", "quarantined"];
/** Decisions a restack supersedes (supersedeStale): still pending, or approved but not landed. */
const SUPERSEDABLE_STATUSES: readonly DecisionStatus[] = [...PENDING_STATUSES, "approved"];

export interface ProjectInfo {
  name: string;
  trunk_repo: string;
  trunk_remote: string;
  /** Jurisdiction of the namespace holding its repos; "" = the Worker's main one (projects made before Oct 12). */
  jurisdiction: string;
  created_at: string;
}

/** The branch Artifacts gives a new trunk; assumed for projects recorded before it was stored. */
export const DEFAULT_BRANCH = "main";

/** One row of the project directory (GET /v1/projects): what a repo browser needs without opening the project. */
export interface DirectoryEntry extends ProjectInfo {
  default_branch: string;
  public: boolean;
  /** The newest audit event: a declare, push, decision, land, token or external push event. */
  last_activity: string | null;
  /** The last push an intent reported; null if none. */
  last_push_at: string | null;
  counts: {
    intents: number;
    /** Intents open or proposed. */
    open: number;
    agents: number;
    /** Decisions waiting for a person (pending_human or quarantined) whose intent is still active. */
    pending_human: number;
    landed: number;
  };
}

export interface Intent {
  id: string;
  goal: string;
  reason: string;
  agent: string;
  declared_paths: string[];
  actual_paths: string[];
  fork_repo: string;
  fork_remote: string;
  /** Commit the fork started from: a trunk commit, or the parent intent's fork head when stacked. The diff base. */
  base_commit: string | null;
  /** Intent this one is stacked on (forked from its fork); null for intents forked from trunk. */
  parent_id: string | null;
  /** Trunk commit the stack started from; the policy is read from trunk here. Equals base_commit when not stacked. */
  trunk_base: string | null;
  head_commit: string | null;
  status: IntentStatus;
  /** Set when this intent continues another one through `wayglass session fork`. */
  parent_intent_id: string | null;
  /** The shared session it was forked from. */
  session_id: string | null;
  /** The author's declared model family (claude, openai, gemini, other, human); null if none. Self-reported. */
  agent_family: string | null;
  /** When the agent last reported a push (null: never). */
  last_push_at: string | null;
  /**
   * Open or proposed, but no push for STALE_INTENT_HOURS (from the last push,
   * or from the declare if it never pushed). Reported, never acted on: the
   * author or the admin decides whether to abandon it.
   */
  stale: boolean;
  /** Set when the intent was abandoned: by whom and why. */
  abandoned_by: string | null;
  abandoned_reason: string | null;
  created_at: string;
  updated_at: string;
}

/** What DELETE /v1/projects/:name must remove from Artifacts before the state goes. */
export interface DeletionPlan {
  project: ProjectInfo;
  /** Fork repo names, newest first. */
  forks: string[];
  /** Unrevoked, unexpired token ids (fork tokens and issued land tokens). */
  tokens: { repo: string; id: string }[];
  /** Trunk push event subscriptions (src/subscriptions.ts). */
  subscription_ids: string[];
  counts: { intents: number; decisions: number; audit_events: number; sessions: number };
}

export interface Overlap {
  intent_id: string;
  goal: string;
  agent: string;
  status: IntentStatus;
  paths: PathOverlap[];
  /** "parent" or "child" when the overlap is a session fork and its source: expected, but both can still land. */
  relation?: "parent" | "child";
  /** The other intent has not pushed for STALE_INTENT_HOURS; it may be dead weight rather than real competition. */
  stale?: boolean;
}

export interface Decision {
  id: string;
  intent_id: string;
  /** Fork head the server classified. */
  commit_sha: string | null;
  base_commit: string | null;
  /** Server-derived tags. The only tags that decide anything. */
  tags: Record<string, unknown>;
  matched_rule: string | null;
  action: Action;
  reason: string;
  status: DecisionStatus;
  approvals_needed: number;
  /** Paths the server saw change between base_commit and commit_sha. */
  paths: string[];
  /** Shadow matches, overridden rule, truncation and policy errors. */
  detail: Record<string, unknown>;
  /** What the client said it would be. Stored for comparison; never decides. */
  client_preview: Record<string, unknown> | null;
  landed_commit: string | null;
  /** Trunk commit the policy was read from at propose time (null: empty trunk, or a decision from before Oct 14). */
  policy_commit: string | null;
  /** Digest of the trunk's .wayglass/policy.yaml the decision was made under ("sha256:<hex>" or "absent"); null before Oct 14. Land refuses when trunk's differs. */
  policy_sha256: string | null;
  decided_by: string | null;
  decided_at: string | null;
  created_at: string;
}

export type LandStatus = "reserved" | "issued" | "landed" | "rejected" | "expired" | "cancelled";

export interface Land {
  id: string;
  intent_id: string;
  decision_id: string;
  token_id: string | null;
  status: LandStatus;
  /** Trunk head when the token was issued; a merge must sit directly on it. Null for lands from before Oct 13. */
  trunk_before: string | null;
  expires_at: string;
  landed_commit: string | null;
  /**
   * Whether Artifacts confirmed the trunk token gone when the land closed:
   * true revoked, false the revocation failed (the token lives until
   * expires_at, and the project's land slot stays busy that long), null
   * still open or closed before this was recorded.
   */
  token_revoked: boolean | null;
  created_at: string;
  updated_at: string;
}

export interface Approval {
  decision_id: string;
  /** The identity the verdict counts as: a key's bound name, or "admin" for the admin key. */
  by: string;
  /** The name the admin key reported with its verdict; null for a bound key, which cannot name itself. */
  claimed_by: string | null;
  /** The decider's declared model family; null if none. Self-reported. */
  family: string | null;
  verdict: "approve" | "reject";
  reason: string;
  at: string;
  /** How the decider signed in ("admin", "key", "access"); null for approvals recorded before Oct 14. */
  identity_source: string | null;
}

export interface AuditEvent {
  seq: number;
  at: string;
  kind: string;
  actor: string;
  intent_id: string | null;
  decision_id: string | null;
  data: Record<string, unknown>;
}

export interface NewIntent {
  id: string;
  goal: string;
  reason: string;
  agent: string;
  declared_paths: string[];
  fork_repo: string;
  fork_remote: string;
  base_commit: string | null;
  parent_id: string | null;
  trunk_base: string | null;
  /** Id and expiry of the fork token handed out with the intent (never the secret). */
  fork_token: { id: string; expires_at: string };
  /** Session forks only: the intent being continued and the session it was shared in. */
  parent_intent_id?: string | null;
  session_id?: string | null;
  /** The author's declared model family, already normalised; null if none. */
  agent_family?: string | null;
}

/** What an agent hands on with `wayglass session share`. Frozen when shared. */
export interface SessionSnapshot {
  intent: Intent;
  decisions: (Decision & { approvals: Approval[] })[];
  overlaps: Overlap[];
  /** The intent's audit events up to the share, oldest first (at most SESSION_HISTORY). */
  history: AuditEvent[];
}

export interface Session {
  id: string;
  intent_id: string;
  shared_by: string;
  /** Fork head the Worker read from Artifacts when the session was shared. */
  head_commit: string | null;
  /** The sharing agent's free-form handoff notes. */
  notes: string;
  snapshot: SessionSnapshot;
  created_at: string;
}

export interface NewSession {
  id: string;
  intent_id: string;
  head_commit: string | null;
  notes: string;
  actor: string;
}

const SESSION_HISTORY = 100;

export interface PushInput {
  paths: string[];
  commit: string | null;
  actor: string;
  /** Whether the caller holds the admin key; otherwise only the intent's author may report a push. */
  admin: boolean;
}

/** A decision the Worker derived itself from Artifacts objects. */
export interface ProposeInput {
  decision_id: string;
  head_commit: string;
  /** The fork and base the Worker diffed; must still be the intent's. */
  fork_repo: string;
  base_commit: string | null;
  decision: ServerDecision;
  /** Trunk commit and digest of the policy the decision was evaluated under. */
  policy_commit: string | null;
  policy_sha256: string;
  paths: string[];
  /** Lines added and removed per file, for the review screen's diff summary. */
  files?: FileStat[];
  truncated_reasons: string[];
  /** Which commit(s) the change was diffed from (ownchange.ts). */
  diff_base?: DiffBase;
  client_preview: Record<string, unknown> | null;
  actor: string;
}

export interface FileStat {
  path: string;
  added: number;
  deleted: number;
}

const DECISION_STATUSES: readonly string[] = ["approved", "rejected", "pending_agent", "pending_human", "quarantined", "blocked", "superseded"];

export interface DecideInput {
  verdict: "approve" | "reject";
  /** The identity the verdict counts as (index.ts approver): a bound name, or "admin" for the admin key. */
  by: string;
  /** The name the admin key reported; recorded, never counted. */
  claimed_by?: string | null;
  reason: string;
  /** The decider's declared model family, already normalised; null if none. */
  family?: string | null;
  /** Whether the caller is a person (admin key or a human key); see auth.ts. */
  person: boolean;
  /** How the decider signed in: "admin", "key", "access" (Cloudflare Access) or "ai" (the Workers AI reviewer). */
  identity_source?: string;
  /** The AI reviewer's model, gateway, latency and tokens (aireview.ts); audited with the verdict. */
  ai?: Record<string, unknown>;
}

/** AI advice for the person a decision waits on (aiadvice.ts); never a verdict. */
export interface AdviseInput {
  by: string;
  family: string;
  identity_source: string;
  advice: Record<string, unknown>;
}

/** An AI reviewer handing an agent_review decision to a person (aireview.ts). */
export interface EscalateInput {
  by: string;
  reason: string;
  concerns: string[];
  family: string;
  identity_source: string;
  ai: Record<string, unknown>;
}

const SCHEMA = `
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS intents (
  id             TEXT PRIMARY KEY,
  goal           TEXT NOT NULL,
  reason         TEXT NOT NULL,
  agent          TEXT NOT NULL,
  declared_paths TEXT NOT NULL,  -- JSON array
  actual_paths   TEXT NOT NULL,  -- JSON array
  fork_repo      TEXT NOT NULL,
  fork_remote    TEXT NOT NULL,
  head_commit    TEXT,
  status         TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS decisions (
  id               TEXT PRIMARY KEY,
  intent_id        TEXT NOT NULL REFERENCES intents(id),
  commit_sha       TEXT,
  tags             TEXT NOT NULL,  -- JSON object
  matched_rule     TEXT,
  action           TEXT NOT NULL,
  reason           TEXT NOT NULL,
  status           TEXT NOT NULL,
  approvals_needed INTEGER NOT NULL,
  decided_by       TEXT,
  decided_at       TEXT,
  created_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS decisions_commit ON decisions(commit_sha);
-- Token metadata only (id, never the secret), so tokens can be revoked.
CREATE TABLE IF NOT EXISTS tokens (
  id         TEXT PRIMARY KEY,
  repo       TEXT NOT NULL,
  intent_id  TEXT,
  purpose    TEXT NOT NULL,   -- fork
  expires_at TEXT NOT NULL,
  revoked_at TEXT,
  created_at TEXT NOT NULL
);
-- One row per trunk write token issued to land an approved change.
CREATE TABLE IF NOT EXISTS lands (
  id            TEXT PRIMARY KEY,
  intent_id     TEXT NOT NULL REFERENCES intents(id),
  decision_id   TEXT NOT NULL REFERENCES decisions(id),
  token_id      TEXT,
  status        TEXT NOT NULL,
  expires_at    TEXT NOT NULL,
  landed_commit TEXT,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS approvals (
  decision_id TEXT NOT NULL REFERENCES decisions(id),
  by          TEXT NOT NULL,
  verdict     TEXT NOT NULL,
  reason      TEXT NOT NULL,
  at          TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,
  at          TEXT NOT NULL,
  kind        TEXT NOT NULL,
  actor       TEXT NOT NULL,
  intent_id   TEXT,
  decision_id TEXT,
  data        TEXT NOT NULL  -- JSON object
);
-- The audit log is append-only. No code path updates or deletes it, and the
-- database refuses to as well.
CREATE TRIGGER IF NOT EXISTS audit_no_update BEFORE UPDATE ON audit
  BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS audit_no_delete BEFORE DELETE ON audit
  BEGIN SELECT RAISE(ABORT, 'audit log is append-only'); END;
CREATE TRIGGER IF NOT EXISTS approvals_no_update BEFORE UPDATE ON approvals
  BEGIN SELECT RAISE(ABORT, 'approvals are append-only'); END;
CREATE TRIGGER IF NOT EXISTS approvals_no_delete BEFORE DELETE ON approvals
  BEGIN SELECT RAISE(ABORT, 'approvals are append-only'); END;
-- Shared session snapshots (wayglass session share). Append-only: a session
-- URL always shows what was handed on, even after the intent moves on.
CREATE TABLE IF NOT EXISTS sessions (
  id          TEXT PRIMARY KEY,
  intent_id   TEXT NOT NULL REFERENCES intents(id),
  shared_by   TEXT NOT NULL,
  head_commit TEXT,
  notes       TEXT NOT NULL,
  snapshot    TEXT NOT NULL,  -- JSON SessionSnapshot
  created_at  TEXT NOT NULL
);
-- Artifacts binding calls the Worker made, per intent ('' = the project
-- itself) and operation name (src/cost.ts). Counters only, no payloads.
CREATE TABLE IF NOT EXISTS costs (
  intent_id TEXT NOT NULL,
  op        TEXT NOT NULL,
  count     INTEGER NOT NULL,
  PRIMARY KEY (intent_id, op)
);
-- AI reviewer and adviser calls (src/aireview.ts, src/aiadvice.ts), per UTC
-- day, kind (review, advice), model and outcome: counters, tokens and the
-- list-price estimate. The daily budget (AI_DAILY_REVIEWS) counts calls here.
CREATE TABLE IF NOT EXISTS ai_budget (day TEXT PRIMARY KEY, calls INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS ai_usage (
  day               TEXT NOT NULL,
  kind              TEXT NOT NULL,
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  outcome           TEXT NOT NULL,
  requests          INTEGER NOT NULL,  -- reviews or advice written
  calls             INTEGER NOT NULL,  -- model calls made (a fallback adds one)
  prompt_tokens     INTEGER NOT NULL,
  completion_tokens INTEGER NOT NULL,
  est_usd           REAL NOT NULL,
  unpriced          INTEGER NOT NULL,
  PRIMARY KEY (day, kind, provider, model, outcome)
);
-- Fork repos an intent left behind when it was re-stacked onto another
-- intent (wayglass intent restack): kept like an abandoned intent's fork,
-- and removed with the project.
CREATE TABLE IF NOT EXISTS retired_forks (
  repo       TEXT PRIMARY KEY,
  intent_id  TEXT NOT NULL,
  retired_at TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS sessions_no_update BEFORE UPDATE ON sessions
  BEGIN SELECT RAISE(ABORT, 'sessions are append-only'); END;
CREATE TRIGGER IF NOT EXISTS sessions_no_delete BEFORE DELETE ON sessions
  BEGIN SELECT RAISE(ABORT, 'sessions are append-only'); END;
`;

/** Columns added after v1 of the schema; added in place on older databases. */
const ADDED_COLUMNS: [table: string, column: string, decl: string][] = [
  ["intents", "base_commit", "TEXT"],
  ["decisions", "base_commit", "TEXT"],
  ["decisions", "paths", "TEXT NOT NULL DEFAULT '[]'"],
  ["decisions", "detail", "TEXT NOT NULL DEFAULT '{}'"],
  ["decisions", "client_preview", "TEXT"],
  ["decisions", "landed_commit", "TEXT"],
  ["decisions", "policy_commit", "TEXT"],
  ["decisions", "policy_sha256", "TEXT"],
  ["intents", "parent_id", "TEXT"],
  ["intents", "trunk_base", "TEXT"],
  ["lands", "trunk_before", "TEXT"],
  ["lands", "token_revoked", "INTEGER"],
  ["intents", "parent_intent_id", "TEXT"],
  ["intents", "session_id", "TEXT"],
  ["intents", "agent_family", "TEXT"],
  ["approvals", "family", "TEXT"],
  ["intents", "last_push_at", "TEXT"],
  ["intents", "abandoned_by", "TEXT"],
  ["intents", "abandoned_reason", "TEXT"],
  ["approvals", "identity_source", "TEXT"],
  ["approvals", "claimed_by", "TEXT"],
];

/** Default for the STALE_INTENT_HOURS var: an intent with no push for a day is reported stale. */
export const DEFAULT_STALE_HOURS = 24;

type Row = Record<string, SqlStorageValue>;

const strOrNull = (v: SqlStorageValue | undefined) => (v === null || v === undefined ? null : String(v));

const now = () => new Date().toISOString();

/** Who is asking to land: the request's actor, and whether it holds the admin key. */
export interface LandCaller {
  actor: string;
  admin: boolean;
}

/**
 * Only the intent's author or the admin key may land an approved change
 * (begin, finish or abort). Approval says the change may go in; it does not
 * let any other logged-in key push it to trunk in the author's name.
 */
export function landRefusal(intent: Intent, caller: LandCaller): Failure | null {
  if (caller.admin || caller.actor === intent.agent) return null;
  return failResult(
    "not_author",
    `only ${intent.agent} (the author of intent ${intent.id}) or the admin key may land it; ${caller.actor} may not`,
  );
}

function rowToIntent(r: Row, staleBefore: string | null = null): Intent {
  const status = String(r.status) as IntentStatus;
  const lastPush = strOrNull(r.last_push_at);
  return {
    id: String(r.id),
    goal: String(r.goal),
    reason: String(r.reason),
    agent: String(r.agent),
    declared_paths: JSON.parse(String(r.declared_paths)) as string[],
    actual_paths: JSON.parse(String(r.actual_paths)) as string[],
    fork_repo: String(r.fork_repo),
    fork_remote: String(r.fork_remote),
    base_commit: strOrNull(r.base_commit),
    parent_id: strOrNull(r.parent_id),
    // Intents from before stacking: the base was always a trunk commit.
    trunk_base: strOrNull(r.trunk_base ?? r.base_commit),
    head_commit: r.head_commit === null ? null : String(r.head_commit),
    status,
    parent_intent_id: strOrNull(r.parent_intent_id),
    session_id: strOrNull(r.session_id),
    agent_family: strOrNull(r.agent_family),
    last_push_at: lastPush,
    stale: staleBefore !== null && ACTIVE_STATUSES.includes(status) && (lastPush ?? String(r.created_at)) < staleBefore,
    abandoned_by: strOrNull(r.abandoned_by),
    abandoned_reason: strOrNull(r.abandoned_reason),
    created_at: String(r.created_at),
    updated_at: String(r.updated_at),
  };
}

function rowToDecision(r: Row): Decision {
  return {
    id: String(r.id),
    intent_id: String(r.intent_id),
    commit_sha: r.commit_sha === null ? null : String(r.commit_sha),
    base_commit: strOrNull(r.base_commit),
    tags: JSON.parse(String(r.tags)) as Record<string, unknown>,
    matched_rule: r.matched_rule === null ? null : String(r.matched_rule),
    action: String(r.action) as Action,
    reason: String(r.reason),
    status: String(r.status) as DecisionStatus,
    approvals_needed: Number(r.approvals_needed),
    paths: JSON.parse(String(r.paths ?? "[]")) as string[],
    detail: JSON.parse(String(r.detail ?? "{}")) as Record<string, unknown>,
    client_preview: r.client_preview == null ? null : (JSON.parse(String(r.client_preview)) as Record<string, unknown>),
    landed_commit: strOrNull(r.landed_commit),
    policy_commit: strOrNull(r.policy_commit),
    policy_sha256: strOrNull(r.policy_sha256),
    decided_by: r.decided_by === null ? null : String(r.decided_by),
    decided_at: r.decided_at === null ? null : String(r.decided_at),
    created_at: String(r.created_at),
  };
}

function short(sha: string | null): string {
  return sha ? sha.slice(0, 7) : "none";
}

function rowToLand(r: Row): Land {
  return {
    id: String(r.id),
    intent_id: String(r.intent_id),
    decision_id: String(r.decision_id),
    token_id: strOrNull(r.token_id),
    status: String(r.status) as LandStatus,
    trunk_before: strOrNull(r.trunk_before),
    expires_at: String(r.expires_at),
    landed_commit: strOrNull(r.landed_commit),
    token_revoked: r.token_revoked === null || r.token_revoked === undefined ? null : Number(r.token_revoked) === 1,
    created_at: String(r.created_at),
    updated_at: String(r.updated_at),
  };
}

function rowToAudit(r: Row): AuditEvent {
  return {
    seq: Number(r.seq),
    at: String(r.at),
    kind: String(r.kind),
    actor: String(r.actor),
    intent_id: r.intent_id === null ? null : String(r.intent_id),
    decision_id: r.decision_id === null ? null : String(r.decision_id),
    data: JSON.parse(String(r.data)) as Record<string, unknown>,
  };
}

/** Initial decision status for a policy action. */
function statusFor(action: Action): DecisionStatus {
  switch (action) {
    case "auto_merge":
      return "approved";
    case "agent_review":
      return "pending_agent";
    case "human_approve":
    case "multi_approve":
      return "pending_human";
    case "quarantine":
      return "quarantined";
    case "block":
      return "blocked";
  }
}

// Written by markMoved after the project's state was copied, verified and
// wiped here (src/residency.ts). Kept apart from meta so info() stays null.
const MOVED_SCHEMA = `
CREATE TABLE IF NOT EXISTS residency_moved (
  jurisdiction TEXT NOT NULL,
  at           TEXT NOT NULL,
  counts       TEXT NOT NULL  -- JSON {table: rows}
);
`;

/** Where a project's state went, when this object no longer holds it. */
export interface ResidencyStatus {
  moved_to: string | null;
  moved_at: string | null;
}

/** Rows of one table, in rowid order, with each row's rowid as `_rowid`. */
export interface RowPage {
  rows: Row[];
  /** The last rowid in `rows`; pass it back as `after`. */
  last: number;
  done: boolean;
}

/** Row counts and a SHA-256 over every row of every table, for verifying a copy. */
export interface StateDigest {
  counts: Record<string, number>;
  digest: string;
}

const PAGE_ROWS = 200;
const PAGE_BYTES = 1 << 20;
const IDENT = /^[a-z_][a-z0-9_]*$/;

export class Project extends DurableObject<Env> {
  private readonly sql: SqlStorage;
  /** Set once the state moved to a pinned object; every method then refuses (see refuseOnceMoved). */
  private moved: ResidencyStatus | null = null;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec(SCHEMA);
    for (const [table, column, decl] of ADDED_COLUMNS) {
      const has = this.sql.exec("SELECT 1 FROM pragma_table_info(?) WHERE name = ?", table, column).toArray().length > 0;
      if (!has) this.sql.exec(`ALTER TABLE ${table} ADD COLUMN ${column} ${decl}`);
    }
    this.sql.exec(MOVED_SCHEMA);
    const moved = this.residencyStatus();
    if (moved.moved_to) this.moved = moved;
  }

  // ---- residency (src/residency.ts) -----------------------------------------

  residencyStatus(): ResidencyStatus {
    const r = this.sql.exec("SELECT jurisdiction, at FROM residency_moved LIMIT 1").toArray()[0];
    return r ? { moved_to: String(r.jurisdiction), moved_at: String(r.at) } : { moved_to: null, moved_at: null };
  }

  /** The project's own tables (never SQLite's, the KV shim's or the tombstone), by name. */
  private stateTables(): string[] {
    return this.sql
      .exec("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '\\_cf\\_%' ESCAPE '\\' AND name <> 'residency_moved' ORDER BY name")
      .toArray()
      .map((r) => String(r.name))
      .filter((t) => IDENT.test(t));
  }

  private columnsOf(table: string): string[] {
    return this.sql
      .exec("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
      .toArray()
      .map((r) => String(r.name));
  }

  /** Whether the audit log has an event of this kind. */
  hasAudit(kind: string): boolean {
    return this.sql.exec("SELECT 1 FROM audit WHERE kind = ? LIMIT 1", kind).toArray().length > 0;
  }

  /** A page of a table's rows after rowid `after`, for copying this object's state verbatim. */
  exportRows(table: string, after = 0): RowPage {
    if (!this.stateTables().includes(table)) throw new Error(`no such table: ${table}`);
    const rows = this.sql.exec(`SELECT rowid AS _rowid, * FROM ${table} WHERE rowid > ? ORDER BY rowid LIMIT ?`, after, PAGE_ROWS + 1).toArray();
    let bytes = 0;
    const page: Row[] = [];
    for (const r of rows.slice(0, PAGE_ROWS)) {
      bytes += JSON.stringify(r).length;
      page.push(r);
      if (bytes > PAGE_BYTES) break;
    }
    const last = page.length ? Number(page[page.length - 1]!._rowid) : after;
    return { rows: page, last, done: page.length === rows.length };
  }

  /**
   * Inserts exported rows verbatim, rowid included, so sequence numbers and
   * order survive the move. A row already present (same rowid or key) is
   * left alone; whether it was identical is settled by the digest afterwards.
   */
  importRows(table: string, rows: Row[]): number {
    if (!this.stateTables().includes(table)) throw new Error(`no such table: ${table}`);
    const columns = this.columnsOf(table);
    let inserted = 0;
    this.ctx.storage.transactionSync(() => {
      for (const row of rows) {
        const names = Object.keys(row).filter((c) => c !== "_rowid");
        const unknown = names.find((c) => !columns.includes(c));
        if (unknown !== undefined) throw new Error(`table ${table} has no column ${unknown}`);
        const values = names.map((c) => row[c]!);
        const r = this.sql.exec(
          `INSERT OR IGNORE INTO ${table} (rowid${names.map((c) => `, ${c}`).join("")}) VALUES (?${", ?".repeat(names.length)})`,
          Number(row._rowid),
          ...values,
        );
        inserted += r.rowsWritten;
      }
    });
    return inserted;
  }

  /** Row counts per table and a SHA-256 over every row (rowid and columns by name), for comparing two objects. */
  async stateDigest(): Promise<StateDigest> {
    const counts: Record<string, number> = {};
    const lines: string[] = [];
    for (const table of this.stateTables()) {
      const rows = this.sql.exec(`SELECT rowid AS _rowid, * FROM ${table} ORDER BY rowid`).toArray();
      counts[table] = rows.length;
      for (const r of rows) {
        const cells = Object.keys(r)
          .sort()
          .map((k) => [k, r[k] instanceof ArrayBuffer ? [...new Uint8Array(r[k] as ArrayBuffer)] : r[k]]);
        lines.push(`${table}\t${JSON.stringify(cells)}`);
      }
    }
    const d = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(lines.join("\n")));
    return { counts, digest: [...new Uint8Array(d)].map((b) => b.toString(16).padStart(2, "0")).join("") };
  }

  /**
   * Erases this object's state and leaves only a tombstone naming where it
   * went, after the copy there was verified. From the next call on, every
   * method refuses (see refuseOnceMoved).
   */
  async markMoved(jurisdiction: string, counts: Record<string, number>): Promise<ResidencyStatus> {
    if (!this.info()) throw new Error("nothing to move: this object holds no project");
    await this.ctx.storage.deleteAll();
    this.sql.exec(MOVED_SCHEMA);
    const at = now();
    this.sql.exec("INSERT INTO residency_moved (jurisdiction, at, counts) VALUES (?, ?, ?)", jurisdiction, at, JSON.stringify(counts));
    this.moved = { moved_to: jurisdiction, moved_at: at };
    return this.moved;
  }

  // ---- project ------------------------------------------------------------

  info(): ProjectInfo | null {
    const rows = this.sql.exec("SELECT key, value FROM meta").toArray();
    if (rows.length === 0) return null;
    const m = Object.fromEntries(rows.map((r) => [String(r.key), String(r.value)]));
    return {
      name: m.name ?? "",
      trunk_repo: m.trunk_repo ?? "",
      trunk_remote: m.trunk_remote ?? "",
      jurisdiction: m.jurisdiction ?? "",
      created_at: m.created_at ?? "",
    };
  }

  init(p: { name: string; trunk_repo: string; trunk_remote: string; jurisdiction: string; actor: string; default_branch?: string }): Result<ProjectInfo> {
    if (this.info()) return failResult("conflict", `project ${p.name} already exists`);
    const at = now();
    this.ctx.storage.transactionSync(() => {
      for (const [k, v] of [
        ["name", p.name],
        ["trunk_repo", p.trunk_repo],
        ["trunk_remote", p.trunk_remote],
        ["jurisdiction", p.jurisdiction],
        ["created_at", at],
        ["default_branch", p.default_branch || DEFAULT_BRANCH],
      ] as const) {
        this.sql.exec("INSERT INTO meta (key, value) VALUES (?, ?)", k, v);
      }
      this.appendAudit("project.created", p.actor, null, null, { name: p.name, trunk_repo: p.trunk_repo, jurisdiction: p.jurisdiction });
    });
    return okResult(this.info()!);
  }

  /**
   * The project's directory row, in one call: counts and the last activity
   * come from SQL, so listing a namespace costs one RPC per project and no
   * Artifacts operation.
   */
  directoryEntry(): DirectoryEntry | null {
    const info = this.info();
    if (!info) return null;
    const one = (sql: string, ...args: SqlStorageValue[]) => this.sql.exec(sql, ...args).toArray()[0];
    const count = (sql: string, ...args: SqlStorageValue[]) => Number(one(sql, ...args)?.n ?? 0);
    const branch = one("SELECT value FROM meta WHERE key = 'default_branch'")?.value;
    return {
      ...info,
      default_branch: branch ? String(branch) : DEFAULT_BRANCH,
      public: this.isPublic(),
      last_activity: strOrNull(one("SELECT MAX(at) AS at FROM audit")?.at),
      last_push_at: strOrNull(one("SELECT MAX(last_push_at) AS at FROM intents")?.at),
      counts: {
        intents: count("SELECT COUNT(*) AS n FROM intents"),
        open: count("SELECT COUNT(*) AS n FROM intents WHERE status IN ('open', 'proposed')"),
        agents: count("SELECT COUNT(DISTINCT agent) AS n FROM intents"),
        pending_human: count(
          "SELECT COUNT(*) AS n FROM decisions d JOIN intents i ON i.id = d.intent_id WHERE d.status IN ('pending_human', 'quarantined') AND i.status <> 'abandoned'",
        ),
        landed: count("SELECT COUNT(*) AS n FROM intents WHERE status = 'landed'"),
      },
    };
  }

  /** Whether the admin opted this project into the public read routes (src/public.ts). Default off. */
  isPublic(): boolean {
    return this.sql.exec("SELECT value FROM meta WHERE key = 'public'").toArray()[0]?.value === "true";
  }

  /** Turns the public read view on or off. Audited, so the trail shows who exposed it and when. */
  setPublic(on: boolean, actor: string): Result<{ public: boolean }> {
    if (!this.info()) return failResult("not_found", "project not found");
    if (this.isPublic() === on) return okResult({ public: on });
    this.ctx.storage.transactionSync(() => {
      this.sql.exec("INSERT INTO meta (key, value) VALUES ('public', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", String(on));
      this.appendAudit(on ? "project.made_public" : "project.made_private", actor, null, null, {});
    });
    return okResult({ public: on });
  }

  // ---- delete (DELETE /v1/projects/:name; throwaway projects only) ----------

  /**
   * Everything a project delete must clean up outside this object: every fork
   * repo it recorded (newest first, so a stacked fork goes before the fork it
   * was made from), the trunk, live token ids and trunk push subscriptions.
   */
  deletionPlan(): Result<DeletionPlan> {
    const info = this.info();
    if (!info) return failResult("not_found", "project not found");
    const at = now();
    // Newest fork first, in the order the forks were made (the audit log's
    // order): a restack forks an older intent from a newer one. Forks the log
    // does not name (none known) still go, after those.
    const forks = this.sql
      .exec("SELECT data FROM audit WHERE kind IN ('intent.declared', 'intent.restacked') ORDER BY seq DESC")
      .toArray()
      .map((r) => (JSON.parse(String(r.data)) as { fork_repo?: unknown }).fork_repo)
      .filter((f): f is string => typeof f === "string" && f !== "");
    for (const r of this.sql.exec("SELECT fork_repo AS repo FROM intents UNION ALL SELECT repo FROM retired_forks").toArray()) forks.push(String(r.repo));
    const tokens = this.sql
      .exec("SELECT id, repo FROM tokens WHERE revoked_at IS NULL AND expires_at > ?", at)
      .toArray()
      .map((r) => ({ repo: String(r.repo), id: String(r.id) }));
    for (const r of this.sql.exec("SELECT token_id FROM lands WHERE (status = 'issued' OR token_revoked = 0) AND token_id IS NOT NULL AND expires_at > ?", at).toArray()) {
      tokens.push({ repo: info.trunk_repo, id: String(r.token_id) });
    }
    const subscriptions = this.sql
      .exec("SELECT data FROM audit WHERE kind = 'trunk.events_subscribed'")
      .toArray()
      .map((r) => (JSON.parse(String(r.data)) as { subscription_id?: unknown }).subscription_id)
      .filter((id): id is string => typeof id === "string" && id !== "");
    const count = (table: string) => Number(this.sql.exec(`SELECT COUNT(*) AS n FROM ${table}`).one().n);
    return okResult({
      project: info,
      forks: [...new Set(forks)],
      tokens,
      subscription_ids: [...new Set(subscriptions)],
      counts: { intents: count("intents"), decisions: count("decisions"), audit_events: count("audit"), sessions: count("sessions") },
    });
  }

  /**
   * Erases this project's storage, append-only audit log included: the one
   * path that does, reached only from the admin-only, throwaway-only delete
   * route after its repos are gone. The empty schema is put back so this
   * object answers like a project that never existed.
   */
  async wipe(): Promise<void> {
    await this.ctx.storage.deleteAll();
    this.sql.exec(SCHEMA);
  }

  // ---- intents ------------------------------------------------------------

  createIntent(input: NewIntent): Result<{ intent: Intent; overlaps: Overlap[] }> {
    if (!this.info()) return failResult("not_found", "project not found");
    if (this.getIntent(input.id)) return failResult("conflict", `intent ${input.id} already exists`);
    if (input.parent_id) {
      // The route checked this before forking; the parent may have landed or
      // been abandoned since. A child of a finished parent could never land
      // (and an abandoned parent's re-stacking has already run without it).
      const parent = this.getIntent(input.parent_id);
      if (!parent) return failResult("not_found", `intent ${input.parent_id} not found`);
      if (!ACTIVE_STATUSES.includes(parent.status)) {
        return failResult("conflict", `intent ${input.parent_id} is ${parent.status}; stack only on an unlanded intent (a landed one is already on trunk)`);
      }
    }
    const at = now();
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        `INSERT INTO intents (id, goal, reason, agent, declared_paths, actual_paths, fork_repo, fork_remote, base_commit, parent_id, trunk_base, head_commit, status,
                              parent_intent_id, session_id, agent_family, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?, NULL, 'open', ?, ?, ?, ?, ?)`,
        input.id,
        input.goal,
        input.reason,
        input.agent,
        JSON.stringify(input.declared_paths),
        input.fork_repo,
        input.fork_remote,
        input.base_commit,
        input.parent_id,
        input.trunk_base,
        input.parent_intent_id ?? null,
        input.session_id ?? null,
        input.agent_family ?? null,
        at,
        at,
      );
      this.insertForkToken(input.id, input.fork_repo, input.fork_token, at);
      this.appendAudit("intent.declared", input.agent, input.id, null, {
        goal: input.goal,
        reason: input.reason,
        declared_paths: input.declared_paths,
        fork_repo: input.fork_repo,
        base_commit: input.base_commit,
        ...(input.agent_family ? { agent_family: input.agent_family } : {}),
        ...(input.parent_id ? { stacked_on: input.parent_id } : {}),
        ...(input.parent_intent_id ? { parent_intent_id: input.parent_intent_id, session_id: input.session_id ?? null } : {}),
      });
      if (input.parent_intent_id) {
        // Logged on the parent too, so its history shows who continued it.
        this.appendAudit("session.forked", input.agent, input.parent_intent_id, null, {
          session_id: input.session_id ?? null,
          child_intent_id: input.id,
          fork_repo: input.fork_repo,
        });
      }
    });
    const intent = this.getIntent(input.id)!;
    const overlaps = this.overlapsFor(intent);
    if (overlaps.length > 0) this.appendAudit("intent.overlap", "wayglass", intent.id, null, { overlaps });
    return okResult({ intent, overlaps });
  }

  listIntents(status?: IntentStatus): Result<{ intents: Intent[] }> {
    if (!this.info()) return failResult("not_found", "project not found");
    const rows = status
      ? this.sql.exec("SELECT * FROM intents WHERE status = ? ORDER BY created_at, id", status).toArray()
      : this.sql.exec("SELECT * FROM intents ORDER BY created_at, id").toArray();
    return okResult({ intents: rows.map((r) => this.toIntent(r)) });
  }

  /**
   * Records the files an agent actually changed on its fork, then re-checks
   * overlaps. Only the author or the admin: what an intent reports as pushed
   * feeds overlap warnings, the head shown in watch and its stale check, so
   * another agent's key must not be able to write it.
   */
  recordPush(id: string, input: PushInput): Result<{ intent: Intent; overlaps: Overlap[] }> {
    const intent = this.getIntent(id);
    if (!intent) return failResult("not_found", `intent ${id} not found`);
    if (!ACTIVE_STATUSES.includes(intent.status)) return failResult("conflict", `intent ${id} is ${intent.status}`);
    if (!input.admin && input.actor !== intent.agent) {
      return failResult("forbidden", `only ${intent.agent} (the author) or the admin may record a push for intent ${id}`);
    }
    const actual = [...new Set([...intent.actual_paths, ...input.paths])].sort();
    const at = now();
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "UPDATE intents SET actual_paths = ?, head_commit = COALESCE(?, head_commit), last_push_at = ?, updated_at = ? WHERE id = ?",
        JSON.stringify(actual),
        input.commit,
        at,
        at,
        id,
      );
      this.appendAudit("intent.pushed", input.actor, id, null, { paths: input.paths, commit: input.commit });
    });
    const updated = this.getIntent(id)!;
    const overlaps = this.overlapsFor(updated);
    if (overlaps.length > 0) this.appendAudit("intent.overlap", "wayglass", id, null, { overlaps });
    return okResult({ intent: updated, overlaps });
  }

  /** Overlaps between one intent and every other active intent (declared + actual paths). */
  overlapsFor(intent: Intent): Overlap[] {
    const mine = [...new Set([...intent.declared_paths, ...intent.actual_paths])];
    const placeholders = ACTIVE_STATUSES.map(() => "?").join(",");
    const others = this.sql
      .exec(`SELECT * FROM intents WHERE id != ? AND status IN (${placeholders}) ORDER BY created_at, id`, intent.id, ...ACTIVE_STATUSES)
      .toArray()
      .map((r) => this.toIntent(r));
    // A stack shares its ancestors' code on purpose; that is not a conflict,
    // at any depth (declare --on chains, session forks of stacked intents).
    const parentOf = new Map<string, string | null>([[intent.id, intent.parent_id], ...others.map((o) => [o.id, o.parent_id] as [string, string | null])]);
    const inStack = (child: string, ancestor: string) => {
      for (let p = parentOf.get(child) ?? null, n = 0; p && n < 64; p = parentOf.get(p) ?? null, n++) if (p === ancestor) return true;
      return false;
    };
    const out: Overlap[] = [];
    for (const o of others) {
      if (inStack(intent.id, o.id) || inStack(o.id, intent.id)) continue;
      const paths = intersectSets(mine, [...new Set([...o.declared_paths, ...o.actual_paths])]);
      if (paths.length === 0) continue;
      const relation = o.id === intent.parent_intent_id ? "parent" : o.parent_intent_id === intent.id ? "child" : undefined;
      out.push({ intent_id: o.id, goal: o.goal, agent: o.agent, status: o.status, paths, ...(relation ? { relation } : {}), ...(o.stale ? { stale: true } : {}) });
    }
    return out;
  }

  /**
   * Abandons an open or proposed intent: only its author or the admin may.
   * The fork repo is kept (nothing is deleted); the Worker revokes its live
   * fork tokens and no new ones are minted. The intent drops out of overlap
   * checks, watch's open list and the review queues.
   *
   * Intents stacked on it take over its commits: each active child is
   * re-stacked onto the abandoned intent's own base (its parent, or trunk),
   * so the child's next proposal classifies the abandoned commits too. A
   * decision made before that no longer covers the whole change, so the
   * child must propose again before it can land (beginLand checks the base).
   */
  abandonIntent(
    id: string,
    input: { actor: string; admin: boolean; reason: string },
  ): Result<{ intent: Intent; restacked: string[]; fork_token_ids: string[] }> {
    if (!this.info()) return failResult("not_found", "project not found");
    const intent = this.getIntent(id);
    if (!intent) return failResult("not_found", `intent ${id} not found`);
    if (!ACTIVE_STATUSES.includes(intent.status)) return failResult("conflict", `intent ${id} is ${intent.status}`);
    if (!input.admin && input.actor !== intent.agent) {
      return failResult("forbidden", `only ${intent.agent} (the author) or the admin may abandon intent ${id}`);
    }
    const at = now();
    const landing = this.landInProgress(id, at);
    if (landing) return failResult("conflict", `intent ${id} is landing (${landing.id}); finish or abort the land first`);
    const children = this.sql
      .exec(`SELECT * FROM intents WHERE parent_id = ? AND status IN (${ACTIVE_STATUSES.map(() => "?").join(",")}) ORDER BY created_at, id`, id, ...ACTIVE_STATUSES)
      .toArray()
      .map((r) => this.toIntent(r));
    const tokenIds = this.sql
      .exec("SELECT id FROM tokens WHERE intent_id = ? AND purpose = 'fork' AND revoked_at IS NULL AND expires_at > ?", id, at)
      .toArray()
      .map((r) => String(r.id));
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "UPDATE intents SET status = 'abandoned', abandoned_by = ?, abandoned_reason = ?, updated_at = ? WHERE id = ?",
        input.actor,
        input.reason,
        at,
        id,
      );
      for (const c of children) {
        this.sql.exec("UPDATE intents SET parent_id = ?, base_commit = ?, updated_at = ? WHERE id = ?", intent.parent_id, intent.base_commit, at, c.id);
        const superseded = this.supersedeStale(c.id, intent.base_commit, "wayglass", `${id} was abandoned`, at);
        this.appendAudit("intent.restacked", "wayglass", c.id, null, {
          reason: `${id} was abandoned; this intent now carries its commits`,
          from_parent: id,
          to_parent: intent.parent_id,
          base_commit: intent.base_commit,
          previous_base_commit: c.base_commit,
          ...(superseded ? { superseded_decision: superseded } : {}),
        });
      }
      this.appendAudit("intent.abandoned", input.actor, id, null, {
        reason: input.reason,
        by_admin: input.admin && input.actor !== intent.agent,
        fork_repo: intent.fork_repo,
        fork_kept: true,
        restacked: children.map((c) => c.id),
      });
    });
    return okResult({ intent: this.getIntent(id)!, restacked: children.map((c) => c.id), fork_token_ids: tokenIds });
  }

  /**
   * Checks that intent `id` may be re-stacked onto `on` (wayglass intent
   * restack): only its author or the admin; both active; `on` is not the
   * intent itself or stacked on it at any depth (a cycle); no land of `id`
   * in progress. The route then forks `on`'s fork and calls restackIntent,
   * which checks again inside its transaction.
   */
  restackCheck(
    id: string,
    on: string,
    caller: { actor: string; admin: boolean },
  ): Result<{ project: ProjectInfo; intent: Intent; parent: Intent; retired: number }> {
    const project = this.info();
    if (!project) return failResult("not_found", "project not found");
    const intent = this.getIntent(id);
    if (!intent) return failResult("not_found", `intent ${id} not found`);
    if (!ACTIVE_STATUSES.includes(intent.status)) return failResult("conflict", `intent ${id} is ${intent.status}; only an open or proposed intent can be re-stacked`);
    if (!caller.admin && caller.actor !== intent.agent) {
      return failResult("forbidden", `only ${intent.agent} (the author) or the admin may restack intent ${id}`);
    }
    if (on === id) return failResult("conflict", `intent ${id} cannot be stacked on itself`);
    const parent = this.getIntent(on);
    if (!parent) return failResult("not_found", `intent ${on} not found`);
    if (parent.status === "landed") {
      return failResult("conflict", `intent ${on} is landed, so its change is already on trunk; run wayglass sync ${id} in its clone to merge the trunk instead of stacking on it`);
    }
    if (!ACTIVE_STATUSES.includes(parent.status)) return failResult("conflict", `intent ${on} is ${parent.status}; stack only on an open or proposed intent`);
    for (let p: Intent | null = parent, n = 0; p?.parent_id && n < 64; n++) {
      if (p.parent_id === id) return failResult("conflict", `intent ${on} is stacked on ${id}; stacking ${id} on it would make a cycle`);
      p = this.getIntent(p.parent_id);
    }
    const landing = this.landInProgress(id, now());
    if (landing) return failResult("conflict", `intent ${id} is landing (${landing.id}); finish or abort the land first`);
    const retired = Number(this.sql.exec("SELECT COUNT(*) AS n FROM retired_forks WHERE intent_id = ?", id).one().n);
    return okResult({ project, intent, parent, retired });
  }

  /**
   * Records a restack: the intent moves to the fork the route made from its
   * new parent's fork, and diffs from that parent's head from now on. Its
   * old fork is kept (retired_forks) and its live tokens there are returned
   * for the route to revoke. A decision made before this covered another
   * base, so beginLand refuses it until the intent proposes again. Audited
   * as intent.restacked with the caller's reason.
   */
  restackIntent(
    id: string,
    input: {
      on: string;
      actor: string;
      admin: boolean;
      reason: string;
      fork_repo: string;
      fork_remote: string;
      base_commit: string | null;
      parent_id: string | null;
      trunk_base: string | null;
      fork_token: { id: string; expires_at: string };
    },
  ): Result<{ intent: Intent; previous: { parent_id: string | null; base_commit: string | null; fork_repo: string; fork_remote: string }; superseded_decision: string | null; old_token_ids: string[]; overlaps: Overlap[] }> {
    const check = this.restackCheck(id, input.on, input);
    if (!check.ok) return check;
    const { intent } = check.data;
    const at = now();
    const oldTokens = this.sql
      .exec("SELECT id FROM tokens WHERE intent_id = ? AND repo = ? AND purpose = 'fork' AND revoked_at IS NULL AND expires_at > ?", id, intent.fork_repo, at)
      .toArray()
      .map((r) => String(r.id));
    // Any decision on another base no longer covers the change (land refuses
    // it); one still pending or approved is also marked superseded so nobody
    // decides or lands it, and every view says so.
    let superseded: string | null = null;
    this.ctx.storage.transactionSync(() => {
      superseded = this.supersedeStale(id, input.base_commit, input.actor, `restacked onto ${input.on}`, at);
      this.sql.exec("INSERT INTO retired_forks (repo, intent_id, retired_at) VALUES (?, ?, ?)", intent.fork_repo, id, at);
      this.sql.exec(
        `UPDATE intents SET fork_repo = ?, fork_remote = ?, parent_id = ?, base_commit = ?, trunk_base = ?, head_commit = NULL,
           updated_at = ? WHERE id = ?`,
        input.fork_repo,
        input.fork_remote,
        input.parent_id,
        input.base_commit,
        input.trunk_base,
        at,
        id,
      );
      this.insertForkToken(id, input.fork_repo, input.fork_token, at);
      this.appendAudit("intent.restacked", input.actor, id, null, {
        reason: input.reason,
        by_admin: input.admin && input.actor !== intent.agent,
        from_parent: intent.parent_id,
        to_parent: input.parent_id,
        base_commit: input.base_commit,
        previous_base_commit: intent.base_commit,
        fork_repo: input.fork_repo,
        previous_fork_repo: intent.fork_repo,
        previous_fork_kept: true,
        ...(superseded ? { superseded_decision: superseded } : {}),
      });
    });
    const updated = this.getIntent(id)!;
    return okResult({
      intent: updated,
      previous: { parent_id: intent.parent_id, base_commit: intent.base_commit, fork_repo: intent.fork_repo, fork_remote: intent.fork_remote },
      superseded_decision: superseded,
      old_token_ids: oldTokens,
      overlaps: this.overlapsFor(updated),
    });
  }

  /**
   * Marks the intent's latest decision superseded when it is still pending or
   * already approved and was made on another base than `base`: after a
   * restack it no longer covers the whole change, so nobody may decide or
   * land it and it leaves every review queue. An approval keeps who gave it:
   * decided_by/decided_at and the append-only approvals stay as they were,
   * and the audit records them again. Audited as decision.superseded. Call
   * inside a transaction. Returns the decision id, or null when nothing was
   * superseded.
   */
  private supersedeStale(intentId: string, base: string | null, actor: string, why: string, at: string): string | null {
    const d = this.latestDecision(intentId);
    if (!d || !SUPERSEDABLE_STATUSES.includes(d.status) || d.base_commit === base) return null;
    const approved = d.status === "approved";
    if (approved) {
      this.sql.exec("UPDATE decisions SET status = 'superseded' WHERE id = ?", d.id);
    } else {
      this.sql.exec("UPDATE decisions SET status = 'superseded', decided_by = ?, decided_at = ? WHERE id = ?", actor, at, d.id);
    }
    const approvedBy = approved ? [...new Set(this.approvalsFor(d.id).filter((a) => a.verdict === "approve").map((a) => a.by))] : [];
    this.appendAudit("decision.superseded", actor, intentId, d.id, {
      reason: approved
        ? `${why}; the approval covered another base, so the change must be proposed again and decided again`
        : `${why}; the change must be proposed again`,
      previous_status: d.status,
      previous_base_commit: d.base_commit,
      base_commit: base,
      ...(approved ? { previous_decided_by: d.decided_by, previous_decided_at: d.decided_at, approved_by: approvedBy } : {}),
    });
    return d.id;
  }

  /** Marks fork tokens the Worker revoked in Artifacts. Metadata only. */
  markTokensRevoked(ids: string[]): void {
    const at = now();
    for (const t of ids) this.sql.exec("UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL", at, t);
  }

  // ---- decisions ----------------------------------------------------------

  /** Read-only view the Worker needs to derive a decision for an intent. */
  intentContext(intentId: string): Result<{ project: ProjectInfo; intent: Intent; latest_decision: Decision | null }> {
    const project = this.info();
    if (!project) return failResult("not_found", "project not found");
    const intent = this.getIntent(intentId);
    if (!intent) return failResult("not_found", `intent ${intentId} not found`);
    return okResult({ project, intent, latest_decision: this.latestDecision(intentId) });
  }

  /**
   * Records a proposal and the decision the Worker derived itself: it read
   * the fork head and the trunk base from Artifacts, diffed them and ran the
   * policy evaluator. Client-reported tags arrive as client_preview and are
   * stored for comparison only.
   */
  propose(intentId: string, input: ProposeInput): Result<{ intent: Intent; decision: Decision; overlaps: Overlap[] }> {
    const intent = this.getIntent(intentId);
    if (!intent) return failResult("not_found", `intent ${intentId} not found`);
    if (!ACTIVE_STATUSES.includes(intent.status)) return failResult("conflict", `intent ${intentId} is ${intent.status}`);
    // The Worker read the intent, then Artifacts, then calls here: a restack in
    // between moved the intent to another fork and base, so what was diffed is
    // not the intent's change any more.
    if (input.fork_repo !== intent.fork_repo || input.base_commit !== intent.base_commit) {
      return failResult("conflict", `intent ${intentId} was re-stacked while it was being proposed; run wayglass propose again`);
    }
    // A trunk token for the earlier decision may be out: that land verifies
    // against its own decision, and a new one here would leave the intent with
    // a pending decision after it landed.
    const landing = this.landInProgress(intentId, now());
    if (landing) return failResult("conflict", `intent ${intentId} is landing (${landing.id}); finish or abort the land before proposing again`);
    const d = input.decision;
    // An AI reviewer sent the previous decision to a person. Proposing the
    // very same change again (same commit, base and policy) is a retry, not
    // a revision, so the escalation travels with it: otherwise an agent could
    // clear the human gate by proposing twice. A new commit is a revised
    // change; policy decides it afresh and the reviewer looks again.
    const previous = this.latestDecision(intentId);
    const carried =
      previous &&
      previous.status === "pending_human" &&
      previous.detail.escalation &&
      previous.commit_sha === input.head_commit &&
      previous.base_commit === input.base_commit &&
      previous.policy_sha256 === input.policy_sha256 &&
      (previous.detail.escalation as { from_action?: string }).from_action === d.action
        ? previous
        : null;
    const action: Action = carried ? "human_approve" : d.action;
    const reason = carried ? previous!.reason : d.reason;
    const status = statusFor(action);
    const approvalsNeeded = action === "multi_approve" ? Math.max(2, d.approvers) : 1;
    const at = now();
    const autoDecided = status === "approved" || status === "blocked";
    const detail = {
      ...(carried ? { escalation: { ...(carried.detail.escalation as Record<string, unknown>), carried_from: carried.id } } : {}),
      default: d.default,
      shadow_matches: d.shadow_matches ?? [],
      would_differ: d.would_differ ?? false,
      overrode: d.overrode ?? null,
      reviewer: d.reviewer ?? null,
      ai: d.ai ?? null,
      policy_error: d.policy_error ?? null,
      truncated: input.truncated_reasons,
      files: input.files ?? [],
      diff_base: input.diff_base ?? null,
    };
    const preview = input.client_preview;
    const previewAgrees = preview ? preview.action === d.action : null;
    const actual = [...new Set([...intent.actual_paths, ...input.paths])].sort();
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        `INSERT INTO decisions (id, intent_id, commit_sha, base_commit, tags, matched_rule, action, reason, status, approvals_needed,
                                paths, detail, client_preview, policy_commit, policy_sha256, decided_by, decided_at, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        input.decision_id,
        intentId,
        input.head_commit,
        input.base_commit,
        JSON.stringify(d.tags),
        d.matched_rule,
        action,
        reason,
        status,
        approvalsNeeded,
        JSON.stringify(input.paths),
        JSON.stringify(detail),
        preview ? JSON.stringify(preview) : null,
        input.policy_commit,
        input.policy_sha256,
        autoDecided ? "policy" : null,
        autoDecided ? at : null,
        at,
      );
      this.sql.exec(
        "UPDATE intents SET status = 'proposed', head_commit = ?, actual_paths = ?, updated_at = ? WHERE id = ?",
        input.head_commit,
        JSON.stringify(actual),
        at,
        intentId,
      );
      this.appendAudit("decision.proposed", input.actor, intentId, input.decision_id, {
        decided_by: "server",
        commit: input.head_commit,
        base_commit: input.base_commit,
        policy_commit: input.policy_commit,
        policy_sha256: input.policy_sha256,
        tags: d.tags,
        matched_rule: d.matched_rule,
        action,
        reason,
        status,
        ...(carried ? { rule_action: d.action, escalation_carried_from: carried.id } : {}),
        ...detail,
        client_preview: preview,
        preview_agrees: previewAgrees,
      });
    });
    const updated = this.getIntent(intentId)!;
    const overlaps = this.overlapsFor(updated);
    if (overlaps.length > 0) this.appendAudit("intent.overlap", "wayglass", intentId, null, { overlaps });
    return okResult({ intent: updated, decision: this.getDecision(input.decision_id)!, overlaps });
  }

  // ---- landing ------------------------------------------------------------

  /**
   * Reserves the right to land an intent. Only the intent's latest decision
   * counts, and only once it is approved (auto_merge, or approvals satisfied).
   * One land per project at a time: this also serialises pushes to trunk.
   */
  beginLand(intentId: string, landId: string, caller: LandCaller): Result<{ project: ProjectInfo; intent: Intent; decision: Decision }> {
    const project = this.info();
    if (!project) return failResult("not_found", "project not found");
    const intent = this.getIntent(intentId);
    if (!intent) return failResult("not_found", `intent ${intentId} not found`);
    const refused = landRefusal(intent, caller);
    if (refused) return refused;
    const actor = caller.actor;
    if (intent.status !== "proposed") return failResult("conflict", `intent ${intentId} is ${intent.status}; propose it first`);
    const decision = this.latestDecision(intentId);
    if (!decision) return failResult("conflict", `intent ${intentId} has no decision`);
    if (decision.status === "superseded") {
      return failResult("conflict", `decision ${decision.id} was superseded when intent ${intentId} was re-stacked; run wayglass propose again so the decision covers the whole change`);
    }
    if (decision.status !== "approved") {
      return failResult("conflict", `decision ${decision.id} is ${decision.status} (${decision.action}); it must be approved before landing`);
    }
    // Re-stacked after the decision (its parent was abandoned): the decision
    // classified a smaller diff than the one that would land.
    if (decision.base_commit && decision.base_commit !== intent.base_commit) {
      return failResult(
        "conflict",
        `intent ${intentId} was re-stacked onto ${intent.base_commit ?? "trunk"} after decision ${decision.id}; run wayglass propose again so the decision covers the whole change`,
      );
    }
    if (intent.parent_id) {
      const parent = this.getIntent(intent.parent_id);
      if (parent?.status !== "landed") {
        return failResult("conflict", `intent ${intentId} is stacked on ${intent.parent_id}, which is ${parent?.status ?? "missing"}; land ${intent.parent_id} first`);
      }
    }
    const at = now();
    this.sql.exec("UPDATE lands SET status = 'expired', updated_at = ? WHERE status IN ('reserved', 'issued') AND expires_at <= ?", at, at);
    const busy = this.sql.exec("SELECT * FROM lands WHERE status IN ('reserved', 'issued') LIMIT 1").toArray()[0];
    if (busy) {
      const b = rowToLand(busy);
      return failResult("conflict", `intent ${b.intent_id} is landing until ${b.expires_at}; try again after it finishes`);
    }
    // A closed land whose trunk token Artifacts did not confirm revoked still
    // holds a live write token: one land at a time means the slot waits for
    // that token to expire (or for the Worker to revoke it on a later try).
    const lingering = this.unrevokedLandTokens(at)[0];
    if (lingering) {
      return failResult(
        "conflict",
        `the trunk token of intent ${lingering.intent_id}'s last land could not be revoked and lives until ${lingering.expires_at}; one land at a time, so try again after that`,
      );
    }
    // A reservation lasts only long enough to mint the token.
    const expires = new Date(Date.now() + 60_000).toISOString();
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "INSERT INTO lands (id, intent_id, decision_id, token_id, status, trunk_before, expires_at, landed_commit, created_at, updated_at) VALUES (?, ?, ?, NULL, 'reserved', NULL, ?, NULL, ?, ?)",
        landId,
        intentId,
        decision.id,
        expires,
        at,
        at,
      );
      this.appendAudit("land.reserved", actor, intentId, decision.id, { land_id: landId, commit: decision.commit_sha });
    });
    return okResult({ project, intent, decision });
  }

  /** Records the trunk token minted for a reserved land (its id only). */
  issueLand(landId: string, token: { id: string; expires_at: string }, trunkBefore: string | null, actor: string): Result<{ land: Land }> {
    const land = this.getLand(landId);
    if (!land || land.status !== "reserved") return failResult("conflict", `land ${landId} is not reserved`);
    const at = now();
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "UPDATE lands SET status = 'issued', token_id = ?, trunk_before = ?, expires_at = ?, updated_at = ? WHERE id = ?",
        token.id,
        trunkBefore,
        token.expires_at,
        at,
        landId,
      );
      this.appendAudit("land.token_issued", actor, land.intent_id, land.decision_id, {
        land_id: landId,
        scope: "write",
        expires_at: token.expires_at,
        trunk_before: trunkBefore,
      });
    });
    return okResult({ land: this.getLand(landId)! });
  }

  cancelLand(landId: string, reason: string, actor: string): void {
    const land = this.getLand(landId);
    if (!land || land.status !== "reserved") return;
    this.ctx.storage.transactionSync(() => {
      this.sql.exec("UPDATE lands SET status = 'cancelled', updated_at = ? WHERE id = ?", now(), landId);
      this.appendAudit("land.cancelled", actor, land.intent_id, land.decision_id, { land_id: landId, reason });
    });
  }

  /**
   * Gives up an issued land the client could not finish (e.g. a merge
   * conflict), so the next intent can land without waiting for the token to
   * expire. The caller revokes the token. Nothing was verified, so the intent
   * stays proposed.
   */
  abortLand(intentId: string, reason: string, tokenRevoked: boolean, actor: string): Result<{ land: Land }> {
    const land = this.openLand(intentId);
    if (!land) return failResult("conflict", `intent ${intentId} has no land in progress`);
    this.ctx.storage.transactionSync(() => {
      this.sql.exec("UPDATE lands SET status = 'cancelled', token_revoked = ?, updated_at = ? WHERE id = ?", tokenRevoked ? 1 : 0, now(), land.id);
      this.appendAudit("land.aborted", actor, land.intent_id, land.decision_id, { land_id: land.id, reason, token_revoked: tokenRevoked });
    });
    return okResult({ land: this.getLand(land.id)! });
  }

  /** The issued land for an intent, with what the Worker needs to verify the landed commit. */
  landContext(intentId: string): Result<{ project: ProjectInfo; intent: Intent; decision: Decision; land: Land }> {
    const project = this.info();
    if (!project) return failResult("not_found", "project not found");
    const intent = this.getIntent(intentId);
    if (!intent) return failResult("not_found", `intent ${intentId} not found`);
    const land = this.openLand(intentId);
    if (!land) return failResult("conflict", `intent ${intentId} has no land in progress; call land first`);
    return okResult({ project, intent, decision: this.getDecision(land.decision_id)!, land });
  }

  /**
   * The land `landed` and `land/abort` close: the intent's newest issued
   * land, or one whose token expired before the client reported back (the
   * next beginLand marks those expired). The push may still have gone
   * through while the token lived, and the Worker verifies what trunk holds
   * either way; without this a late report would strand a landed change as
   * "proposed" with no record of the trunk commit.
   */
  private openLand(intentId: string): Land | null {
    const row = this.sql
      .exec(
        "SELECT * FROM lands WHERE intent_id = ? AND (status = 'issued' OR (status = 'expired' AND token_id IS NOT NULL)) ORDER BY created_at DESC LIMIT 1",
        intentId,
      )
      .toArray()[0];
    return row ? rowToLand(row) : null;
  }

  /** A land of this intent that still holds the project's land slot: reserved or issued, and not expired. */
  private landInProgress(intentId: string, at: string): Land | null {
    const row = this.sql
      .exec("SELECT * FROM lands WHERE intent_id = ? AND status IN ('reserved', 'issued') AND expires_at > ? LIMIT 1", intentId, at)
      .toArray()[0];
    return row ? rowToLand(row) : null;
  }

  /**
   * Closed lands whose trunk token Artifacts did not confirm revoked and
   * that has not expired: each still lets its holder push to trunk, so the
   * project's land slot stays busy (beginLand) until the Worker revokes it
   * (landTokenRevoked) or it expires.
   */
  unrevokedLandTokens(at = now()): Land[] {
    return this.sql
      .exec("SELECT * FROM lands WHERE token_revoked = 0 AND token_id IS NOT NULL AND expires_at > ? ORDER BY expires_at ASC", at)
      .toArray()
      .map(rowToLand);
  }

  /** Records that a later retry revoked a closed land's lingering trunk token, freeing the land slot. */
  landTokenRevoked(landId: string, actor: string): void {
    const land = this.getLand(landId);
    if (!land || land.token_revoked !== false) return;
    this.ctx.storage.transactionSync(() => {
      this.sql.exec("UPDATE lands SET token_revoked = 1, updated_at = ? WHERE id = ?", now(), landId);
      this.appendAudit("land.token_revoked", actor, land.intent_id, land.decision_id, { land_id: landId, token_expires_at: land.expires_at });
    });
  }

  /**
   * Closes a land. `ok` means the Worker verified the commit on trunk is the
   * approved change; otherwise the land is rejected and the mismatch is
   * logged. The trunk token has already been revoked either way.
   */
  finishLand(
    landId: string,
    input: { commit: string; ok: boolean; problems: string[]; token_revoked: boolean; actor: string },
  ): Result<{ intent: Intent; decision: Decision; land: Land }> {
    const land = this.getLand(landId);
    if (!land || this.openLand(land.intent_id)?.id !== landId) return failResult("conflict", `land ${landId} is not in progress`);
    const at = now();
    // The intent was abandoned after its token expired (abandon waits only for
    // a live land): the trunk commit is recorded, but an abandoned intent does
    // not become landed behind its author's back.
    const intent = this.getIntent(land.intent_id);
    const problems = intent?.status === "proposed" ? input.problems : [...input.problems, `intent ${land.intent_id} is ${intent?.status ?? "missing"}`];
    const ok = input.ok && problems.length === 0;
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "UPDATE lands SET status = ?, landed_commit = ?, token_revoked = ?, updated_at = ? WHERE id = ?",
        ok ? "landed" : "rejected",
        input.commit,
        input.token_revoked ? 1 : 0,
        at,
        landId,
      );
      if (ok) {
        this.sql.exec("UPDATE decisions SET landed_commit = ? WHERE id = ?", input.commit, land.decision_id);
        this.sql.exec("UPDATE intents SET status = 'landed', updated_at = ? WHERE id = ?", at, land.intent_id);
      }
      this.appendAudit(ok ? "intent.landed" : "land.mismatch", input.actor, land.intent_id, land.decision_id, {
        land_id: landId,
        commit: input.commit,
        token_revoked: input.token_revoked,
        problems,
      });
    });
    if (!ok && problems.length > input.problems.length) {
      return failResult("conflict", `trunk commit ${input.commit} was not recorded as landed: ${problems.join("; ")}`);
    }
    return okResult({ intent: this.getIntent(land.intent_id)!, decision: this.getDecision(land.decision_id)!, land: this.getLand(landId)! });
  }

  // ---- fork tokens --------------------------------------------------------

  /** Checks a fork token refresh: active intent, and only its own agent may ask. */
  forkTokenRequest(intentId: string, actor: string): Result<{ intent: Intent; active_token_ids: string[] }> {
    const intent = this.getIntent(intentId);
    if (!intent) return failResult("not_found", `intent ${intentId} not found`);
    if (!ACTIVE_STATUSES.includes(intent.status)) return failResult("conflict", `intent ${intentId} is ${intent.status}`);
    if (actor !== intent.agent) return failResult("forbidden", `only ${intent.agent} may refresh the token for intent ${intentId}`);
    const ids = this.sql
      .exec("SELECT id FROM tokens WHERE intent_id = ? AND purpose = 'fork' AND revoked_at IS NULL AND expires_at > ?", intentId, now())
      .toArray()
      .map((r) => String(r.id));
    return okResult({ intent, active_token_ids: ids });
  }

  recordForkToken(intentId: string, token: { id: string; expires_at: string }, revokedIds: string[], actor: string): Result<{ expires_at: string }> {
    const intent = this.getIntent(intentId);
    if (!intent) return failResult("not_found", `intent ${intentId} not found`);
    const at = now();
    this.ctx.storage.transactionSync(() => {
      this.insertForkToken(intentId, intent.fork_repo, token, at);
      for (const id of revokedIds) this.sql.exec("UPDATE tokens SET revoked_at = ? WHERE id = ?", at, id);
      this.appendAudit("token.refreshed", actor, intentId, null, { repo: intent.fork_repo, scope: "write", expires_at: token.expires_at, revoked: revokedIds.length });
    });
    return okResult({ expires_at: token.expires_at });
  }


  decide(decisionId: string, input: DecideInput): Result<{ decision: Decision; approvals: Approval[]; waiting_for: string | null }> {
    const d = this.getDecision(decisionId);
    if (!d) return failResult("not_found", `decision ${decisionId} not found`);
    if (d.status === "superseded") {
      return failResult(
        "superseded",
        `decision ${decisionId} was superseded when intent ${d.intent_id} was re-stacked, so it no longer covers the change; nothing was recorded. Next: its author must propose again ('wayglass propose'), then review the new decision`,
      );
    }
    if (!PENDING_STATUSES.includes(d.status)) {
      return failResult("conflict", `decision ${decisionId} is ${d.status} and cannot be changed`);
    }
    // Proposing again replaces the decision: a verdict on the earlier one is a
    // verdict on a change nobody can land, and a person who read it has not
    // read the replacement. Only the latest decision of an intent is decided.
    const latest = this.latestDecision(d.intent_id);
    if (latest && latest.id !== decisionId) {
      return failResult(
        "decision_replaced",
        `decision ${decisionId} (commit ${short(d.commit_sha)}) was replaced by decision ${latest.id} (commit ${short(latest.commit_sha)}) when intent ${d.intent_id} was proposed again; nothing was recorded. Review the new proposal, then decide ${latest.id}`,
      );
    }
    // A refused verdict is part of the record: who tried to decide what they
    // may not (an agent key on a change routed to a person, an author on its
    // own change) is what an audit of the gate asks first.
    const refuse = (code: Parameters<typeof failResult>[0], message: string): Failure => {
      this.appendAudit("decision.refused", input.by, d.intent_id, decisionId, {
        code,
        verdict: input.verdict,
        action: d.action,
        matched_rule: d.matched_rule,
        ...(input.claimed_by ? { claimed_by: input.claimed_by } : {}),
        ...(input.family ? { family: input.family } : {}),
        ...(input.identity_source ? { identity_source: input.identity_source } : {}),
      });
      return failResult(code, message);
    };
    // Policy sent this to a person: an agent key never decides it, whatever
    // name or family it reports. agent_review stays open to agent keys.
    if (HUMAN_STATUSES.includes(d.status) && !input.person) {
      return refuse(
        "needs_human",
        `policy routed decision ${decisionId} to a person (${d.action}, ${d.matched_rule ?? "default"}); an agent key cannot decide it. A person decides with the admin key or a key minted with 'wayglass keys create --human'`,
      );
    }
    // Policy routed this away from the author; the author deciding it would undo that.
    // A bound key cannot name itself. The admin key can, so the name it claims
    // is held to this too: that stops an honest mistake, not an admin set on it.
    const author = this.getIntent(d.intent_id);
    if (author && !ACTIVE_STATUSES.includes(author.status)) {
      return failResult("conflict", `intent ${author.id} is ${author.status}; its decision ${decisionId} can no longer be decided`);
    }
    if (author?.agent === input.by || (input.claimed_by && author?.agent === input.claimed_by)) {
      return refuse("conflict", "the agent that wrote a change cannot decide it");
    }
    // A rule can require an approver from another model family. Only
    // approvals are held to it: anyone allowed to decide may still reject.
    const reviewer = d.detail.reviewer as { different_family?: boolean } | null | undefined;
    if (input.verdict === "approve" && reviewer?.different_family) {
      const refused = familyRefusal(author?.agent_family ?? null, input.family ?? null);
      if (refused) return refuse(refused.code, refused.message);
    }
    const at = now();
    let waitingFor: string | null = null;
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "INSERT INTO approvals (decision_id, by, claimed_by, family, verdict, reason, at, identity_source) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        decisionId,
        input.by,
        input.claimed_by ?? null,
        input.family ?? null,
        input.verdict,
        input.reason,
        at,
        input.identity_source ?? null,
      );
      let next: DecisionStatus | null = null;
      if (input.verdict === "reject") {
        next = "rejected";
      } else {
        // Distinct identities, not distinct names: every admin-key approval
        // is "admin", so multi_approve needs another key for each further
        // approver, however the admin names itself.
        const approvers = Number(
          this.sql.exec("SELECT COUNT(DISTINCT by) AS n FROM approvals WHERE decision_id = ? AND verdict = 'approve'", decisionId).one().n,
        );
        if (approvers >= d.approvals_needed) next = "approved";
        else {
          waitingFor = `needs another approver: ${approvers} of ${d.approvals_needed} distinct approvers so far`;
          if (input.by === ADMIN_IDENTITY) waitingFor += "; the admin key counts once whatever name it gives, so the next approval needs another person's key";
        }
      }
      if (next) {
        this.sql.exec("UPDATE decisions SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?", next, input.by, at, decisionId);
      }
      this.appendAudit(`decision.${input.verdict}`, input.by, d.intent_id, decisionId, {
        reason: input.reason,
        status: next ?? d.status,
        ...(input.claimed_by ? { claimed_by: input.claimed_by } : {}),
        ...(input.family ? { family: input.family } : {}),
        ...(input.identity_source ? { identity_source: input.identity_source } : {}),
        ...(input.ai ? { ai: input.ai } : {}),
      });
    });
    return okResult({ decision: this.getDecision(decisionId)!, approvals: this.approvalsFor(decisionId), waiting_for: waitingFor });
  }

  /**
   * The AI reviewer hands an agent_review decision to a person: the action
   * becomes human_approve and the status pending_human, so every surface
   * that shows a person's queue picks it up, and only a person may decide
   * it from here on (decide's human gate). The AI's reason and concerns go
   * to the audit log (decision.escalated) and the decision's detail. Nothing
   * is approved or rejected; decided_by stays empty.
   */
  escalate(decisionId: string, input: EscalateInput): Result<{ decision: Decision; approvals: Approval[] }> {
    const d = this.getDecision(decisionId);
    if (!d) return failResult("not_found", `decision ${decisionId} not found`);
    if (d.status === "superseded") {
      return failResult("superseded", `decision ${decisionId} was superseded when intent ${d.intent_id} was re-stacked; its author must propose again`);
    }
    if (d.status !== "pending_agent") {
      return failResult("conflict", `decision ${decisionId} is ${d.status} (${d.action}); only an agent_review decision waiting for a reviewer can be escalated`);
    }
    const intent = this.getIntent(d.intent_id);
    if (intent && !ACTIVE_STATUSES.includes(intent.status)) {
      return failResult("conflict", `intent ${intent.id} is ${intent.status}; its decision ${decisionId} can no longer be decided`);
    }
    const at = now();
    // The decision's reason now says why it sits with a person; the rule's
    // own reason stays on the record as rule_reason.
    const escalation = { from_action: d.action, by: input.by, family: input.family, identity_source: input.identity_source, reason: input.reason, concerns: input.concerns, at, ai: input.ai, rule_reason: d.reason };
    const reason = `Escalated by AI reviewer: ${input.reason}`;
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "UPDATE decisions SET action = 'human_approve', status = 'pending_human', reason = ?, detail = ? WHERE id = ?",
        reason,
        JSON.stringify({ ...d.detail, escalation }),
        decisionId,
      );
      this.appendAudit("decision.escalated", input.by, d.intent_id, decisionId, {
        ...escalation,
        matched_rule: d.matched_rule,
        action: "human_approve",
        status: "pending_human",
        decision_reason: reason,
      });
    });
    return okResult({ decision: this.getDecision(decisionId)!, approvals: this.approvalsFor(decisionId) });
  }

  /**
   * Stores AI advice on a decision waiting for a person (aiadvice.ts) and
   * audits it. Nothing about the decision changes but detail.ai_advice: the
   * status, action, approvals and decider are exactly as before, so advice
   * can never approve, reject or move a change.
   */
  advise(decisionId: string, input: AdviseInput): Result<{ decision: Decision }> {
    const d = this.getDecision(decisionId);
    if (!d) return failResult("not_found", `decision ${decisionId} not found`);
    if (d.status === "superseded") {
      return failResult("superseded", `decision ${decisionId} was superseded when intent ${d.intent_id} was re-stacked; its author must propose again`);
    }
    if (!HUMAN_STATUSES.includes(d.status)) {
      return failResult("conflict", `decision ${decisionId} is ${d.status} (${d.action}); advice is for a change waiting for a person`);
    }
    const intent = this.getIntent(d.intent_id);
    if (intent && !ACTIVE_STATUSES.includes(intent.status)) {
      return failResult("conflict", `intent ${intent.id} is ${intent.status}; its decision ${decisionId} can no longer be advised on`);
    }
    const at = now();
    const ai_advice = { ...input.advice, by: input.by, family: input.family, identity_source: input.identity_source, at };
    this.ctx.storage.transactionSync(() => {
      this.sql.exec("UPDATE decisions SET detail = ? WHERE id = ?", JSON.stringify({ ...d.detail, ai_advice }), decisionId);
      this.appendAudit("decision.ai_advice", input.by, d.intent_id, decisionId, {
        ...ai_advice,
        matched_rule: d.matched_rule,
        action: d.action,
        status: d.status,
      });
    });
    return okResult({ decision: this.getDecision(decisionId)! });
  }

  /** "Why is this commit here?" Accepts a full SHA or a prefix of 7+ characters. */
  why(commit: string): Result<{ commit: string; intent: Intent; decision: Decision; approvals: Approval[] }> {
    // A landed trunk commit (merge or fast-forward) wins over a fork head.
    const prefix = `${commit}%`;
    const row =
      this.sql.exec("SELECT * FROM decisions WHERE landed_commit LIKE ? ORDER BY created_at DESC LIMIT 1", prefix).toArray()[0] ??
      this.sql.exec("SELECT * FROM decisions WHERE commit_sha LIKE ? ORDER BY created_at DESC LIMIT 1", prefix).toArray()[0];
    if (!row) return failResult("not_found", `no decision recorded for commit ${commit}`);
    const decision = rowToDecision(row);
    const landed = decision.landed_commit?.startsWith(commit) ? decision.landed_commit : null;
    return okResult({
      commit: landed ?? decision.commit_sha ?? commit,
      intent: this.getIntent(decision.intent_id)!,
      decision,
      approvals: this.approvalsFor(decision.id),
    });
  }

  /**
   * Decisions with their approvals, newest last, for watch and review. At
   * most `limit`, keeping the newest.
   */
  listDecisions(status?: string, limit = 200): Result<{ decisions: (Decision & { approvals: Approval[]; intent_status: IntentStatus | null })[] }> {
    if (!this.info()) return failResult("not_found", "project not found");
    if (status && !DECISION_STATUSES.includes(status)) return failResult("bad_request", "unknown status");
    const rows = status
      ? this.sql.exec("SELECT * FROM decisions WHERE status = ? ORDER BY created_at DESC, rowid DESC LIMIT ?", status, limit).toArray()
      : this.sql.exec("SELECT * FROM decisions ORDER BY created_at DESC, rowid DESC LIMIT ?", limit).toArray();
    const statuses = new Map<string, IntentStatus>();
    for (const r of this.sql.exec("SELECT id, status FROM intents").toArray()) statuses.set(String(r.id), String(r.status) as IntentStatus);
    // intent_status lets review queues skip decisions of abandoned intents.
    const decisions = rows.reverse().map((r) => {
      const d = rowToDecision(r);
      return { ...d, approvals: this.approvalsFor(d.id), intent_status: statuses.get(d.intent_id) ?? null };
    });
    return okResult({ decisions });
  }

  // ---- sessions -----------------------------------------------------------

  /**
   * Freezes what the next agent needs to continue an intent: the intent, its
   * decisions with approvals, live overlaps, its audit history and the
   * sharer's notes. The fork head comes from Artifacts, read by the Worker.
   */
  shareSession(input: NewSession): Result<{ session: Session }> {
    if (!this.info()) return failResult("not_found", "project not found");
    const intent = this.getIntent(input.intent_id);
    if (!intent) return failResult("not_found", `intent ${input.intent_id} not found`);
    if (!ACTIVE_STATUSES.includes(intent.status)) {
      return failResult("conflict", `intent ${intent.id} is ${intent.status}; only open or proposed intents can be shared`);
    }
    if (this.getSession(input.id)) return failResult("conflict", `session ${input.id} already exists`);
    const decisions = this.sql
      .exec("SELECT * FROM decisions WHERE intent_id = ? ORDER BY created_at, rowid", intent.id)
      .toArray()
      .map(rowToDecision)
      .map((d) => ({ ...d, approvals: this.approvalsFor(d.id) }));
    const history = this.sql
      .exec("SELECT * FROM (SELECT * FROM audit WHERE intent_id = ? ORDER BY seq DESC LIMIT ?) ORDER BY seq", intent.id, SESSION_HISTORY)
      .toArray()
      .map(rowToAudit);
    const snapshot: SessionSnapshot = { intent, decisions, overlaps: this.overlapsFor(intent), history };
    const at = now();
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "INSERT INTO sessions (id, intent_id, shared_by, head_commit, notes, snapshot, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
        input.id,
        intent.id,
        input.actor,
        input.head_commit,
        input.notes,
        JSON.stringify(snapshot),
        at,
      );
      this.appendAudit("session.shared", input.actor, intent.id, null, {
        session_id: input.id,
        head_commit: input.head_commit,
        notes_chars: input.notes.length,
        decisions: decisions.length,
      });
    });
    return okResult({ session: this.getSession(input.id)! });
  }

  /** A shared session and the intents forked from it so far. */
  sessionView(id: string): Result<{ project: ProjectInfo; session: Session; forks: Intent[] }> {
    const project = this.info();
    if (!project) return failResult("not_found", "project not found");
    const session = this.getSession(id);
    if (!session) return failResult("not_found", `session ${id} not found`);
    const forks = this.sql.exec("SELECT * FROM intents WHERE session_id = ? ORDER BY created_at, id", id).toArray().map((r) => this.toIntent(r));
    return okResult({ project, session, forks });
  }

  /** What the Worker needs to fork a session: the parent intent as it is now. */
  sessionForkContext(id: string): Result<{ project: ProjectInfo; session: Session; parent: Intent }> {
    const view = this.sessionView(id);
    if (!view.ok) return view;
    const parent = this.getIntent(view.data.session.intent_id)!;
    if (!ACTIVE_STATUSES.includes(parent.status)) {
      return failResult("conflict", `intent ${parent.id} is ${parent.status}; declare a new intent instead of forking its session`);
    }
    return okResult({ project: view.data.project, session: view.data.session, parent });
  }

  // ---- audit --------------------------------------------------------------

  listAudit(since = 0, limit = 100): Result<{ events: AuditEvent[]; next: number }> {
    if (!this.info()) return failResult("not_found", "project not found");
    const rows = this.sql.exec("SELECT * FROM audit WHERE seq > ? ORDER BY seq LIMIT ?", since, limit).toArray();
    const events = rows.map(rowToAudit);
    return okResult({ events, next: events.at(-1)?.seq ?? since });
  }

  /** Adds one request's Artifacts binding calls to an intent's (or, with null, the project's) counters. */
  recordCost(intentId: string | null, ops: Record<string, number>): boolean {
    if (!this.info()) return false;
    const key = intentId !== null && this.getIntent(intentId) !== null ? intentId : "";
    for (const [op, n] of Object.entries(ops)) {
      if (!Number.isInteger(n) || n <= 0) continue;
      this.sql.exec(
        "INSERT INTO costs (intent_id, op, count) VALUES (?, ?, ?) ON CONFLICT (intent_id, op) DO UPDATE SET count = count + excluded.count",
        key,
        op.slice(0, 32),
        n,
      );
    }
    return true;
  }

  /** What the project has cost in Artifacts operations, as far as the Worker can see. */
  /** Today's AI budget: what AI_DAILY_REVIEWS allows, what reviews and advice used, what is left. */
  aiBudget(dailyCalls?: number): Result<AiBudget> {
    if (!this.info()) return failResult("not_found", "project not found");
    return okResult(this.aiBudgetNow(dailyCalls));
  }

  /** Reserve before external I/O, so concurrent reviews share one hard limit. */
  reserveAiCall(dailyCalls?: number): Result<AiBudget> {
    if (!this.info()) return failResult("not_found", "project not found");
    return this.ctx.storage.transactionSync(() => {
      const budget = this.aiBudgetNow(dailyCalls);
      if (budget.remaining <= 0) return failResult("ai_budget", `the project's daily AI budget is used up (${budget.used_today} of ${budget.daily_calls} model calls on ${budget.day}, AI_DAILY_REVIEWS)`);
      this.sql.exec("INSERT INTO ai_budget (day, calls) VALUES (?, ?) ON CONFLICT(day) DO UPDATE SET calls = excluded.calls", budget.day, budget.used_today + 1);
      return okResult({ ...budget, used_today: budget.used_today + 1, remaining: budget.remaining - 1 });
    });
  }

  // The limit comes from the Worker's env (AI_DAILY_REVIEWS) via the caller
  // when it has one; this object's own env is the fallback.
  private aiBudgetNow(dailyCalls?: number): AiBudget {
    const day = utcDay();
    const daily_calls = dailyCalls ?? aiDailyLimit(this.env);
    const recorded = Number(this.sql.exec("SELECT COALESCE(SUM(calls), 0) AS n FROM ai_usage WHERE day = ?", day).one().n);
    const reserved = Number(this.sql.exec("SELECT calls FROM ai_budget WHERE day = ?", day).toArray()[0]?.calls ?? 0);
    const used = Math.max(recorded, reserved);
    return { day, daily_calls, used_today: used, remaining: Math.max(0, daily_calls - used) };
  }

  /** Adds one review's or advice's model calls to the day's counters. */
  recordAiUsage(input: AiUsageInput): boolean {
    if (!this.info()) return false;
    const calls = Number.isInteger(input.calls) && input.calls >= 0 ? input.calls : 1;
    const priced = input.est_usd !== null && Number.isFinite(input.est_usd);
    this.sql.exec(
      `INSERT INTO ai_usage (day, kind, provider, model, outcome, requests, calls, prompt_tokens, completion_tokens, est_usd, unpriced)
       VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?)
       ON CONFLICT (day, kind, provider, model, outcome) DO UPDATE SET
         requests = requests + 1,
         calls = calls + excluded.calls,
         prompt_tokens = prompt_tokens + excluded.prompt_tokens,
         completion_tokens = completion_tokens + excluded.completion_tokens,
         est_usd = est_usd + excluded.est_usd,
         unpriced = unpriced + excluded.unpriced`,
      utcDay(),
      input.kind,
      input.provider.slice(0, 32),
      input.model.slice(0, 128),
      input.outcome.slice(0, 32),
      calls,
      input.tokens?.prompt ?? 0,
      input.tokens?.completion ?? 0,
      priced ? input.est_usd : 0,
      priced ? 0 : calls,
    );
    return true;
  }

  /**
   * Records that the AI was not run on a decision (the daily budget is used
   * up) in the audit log, so a change that waited longer, or got no advice,
   * shows why.
   */
  noteAiSkipped(decisionId: string, data: Record<string, unknown>): boolean {
    const d = this.getDecision(decisionId);
    if (!d) return false;
    this.appendAudit("decision.ai_skipped", "wayglass", d.intent_id, decisionId, { ...data, action: d.action, status: d.status });
    return true;
  }

  private aiCost(dailyCalls?: number): AiCost {
    const total: AiCost["total"] = { ...aiCostLine(), reviews: 0, approves: 0, escalations: 0, guards: 0, advice: 0 };
    const today = aiCostLine();
    const by_model: Record<string, ReturnType<typeof aiCostLine>> = {};
    const day = utcDay();
    for (const r of this.sql.exec("SELECT day, kind, model, outcome, requests, calls, prompt_tokens, completion_tokens, est_usd, unpriced FROM ai_usage").toArray()) {
      const row = { calls: Number(r.calls), prompt_tokens: Number(r.prompt_tokens), completion_tokens: Number(r.completion_tokens), est_usd: Number(r.est_usd), unpriced: Number(r.unpriced) };
      addAiCost(total, row);
      if (String(r.day) === day) addAiCost(today, row);
      const model = String(r.model);
      by_model[model] ??= aiCostLine();
      addAiCost(by_model[model], row);
      // Outcomes count requests, not calls: a review that fell back is one review.
      const kind = String(r.kind);
      const outcome = String(r.outcome);
      const n = Number(r.requests);
      if (kind === "review") {
        total.reviews += n;
        if (outcome === "approve") total.approves += n;
        else if (outcome === "escalate") total.escalations += n;
        else total.guards += n;
      } else {
        total.advice += n;
      }
    }
    const tokens = total.prompt_tokens + total.completion_tokens;
    return {
      calls: total.calls,
      reviews: total.reviews,
      approvals: total.approves,
      escalations: total.escalations + total.guards,
      advice: total.advice,
      tokens,
      est_usd: total.est_usd,
      budget: this.aiBudgetNow(dailyCalls),
      total,
      today: { ...today, tokens: today.prompt_tokens + today.completion_tokens },
      by_model,
      basis: AI_COST_BASIS,
    };
  }

  costSummary(aiDailyCalls?: number): Result<CostSummary> {
    if (!this.info()) return failResult("not_found", "project not found");
    const per = new Map<string, Record<string, number>>();
    const all: Record<string, number> = {};
    for (const r of this.sql.exec("SELECT intent_id, op, count FROM costs").toArray()) {
      const id = String(r.intent_id);
      const op = String(r.op);
      const n = Number(r.count);
      const m = per.get(id) ?? {};
      m[op] = (m[op] ?? 0) + n;
      per.set(id, m);
      all[op] = (all[op] ?? 0) + n;
    }
    const intents: Record<string, ReturnType<typeof costLine>> = {};
    for (const [id, m] of per) if (id !== "") intents[id] = costLine(m);
    const events: Record<string, number> = {};
    for (const r of this.sql.exec("SELECT kind, COUNT(*) AS n FROM audit WHERE kind LIKE 'artifacts.repo.%' GROUP BY kind").toArray()) {
      events[String(r.kind).slice("artifacts.repo.".length)] = Number(r.n);
    }
    // Only intents metered from declare on: ones landed before metering
    // existed would pull the average down with zeros.
    const landed = this.sql
      .exec("SELECT id FROM intents WHERE status = 'landed'")
      .toArray()
      .map((r) => String(r.id))
      .filter((id) => (intents[id]?.by_op.fork ?? 0) > 0);
    let perLanded: CostSummary["per_landed_change"] = null;
    if (landed.length > 0) {
      const ops = Math.round((landed.reduce((sum, id) => sum + intents[id]!.ops, 0) / landed.length) * 10) / 10;
      perLanded = { landed: landed.length, ops, est_usd: estimateUsd(ops) };
    }
    return okResult({
      scope: COST_SCOPE,
      pricing: PRICING,
      total: costLine(all),
      project: costLine(per.get("") ?? {}),
      intents,
      events,
      per_landed_change: perLanded,
      ai: this.aiCost(aiDailyCalls),
    });
  }

  /**
   * Appends an event that came from outside the request path (e.g. the
   * Artifacts queue). Queues deliver at least once, so an event carrying an
   * `event_id` that this kind already recorded is dropped, not written twice.
   */
  recordExternalEvent(kind: string, actor: string, intentId: string | null, data: Record<string, unknown>): boolean {
    if (!this.info()) return false;
    if (typeof data.event_id === "string" && data.event_id !== "") {
      const dup = this.sql.exec("SELECT 1 FROM audit WHERE kind = ? AND json_extract(data, '$.event_id') = ? LIMIT 1", kind, data.event_id).toArray();
      if (dup.length > 0) return true;
    }
    const known = intentId !== null && this.getIntent(intentId) !== null;
    this.appendAudit(kind, actor, known ? intentId : null, null, data);
    return true;
  }

  // ---- internals ----------------------------------------------------------

  /** The ONLY write path to the audit table. INSERT only. */
  private appendAudit(kind: string, actor: string, intentId: string | null, decisionId: string | null, data: Record<string, unknown>): void {
    this.sql.exec(
      "INSERT INTO audit (at, kind, actor, intent_id, decision_id, data) VALUES (?, ?, ?, ?, ?, ?)",
      now(),
      kind,
      actor,
      intentId,
      decisionId,
      JSON.stringify(data),
    );
  }

  /** Cut-off for `stale`: intents whose last push (or declare) is older than this. */
  private staleBefore(): string {
    const hours = Number(this.env.STALE_INTENT_HOURS ?? DEFAULT_STALE_HOURS);
    const h = Number.isFinite(hours) && hours > 0 ? hours : DEFAULT_STALE_HOURS;
    return new Date(Date.now() - h * 3_600_000).toISOString();
  }

  private toIntent(r: Row): Intent {
    return rowToIntent(r, this.staleBefore());
  }

  private getIntent(id: string): Intent | null {
    const row = this.sql.exec("SELECT * FROM intents WHERE id = ?", id).toArray()[0];
    return row ? this.toIntent(row) : null;
  }

  private latestDecision(intentId: string): Decision | null {
    const row = this.sql.exec("SELECT * FROM decisions WHERE intent_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1", intentId).toArray()[0];
    return row ? rowToDecision(row) : null;
  }

  private getSession(id: string): Session | null {
    const r = this.sql.exec("SELECT * FROM sessions WHERE id = ?", id).toArray()[0];
    if (!r) return null;
    return {
      id: String(r.id),
      intent_id: String(r.intent_id),
      shared_by: String(r.shared_by),
      head_commit: strOrNull(r.head_commit),
      notes: String(r.notes),
      snapshot: JSON.parse(String(r.snapshot)) as SessionSnapshot,
      created_at: String(r.created_at),
    };
  }

  private getLand(id: string): Land | null {
    const row = this.sql.exec("SELECT * FROM lands WHERE id = ?", id).toArray()[0];
    return row ? rowToLand(row) : null;
  }

  private insertForkToken(intentId: string, repo: string, token: { id: string; expires_at: string }, at: string): void {
    this.sql.exec(
      "INSERT INTO tokens (id, repo, intent_id, purpose, expires_at, revoked_at, created_at) VALUES (?, ?, ?, 'fork', ?, NULL, ?)",
      token.id,
      repo,
      intentId,
      token.expires_at,
      at,
    );
  }

  private getDecision(id: string): Decision | null {
    const row = this.sql.exec("SELECT * FROM decisions WHERE id = ?", id).toArray()[0];
    return row ? rowToDecision(row) : null;
  }

  private approvalsFor(decisionId: string): Approval[] {
    return this.sql
      .exec("SELECT * FROM approvals WHERE decision_id = ? ORDER BY at, rowid", decisionId)
      .toArray()
      .map((r) => ({
        decision_id: String(r.decision_id),
        by: String(r.by),
        claimed_by: strOrNull(r.claimed_by),
        family: strOrNull(r.family),
        verdict: String(r.verdict) as "approve" | "reject",
        reason: String(r.reason),
        at: String(r.at),
        identity_source: strOrNull(r.identity_source),
      }));
  }
}

/**
 * Once a project's state has moved to a pinned object (markMoved), every
 * method of the object it left answers with an error naming where it went:
 * nothing is read from or written to the wrong jurisdiction, whatever path
 * reaches it. Wrapped on the prototype, where RPC looks methods up (an
 * instance property is not callable over RPC); only the status query is left
 * as is, so the move can be verified and a lost directory entry repaired.
 */
function refuseOnceMoved(): void {
  const keep = new Set(["constructor", "residencyStatus"]);
  const proto = Project.prototype as unknown as Record<string, unknown>;
  for (const name of Object.getOwnPropertyNames(proto)) {
    const original = proto[name];
    if (keep.has(name) || typeof original !== "function") continue;
    proto[name] = function (this: { moved: ResidencyStatus | null }, ...args: unknown[]) {
      const moved = this.moved;
      if (moved?.moved_to) throw new Error(`project_moved: this project's state moved to the ${moved.moved_to} jurisdiction on ${moved.moved_at}; this object holds nothing`);
      return (original as (...a: unknown[]) => unknown).apply(this, args);
    };
  }
}
refuseOnceMoved();
