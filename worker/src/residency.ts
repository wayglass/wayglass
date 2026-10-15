// Data residency for project state (docs/ARCHITECTURE.md D13).
//
// A project's repos live in the Artifacts namespace its jurisdiction picked.
// Its Worker state (intents, decisions, audit log, sessions, cost meters: the
// Project Durable Object's SQLite) must live there too, so a project in a
// jurisdiction Durable Objects can pin to (eu, fedramp) gets its object from
// `env.PROJECT.jurisdiction(j)`. Projects in the Worker's main jurisdiction
// keep the unpinned object they always had.
//
// Which object a name maps to is decided before any request reaches it: the
// name -> jurisdiction directory in the Registry Durable Object is written
// when a project is created (before its state exists) and read, once per
// isolate, by every route, the queue consumer and the public views. A project
// from before the directory existed is looked up in the unpinned object once:
// one in the main jurisdiction is registered as it is; one in a pinned
// jurisdiction is refused (`residency_pending`) until the admin runs
// POST /v1/projects/:name/migrate-residency. Nothing falls back silently.

import type { Project } from "./project";
import { registryStub } from "./keys";

export type ProjectStub = DurableObjectStub<Project>;

/** Jurisdictions a Durable Object can be pinned to. Every other value means the unpinned object. */
export const DO_JURISDICTIONS: readonly string[] = ["eu", "fedramp"];

export const isPinnedJurisdiction = (jurisdiction: string): jurisdiction is DurableObjectJurisdiction =>
  DO_JURISDICTIONS.includes(jurisdiction);

export type ResidencyCode = "residency_pending" | "project_moved";

/** Thrown when a project's state cannot be served from where its jurisdiction says it must live. */
export class ResidencyError extends Error {
  constructor(
    readonly code: ResidencyCode,
    message: string,
  ) {
    super(message);
  }
}

/** The message a moved (tombstoned) object answers every call with. Checked by prefix across the RPC boundary. */
export const MOVED_PREFIX = "project_moved: ";

/** Turns an error thrown by a moved Durable Object over RPC back into a ResidencyError; null for any other error. */
export function movedError(err: unknown): ResidencyError | null {
  if (err instanceof ResidencyError) return err;
  if (err instanceof Error && err.message.startsWith(MOVED_PREFIX)) return new ResidencyError("project_moved", err.message.slice(MOVED_PREFIX.length));
  return null;
}

type Namespace = Pick<DurableObjectNamespace<Project>, "idFromName" | "get">;

// workerd does not implement jurisdiction(), so tests substitute an
// observable namespace (test/setup.ts). Production never falls back.
let pinnedForTests: ((jurisdiction: DurableObjectJurisdiction) => Namespace) | null = null;

/** Test hook. Only test files call this. */
export function setPinnedNamespaceForTests(fn: ((jurisdiction: DurableObjectJurisdiction) => Namespace) | null): void {
  pinnedForTests = fn;
}

function namespaceIn(env: Env, jurisdiction: string): Namespace {
  if (!isPinnedJurisdiction(jurisdiction)) return env.PROJECT;
  if (pinnedForTests) return pinnedForTests(jurisdiction);
  return env.PROJECT.jurisdiction(jurisdiction);
}

/** The project object for a name in a known jurisdiction. No lookup: callers that know (create, migrate) use this. */
export function stubIn(env: Env, name: string, jurisdiction: string): ProjectStub {
  const ns = namespaceIn(env, jurisdiction);
  return ns.get(ns.idFromName(name)) as ProjectStub;
}

// name -> jurisdiction, per isolate. A project's jurisdiction never changes
// (Artifacts cannot move a repo), so an entry never goes stale; it is only
// dropped when the project is deleted.
const known = new Map<string, string>();

/** Test hook: forgets every cached jurisdiction so a test sees the directory. */
export function resetResidencyCacheForTests(): void {
  known.clear();
}

/** Records a project's jurisdiction in the directory and this isolate's cache. */
export async function registerProject(env: Env, name: string, jurisdiction: string): Promise<void> {
  await registryStub(env).registerProject(name, jurisdiction);
  known.set(name, jurisdiction);
}

/** Forgets a deleted project, so the name can be created again in any jurisdiction. */
export async function unregisterProject(env: Env, name: string): Promise<void> {
  known.delete(name);
  await registryStub(env).unregisterProject(name);
}

/**
 * The jurisdiction a project's state lives in, or null when no project of
 * that name exists. Throws ResidencyError when the project exists but its
 * state is not where it must be.
 */
export async function projectJurisdiction(env: Env, name: string): Promise<string | null> {
  const hit = known.get(name);
  if (hit !== undefined) return hit;
  const registry = registryStub(env);
  const listed = await registry.projectJurisdiction(name);
  if (listed !== null) {
    known.set(name, listed);
    return listed;
  }
  // Not in the directory: a project from before it existed (always in the
  // unpinned object), or no project at all.
  const legacy = stubIn(env, name, "");
  let info: Awaited<ReturnType<Project["info"]>>;
  try {
    info = await legacy.info();
  } catch (err) {
    const moved = movedError(err);
    if (!moved) throw err;
    // Its tombstone names where the state went (written after a verified
    // copy), so a lost directory entry is repaired from it.
    const status = await legacy.residencyStatus();
    if (!status.moved_to) throw moved;
    await registerProject(env, name, status.moved_to);
    return status.moved_to;
  }
  if (!info) return null;
  if (isPinnedJurisdiction(info.jurisdiction)) {
    throw new ResidencyError(
      "residency_pending",
      `project ${name} is in jurisdiction ${info.jurisdiction} but its state has not been moved there yet; an admin must run POST /v1/projects/${name}/migrate-residency (wayglass project migrate-residency ${name})`,
    );
  }
  await registerProject(env, name, info.jurisdiction);
  return info.jurisdiction;
}

/**
 * The Durable Object holding a project's state, in the project's
 * jurisdiction. For a name with no project, the unpinned object (which
 * answers not_found); POST /v1/projects does not use this.
 */
export async function projectStub(env: Env, name: string): Promise<ProjectStub> {
  const jurisdiction = await projectJurisdiction(env, name);
  return stubIn(env, name, jurisdiction ?? "");
}

// Foreign keys are enforced, so parents go before children; tables the
// order does not name follow, by name.
const COPY_ORDER = ["meta", "intents", "decisions", "approvals", "lands", "tokens", "sessions", "audit", "costs", "ai_usage", "ai_budget", "retired_forks"];
const copyOrder = (tables: string[]) => [...COPY_ORDER.filter((t) => tables.includes(t)), ...tables.filter((t) => !COPY_ORDER.includes(t)).sort()];

export interface MigrationReport {
  project: string;
  jurisdiction: string;
  /** False when the state was already in place (a rerun). */
  migrated: boolean;
  /** Rows per table, as copied and verified. */
  counts: Record<string, number>;
  /** SHA-256 of the copied state, equal on both sides at verification. */
  digest: string;
  /** Whether the unpinned object now refuses every call and holds no state. */
  source_tombstoned: boolean;
}

/**
 * Moves a project's state from the unpinned object into the object pinned to
 * its jurisdiction, for projects created before pinning existed. Steps, each
 * safe to repeat: copy every table row for row (existing identical rows are
 * skipped), verify the copy (row counts and a digest of every row on both
 * sides), tombstone and wipe the source, record the move in the project's
 * audit log, register the name in the directory. A target that already holds
 * another project's state, or rows that differ from the source, is refused
 * before the source is touched.
 */
export async function migrateResidency(env: Env, name: string, actor: string): Promise<MigrationReport> {
  const source = stubIn(env, name, "");
  let info: Awaited<ReturnType<Project["info"]>>;
  try {
    info = await source.info();
  } catch (err) {
    if (!movedError(err)) throw err;
    info = null;
  }
  if (info === null) {
    // Nothing left in the unpinned object: either the move is done, or there
    // was never such a project.
    const status = await source.residencyStatus();
    if (!status.moved_to) throw new ResidencyError("residency_pending", `no project ${name} in the unpinned object; nothing to migrate`);
    const target = stubIn(env, name, status.moved_to);
    const copied = await target.info();
    if (!copied) throw new ResidencyError("project_moved", `project ${name} was marked moved to ${status.moved_to} but no state is there`);
    await registerProject(env, name, status.moved_to);
    const digest = await target.stateDigest();
    return { project: name, jurisdiction: status.moved_to, migrated: false, counts: digest.counts, digest: digest.digest, source_tombstoned: true };
  }
  if (!isPinnedJurisdiction(info.jurisdiction)) {
    throw new ResidencyError("residency_pending", `project ${name} is in jurisdiction ${info.jurisdiction || "the Worker's main one"}, which has no pinned Durable Objects; nothing to migrate`);
  }
  const jurisdiction = info.jurisdiction;
  const target = stubIn(env, name, jurisdiction);
  const before = await target.stateDigest();
  const held = await target.info();
  if (held && held.name !== info.name) throw new ResidencyError("project_moved", `the ${jurisdiction} object for ${name} already holds project ${held.name}; refusing to overwrite it`);
  if (await target.hasAudit("project.residency_migrated")) throw new ResidencyError("project_moved", `the ${jurisdiction} object for ${name} already records a residency migration; refusing to copy over it`);
  const expected = await source.stateDigest();
  if (before.digest !== expected.digest) {
    for (const table of copyOrder(Object.keys(expected.counts))) {
      let after = 0;
      for (;;) {
        const page = await source.exportRows(table, after);
        if (page.rows.length === 0) break;
        await target.importRows(table, page.rows);
        after = page.last;
        if (page.done) break;
      }
    }
  }
  const verified = await target.stateDigest();
  const mismatch = Object.keys(expected.counts).find((t) => expected.counts[t] !== verified.counts[t]);
  if (mismatch !== undefined || verified.digest !== expected.digest) {
    const where = mismatch === undefined ? "digest" : `${mismatch}: ${expected.counts[mismatch]} rows at the source, ${verified.counts[mismatch] ?? 0} at the target`;
    throw new ResidencyError("project_moved", `copy of ${name} to ${jurisdiction} did not verify (${where}); the source was left untouched`);
  }
  await source.markMoved(jurisdiction, expected.counts);
  await target.recordExternalEvent("project.residency_migrated", actor, null, {
    event_id: `residency:${name}:${jurisdiction}`,
    from: "unpinned",
    to: jurisdiction,
    counts: expected.counts,
    digest: expected.digest,
  });
  await registerProject(env, name, jurisdiction);
  return { project: name, jurisdiction, migrated: true, counts: expected.counts, digest: expected.digest, source_tombstoned: true };
}
