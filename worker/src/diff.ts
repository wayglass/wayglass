// Re-derives a change from Artifacts objects so policy never depends on what
// the client says it changed (docs/ARCHITECTURE.md D3). Artifacts has no diff
// API, so this walks two trees with readTree, skipping identical subtrees by
// hash, and diffs changed text blobs line by line.
//
// Bounded on purpose: every read is a request (and maybe a billed operation),
// and the input is untrusted. When a cap is hit the result is marked
// truncated, which the evaluator turns into human_approve at minimum.

import { RepoHostError, type RepoReader, type TreeEntry } from "./artifacts";
import type { WireChange } from "./policy";

export interface DiffCaps {
  /** Most changed paths to collect. */
  maxFiles: number;
  /** Most readTree calls across both sides. */
  maxTreeReads: number;
  /** Largest blob, per side, that is read for line diffs. */
  maxBlobBytes: number;
  /** Most blob bytes read in total. */
  maxTotalBytes: number;
}

export const DEFAULT_CAPS: DiffCaps = {
  maxFiles: 300,
  maxTreeReads: 400,
  maxBlobBytes: 512 * 1024,
  maxTotalBytes: 8 * 1024 * 1024,
};

export interface TreeDiff {
  changes: WireChange[];
  /** Every changed path, sorted. */
  paths: string[];
  /**
   * Blob hash and git mode on each side per changed path (null: absent
   * there). Not part of the policy wire. Land verification compares both:
   * a symlink or an executable with the same bytes as the approved file is
   * not the approved file.
   */
  entries: Record<string, { before: string | null; after: string | null; before_mode: string | null; after_mode: string | null }>;
  truncated: boolean;
  /** Why the diff is incomplete, for the audit record. */
  truncated_reasons: string[];
}

interface Side {
  repo: RepoReader;
  tree: string | null;
}

interface PathChange {
  path: string;
  before: TreeEntry | null;
  after: TreeEntry | null;
}

class Budget {
  treeReads = 0;
  bytes = 0;
  reasons = new Set<string>();
  constructor(readonly caps: DiffCaps) {}
  truncate(reason: string) {
    this.reasons.add(reason);
  }
}

async function readTree(side: RepoReader, hash: string | null, budget: Budget): Promise<TreeEntry[] | null> {
  if (hash === null) return [];
  if (budget.treeReads >= budget.caps.maxTreeReads) {
    budget.truncate(`more than ${budget.caps.maxTreeReads} directories changed`);
    return null;
  }
  budget.treeReads++;
  const t = await side.readTree(hash);
  if (!t) throw new RepoHostError("NOT_FOUND", `tree ${hash} not found`);
  return t;
}

const isDir = (e: TreeEntry | undefined | null) => e?.type === "tree";

async function walk(a: Side, b: Side, prefix: string, out: PathChange[], budget: Budget): Promise<void> {
  if (a.tree === b.tree) return;
  const [left, right] = await Promise.all([readTree(a.repo, a.tree, budget), readTree(b.repo, b.tree, budget)]);
  if (left === null || right === null) return;
  const l = new Map(left.map((e) => [e.name, e]));
  const r = new Map(right.map((e) => [e.name, e]));
  const names = [...new Set([...l.keys(), ...r.keys()])].sort();
  for (const name of names) {
    if (out.length >= budget.caps.maxFiles) {
      budget.truncate(`more than ${budget.caps.maxFiles} files changed`);
      return;
    }
    const le = l.get(name) ?? null;
    const re = r.get(name) ?? null;
    if (le && re && le.hash === re.hash && le.mode === re.mode) continue;
    const path = prefix + name;
    if (isDir(le) || isDir(re)) {
      await walk(
        { repo: a.repo, tree: isDir(le) ? le!.hash : null },
        { repo: b.repo, tree: isDir(re) ? re!.hash : null },
        path + "/",
        out,
        budget,
      );
      // A file replaced by a directory (or the reverse) is also a change to the file.
      if (le && !isDir(le)) out.push({ path, before: le, after: null });
      if (re && !isDir(re)) out.push({ path, before: null, after: re });
    } else {
      out.push({ path, before: le, after: re });
    }
  }
}

/** Splits text into lines the way git counts them: a trailing newline does not start a new line. */
export function splitLines(text: string): string[] {
  if (text === "") return [];
  const lines = text.split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines;
}

/**
 * Lines added and the count of lines removed between two texts. Trims the
 * common prefix and suffix, then matches the rest as multisets. That equals
 * `git diff` for edits confined to one region; for moved lines it reports
 * fewer changes than git (a moved line is neither added nor removed), which
 * never hides new content from the content detectors.
 */
export function lineDiff(before: string, after: string): { added: string[]; removed: number } {
  const a = splitLines(before);
  const b = splitLines(after);
  let start = 0;
  while (start < a.length && start < b.length && a[start] === b[start]) start++;
  let endA = a.length;
  let endB = b.length;
  while (endA > start && endB > start && a[endA - 1] === b[endB - 1]) {
    endA--;
    endB--;
  }
  const pool = new Map<string, number>();
  for (let i = start; i < endA; i++) pool.set(a[i]!, (pool.get(a[i]!) ?? 0) + 1);
  const added: string[] = [];
  for (let i = start; i < endB; i++) {
    const line = b[i]!;
    const n = pool.get(line) ?? 0;
    if (n > 0) pool.set(line, n - 1);
    else added.push(line);
  }
  let removed = 0;
  for (const n of pool.values()) removed += n;
  return { added, removed };
}

/**
 * git's heuristic: a NUL byte in the first 8000 bytes means binary. The
 * content detectors then read none of the file, so the evaluator routes it
 * by name (policy's binary.allow) and sends anything else to a person.
 */
export function isBinary(bytes: Uint8Array): boolean {
  const n = Math.min(bytes.length, 8000);
  for (let i = 0; i < n; i++) if (bytes[i] === 0) return true;
  return false;
}

const decoder = new TextDecoder("utf-8");

type Content = { kind: "text"; text: string } | { kind: "binary"; bytes: number } | { kind: "skipped" };

async function content(repo: RepoReader, e: TreeEntry | null, budget: Budget): Promise<Content> {
  if (!e) return { kind: "text", text: "" };
  // A submodule pointer has no content here; its path is what matters, and
  // nothing can be scanned, so it is binary of no size.
  if (e.type === "gitlink") return { kind: "binary", bytes: 0 };
  const bytes = await repo.readBlob(e.hash);
  if (!bytes) throw new RepoHostError("NOT_FOUND", `blob ${e.hash} not found`);
  if (bytes.length > budget.caps.maxBlobBytes) {
    budget.truncate(`a file is larger than ${budget.caps.maxBlobBytes} bytes`);
    return { kind: "skipped" };
  }
  if (budget.bytes + bytes.length > budget.caps.maxTotalBytes) {
    budget.truncate(`the change reads more than ${budget.caps.maxTotalBytes} bytes`);
    return { kind: "skipped" };
  }
  budget.bytes += bytes.length;
  if (isBinary(bytes)) return { kind: "binary", bytes: bytes.length };
  return { kind: "text", text: decoder.decode(bytes) };
}

/**
 * Diffs `head` (usually the fork) against `base` (the trunk at the intent's
 * base commit). Pass `withContent: false` for a path-only diff.
 */
export async function diffTrees(base: Side, head: Side, opts: { caps?: DiffCaps; withContent?: boolean } = {}): Promise<TreeDiff> {
  const budget = new Budget(opts.caps ?? DEFAULT_CAPS);
  const changed: PathChange[] = [];
  await walk(base, head, "", changed, budget);
  changed.sort((x, y) => (x.path < y.path ? -1 : x.path > y.path ? 1 : 0));

  const changes: WireChange[] = [];
  const entries: TreeDiff["entries"] = {};
  for (const c of changed) {
    entries[c.path] = { before: c.before?.hash ?? null, after: c.after?.hash ?? null, before_mode: c.before?.mode ?? null, after_mode: c.after?.mode ?? null };
    const ch: WireChange = { path: c.path, added_lines: [], removed_count: 0 };
    if (opts.withContent !== false) {
      const [before, after] = [await content(base.repo, c.before, budget), await content(head.repo, c.after, budget)];
      if (after.kind === "binary") {
        // What arrives cannot be read, whatever it replaced.
        ch.binary = true;
        if (after.bytes > 0) ch.bytes = after.bytes;
      } else if (after.kind === "text") {
        // A text new side is scanned in full. When the old side was binary
        // or too large to read, every line counts as added: the detectors
        // saw none of it before, and a removed count is unknowable.
        const d = lineDiff(before.kind === "text" ? before.text : "", after.text);
        ch.added_lines = d.added;
        ch.removed_count = d.removed;
      }
      // A skipped new side already marked the diff truncated; a deleted
      // binary (binary before, nothing after) brings nothing new to scan.
    }
    changes.push(ch);
  }
  const reasons = [...budget.reasons];
  return { changes, paths: changes.map((c) => c.path), entries, truncated: reasons.length > 0, truncated_reasons: reasons };
}
