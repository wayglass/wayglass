import { beforeEach, describe, expect, it } from "vitest";
import vectors from "../../testdata/policy-vectors.json";
import type { FakeRepoHost } from "./fake-artifacts";
import {
  call,
  createProject,
  createProjectIn,
  declare,
  uniqueProject,
  useFakeArtifacts,
} from "./helpers";
import { BindingRepoHost } from "../src/artifacts";

let fake: FakeRepoHost;
beforeEach(() => {
  fake = useFakeArtifacts();
});
const get = (p: string) => call("GET", p);
const publicEnv = { PUBLIC_PROJECTS: "demo-*,eu-demo-*" as string } as Partial<Env>;
const publicGet = (cursor = "") =>
  call(
    "GET",
    `/v1/public/projects${cursor ? `?cursor=${encodeURIComponent(cursor)}` : ""}`,
    undefined,
    {},
    publicEnv,
  );

describe("project directory", () => {
  it("requires authentication for the private directory; public needs both gates", async () => {
    const yes = await createProject(uniqueProject("demo-"));
    const flagOff = await createProject(uniqueProject("demo-"));
    const prefixOff = await createProject();
    await call("PATCH", `/v1/projects/${yes}`, { public: true });
    await call("PATCH", `/v1/projects/${prefixOff}`, { public: true });
    await declare(yes, ["docs/**"]);
    expect((await call("GET", "/v1/projects", undefined, {})).status).toBe(401);
    const p = await publicGet();
    expect(p.json.data.projects.map((x: any) => x.name)).toEqual([yes]);
    expect(p.json.data.projects[0].counts).toEqual({
      agents: 1,
      intents: 1,
      open: 1,
    });
    expect(JSON.stringify(p.json)).not.toContain("remote");
    const all = (await get("/v1/projects")).json.data.projects.map(
      (x: any) => x.name,
    );
    expect(all).toEqual([yes, flagOff, prefixOff]);
    await call("PATCH", `/v1/projects/${yes}`, { public: false });
    expect((await publicGet()).json.data.projects).toEqual([]);
  });
  it("gives a repo browser each project's remote, branch, times and counts; the public row stays metadata", async () => {
    const POLICY = (vectors.policies as Record<string, string>).example!;
    const name = await createProject(uniqueProject("demo-browse-"));
    await call("PATCH", `/v1/projects/${name}`, { public: true });
    fake.commit(name, { ".wayglass/policy.yaml": POLICY, "web/login.ts": "export function login() {}\n" });
    const d = await declare(name, ["web/**"], "harden login", "agent-b");
    const { id, fork } = { id: d.json.data.intent.id, fork: d.json.data.fork.name };
    fake.commit(fork, { ".wayglass/policy.yaml": POLICY, "web/login.ts": "export function login() { return 1 }\n" });
    const proposed = await call("POST", `/v1/intents/${id}/propose`, { project: name });
    expect(proposed.json.data.decision.status).toBe("pending_human");
    await declare(name, ["docs/**"], "docs", "agent-a");

    const row = (await get(`/v1/projects?q=${name.slice(0, 11)}`)).json.data.projects.find((p: any) => p.name === name);
    expect(row).toMatchObject({
      name,
      jurisdiction: "us",
      trunk_repo: name,
      default_branch: "main",
      public: true,
      residency: "in_place",
      counts: { intents: 2, open: 2, agents: 2, pending_human: 1, landed: 0 },
    });
    expect(row.trunk_remote).toContain(name);
    expect(row.created_at).toMatch(/^\d{4}-/);
    expect(row.last_activity >= row.created_at).toBe(true);
    expect(row.last_push_at).toBeNull();

    const pub = (await publicGet()).json.data.projects.find((p: any) => p.name === name);
    expect(pub).toEqual({ name, jurisdiction: "us", counts: { intents: 2, open: 2, agents: 2 } });
  });
  it("searches names server-side, case-insensitively, without changing the page walk", async () => {
    const hit = await createProject(uniqueProject("demo-search-"));
    const miss = await createProject(uniqueProject("other-"));
    const found = (p: string) => get(`/v1/projects?q=${encodeURIComponent(p)}`).then((r) => r.json.data);
    expect((await found("DEMO-SEARCH")).projects.map((p: any) => p.name)).toEqual([hit]);
    expect((await found("demo-search")).q).toBe("demo-search");
    expect((await found("no-such-project")).projects).toEqual([]);
    expect((await found("")).projects.map((p: any) => p.name)).toEqual([hit, miss]);
    expect((await get(`/v1/projects?q=${"a".repeat(64)}`)).status).toBe(400);
  });
  it("paginates bounded namespace pages and never lists forks or unregistered repos", async () => {
    for (let n = 0; n < 21; n++)
      fake.repos.set(`unregistered-${n}`, { source: null });
    const name = await createProjectIn("eu", uniqueProject("eu-demo-"));
    await call("PATCH", `/v1/projects/${name}`, { public: true });
    const names: string[] = [];
    let cursor = "",
      pages = 0;
    do {
      const r = await publicGet(cursor);
      names.push(...r.json.data.projects.map((x: any) => x.name));
      cursor = r.json.data.next;
      pages++;
    } while (cursor && pages < 10);
    expect(names).toEqual([name]);
    expect(pages).toBe(4);
  });
  it("rejects malformed and oversized cursors and mutation methods", async () => {
    for (const cursor of [
      "invalid",
      btoa('{"index":999,"cursor":"x"}'),
      "x".repeat(4097),
    ])
      expect((await publicGet(cursor)).status).toBe(400);
    expect(
      (await call("POST", "/v1/public/projects", {}, {}, publicEnv)).status,
    ).toBe(405);
  });
  it("binding directory strips provider metadata and forwards paging", async () => {
    const host = new BindingRepoHost({
      list: async (opts: any) => {
        expect(opts).toEqual({ limit: 20, cursor: "opaque" });
        return {
          repos: [{ name: "demo-a", remote: "private", token: "private" }],
          cursor: "next",
        };
      },
    } as unknown as Artifacts);
    expect(await host.listRepos(20, "opaque")).toEqual({
      names: ["demo-a"],
      cursor: "next",
    });
  });
});

describe("authenticated repository reads", () => {
  it("reads trees, files and historical refs; meters reads without minting tokens", async () => {
    const p = await createProject();
    const a = fake.commit(p, { "dir/file.txt": "before\n" });
    const b = fake.commit(p, {
      "dir/file.txt": "after\n",
      "README.md": "Hello",
    });
    const tokens = fake.tokens.length;
    const before = (await get(`/v1/cost?project=${p}`)).json.data.total.ops;
    expect((await get(`/v1/projects/${p}/browse`)).json.data).toMatchObject({
      ref: b,
      type: "tree",
      entries: [{ name: "README.md" }, { name: "dir" }],
    });
    expect(
      (await get(`/v1/projects/${p}/browse?path=dir%2Ffile.txt&ref=${a}`)).json
        .data,
    ).toMatchObject({ text: "before\n", binary: false, truncated: false });
    expect(fake.tokens).toHaveLength(tokens);
    expect(
      (await get(`/v1/cost?project=${p}`)).json.data.total.ops,
    ).toBeGreaterThan(before);
    expect(
      (await call("GET", `/v1/projects/${p}/browse`, undefined, {})).status,
    ).toBe(401);
    expect(
      (
        await call(
          "GET",
          `/v1/public/projects/${p}/browse`,
          undefined,
          {},
          publicEnv,
        )
      ).status,
    ).toBe(404);
  });
  it("bounds history, directory and file output, flags binary and empty repos", async () => {
    const p = await createProject();
    expect((await get(`/v1/projects/${p}/browse`)).json.data.empty).toBe(true);
    const files = {
      large: "a".repeat(131073),
      binary: new Uint8Array([0, 1, 2]),
    };
    for (let n = 0; n < 22; n++) fake.commit(p, files);
    const hist = (await get(`/v1/projects/${p}/history`)).json.data;
    expect(hist.commits).toHaveLength(20);
    expect(hist.next).toBeTruthy();
    expect(
      (await get(`/v1/projects/${p}/history?cursor=${hist.next}`)).json.data
        .commits,
    ).toHaveLength(2);
    const large = (await get(`/v1/projects/${p}/browse?path=large`)).json.data;
    expect(large.truncated).toBe(true);
    expect(large.text).toHaveLength(131072);
    expect(
      (await get(`/v1/projects/${p}/browse?path=binary`)).json.data,
    ).toMatchObject({ binary: true, text: null });
  });
  it("rejects traversal, invalid refs, absent paths and unsupported methods", async () => {
    const p = await createProject();
    fake.commit(p, { safe: "ok" });
    for (const path of ["../safe", "a//b", "a/./b", "a\\b", "/safe", "a\0b"])
      expect(
        (await get(`/v1/projects/${p}/browse?path=${encodeURIComponent(path)}`))
          .status,
      ).toBe(400);
    expect((await get(`/v1/projects/${p}/browse?ref=main`)).status).toBe(400);
    expect((await get(`/v1/projects/${p}/browse?path=missing`)).status).toBe(
      404,
    );
    expect((await call("POST", `/v1/projects/${p}/browse`, {})).status).toBe(
      405,
    );
  });
  it("reads the exact proposed decision, even after a newer proposal", async () => {
    const p = await createProject();
    const policy = (vectors.policies as Record<string, string>).example!;
    const base = fake.commit(p, {
      ".wayglass/policy.yaml": policy,
      "docs/file.md": "before\n",
    });
    const intent = (await declare(p, ["docs/**"])).json.data;
    const first = fake.commit(intent.fork.name, {
      ".wayglass/policy.yaml": policy,
      "docs/file.md": "first\n",
    });
    const d = (
      await call("POST", `/v1/intents/${intent.intent.id}/propose`, {
        project: p,
      })
    ).json.data.decision;
    fake.commit(intent.fork.name, {
      ".wayglass/policy.yaml": policy,
      "docs/file.md": "second\n",
    });
    await call("POST", `/v1/intents/${intent.intent.id}/propose`, {
      project: p,
    });
    const r = (
      await get(`/v1/projects/${p}/changes/${d.id}?path=docs%2Ffile.md`)
    ).json.data;
    expect(r).toMatchObject({
      decision_id: d.id,
      base,
      commit: first,
      before: { text: "before\n" },
      after: { text: "first\n" },
    });
    expect(
      (
        await get(
          `/v1/projects/${p}/changes/${d.id}?path=.wayglass%2Fpolicy.yaml`,
        )
      ).status,
    ).toBe(400);
    expect(
      (
        await call(
          "GET",
          `/v1/projects/${p}/changes/${d.id}?path=docs%2Ffile.md`,
          undefined,
          {},
        )
      ).status,
    ).toBe(401);
  });
});
