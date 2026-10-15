import { describe, expect, it } from "vitest";
import { BindingRepoHost } from "../src/artifacts";
import { Meter, metered } from "../src/cost";

// BindingRepoHost is otherwise only exercised by the spike against real
// Artifacts. These tests pin one property the fake RepoHost cannot: every
// call on a repo stub happens before that stub is disposed. The real binding
// fails with "RPC stub used after being disposed" otherwise.

function stubBinding() {
  const calls: string[] = [];
  const repo = (name: string) => {
    let disposed = false;
    const use = async (op: string) => {
      await Promise.resolve(); // RPC calls always cross an await
      if (disposed) throw new Error(`RPC stub used after being disposed (${name}.${op})`);
      calls.push(`${name}.${op}`);
    };
    return {
      async revokeToken() {
        await use("revokeToken");
        return true;
      },
      async createToken(scope: "read" | "write", ttl: number) {
        await use("createToken");
        return { id: "tok-1", plaintext: "art_v1_x", scope, expiresAt: new Date(Date.now() + ttl * 1000).toISOString() };
      },
      async fork(forkName: string) {
        await use("fork");
        return { name: forkName, remote: `https://r/${forkName}.git`, defaultBranch: "main", token: "art_v1_fork" };
      },
      [Symbol.dispose]() {
        disposed = true;
      },
    };
  };
  const ns = {
    async create(name: string) {
      return { name, remote: `https://r/${name}.git`, defaultBranch: "main", token: "art_v1_create" };
    },
    async get(name: string) {
      return repo(name);
    },
  };
  return { host: new BindingRepoHost(ns as unknown as Artifacts), calls };
}

describe("BindingRepoHost stub lifetime", () => {
  it("createTrunk revokes the creation token and mints a new one before disposing the repo", async () => {
    const { host, calls } = stubBinding();
    const { repo, token } = await host.createTrunk("p", "d");
    expect(repo.name).toBe("p");
    expect(token.value).toBe("art_v1_x");
    expect(calls).toEqual(["p.revokeToken", "p.createToken"]);
  });

  it("revokeToken completes before disposing the repo", async () => {
    const { host, calls } = stubBinding();
    await expect(host.revokeToken("p", "tok-1")).resolves.toBe(true);
    expect(calls).toEqual(["p.revokeToken"]);
  });

  it("mintToken and fork complete before disposing", async () => {
    const { host, calls } = stubBinding();
    await host.mintToken("p", "write", 60);
    await host.fork("p", "p-f", "d");
    expect(calls).toEqual(["p.createToken", "p.fork", "p-f.revokeToken"]);
  });
});

describe("metered() matches what BindingRepoHost calls", () => {
  // Counts every call on the namespace and on repo stubs, including get().
  function countingBinding() {
    const calls: Record<string, number> = {};
    const hit = (op: string) => (calls[op] = (calls[op] ?? 0) + 1);
    const repo = () => ({
      async revokeToken() { hit("revokeToken"); return true; },
      async createToken(scope: "read" | "write") { hit("createToken"); return { id: "t", plaintext: "x", scope, expiresAt: "" }; },
      async fork(n: string) { hit("fork"); return { name: n, remote: "r", defaultBranch: "main", token: "f" }; },
      async log() { hit("log"); return [{ hash: "h" }]; },
      async readCommit() { hit("readCommit"); return { hash: "h", treeHash: "t", parents: [] }; },
      async readTree() { hit("readTree"); return []; },
      async readBlob() { hit("readBlob"); return null; },
      async readFile() { hit("readFile"); return null; },
      [Symbol.dispose]() {},
    });
    const ns = {
      async create(n: string) { hit("create"); return { name: n, remote: "r", defaultBranch: "main", token: "c" }; },
      async get() { hit("get"); return repo(); },
      async delete() { hit("delete"); return true; },
    };
    return { host: new BindingRepoHost(ns as unknown as Artifacts), calls };
  }

  it.each([
    ["createTrunk", (h: any) => h.createTrunk("p", "d")],
    ["fork", (h: any) => h.fork("p", "p-f", "d")],
    ["mintToken", (h: any) => h.mintToken("p", "write", 60)],
    ["revokeToken", (h: any) => h.revokeToken("p", "t")],
    ["deleteRepo", (h: any) => h.deleteRepo("p")],
    [
      "reader",
      async (h: any) => {
        using r = await h.reader("p");
        await r.head();
        await r.readCommit("h");
        await r.readTree("t");
        await r.readBlob("b");
        await r.readFile("h", "x");
      },
    ],
  ])("%s", async (_name, f) => {
    const { host, calls } = countingBinding();
    const m = new Meter();
    await f(metered(host, m));
    expect(m.ops).toEqual(calls);
  });
});

describe("BindingRepoHost.deleteRepo", () => {
  const host = (impl: (name: string) => Promise<boolean>) => new BindingRepoHost({ delete: impl } as unknown as Artifacts);

  it("reports a missing repo as already gone, whether the binding returns false or throws NOT_FOUND", async () => {
    await expect(host(async () => true).deleteRepo("p")).resolves.toBe(true);
    await expect(host(async () => false).deleteRepo("p")).resolves.toBe(false);
    await expect(host(async () => Promise.reject(Object.assign(new Error("gone"), { code: "NOT_FOUND" }))).deleteRepo("p")).resolves.toBe(false);
  });

  it("passes other failures on", async () => {
    await expect(host(async () => Promise.reject(Object.assign(new Error("busy"), { code: "UPSTREAM_UNAVAILABLE" }))).deleteRepo("p")).rejects.toThrow(/UPSTREAM_UNAVAILABLE/);
  });
});
