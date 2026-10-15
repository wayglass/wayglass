// Read-only views for the web app. Code stays authenticated even on public
// projects; the public directory exposes the same metadata as public.ts.
import {
  namespaces,
  repoHost,
  type RepoReader,
  type CommitInfo,
} from "./artifacts";
import { authenticate } from "./auth";
import { Meter, metered } from "./cost";
import { fail, ok, type Result, type ErrorCode } from "./envelope";
import type { Decision, Intent, ProjectInfo } from "./project";
import { projectAllowed, scrub } from "./public";
import { movedError } from "./residency";
import { projectStub, type ProjectStub } from "./stub";

const NAME = /^[a-z0-9](?:[a-z0-9._]|-(?!-)){0,62}$/;
const SHA = /^[0-9a-f]{40}$/;
const MAX_BYTES = 128 * 1024;
const route =
  /^\/v1\/projects\/([^/]+)\/(browse|history|changes\/d[0-9a-f]{10})$/;
class ReadError extends Error {
  constructor(
    readonly code: ErrorCode,
    message: string,
  ) {
    super(message);
  }
}
const required = <T>(value: T | null | undefined, message: string): T => {
  if (value == null) throw new ReadError("not_found", message);
  return value;
};
const unwrap = <T>(result: Result<T>): T => {
  if (!result.ok) throw new ReadError(result.code, result.message);
  return result.data;
};

export function isBrowseRoute(req: Request): boolean {
  const p = new URL(req.url).pathname;
  return (
    (req.method === "GET" && p === "/v1/projects") ||
    p === "/v1/public/projects" ||
    route.test(p)
  );
}

function pathParam(url: URL): string {
  const path = url.searchParams.get("path") ?? "";
  if (
    path.length > 1024 ||
    path.split("/").length > 32 ||
    /[\x00-\x1f\\]/.test(path) ||
    (path && path.split("/").some((p) => !p || p === "." || p === ".."))
  ) {
    throw new ReadError("bad_request", "invalid repository path");
  }
  return path;
}
function refParam(url: URL, key: string): string | null {
  const value = url.searchParams.get(key);
  if (value !== null && !SHA.test(value))
    throw new ReadError("bad_request", `${key} must be a full commit hash`);
  return value;
}
function file(bytes: Uint8Array | null) {
  if (!bytes)
    return {
      missing: true,
      text: null,
      binary: false,
      truncated: false,
      bytes: 0,
    };
  const truncated = bytes.length > MAX_BYTES;
  const sample = bytes.subarray(0, MAX_BYTES);
  let text: string | null = null;
  try {
    if (!sample.includes(0))
      text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(
        sample,
        { stream: truncated },
      );
  } catch {
    /* binary */
  }
  return {
    missing: false,
    text,
    binary: text === null,
    truncated,
    bytes: bytes.length,
  };
}
/**
 * The `q` search: a case-insensitive substring of the project name, applied
 * server-side to each namespace page (so paging stays stable: the cursor
 * walks the namespace, the filter only drops rows). Project names are
 * lower-case `[a-z0-9._-]`, so anything else can match nothing.
 */
function searchParam(url: URL): string {
  const q = (url.searchParams.get("q") ?? "").trim().toLowerCase();
  if (q.length > 63) throw new ReadError("bad_request", "q is longer than a project name");
  return q;
}

/**
 * Who may list: the private directory is open to every authenticated key,
 * the same reach GET /v1/intents and GET /v1/audit give a key on any project
 * it names. Listing adds discovery, not access: every row carries the same
 * metadata those routes already return, and no token or repo content. The
 * admin key sees the same rows; projects whose state is not where their
 * jurisdiction says are listed with `residency` so the admin can migrate.
 */
async function directory(env: Env, url: URL, publicOnly: boolean) {
  // One namespace page per request: scans never grow with the namespace.
  const all = [...namespaces(env).keys()];
  const q = searchParam(url);
  let index = 0,
    cursor: string | undefined;
  const raw = url.searchParams.get("cursor");
  if (raw) {
    try {
      if (raw.length > 4096) throw new Error();
      const value = JSON.parse(atob(raw));
      if (
        !Number.isInteger(value.index) ||
        value.index < 0 ||
        value.index >= all.length ||
        typeof value.cursor !== "string"
      )
        throw new Error();
      index = value.index;
      cursor = value.cursor || undefined;
    } catch {
      throw new ReadError("bad_request", "invalid directory cursor");
    }
  }
  const page = await repoHost(env, all[index]).listRepos(20, cursor);
  const projects: Record<string, unknown>[] = [];
  for (const name of page.names) {
    if (!NAME.test(name) || (publicOnly && !projectAllowed(env, name)))
      continue;
    if (q && !name.includes(q)) continue;
    let stub: ProjectStub;
    try {
      stub = await projectStub(env, name);
    } catch (err) {
      // A project whose state is not where its jurisdiction says (src/residency.ts)
      // is listed, not served, so the admin can see what to migrate.
      const moved = movedError(err);
      if (!moved) throw err;
      if (!publicOnly) projects.push({ name, jurisdiction: all[index], residency: moved.code, counts: null });
      continue;
    }
    const entry = await stub.directoryEntry();
    if (
      !entry ||
      entry.trunk_repo !== name ||
      (entry.jurisdiction || env.ARTIFACTS_JURISDICTION) !== all[index]
    )
      continue;
    const jurisdiction = entry.jurisdiction || env.ARTIFACTS_JURISDICTION;
    if (publicOnly) {
      if (!entry.public) continue;
      // The public row stays metadata only: no remote, no branch, no times.
      const { intents, open, agents } = entry.counts;
      projects.push({ name, jurisdiction, counts: { intents, open, agents } });
      continue;
    }
    projects.push({
      name,
      jurisdiction,
      trunk_repo: entry.trunk_repo,
      trunk_remote: entry.trunk_remote,
      default_branch: entry.default_branch,
      created_at: entry.created_at,
      public: entry.public,
      residency: "in_place",
      last_activity: entry.last_activity,
      last_push_at: entry.last_push_at,
      counts: entry.counts,
    });
  }
  const next = page.cursor
    ? { index, cursor: page.cursor }
    : index + 1 < all.length
      ? { index: index + 1, cursor: "" }
      : null;
  return {
    projects,
    q: q || null,
    next: next ? btoa(JSON.stringify(next)) : null,
    scope: "Directory listing operations are not attributed to project costs.",
  };
}

async function browse(reader: RepoReader, url: URL) {
  const ref = refParam(url, "ref") ?? (await reader.head());
  if (!ref)
    return { ref: null, path: "", type: "tree", entries: [], empty: true };
  const commit = required(await reader.readCommit(ref), "commit not found");
  const path = pathParam(url);
  let hash = commit.tree;
  if (path) {
    const parts = path.split("/");
    for (let n = 0; n < parts.length; n++) {
      const tree = required(await reader.readTree(hash), "directory not found");
      const entry = required(
        tree.find((e) => e.name === parts[n]),
        "path not found",
      );
      hash = entry.hash;
      if (entry.type !== "tree") {
        if (n !== parts.length - 1)
          throw new ReadError("not_found", "directory not found");
        if (entry.type === "gitlink")
          return { ref, path, type: "gitlink", hash };
        return {
          ref,
          path,
          type: entry.type,
          ...file(await reader.readBlob(hash)),
        };
      }
    }
  }
  const entries = required(await reader.readTree(hash), "directory not found");
  return {
    ref,
    path,
    type: "tree",
    entries: entries.slice(0, 1000),
    truncated: entries.length > 1000,
  };
}

export async function handleBrowse(req: Request, env: Env): Promise<Response> {
  const url = new URL(req.url),
    publicOnly = url.pathname === "/v1/public/projects";
  if (req.method !== "GET")
    return fail("method_not_allowed", "this view is read-only");
  if (!publicOnly) {
    const auth = await authenticate(req, env);
    if (auth instanceof Response) return auth;
  }
  const meter = new Meter();
  let stub: ProjectStub | undefined;
  try {
    if (publicOnly) return ok(scrub(await directory(env, url, true)));
    // The private directory carries each trunk remote on purpose: a key
    // that may read a project may clone it (POST .../trunk/read-token).
    if (url.pathname === "/v1/projects")
      return ok(await directory(env, url, false));
    const match = required(route.exec(url.pathname), "view not found");
    const name = match[1]!,
      view = match[2]!;
    if (!NAME.test(name))
      throw new ReadError("bad_request", "invalid project name");
    stub = await projectStub(env, name);
    const info = required(await stub.info(), "project not found");
    const host = metered(repoHost(env, info.jurisdiction), meter);
    if (view.startsWith("changes/")) {
      const id = view.slice(8);
      const decisions = unwrap(
        (await stub.listDecisions(undefined, 500)) as Result<{
          decisions: Decision[];
        }>,
      ).decisions;
      const decision = required(
        decisions.find((d) => d.id === id),
        "decision not found in recent history",
      );
      const context = unwrap(
        (await stub.intentContext(decision.intent_id)) as Result<{
          project: ProjectInfo;
          intent: Intent;
          latest_decision: Decision | null;
        }>,
      );
      const path = pathParam(url);
      if (!path || !decision.paths.includes(path))
        throw new ReadError("bad_request", "select a path from this decision");
      using reader = await host.reader(context.intent.fork_repo);
      const after = required(decision.commit_sha, "decision has no commit");
      required(
        await reader.readCommit(after),
        "decision commit is no longer available",
      );
      const base = reviewBase(decision);
      if (base)
        required(
          await reader.readCommit(base),
          "decision base is no longer available",
        );
      return ok({
        decision_id: id,
        path,
        base,
        commit: after,
        before: file(base ? await reader.readFile(base, path) : null),
        after: file(await reader.readFile(after, path)),
      });
    }
    using reader = await host.reader(info.trunk_repo);
    if (view === "browse") return ok(await browse(reader, url));
    let next = refParam(url, "cursor") ?? (await reader.head());
    const commits: CommitInfo[] = [];
    while (next && commits.length < 20) {
      const commit = required(
        await reader.readCommit(next),
        "history commit not found",
      );
      commits.push(commit);
      next = commit.parents[0] ?? null;
    }
    return ok({ commits, next, scope: "First-parent history of the trunk." });
  } catch (err) {
    if (err instanceof ReadError) return fail(err.code, err.message);
    const moved = movedError(err);
    if (moved) return fail(moved.code, moved.message);
    // Upstream messages may contain provider/account details. No credentials
    // or provider exception text are returned from these browsing routes.
    return fail("upstream_error", "Repository read failed. Try again shortly.");
  } finally {
    if (stub && meter.total()) await stub.recordCost(null, meter.ops);
  }
}

/**
 * The commit a decision's change was diffed from, when it is one commit in
 * the fork's history (the trunk head the fork contains, or the single merge
 * base); otherwise the intent's fork base, as before.
 */
function reviewBase(decision: Decision): string | null {
  const b = decision.detail?.diff_base as { method?: string; commits?: string[] } | undefined;
  if ((b?.method === "trunk_head" || b?.method === "merge_base") && b.commits?.length === 1) return b.commits[0]!;
  return decision.base_commit;
}
