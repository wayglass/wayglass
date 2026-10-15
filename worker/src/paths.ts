// Path and glob intersection used for live overlap detection.
//
// Patterns are repo-relative, '/'-separated. Supported wildcards:
//   **  any sequence of characters, including '/'
//   *   any sequence of characters except '/'
//   ?   one character except '/'
// A trailing '/' means "everything under this directory" (same as dir/**).

import { hasControl } from "./text";

const MAX_PATTERNS = 200;
const MAX_PATTERN_LENGTH = 512;

export function isGlob(p: string): boolean {
  return /[*?]/.test(p);
}

/** Normalises a pattern and rejects unsafe or malformed input. Returns null if invalid. */
export function normalizePattern(raw: string): string | null {
  let p = raw.trim().replace(/\\/g, "/").replace(/\/{2,}/g, "/");
  while (p.startsWith("./")) p = p.slice(2);
  if (p === "" || p.length > MAX_PATTERN_LENGTH) return null;
  if (hasControl(p, false)) return null;
  if (p.startsWith("/")) return null;
  if (p.split("/").some((seg) => seg === "..")) return null;
  if (p.endsWith("/")) p += "**";
  return p;
}

/** Validates a JSON value as a list of path patterns. */
export function parsePatterns(value: unknown, field: string): { ok: true; paths: string[] } | { ok: false; message: string } {
  if (!Array.isArray(value)) return { ok: false, message: `${field} must be an array of paths` };
  if (value.length > MAX_PATTERNS) return { ok: false, message: `${field} has more than ${MAX_PATTERNS} entries` };
  const out = new Set<string>();
  for (const v of value) {
    if (typeof v !== "string") return { ok: false, message: `${field} must contain only strings` };
    const n = normalizePattern(v);
    if (n === null) return { ok: false, message: `${field} contains an invalid path: ${JSON.stringify(v)}` };
    out.add(n);
  }
  return { ok: true, paths: [...out].sort() };
}

const regexCache = new Map<string, RegExp>();

export function globToRegExp(glob: string): RegExp {
  const cached = regexCache.get(glob);
  if (cached) return cached;
  let re = "^";
  for (let i = 0; i < glob.length; i++) {
    const c = glob[i]!;
    if (c === "*") {
      if (glob[i + 1] === "*") {
        // "**/" also matches zero directories, so "**/x" matches "x".
        if (glob[i + 2] === "/") {
          re += "(?:.*/)?";
          i += 2;
        } else {
          re += ".*";
          i += 1;
        }
      } else {
        re += "[^/]*";
      }
    } else if (c === "?") {
      re += "[^/]";
    } else {
      re += c.replace(/[.+^${}()|[\]\\]/g, "\\$&");
    }
  }
  re += "$";
  const compiled = new RegExp(re);
  regexCache.set(glob, compiled);
  return compiled;
}

export function matches(glob: string, path: string): boolean {
  return globToRegExp(glob).test(path);
}

/** The literal part of a glob before its first wildcard. */
function staticPrefix(glob: string): string {
  const i = glob.search(/[*?]/);
  return i === -1 ? glob : glob.slice(0, i);
}

/**
 * Reports whether two patterns could name the same file.
 * - path vs path: equal.
 * - glob vs path: the glob matches the path.
 * - glob vs glob: approximate and conservative. They overlap unless their
 *   static prefixes diverge (e.g. "docs/*.md" vs "src/**" do not overlap;
 *   "**\/*.md" vs "src/**" do). False positives cost a warning; false
 *   negatives cost a merge conflict, so we lean to warning.
 */
export function patternsIntersect(a: string, b: string): boolean {
  const ga = isGlob(a);
  const gb = isGlob(b);
  if (!ga && !gb) return a === b;
  if (ga && !gb) return matches(a, b);
  if (!ga && gb) return matches(b, a);
  const pa = staticPrefix(a);
  const pb = staticPrefix(b);
  return pa.startsWith(pb) || pb.startsWith(pa);
}

export interface PathOverlap {
  mine: string;
  theirs: string;
}

/** All intersecting pattern pairs between two sets. */
export function intersectSets(mine: readonly string[], theirs: readonly string[]): PathOverlap[] {
  const out: PathOverlap[] = [];
  for (const m of mine) {
    for (const t of theirs) {
      if (patternsIntersect(m, t)) out.push({ mine: m, theirs: t });
    }
  }
  return out;
}
