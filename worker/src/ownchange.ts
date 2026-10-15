// The intent's own change: what landing it would add to the trunk, and
// nothing the intent merely merged in from the trunk (wayglass sync, a
// land_conflict resolution).
//
// Land puts the fork head F onto the trunk head T by fast-forward or merge,
// so the files it changes are those where F differs from both the merge
// base and T. Diffing from the intent's fork base instead (the old way) also
// counts every trunk file a sync merged in: in the 100-agent scale run a
// one-file docs change that synced about 50 landed docs crossed the 40-file
// high-risk line and went to a person.
//
// Safety: the result must never cover fewer files than land would change.
//  - Every merge base of F and T is a "boundary" commit: an ancestor of F
//    that is also an ancestor of T, reached from F only through commits that
//    are not. The walk below stops only at commits it has proven to be
//    ancestors of T, so it collects every merge base.
//  - "Not an ancestor of T" is proven by absence from the trunk's object
//    store (git keeps every ancestor of a ref). Presence is not proof (an
//    agent with a land token could push other refs), so a commit found in
//    the trunk store is accepted only once a bounded walk from T reaches it.
//  - With one boundary B, diff(B, F) covers every file land changes. With T
//    itself a boundary, F contains the trunk and diff(T, F) is exactly what
//    a fast-forward changes. With several, the files are those that differ
//    from T and from at least one boundary: a file equal to every merge base
//    or to T is left alone by a merge.
//  - Anything the walk cannot settle within its budget falls back to the
//    old diff from the fork base (classify more, fail closed), and a
//    truncated diff still forces human_approve in the evaluator.

import type { RepoReader } from "./artifacts";
import { diffTrees, type TreeDiff } from "./diff";

/** Most fork commits examined looking for the merge base. */
export const MAX_OWN_WALK = 64;
/** Most trunk commits walked from the trunk head to prove an ancestor. */
export const MAX_TRUNK_WALK = 128;
/** Most boundary commits handled before falling back. */
export const MAX_BOUNDARIES = 4;

export interface DiffBase {
  /**
   * trunk_head: the fork contains the trunk head, diffed from it.
   * merge_base: diffed from the single merge base with the trunk head.
   * merge_bases: several boundaries; files differing from the trunk head and from one of them.
   * fork_base: fallback, diffed from the intent's fork base (the pre-Oct 14 behaviour).
   */
  method: "trunk_head" | "merge_base" | "merge_bases" | "fork_base";
  commits: string[];
  /** Why the fallback was used. */
  fallback?: string;
}

export interface OwnChange {
  diff: TreeDiff;
  base: DiffBase;
}

/**
 * Finds the boundary commits between the fork head and the trunk head, or a
 * reason it could not. `trunkBase` is the trunk commit the intent was forked
 * from (recorded by the Worker, so trusted to be on the trunk).
 */
export async function boundaries(
  fork: RepoReader,
  trunk: RepoReader,
  head: string,
  trunkHead: string,
  trunkBase: string | null,
): Promise<string[] | { fallback: string }> {
  // Commits proven to be ancestors of trunkHead (or the trusted fork base).
  const onTrunk = new Set<string>([trunkHead]);
  if (trunkBase) onTrunk.add(trunkBase);
  const tQueue = [trunkHead];
  const tSeen = new Set<string>();
  const provenOnTrunk = async (x: string): Promise<boolean> => {
    while (!onTrunk.has(x) && tQueue.length > 0 && tSeen.size < MAX_TRUNK_WALK) {
      const h = tQueue.shift()!;
      if (tSeen.has(h)) continue;
      tSeen.add(h);
      // Below the fork base nothing can be a newer merge base.
      if (h === trunkBase) continue;
      const c = await trunk.readCommit(h);
      if (!c) continue;
      for (const p of c.parents) {
        onTrunk.add(p);
        tQueue.push(p);
      }
    }
    return onTrunk.has(x);
  };

  const found = new Set<string>();
  const seen = new Set<string>();
  const queue = [head];
  while (queue.length > 0) {
    const x = queue.shift()!;
    if (seen.has(x)) continue;
    seen.add(x);
    if (onTrunk.has(x)) {
      found.add(x);
      continue;
    }
    if (seen.size > MAX_OWN_WALK) return { fallback: `more than ${MAX_OWN_WALK} fork commits before the trunk` };
    if (await trunk.readCommit(x)) {
      if (await provenOnTrunk(x)) {
        found.add(x);
        continue;
      }
      return { fallback: `commit ${x} is in the trunk's store but could not be proven an ancestor of the trunk head` };
    }
    const c = await fork.readCommit(x);
    if (!c) return { fallback: `fork commit ${x} could not be read` };
    queue.push(...c.parents);
  }
  if (found.size === 0) return { fallback: "the fork shares no history with the trunk" };
  if (found.size > MAX_BOUNDARIES) return { fallback: `more than ${MAX_BOUNDARIES} merge bases` };
  return [...found].sort();
}

async function treeOf(repo: RepoReader, commit: string): Promise<string> {
  const c = await repo.readCommit(commit);
  if (!c) throw new Error(`commit ${commit} could not be read`);
  return c.tree;
}

/**
 * The intent's own change, for classification, risk size and the decision's
 * path set. `fallback` computes the old diff from the fork base.
 */
export async function ownChange(
  fork: RepoReader,
  trunk: RepoReader,
  head: { commit: string; tree: string },
  trunkHead: string | null,
  trunkBase: string | null,
  fallback: () => Promise<TreeDiff>,
): Promise<OwnChange> {
  const fall = async (reason: string): Promise<OwnChange> => ({
    diff: await fallback(),
    base: { method: "fork_base", commits: trunkBase ? [trunkBase] : [], fallback: reason },
  });
  if (!trunkHead) return fall("the trunk is empty");
  const found = await boundaries(fork, trunk, head.commit, trunkHead, trunkBase);
  if (!Array.isArray(found)) return fall(found.fallback);

  const headSide = { repo: fork, tree: head.tree };
  if (found.includes(trunkHead)) {
    const diff = await diffTrees({ repo: trunk, tree: await treeOf(trunk, trunkHead) }, headSide);
    return { diff, base: { method: "trunk_head", commits: [trunkHead] } };
  }
  if (found.length === 1) {
    const diff = await diffTrees({ repo: trunk, tree: await treeOf(trunk, found[0]!) }, headSide);
    return { diff, base: { method: "merge_base", commits: found } };
  }
  // Several boundaries (typically the fork base and the trunk commit a sync
  // merged, which the walk cannot order cheaply): keep the files that differ
  // from the trunk head and from at least one boundary. Line content comes
  // from the trunk-head diff, which holds every line the change adds.
  const vsTrunk = await diffTrees({ repo: trunk, tree: await treeOf(trunk, trunkHead) }, headSide);
  const keep = new Set<string>();
  for (const b of found) {
    const d = await diffTrees({ repo: trunk, tree: await treeOf(trunk, b) }, headSide, { withContent: false });
    // An incomplete path list cannot filter safely: keep everything that differs from the trunk.
    if (d.truncated) return { diff: vsTrunk, base: { method: "merge_bases", commits: found } };
    for (const p of d.paths) keep.add(p);
  }
  const changes = vsTrunk.changes.filter((c) => keep.has(c.path));
  return {
    diff: { ...vsTrunk, changes, paths: changes.map((c) => c.path) },
    base: { method: "merge_bases", commits: found },
  };
}
