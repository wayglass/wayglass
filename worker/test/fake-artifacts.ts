// FAKE Artifacts host for tests only. It records calls and returns
// deterministic repos and tokens, and keeps a small content-addressed object
// store so the Worker's tree diff runs against real tree shapes. Nothing here
// talks to Cloudflare.

import { RepoHostError, type CommitInfo, type RepoHost, type RepoReader, type RepoRef, type RepoToken, type TreeEntry } from "../src/artifacts";

type Obj = { kind: "blob"; bytes: Uint8Array } | { kind: "tree"; entries: TreeEntry[] } | { kind: "commit"; info: CommitInfo };

/** Files for a commit: path -> text, or Uint8Array for binary content. */
export type Files = Record<string, string | Uint8Array>;

const enc = new TextEncoder();

export class FakeRepoHost implements RepoHost {
  repos = new Map<string, { source: string | null }>();
  /** Default-branch tip per repo. */
  heads = new Map<string, string | null>();
  tokens: { id: string; repo: string; scope: "read" | "write"; ttl: number }[] = [];
  revoked: { repo: string; id: string }[] = [];
  /** Counts object reads, to check the diff stays bounded. */
  reads = { tree: 0, blob: 0, commit: 0, file: 0 };
  failNextFork = false;
  /** Codes the next forks fail with, one per call (e.g. Artifacts' INTERNAL_ERROR under load). */
  forkFailures: string[] = [];
  forkCalls = 0;
  reservedForkNames = new Set<string>();
  /** Repos whose delete fails like a transient Artifacts error. */
  failDelete = new Set<string>();
  deleted: string[] = [];
  /** Commits whose readFile fails like a transient Artifacts error. */
  failReadFile = new Set<string>();

  // Objects are shared across repos (forks share history), keyed by content.
  private objects = new Map<string, Obj>();
  /**
   * Commits each repo's store holds, like Artifacts: readCommit of a commit
   * the repo never received returns null. A commit brings its ancestry (a
   * push carries it); a fork starts with its source's commits.
   */
  private stores = new Map<string, Set<string>>();

  private store(repo: string): Set<string> {
    let s = this.stores.get(repo);
    if (!s) this.stores.set(repo, (s = new Set()));
    return s;
  }

  /** Adds a commit and its ancestry to a repo's store. */
  private receive(repo: string, commit: string) {
    const s = this.store(repo);
    const queue = [commit];
    while (queue.length > 0) {
      const h = queue.pop()!;
      if (s.has(h)) continue;
      const o = this.objects.get(h);
      if (o?.kind !== "commit") continue;
      s.add(h);
      queue.push(...o.info.parents);
    }
  }
  private ids = new Map<string, string>();
  private seq = 0;

  private ref(name: string): RepoRef {
    return { name, remote: `https://fake.artifacts.invalid/git/test/${name}.git`, defaultBranch: "main" };
  }

  private token(repo: string, scope: "read" | "write", ttl: number): RepoToken {
    const id = `tok${this.tokens.length + 1}`;
    this.tokens.push({ id, repo, scope, ttl });
    return { id, value: `fake_token_for_${repo}`, scope, expiresAt: new Date(Date.now() + ttl * 1000).toISOString() };
  }

  /** Stores an object and returns a stable 40-hex id for its content. */
  private put(key: string, obj: Obj): string {
    let id = this.ids.get(key);
    if (!id) {
      this.seq += 1;
      id = this.seq.toString(16).padStart(40, "0");
      this.ids.set(key, id);
      this.objects.set(id, obj);
    }
    return id;
  }

  private blob(content: string | Uint8Array): string {
    const bytes = typeof content === "string" ? enc.encode(content) : content;
    return this.put(`blob:${[...bytes].join(",")}`, { kind: "blob", bytes });
  }

  private tree(files: Files): string {
    const dirs = new Map<string, Files>();
    const entries: TreeEntry[] = [];
    for (const [path, content] of Object.entries(files)) {
      const slash = path.indexOf("/");
      if (slash === -1) {
        entries.push({ name: path, mode: "100644", hash: this.blob(content), type: "blob" });
      } else {
        const dir = path.slice(0, slash);
        const sub = dirs.get(dir) ?? {};
        sub[path.slice(slash + 1)] = content;
        dirs.set(dir, sub);
      }
    }
    for (const [dir, sub] of dirs) entries.push({ name: dir, mode: "40000", hash: this.tree(sub), type: "tree" });
    entries.sort((a, b) => (a.name < b.name ? -1 : 1));
    return this.put(`tree:${entries.map((e) => `${e.mode} ${e.name} ${e.hash}`).join("|")}`, { kind: "tree", entries });
  }

  /** Commits `files` (the full tree, not a delta) on top of the repo's head. */
  commit(repo: string, files: Files, parents?: string[]): string {
    if (!this.repos.has(repo)) throw new Error(`no repo ${repo}`);
    const tree = this.tree(files);
    const head = this.heads.get(repo) ?? null;
    const ps = parents ?? (head ? [head] : []);
    this.seq += 1;
    const hash = `c${this.seq.toString(16).padStart(39, "0")}`;
    this.objects.set(hash, { kind: "commit", info: { hash, tree, parents: ps } });
    this.heads.set(repo, hash);
    this.receive(repo, hash);
    return hash;
  }

  /** Points a repo's head at a commit (e.g. a land pushed to trunk). */
  setHead(repo: string, commit: string) {
    this.heads.set(repo, commit);
    this.receive(repo, commit);
  }

  /** Puts a commit into a repo's store without moving its head (another ref was pushed). */
  pushObjects(repo: string, commit: string) {
    this.receive(repo, commit);
  }

  /** Files of a commit's tree, for building the next commit. */
  filesAt(commit: string): Files {
    const c = this.objects.get(commit);
    if (c?.kind !== "commit") throw new Error(`no commit ${commit}`);
    const out: Files = {};
    const walk = (tree: string, prefix: string) => {
      const t = this.objects.get(tree);
      if (t?.kind !== "tree") return;
      for (const e of t.entries) {
        const o = this.objects.get(e.hash)!;
        if (o.kind === "tree") walk(e.hash, `${prefix}${e.name}/`);
        else if (o.kind === "blob") out[prefix + e.name] = o.bytes;
      }
    };
    walk(c.info.tree, "");
    return out;
  }

  async listRepos(limit: number, cursor?: string) {
    const start = Number(cursor || 0), names = [...this.repos.keys()];
    return { names: names.slice(start, start + limit), cursor: start + limit < names.length ? String(start + limit) : null };
  }

  async createTrunk(name: string) {
    if (this.repos.has(name)) throw new Error("ALREADY_EXISTS");
    this.repos.set(name, { source: null });
    this.heads.set(name, null);
    return { repo: this.ref(name), token: this.token(name, "write", 900) };
  }

  async fork(source: string, name: string) {
    this.forkCalls++;
    // Like Artifacts: a failed fork's name stays taken, though no repo exists.
    if (this.reservedForkNames.has(name)) throw new RepoHostError("ALREADY_EXISTS", `repo already exists: ${name}`);
    const code = this.forkFailures.shift();
    if (code) {
      this.reservedForkNames.add(name);
      throw new RepoHostError(code, `fake fork failure (${code})`);
    }
    if (this.failNextFork) {
      this.failNextFork = false;
      throw new RepoHostError("UPSTREAM_UNAVAILABLE", "fake fork failure");
    }
    if (!this.repos.has(source)) throw new Error(`NOT_FOUND ${source}`);
    this.repos.set(name, { source });
    this.heads.set(name, this.heads.get(source) ?? null);
    this.stores.set(name, new Set(this.store(source)));
    return this.ref(name);
  }

  async mintToken(repo: string, scope: "read" | "write", ttl: number) {
    if (!this.repos.has(repo)) throw new Error(`NOT_FOUND ${repo}`);
    return this.token(repo, scope, ttl);
  }

  async revokeToken(repo: string, id: string) {
    if (this.revoked.some((r) => r.id === id)) return false;
    this.revoked.push({ repo, id });
    return true;
  }

  async deleteRepo(repo: string) {
    if (this.failDelete.has(repo)) throw new RepoHostError("UPSTREAM_UNAVAILABLE", `fake delete failure for ${repo}`);
    if (!this.repos.has(repo)) return false;
    this.repos.delete(repo);
    this.heads.delete(repo);
    this.deleted.push(repo);
    return true;
  }

  async reader(repo: string): Promise<RepoReader> {
    if (!this.repos.has(repo)) throw new RepoHostError("NOT_FOUND", `artifacts get failed (NOT_FOUND): ${repo}`);
    const get = (hash: string) => this.objects.get(hash);
    const reads = this.reads;
    const heads = this.heads;
    const store = this.store(repo);
    return {
      head: async () => heads.get(repo) ?? null,
      readCommit: async (hash) => {
        reads.commit++;
        if (!store.has(hash)) return null;
        const o = get(hash);
        return o?.kind === "commit" ? o.info : null;
      },
      readTree: async (hash) => {
        reads.tree++;
        const o = get(hash);
        return o?.kind === "tree" ? o.entries : null;
      },
      readBlob: async (hash) => {
        reads.blob++;
        const o = get(hash);
        return o?.kind === "blob" ? o.bytes : null;
      },
      readFile: async (ref, path) => {
        reads.file++;
        if (this.failReadFile.has(ref)) throw new RepoHostError("UPSTREAM", `artifacts readFile failed: ${ref}`);
        const c = get(ref);
        if (c?.kind !== "commit") return null;
        let tree = c.info.tree;
        const parts = path.split("/");
        for (const [i, part] of parts.entries()) {
          const t = get(tree);
          if (t?.kind !== "tree") return null;
          const e = t.entries.find((x) => x.name === part);
          if (!e) return null;
          if (i === parts.length - 1) {
            const b = get(e.hash);
            return b?.kind === "blob" ? b.bytes : null;
          }
          tree = e.hash;
        }
        return null;
      },
      [Symbol.dispose]: () => {},
    };
  }
}
