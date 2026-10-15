// The only place the Worker talks to Cloudflare Artifacts.
//
// REAL:   BindingRepoHost calls env.ARTIFACTS (the Workers binding). This is
//         what runs in `wrangler dev` / production.
// FAKED:  Tests replace it with test/fake-artifacts.ts via setRepoHostForTests().
//         The fake is never selected by config or env vars, so a deployed
//         Worker cannot silently run on a mock.

/** Lifetime of a write token for an agent's own fork. Agents refresh it via POST /v1/intents/:id/token. */
export const FORK_TOKEN_TTL_SECONDS = 900;
/** Lifetime of the trunk write token handed to `wayglass init` for the first push. */
export const TRUNK_INIT_TOKEN_TTL_SECONDS = 900;
/** Lifetime of the trunk write token for landing one approved change. */
export const LAND_TOKEN_TTL_SECONDS = 300;
/** Lifetime of the read-only fork token a reviewer gets to see a change. */
export const READ_TOKEN_TTL_SECONDS = 300;

export interface RepoRef {
  name: string;
  remote: string;
  defaultBranch: string;
}

export interface RepoToken {
  /** Token id, safe to store; used to revoke. */
  id: string;
  /** Plaintext token. Returned to the caller once; never stored or logged. */
  value: string;
  scope: "read" | "write";
  expiresAt: string;
}

export interface CommitInfo {
  hash: string;
  tree: string;
  parents: string[];
  message?: string;
  author?: string;
  committed_at?: number;
}

export interface TreeEntry {
  name: string;
  mode: string;
  hash: string;
  type: "tree" | "blob" | "symlink" | "gitlink" | "exec";
}

/** Read access to one repo's objects. Dispose it when done. */
export interface RepoReader extends Disposable {
  /** Commit at the tip of the default branch, or null for an empty repo. */
  head(): Promise<string | null>;
  readCommit(hash: string): Promise<CommitInfo | null>;
  readTree(hash: string): Promise<TreeEntry[] | null>;
  readBlob(hash: string): Promise<Uint8Array | null>;
  /** A file's bytes at a commit, or null if it does not exist there. */
  readFile(ref: string, path: string): Promise<Uint8Array | null>;
}

export interface RepoHost {
  /** Bounded namespace listing. Only names leave the binding adapter. */
  listRepos(limit: number, cursor?: string): Promise<{ names: string[]; cursor: string | null }>;
  /** Creates the trunk repo and returns a short-lived write token for it. */
  createTrunk(name: string, description: string): Promise<{ repo: RepoRef; token: RepoToken }>;
  /** Forks `source` into `name` (default branch only). */
  fork(source: string, name: string, description: string): Promise<RepoRef>;
  /** Mints a token scoped to exactly one repo. */
  mintToken(repo: string, scope: "read" | "write", ttlSeconds: number): Promise<RepoToken>;
  /** Revokes a token by id. Returns false if it was already gone. */
  revokeToken(repo: string, tokenId: string): Promise<boolean>;
  /** Opens a repo for reading objects. */
  reader(repo: string): Promise<RepoReader>;
  /** Deletes a repo and its tokens. Returns false if it was already gone. */
  deleteRepo(repo: string): Promise<boolean>;
}

export class RepoHostError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

/**
 * Waits before each retry of a fork that failed with INTERNAL_ERROR. Live
 * scale runs (Oct 14: 5 and 6 of 10 agents declaring at once) see Artifacts
 * answer concurrent forks of one trunk with INTERNAL_ERROR. The failed name
 * stays taken (a retry gets ALREADY_EXISTS) although no repo is listed or
 * found under it, so each retry forks under a fresh name.
 */
export const FORK_RETRY_DELAYS_MS = [300, 900, 2000];

/** The name of retry n (1-based) of a fork: "<name>--f<n>". Fork names still start "<project>--<intent-id>". */
export const forkRetryName = (name: string, n: number) => `${name}--f${n}`;

/**
 * host.fork with jittered retries on INTERNAL_ERROR only, each under a fresh
 * name (forkRetryName). Any other error, and the last INTERNAL_ERROR, is
 * thrown as is. Each try goes through the host, so a metered host counts
 * every binding call.
 */
export async function forkWithRetry(
  host: RepoHost,
  source: string,
  name: string,
  description: string,
  delays: readonly number[] = FORK_RETRY_DELAYS_MS,
): Promise<RepoRef> {
  for (let i = 0; ; i++) {
    const tryName = i === 0 ? name : forkRetryName(name, i);
    try {
      return await host.fork(source, tryName, description);
    } catch (err) {
      if (!(err instanceof RepoHostError) || err.code !== "INTERNAL_ERROR" || i >= delays.length) throw err;
      console.warn("artifacts fork failed with INTERNAL_ERROR; retrying under a new name", tryName, i + 1);
      const ms = delays[i]!;
      await new Promise((r) => setTimeout(r, ms / 2 + Math.random() * ms));
    }
  }
}

function wrap(op: string, err: unknown): RepoHostError {
  const code = typeof err === "object" && err !== null && "code" in err ? String((err as { code: unknown }).code) : "INTERNAL_ERROR";
  const msg = err instanceof Error ? err.message : String(err);
  return new RepoHostError(code, `artifacts ${op} failed (${code}): ${msg}`);
}

async function guard<T>(op: string, f: () => Promise<T>): Promise<T> {
  try {
    return await f();
  } catch (err) {
    throw wrap(op, err);
  }
}

class BindingRepoReader implements RepoReader {
  constructor(private readonly repo: ArtifactsRepo) {}

  async head() {
    const log = await guard("log", () => this.repo.log({ limit: 1 }));
    return log[0]?.hash ?? null;
  }

  async readCommit(hash: string) {
    const c = await guard("readCommit", () => this.repo.readCommit(hash));
    return c ? { hash: c.hash, tree: c.treeHash, parents: c.parents, message: c.message, author: c.author?.name, committed_at: c.committedAt } : null;
  }

  async readTree(hash: string) {
    const t = await guard("readTree", () => this.repo.readTree(hash));
    return t ? t.map((e) => ({ name: e.name, mode: e.mode, hash: e.hash, type: e.type })) : null;
  }

  async readBlob(hash: string) {
    const b = await guard("readBlob", () => this.repo.readBlob(hash));
    return b ? new Uint8Array(await b.arrayBuffer()) : null;
  }

  async readFile(ref: string, path: string) {
    const b = await guard("readFile", () => this.repo.readFile({ ref, path }));
    return b ? new Uint8Array(await b.arrayBuffer()) : null;
  }

  [Symbol.dispose]() {
    this.repo[Symbol.dispose]();
  }
}

/** REAL implementation over the Workers binding. Exercised by the spike, not by unit tests. */
export class BindingRepoHost implements RepoHost {
  constructor(private readonly ns: Artifacts) {}

  async listRepos(limit: number, cursor?: string) {
    const page = await guard("list", () => this.ns.list({ limit, cursor }));
    return { names: page.repos.map(r => r.name), cursor: page.cursor ?? null };
  }

  async createTrunk(name: string, description: string) {
    const created = await guard("create", () => this.ns.create(name, { description }));
    // The creation token's lifetime is not ours to choose, so revoke it and
    // hand out a short-lived one instead (Zero Trust: no long-lived write tokens).
    // `return await`, not `return`: `using` disposes the stub when this
    // function returns, and the callback still needs it after its first await.
    using repo = await this.get(name);
    return await guard("token", async () => {
      await repo.revokeToken(created.token);
      const t = await repo.createToken("write", TRUNK_INIT_TOKEN_TTL_SECONDS);
      return {
        repo: { name: created.name, remote: created.remote, defaultBranch: created.defaultBranch },
        token: { id: t.id, value: t.plaintext, scope: t.scope, expiresAt: t.expiresAt },
      };
    });
  }

  async fork(source: string, name: string, description: string) {
    using repo = await this.get(source);
    const f = await guard("fork", () => repo.fork(name, { description, defaultBranchOnly: true }));
    // The fork's initial token is not ours to scope; drop it. Agents get
    // short-lived tokens from mintToken instead.
    try {
      using forked = await this.get(f.name);
      await forked.revokeToken(f.token);
    } catch (err) {
      // A fork can still be settling (FORK_IN_PROGRESS); the token then
      // expires on its own. Log it so it is visible, but do not fail the intent.
      console.warn("could not revoke fork creation token", f.name, err);
    }
    return { name: f.name, remote: f.remote, defaultBranch: f.defaultBranch };
  }

  async mintToken(repoName: string, scope: "read" | "write", ttlSeconds: number) {
    using repo = await this.get(repoName);
    const t = await guard("createToken", () => repo.createToken(scope, ttlSeconds));
    return { id: t.id, value: t.plaintext, scope: t.scope, expiresAt: t.expiresAt };
  }

  async revokeToken(repoName: string, tokenId: string) {
    using repo = await this.get(repoName);
    return await guard("revokeToken", () => repo.revokeToken(tokenId));
  }

  async reader(repoName: string): Promise<RepoReader> {
    return new BindingRepoReader(await this.get(repoName));
  }

  async deleteRepo(repoName: string) {
    try {
      return await guard("delete", () => this.ns.delete(repoName));
    } catch (err) {
      // The binding documents `false` for a missing repo; treat a NOT_FOUND throw the same.
      if (err instanceof RepoHostError && err.code === "NOT_FOUND") return false;
      throw err;
    }
  }

  private get(name: string): Promise<ArtifactsRepo> {
    return guard("get", () => this.ns.get(name));
  }
}

/** One Artifacts namespace the Worker can store projects in. */
export interface Namespace {
  /** Tags every change in it, and must match how the namespace was created. */
  jurisdiction: string;
  name: string;
  binding: Artifacts | undefined;
}

/**
 * The namespaces this Worker is bound to, by jurisdiction. The main one comes
 * from ARTIFACTS / ARTIFACTS_NAMESPACE / ARTIFACTS_JURISDICTION; an EU one from
 * ARTIFACTS_EU / ARTIFACTS_EU_NAMESPACE when bound. A project picks one at
 * creation, and its repos, tokens and tags stay there for good.
 */
export function namespaces(env: Env): Map<string, Namespace> {
  const m = new Map<string, Namespace>();
  const main: string = env.ARTIFACTS_JURISDICTION ?? "";
  m.set(main, { jurisdiction: main, name: env.ARTIFACTS_NAMESPACE ?? "", binding: env.ARTIFACTS });
  if (env.ARTIFACTS_EU_NAMESPACE && main !== "eu") {
    m.set("eu", { jurisdiction: "eu", name: env.ARTIFACTS_EU_NAMESPACE, binding: env.ARTIFACTS_EU });
  }
  return m;
}

/** The namespace for a jurisdiction; "" or undefined means the Worker's main one. */
export function namespaceFor(env: Env, jurisdiction?: string | null): Namespace {
  const all = namespaces(env);
  const j = jurisdiction || (env.ARTIFACTS_JURISDICTION ?? "");
  const ns = all.get(j);
  if (!ns) throw new RepoHostError("INVALID_INPUT", `no Artifacts namespace for jurisdiction "${j}" (have: ${[...all.keys()].join(", ") || "none"})`);
  return ns;
}

const testOverrides = new Map<string, RepoHost>();

/** Test hook. Only test files call this. "*" serves every jurisdiction without its own. */
export function setRepoHostForTests(host: RepoHost | null, jurisdiction = "*"): void {
  if (host) testOverrides.set(jurisdiction, host);
  else testOverrides.clear();
}

/** The repo host for a project's jurisdiction ("" = the Worker's main namespace). */
export function repoHost(env: Env, jurisdiction?: string | null): RepoHost {
  const ns = namespaceFor(env, jurisdiction);
  const fake = testOverrides.get(ns.jurisdiction) ?? testOverrides.get("*");
  if (fake) return fake;
  if (!ns.binding) throw new RepoHostError("INTERNAL_ERROR", `the Artifacts binding for jurisdiction "${ns.jurisdiction}" is missing`);
  return new BindingRepoHost(ns.binding);
}
