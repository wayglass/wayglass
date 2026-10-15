import { describe, expect, it } from "vitest";
import { DEFAULT_CAPS, diffTrees, isBinary, lineDiff, splitLines } from "../src/diff";
import { FakeRepoHost, type Files } from "./fake-artifacts";

describe("lineDiff", () => {
  it.each([
    ["identical", "a\nb\n", "a\nb\n", [], 0],
    ["append", "a\n", "a\nb\n", ["b"], 0],
    ["edit middle", "a\nb\nc\n", "a\nB\nc\n", ["B"], 1],
    ["new file", "", "x\ny\n", ["x", "y"], 0],
    ["deleted file", "x\ny\n", "", [], 2],
    ["no trailing newline", "a", "a\nb", ["b"], 0],
    ["duplicate lines count once each", "x\nx\n", "x\nx\nx\n", ["x"], 0],
    ["moved line is neither added nor removed", "a\nb\nc\n", "b\nc\na\n", [], 0],
  ])("%s", (_n, before, after, added, removed) => {
    expect(lineDiff(before, after)).toEqual({ added, removed });
  });

  it("splits lines like git", () => {
    expect(splitLines("")).toEqual([]);
    expect(splitLines("a\n")).toEqual(["a"]);
    expect(splitLines("a\n\n")).toEqual(["a", ""]);
    expect(splitLines("a")).toEqual(["a"]);
  });

  it("detects binary content by NUL byte", () => {
    expect(isBinary(new Uint8Array([104, 105]))).toBe(false);
    expect(isBinary(new Uint8Array([137, 80, 0, 1]))).toBe(true);
  });
});

describe("diffTrees", () => {
  async function trees(before: Files, after: Files) {
    const fake = new FakeRepoHost();
    fake.repos.set("r", { source: null });
    const a = fake.commit("r", before);
    const b = fake.commit("r", after);
    const repo = await fake.reader("r");
    const ta = (await repo.readCommit(a))!.tree;
    const tb = (await repo.readCommit(b))!.tree;
    Object.assign(fake.reads, { tree: 0, blob: 0, commit: 0, file: 0 });
    return { fake, repo, ta, tb };
  }

  it("lists added, modified and deleted files in path order with line counts", async () => {
    const { repo, ta, tb } = await trees(
      { "a.txt": "1\n", "src/x.ts": "x\n", "src/gone.ts": "g\nh\n" },
      { "a.txt": "1\n2\n", "src/x.ts": "x\n", "src/new.ts": "n\n" },
    );
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb });
    expect(d.truncated).toBe(false);
    expect(d.changes).toEqual([
      { path: "a.txt", added_lines: ["2"], removed_count: 0 },
      { path: "src/gone.ts", added_lines: [], removed_count: 2 },
      { path: "src/new.ts", added_lines: ["n"], removed_count: 0 },
    ]);
  });

  it("skips unchanged subtrees without reading them", async () => {
    const big: Files = {};
    for (let i = 0; i < 50; i++) big[`vendor/lib${i}/x.js`] = `v${i}\n`;
    const { fake, repo, ta, tb } = await trees({ ...big, "docs/a.md": "a\n" }, { ...big, "docs/a.md": "b\n" });
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb });
    expect(d.paths).toEqual(["docs/a.md"]);
    expect(fake.reads.tree).toBe(4); // two roots, two docs/ trees
    expect(fake.reads.blob).toBe(2);
  });

  it("handles a file replaced by a directory", async () => {
    const { repo, ta, tb } = await trees({ x: "file\n" }, { "x/y": "nested\n" });
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb });
    expect(d.paths).toEqual(["x", "x/y"]);
  });

  it("diffs against an empty base", async () => {
    const { repo, tb } = await trees({}, { "a/b.txt": "hi\n" });
    const d = await diffTrees({ repo, tree: null }, { repo, tree: tb });
    expect(d.changes).toEqual([{ path: "a/b.txt", added_lines: ["hi"], removed_count: 0 }]);
  });

  it("marks binary files with their size and does not read their lines", async () => {
    const { repo, ta, tb } = await trees({ "img.png": new Uint8Array([1, 0, 2]) }, { "img.png": new Uint8Array([1, 0, 3, 4]) });
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb });
    expect(d.changes).toEqual([{ path: "img.png", added_lines: [], removed_count: 0, binary: true, bytes: 4 }]);
  });

  // Binary means the detectors read nothing, so the flag follows what
  // arrives: a secret behind a NUL byte is unscanned whatever the file is
  // called, a deleted binary brings nothing new, and a file that turns
  // from binary to text is scanned as a fresh add.
  it.each([
    ["a secret behind a NUL byte in a .txt", { "notes.txt": "plain\n" }, { "notes.txt": new Uint8Array([0, 97, 112, 105, 95, 107, 101, 121]) }, [{ path: "notes.txt", added_lines: [], removed_count: 0, binary: true, bytes: 8 }]],
    ["an allowlisted name holding text is scanned", { "a.md": "x\n" }, { "a.md": "x\n", "logo.png": "api_key = 1\n" }, [{ path: "logo.png", added_lines: ["api_key = 1"], removed_count: 0 }]],
    ["a deleted binary is not binary", { "blob.bin": new Uint8Array([0, 1]) }, {}, [{ path: "blob.bin", added_lines: [], removed_count: 0 }]],
    ["binary to text is a fresh add", { "f": new Uint8Array([0, 1]) }, { "f": "one\ntwo\n" }, [{ path: "f", added_lines: ["one", "two"], removed_count: 0 }]],
    ["text to binary is binary", { "f": "one\n" }, { "f": new Uint8Array([0, 1, 2]) }, [{ path: "f", added_lines: [], removed_count: 0, binary: true, bytes: 3 }]],
    ["a rename to a binary name is a delete plus a binary add", { "notes.txt": "one\n" }, { "notes.dat": new Uint8Array([0, 1]) }, [
      { path: "notes.dat", added_lines: [], removed_count: 0, binary: true, bytes: 2 },
      { path: "notes.txt", added_lines: [], removed_count: 1 },
    ]],
  ])("%s", async (_n, before, after, changes) => {
    const { repo, ta, tb } = await trees(before as Files, after as Files);
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb });
    expect(d.changes).toEqual(changes);
    expect(d.truncated).toBe(false);
  });

  it("path-only mode reads no blobs", async () => {
    const { fake, repo, ta, tb } = await trees({ "a.txt": "1\n" }, { "a.txt": "2\n" });
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb }, { withContent: false });
    expect(d.paths).toEqual(["a.txt"]);
    expect(fake.reads.blob).toBe(0);
  });

  it.each([
    ["files", { maxFiles: 2 }, /files changed/],
    ["tree reads", { maxTreeReads: 2 }, /directories/],
    ["blob size", { maxBlobBytes: 4 }, /larger than/],
    ["total bytes", { maxTotalBytes: 6 }, /reads more than/],
  ])("marks the diff truncated when the %s cap is hit", async (_n, cap, reason) => {
    const { repo, ta, tb } = await trees({}, { "a/1.txt": "11111\n", "b/2.txt": "2\n", "c/3.txt": "3\n" });
    const d = await diffTrees({ repo, tree: ta }, { repo, tree: tb }, { caps: { ...DEFAULT_CAPS, ...cap } });
    expect(d.truncated).toBe(true);
    expect(d.truncated_reasons.join(" ")).toMatch(reason);
  });
});
