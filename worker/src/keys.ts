// Per-agent API keys. The admin mints one per agent or person; the key is
// bound to that name, so a request made with it is attributed to that name
// whatever X-Wayglass-Agent, body.agent or body.by say. The Worker stores
// only a SHA-256 of each key: a leaked database row cannot be replayed.
//
// One Registry Durable Object (named "keys") holds every key for the Worker.

import { DurableObject } from "cloudflare:workers";
import { failResult, okResult, type Result } from "./envelope";

/** Every agent key starts with this, so auth can tell it from the admin key without a lookup. */
export const KEY_PREFIX = "wgk_";
/** Names an agent key can be bound to: what shows in the audit log. */
export const AGENT_NAME = /^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$/;
/**
 * The identity every admin-key verdict is recorded under, whatever name the
 * caller reports (kept as claimed_by). No key may be bound to it, so a bound
 * key and the admin key are never counted as one approver by accident.
 */
export const ADMIN_IDENTITY = "admin";
const KEY_ID = /^k[0-9a-f]{10}$/;

/**
 * Who holds a key. Only a person may decide a change policy routed to a
 * person (pending_human, quarantined); an agent key never can. The admin key
 * counts as a person. Keys minted before kinds existed are agents.
 */
export type KeyKind = "human" | "agent";
export const KEY_KINDS: readonly KeyKind[] = ["human", "agent"];

/**
 * How a key was minted: "key" by the admin (POST /v1/keys), "access" for a
 * person who signed in through Cloudflare Access (POST /v1/human/session).
 */
export type KeySource = "key" | "access";

export interface KeyInfo {
  id: string;
  agent: string;
  kind: KeyKind;
  source: KeySource;
  /** When the key stops working; null for keys that never expire (every admin-minted key). */
  expires_at: string | null;
  note: string;
  created_by: string;
  created_at: string;
  last_used_at: string | null;
  revoked_at: string | null;
}

const SCHEMA = `
CREATE TABLE IF NOT EXISTS keys (
  id TEXT PRIMARY KEY,
  hash TEXT NOT NULL UNIQUE,
  agent TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  last_used_at TEXT,
  revoked_at TEXT
);
`;

// Columns added after the first deploy: [column, declaration].
const ADDED_COLUMNS: readonly [string, string][] = [
  ["kind", "TEXT NOT NULL DEFAULT 'agent'"],
  ["source", "TEXT NOT NULL DEFAULT 'key'"],
  ["expires_at", "TEXT"],
];

// Append-only record of key minting and revocation. Project audit logs are
// per project; keys belong to the Worker, so their events live here.
const EVENTS_SCHEMA = `
CREATE TABLE IF NOT EXISTS key_events (
  seq    INTEGER PRIMARY KEY AUTOINCREMENT,
  at     TEXT NOT NULL,
  kind   TEXT NOT NULL,
  actor  TEXT NOT NULL,
  key_id TEXT NOT NULL,
  data   TEXT NOT NULL DEFAULT '{}'
);
CREATE TRIGGER IF NOT EXISTS key_events_no_update BEFORE UPDATE ON key_events
  BEGIN SELECT RAISE(ABORT, 'key_events are append-only'); END;
CREATE TRIGGER IF NOT EXISTS key_events_no_delete BEFORE DELETE ON key_events
  BEGIN SELECT RAISE(ABORT, 'key_events are append-only'); END;
`;

// Which jurisdiction each project's state lives in (src/residency.ts). Names
// and jurisdictions only: no project data, so this global object holds it.
const DIRECTORY_SCHEMA = `
CREATE TABLE IF NOT EXISTS project_directory (
  name          TEXT PRIMARY KEY,
  jurisdiction  TEXT NOT NULL,
  registered_at TEXT NOT NULL
);
`;

export interface KeyEvent {
  seq: number;
  at: string;
  kind: "key.created" | "key.revoked";
  actor: string;
  key_id: string;
  data: Record<string, unknown>;
}

/** What lookup found: a live key, a key past its expiry, or nothing. */
export type Lookup =
  | { id: string; agent: string; kind: KeyKind; source: KeySource; expires_at: string | null }
  | { expired: true; id: string }
  | null;

const encoder = new TextEncoder();

export async function hashKey(key: string): Promise<string> {
  const d = await crypto.subtle.digest("SHA-256", encoder.encode(key));
  return [...new Uint8Array(d)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

function hex(bytes: number): string {
  return [...crypto.getRandomValues(new Uint8Array(bytes))].map((b) => b.toString(16).padStart(2, "0")).join("");
}

const now = () => new Date().toISOString();

function row(r: Record<string, SqlStorageValue>): KeyInfo {
  return {
    id: String(r.id),
    agent: String(r.agent),
    kind: r.kind === "human" ? "human" : "agent",
    source: r.source === "access" ? "access" : "key",
    expires_at: r.expires_at == null ? null : String(r.expires_at),
    note: String(r.note ?? ""),
    created_by: String(r.created_by),
    created_at: String(r.created_at),
    last_used_at: r.last_used_at == null ? null : String(r.last_used_at),
    revoked_at: r.revoked_at == null ? null : String(r.revoked_at),
  };
}

export class Registry extends DurableObject<Env> {
  private readonly sql: SqlStorage;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    this.sql.exec(SCHEMA);
    for (const [column, decl] of ADDED_COLUMNS) {
      const has = this.sql.exec("SELECT 1 FROM pragma_table_info('keys') WHERE name = ?", column).toArray().length > 0;
      if (!has) this.sql.exec(`ALTER TABLE keys ADD COLUMN ${column} ${decl}`);
    }
    this.sql.exec(EVENTS_SCHEMA);
    this.sql.exec(DIRECTORY_SCHEMA);
  }

  // ---- project directory (src/residency.ts) --------------------------------

  /** The jurisdiction a project's state lives in; null when the name is not registered. */
  projectJurisdiction(name: string): string | null {
    const r = this.sql.exec("SELECT jurisdiction FROM project_directory WHERE name = ?", name).toArray()[0];
    return r ? String(r.jurisdiction) : null;
  }

  /** Records (or corrects) where a project's state lives. */
  registerProject(name: string, jurisdiction: string): void {
    this.sql.exec(
      "INSERT INTO project_directory (name, jurisdiction, registered_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET jurisdiction = excluded.jurisdiction, registered_at = excluded.registered_at",
      name,
      jurisdiction,
      now(),
    );
  }

  unregisterProject(name: string): void {
    this.sql.exec("DELETE FROM project_directory WHERE name = ?", name);
  }

  /** Every registered project and its jurisdiction, by name. */
  directory(): { name: string; jurisdiction: string; registered_at: string }[] {
    return this.sql
      .exec("SELECT name, jurisdiction, registered_at FROM project_directory ORDER BY name")
      .toArray()
      .map((r) => ({ name: String(r.name), jurisdiction: String(r.jurisdiction), registered_at: String(r.registered_at) }));
  }

  private event(kind: KeyEvent["kind"], actor: string, keyId: string, data: Record<string, unknown>): void {
    this.sql.exec("INSERT INTO key_events (at, kind, actor, key_id, data) VALUES (?, ?, ?, ?, ?)", now(), kind, actor, keyId, JSON.stringify(data));
  }

  /** Key events, oldest first, after seq `after`; at most 500. */
  events(after = 0): KeyEvent[] {
    return this.sql
      .exec("SELECT * FROM key_events WHERE seq > ? ORDER BY seq LIMIT 500", after)
      .toArray()
      .map((r) => ({ seq: Number(r.seq), at: String(r.at), kind: String(r.kind) as KeyEvent["kind"], actor: String(r.actor), key_id: String(r.key_id), data: JSON.parse(String(r.data)) as Record<string, unknown> }));
  }

  /** Mints a key for agent. The plaintext is returned once and never stored. */
  async create(
    agent: string,
    createdBy: string,
    note = "",
    kind: KeyKind = "agent",
    opts: { source?: KeySource; expiresAt?: string | null } = {},
  ): Promise<Result<{ key: string; info: KeyInfo }>> {
    if (!AGENT_NAME.test(agent)) return failResult("bad_request", "agent must be 1-128 letters, digits, '.', '_', '@', '+' or '-'");
    if (agent === ADMIN_IDENTITY) return failResult("bad_request", `'${ADMIN_IDENTITY}' names the admin key's verdicts; bind the key to a person's or agent's name`);
    if (!KEY_KINDS.includes(kind)) return failResult("bad_request", "kind must be human or agent");
    const source: KeySource = opts.source ?? "key";
    const expiresAt = opts.expiresAt ?? null;
    if (expiresAt !== null && !(Date.parse(expiresAt) > Date.now())) return failResult("bad_request", "expires_at must be in the future");
    const key = KEY_PREFIX + hex(32);
    const id = "k" + hex(5);
    const at = now();
    const hash = await hashKey(key);
    this.ctx.storage.transactionSync(() => {
      this.sql.exec(
        "INSERT INTO keys (id, hash, agent, kind, source, expires_at, note, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
        id,
        hash,
        agent,
        kind,
        source,
        expiresAt,
        note,
        createdBy,
        at,
      );
      this.event("key.created", createdBy, id, { agent, kind, source, ...(expiresAt ? { expires_at: expiresAt } : {}) });
    });
    return okResult({ key, info: { id, agent, kind, source, expires_at: expiresAt, note, created_by: createdBy, created_at: at, last_used_at: null, revoked_at: null } });
  }

  /** The agent a key hash belongs to; null if unknown or revoked; expired once past expires_at. */
  lookup(hash: string): Lookup {
    const r = this.sql.exec("SELECT id, agent, kind, source, expires_at FROM keys WHERE hash = ? AND revoked_at IS NULL", hash).toArray()[0];
    if (!r) return null;
    // Expiry is enforced here, on every request: a session key needs no cleanup to stop working.
    if (r.expires_at != null && !(Date.parse(String(r.expires_at)) > Date.now())) return { expired: true, id: String(r.id) };
    // Coarse on purpose: one write per key per minute at most.
    const at = now();
    this.sql.exec("UPDATE keys SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)", at, r.id, new Date(Date.now() - 60_000).toISOString());
    return {
      id: String(r.id),
      agent: String(r.agent),
      kind: r.kind === "human" ? "human" : "agent",
      source: r.source === "access" ? "access" : "key",
      expires_at: r.expires_at == null ? null : String(r.expires_at),
    };
  }

  /** Live keys; with includeRevoked, also revoked and expired ones. */
  list(includeRevoked = false): KeyInfo[] {
    const q = includeRevoked
      ? "SELECT * FROM keys ORDER BY created_at"
      : "SELECT * FROM keys WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at";
    return (includeRevoked ? this.sql.exec(q) : this.sql.exec(q, now())).toArray().map(row);
  }

  revoke(id: string, actor = "admin"): Result<KeyInfo> {
    if (!KEY_ID.test(id)) return failResult("bad_request", "invalid key id");
    const r = this.sql.exec("SELECT * FROM keys WHERE id = ?", id).toArray()[0];
    if (!r) return failResult("not_found", `no key ${id}`);
    if (r.revoked_at == null) {
      this.ctx.storage.transactionSync(() => {
        this.sql.exec("UPDATE keys SET revoked_at = ? WHERE id = ?", now(), id);
        this.event("key.revoked", actor, id, { agent: String(r.agent) });
      });
    }
    return okResult(row(this.sql.exec("SELECT * FROM keys WHERE id = ?", id).toArray()[0]!));
  }
}

export function registryStub(env: Env): DurableObjectStub<Registry> {
  return env.REGISTRY.get(env.REGISTRY.idFromName("keys")) as DurableObjectStub<Registry>;
}
