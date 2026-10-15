import { describe, expect, it } from "vitest";
import { intersectSets, matches, normalizePattern, parsePatterns, patternsIntersect } from "../src/paths";

describe("normalizePattern", () => {
  it.each([
    ["web/login.ts", "web/login.ts"],
    ["./web/login.ts", "web/login.ts"],
    ["web//a.ts", "web/a.ts"],
    ["web/", "web/**"],
    ["  docs/*.md ", "docs/*.md"],
    ["/etc/passwd", null],
    ["../x", null],
    ["a/../../x", null],
    ["", null],
    ["web/\u001b[2Jx.ts", null],
    ["web/a\tb", null],
    ["web/\u202ecod.exe", null],
  ])("%j -> %j", (input, want) => {
    expect(normalizePattern(input)).toBe(want);
  });
});

describe("matches", () => {
  it.each([
    ["docs/**", "docs/a/b.md", true],
    ["docs/*.md", "docs/a.md", true],
    ["docs/*.md", "docs/a/b.md", false],
    ["**/*.md", "README.md", true],
    ["**/*.md", "a/b/c.md", true],
    ["**/auth/**", "web/auth/x.ts", true],
    ["**/login*", "web/login.ts", true],
    ["web/?.ts", "web/a.ts", true],
    ["web/?.ts", "web/ab.ts", false],
    ["web/a.ts", "web/a.ts", true],
    ["web/a+b.ts", "web/a+b.ts", true],
    ["web/a.ts", "web/aXts", false],
  ])("%s ~ %s = %s", (glob, path, want) => {
    expect(matches(glob, path)).toBe(want);
  });
});

describe("patternsIntersect", () => {
  it.each([
    ["web/login.ts", "web/login.ts", true],
    ["web/login.ts", "web/logout.ts", false],
    ["web/**", "web/login.ts", true],
    ["web/login.ts", "web/**", true],
    ["docs/**", "web/login.ts", false],
    ["docs/*.md", "src/**", false],
    ["web/**", "web/auth/**", true],
    ["**/*.md", "src/**", true],
    ["src/a*.ts", "src/b*.ts", false], // static prefixes diverge
    ["src/*.ts", "src/a*", true], // conservative: shared prefix "src/"
  ])("%s vs %s = %s", (a, b, want) => {
    expect(patternsIntersect(a, b)).toBe(want);
    expect(patternsIntersect(b, a)).toBe(want);
  });
});

describe("intersectSets", () => {
  it("returns every intersecting pair", () => {
    expect(intersectSets(["web/**", "docs/a.md"], ["web/login.ts", "docs/b.md", "docs/a.md"])).toEqual([
      { mine: "web/**", theirs: "web/login.ts" },
      { mine: "docs/a.md", theirs: "docs/a.md" },
    ]);
  });
});

describe("parsePatterns", () => {
  it("dedupes and sorts", () => {
    expect(parsePatterns(["b", "a", "./a"], "paths")).toEqual({ ok: true, paths: ["a", "b"] });
  });
  it("rejects non-arrays and bad entries", () => {
    expect(parsePatterns("a", "paths").ok).toBe(false);
    expect(parsePatterns([1], "paths").ok).toBe(false);
    expect(parsePatterns(["../x"], "paths").ok).toBe(false);
    expect(parsePatterns(Array.from({ length: 201 }, (_, i) => `f${i}`), "paths").ok).toBe(false);
  });
});
