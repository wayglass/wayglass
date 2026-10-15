// Queue consumer for Artifacts event subscriptions (queue: artifacts-events).
// Envelope per developers.cloudflare.com/artifacts/guides/event-subscriptions/:
//   { type, source: { type, namespace, repoName }, payload: {...}, metadata: {...} }
// POST /v1/projects subscribes each new trunk to `pushed` (src/subscriptions.ts).
// Forks are not subscribed: agents report their own pushes via POST /v1/intents/:id/push.

import { namespaceFor, namespaces } from "./artifacts";
import { projectStub } from "./stub";
import { movedError } from "./residency";

export interface ArtifactsEvent {
  type: string;
  source?: { type?: string; namespace?: string; repoName?: string; repo_name?: string };
  payload?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
}

export interface PushedSummary {
  repo: string;
  /** The namespace the event names, "" when it does not. */
  namespace: string;
  project: string;
  intent_id: string | null;
  ref: string;
  before: string;
  after: string;
  commits: string[];
  total_commits: number;
}

const str = (v: unknown) => (typeof v === "string" ? v : "");

// Only routing fields are kept, and only in their expected shape. Commit
// messages, author and committer names and emails are written by whoever
// pushed: they are never read, stored or logged (rule 9 of the Artifacts
// skill). A ref is pusher-chosen too, so one that is not a plain ref name
// is recorded as "".
const OBJECT_ID = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const REF = /^refs\/[\x21-\x7e]{1,250}$/;
const oid = (v: unknown) => (OBJECT_ID.test(str(v)) ? str(v) : "");
const refName = (v: unknown) => (REF.test(str(v)) ? str(v) : "");

// Repos the Worker makes: a trunk named after its project (src/index.ts
// PROJECT_NAME), an intent's fork "<project>--<intent-id>", or a restacked
// fork "<project>--<intent-id>--r<n>". Anything else in the namespace is not
// Wayglass's, so its pushes are not written into a project's audit log.
const REPO = /^([a-z0-9](?:[a-z0-9._]|-(?!-)){0,62})(?:--(i[0-9a-f]{10})(?:--r[1-9][0-9]{0,8})?)?$/;

/** Parses a cf.artifacts.repo.pushed event. Returns null for anything else or malformed input. */
export function parsePushed(body: unknown): PushedSummary | null {
  if (typeof body !== "object" || body === null) return null;
  const e = body as ArtifactsEvent;
  if (e.type !== "cf.artifacts.repo.pushed") return null;
  // The docs' prose says repo_name, their example says repoName; accept both.
  const repo = str(e.source?.repoName) || str(e.source?.repo_name);
  const p = e.payload;
  if (!repo || !p || typeof p !== "object") return null;
  const m = REPO.exec(repo);
  if (!m) return null;
  const commits = Array.isArray(p.commits) ? p.commits.slice(0, 1000).map((c) => oid((c as { id?: unknown })?.id)).filter(Boolean) : [];
  const total = p.totalCommitsCount;
  return {
    repo,
    namespace: str(e.source?.namespace),
    project: m[1]!,
    intent_id: m[2] ?? null,
    ref: refName(p.ref),
    before: oid(p.before),
    after: oid(p.after),
    commits,
    total_commits: typeof total === "number" && Number.isInteger(total) && total >= 0 ? total : commits.length,
  };
}

/**
 * Why a parsed push is not recorded, or null when it belongs to `project`.
 * The event's namespace must be the one the project's jurisdiction picked: a
 * same-named repo in another namespace is not this project's, and a
 * namespace this Worker is not bound to is nobody's. An event that names no
 * namespace is taken as is (the docs show the field, but it is not relied on).
 */
export function pushRejection(env: Env, pushed: PushedSummary, projectJurisdiction: string): string | null {
  if (!pushed.namespace) return null;
  if (![...namespaces(env).values()].some((ns) => ns.name === pushed.namespace)) return "namespace is not bound to this Worker";
  if (namespaceFor(env, projectJurisdiction).name !== pushed.namespace) return "namespace does not match the project's jurisdiction";
  return null;
}

export async function handleQueue(batch: MessageBatch<unknown>, env: Env, _ctx: ExecutionContext): Promise<void> {
  for (const msg of batch.messages) {
    const pushed = parsePushed(msg.body);
    if (!pushed) {
      // Other event types and malformed bodies are acknowledged and dropped: retrying cannot fix them.
      msg.ack();
      continue;
    }
    try {
      const { project, intent_id, namespace, ...data } = pushed;
      const stub = await projectStub(env, project);
      const info = await stub.info();
      const why = info ? pushRejection(env, pushed, info.jurisdiction) : "unknown project";
      if (why) {
        console.warn("push event dropped:", why, project, namespace);
        msg.ack();
        continue;
      }
      // Delivery is at least once; the message id makes a redelivery a no-op.
      await stub.recordExternalEvent("artifacts.repo.pushed", "artifacts", intent_id, { ...data, event_id: msg.id });
      msg.ack();
    } catch (err) {
      // A project whose state is not where its jurisdiction says it must be
      // (src/residency.ts) is retried, never written elsewhere: the push is
      // recorded once the admin has run migrate-residency.
      console.error("failed to record push event", movedError(err)?.message ?? err);
      msg.retry();
    }
  }
}
