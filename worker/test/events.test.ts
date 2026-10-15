// Queue consumer for cf.artifacts.repo.pushed (src/events.ts). Queues deliver
// at least once and the payload is written by whoever pushed, so the consumer
// must be idempotent, keep only routing fields, and write a push only into the
// project whose namespace it came from.

import { env } from "cloudflare:workers";
import { createExecutionContext, createMessageBatch, getQueueResult } from "cloudflare:test";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { setRepoHostForTests } from "../src/artifacts";
import { parsePushed, pushRejection } from "../src/events";
import worker from "../src/index";
import { FakeRepoHost } from "./fake-artifacts";
import { call, createProject, createProjectIn, declare } from "./helpers";

beforeEach(() => {
  setRepoHostForTests(null);
  setRepoHostForTests(new FakeRepoHost(), "us");
  setRepoHostForTests(new FakeRepoHost(), "eu");
});
afterEach(() => setRepoHostForTests(null));

const SHA_A = "a".repeat(40);
const SHA_B = "b".repeat(40);

const pushed = (repoName: string, namespace = "wayglass", payload: Record<string, unknown> = {}) => ({
  type: "cf.artifacts.repo.pushed",
  source: { type: "artifacts.repo", namespace, repoName },
  payload: {
    ref: "refs/heads/main",
    before: SHA_A,
    after: SHA_B,
    commits: [{ id: SHA_B, message: "feat: x", author: { name: "a", email: "a@x" }, parents: [SHA_A] }],
    totalCommitsCount: 1,
    commitsTruncated: false,
    ...payload,
  },
  metadata: { eventSchemaVersion: 1 },
});

async function deliver(messages: { id: string; body: unknown }[]) {
  const batch = createMessageBatch<unknown>(
    "artifacts-events",
    messages.map((m) => ({ id: m.id, timestamp: new Date(), attempts: 1, body: m.body })),
  );
  const ctx = createExecutionContext();
  await worker.queue(batch, env, ctx);
  return getQueueResult(batch, ctx);
}

async function pushEvents(project: string) {
  const r = await call("GET", `/v1/audit?project=${project}`);
  return (r.json.data.events as { kind: string; intent_id: string | null; data: Record<string, unknown> }[]).filter((e) => e.kind === "artifacts.repo.pushed");
}

describe("parsePushed", () => {
  it.each([
    ["demo", "demo", null],
    ["demo--i0123456789", "demo", "i0123456789"],
    ["demo--i0123456789--r2", "demo", "i0123456789"],
    ["my.project_1", "my.project_1", null],
  ])("accepts %s as a Wayglass repo", (repo, project, intent) => {
    expect(parsePushed(pushed(repo))).toMatchObject({ repo, project, intent_id: intent });
  });

  it.each([
    "Demo", // uppercase: never a project name
    "demo--junk", // not an intent id: not a fork the Worker made
    "demo--i0123456789--x", // unknown suffix
    "demo--i0123456789--r0",
    "--i0123456789",
    "demo--", // trailing separator
    "a".repeat(64), // longer than a project name
    "../demo",
    "demo\u0000",
  ])("drops a repo named %s", (repo) => {
    expect(parsePushed(pushed(repo))).toBeNull();
  });

  it("drops events that are not a push, or have no payload", () => {
    expect(parsePushed(pushed("demo", "wayglass", {}))).not.toBeNull();
    expect(parsePushed({ ...pushed("demo"), type: "cf.artifacts.repo.created" })).toBeNull();
    expect(parsePushed({ ...pushed("demo"), payload: undefined })).toBeNull();
    expect(parsePushed({ ...pushed("demo"), payload: "x" })).toBeNull();
    expect(parsePushed({ type: "cf.artifacts.repo.pushed", source: {} })).toBeNull();
    expect(parsePushed(null)).toBeNull();
    expect(parsePushed(42)).toBeNull();
  });

  it("keeps the namespace and either spelling of the repo name", () => {
    expect(parsePushed(pushed("demo", "wayglass-eu"))).toMatchObject({ namespace: "wayglass-eu" });
    const snake = { ...pushed("demo"), source: { type: "artifacts.repo", namespace: "wayglass", repo_name: "demo" } };
    expect(parsePushed(snake)).toMatchObject({ repo: "demo", namespace: "wayglass" });
    expect(parsePushed({ ...pushed("demo"), source: { repoName: "demo" } })).toMatchObject({ namespace: "" });
  });

  it("clamps the commit count to a non-negative integer and the commit list to ids", () => {
    expect(parsePushed(pushed("demo", "wayglass", { totalCommitsCount: -5 }))).toMatchObject({ total_commits: 1 });
    expect(parsePushed(pushed("demo", "wayglass", { totalCommitsCount: 1.5 }))).toMatchObject({ total_commits: 1 });
    expect(parsePushed(pushed("demo", "wayglass", { totalCommitsCount: "9" }))).toMatchObject({ total_commits: 1 });
    expect(parsePushed(pushed("demo", "wayglass", { totalCommitsCount: 7 }))).toMatchObject({ total_commits: 7 });
    expect(parsePushed(pushed("demo", "wayglass", { commits: "nope" }))).toMatchObject({ commits: [], total_commits: 1 });
    expect(parsePushed(pushed("demo", "wayglass", { commits: [null, 1, { id: SHA_A }, { id: "x" }] }))).toMatchObject({ commits: [SHA_A] });
  });

  it("records a deleted branch (zero after) and a tag ref as they are", () => {
    const zero = "0".repeat(40);
    expect(parsePushed(pushed("demo", "wayglass", { after: zero, commits: [], totalCommitsCount: 0 }))).toMatchObject({ after: zero, commits: [], total_commits: 0 });
    expect(parsePushed(pushed("demo", "wayglass", { ref: "refs/tags/v1" }))).toMatchObject({ ref: "refs/tags/v1" });
  });

  it("never carries commit messages, authors or emails", () => {
    const got = parsePushed(pushed("demo--i0123456789"))!;
    expect(JSON.stringify(got)).not.toMatch(/feat: x|author|email|a@x|message/);
    expect(Object.keys(got).sort()).toEqual(["after", "before", "commits", "intent_id", "namespace", "project", "ref", "repo", "total_commits"]);
  });
});

describe("pushRejection", () => {
  it("accepts a push from the project's own namespace, or one naming none", () => {
    expect(pushRejection(env, parsePushed(pushed("demo", "wayglass"))!, "us")).toBeNull();
    expect(pushRejection(env, parsePushed(pushed("demo", "wayglass-eu"))!, "eu")).toBeNull();
    expect(pushRejection(env, { ...parsePushed(pushed("demo"))!, namespace: "" }, "eu")).toBeNull();
  });

  it("refuses a push from another namespace, bound or not", () => {
    expect(pushRejection(env, parsePushed(pushed("demo", "wayglass-eu"))!, "us")).toMatch(/jurisdiction/);
    expect(pushRejection(env, parsePushed(pushed("demo", "wayglass"))!, "eu")).toMatch(/jurisdiction/);
    expect(pushRejection(env, parsePushed(pushed("demo", "someone-else"))!, "us")).toMatch(/not bound/);
  });
});

describe("queue consumer", () => {
  it("records a redelivered message once", async () => {
    const project = await createProject();
    const id = (await declare(project, ["a"])).json.data.intent.id as string;
    const body = pushed(`${project}--${id}`);
    const first = await deliver([{ id: "m-dup", body }]);
    expect(first.explicitAcks).toEqual(["m-dup"]);
    // The same message again (a retry, or a second delivery of the same event).
    const again = await deliver([{ id: "m-dup", body }]);
    expect(again.explicitAcks).toEqual(["m-dup"]);
    expect(again.retryMessages).toEqual([]);

    const events = await pushEvents(project);
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ intent_id: id, data: { repo: `${project}--${id}`, after: SHA_B, event_id: "m-dup" } });
    // Nor does the duplicate count twice as git traffic.
    expect((await call("GET", `/v1/cost?project=${project}`)).json.data.events).toEqual({ pushed: 1 });
  });

  it("records distinct messages for the same commit separately", async () => {
    const project = await createProject();
    // Two branches pushed to the same tip are two pushes, not one.
    await deliver([
      { id: "m-1", body: pushed(project) },
      { id: "m-2", body: pushed(project, "wayglass", { ref: "refs/heads/feature" }) },
    ]);
    const events = await pushEvents(project);
    expect(events.map((e) => e.data.ref)).toEqual(["refs/heads/main", "refs/heads/feature"]);
  });

  it("drops a push whose namespace is not the project's, without a retry", async () => {
    const us = await createProject();
    const eu = await createProjectIn("eu");
    const r = await deliver([
      { id: "m-us-wrong", body: pushed(us, "wayglass-eu") },
      { id: "m-eu-wrong", body: pushed(eu, "wayglass") },
      { id: "m-unbound", body: pushed(us, "another-account-namespace") },
      { id: "m-eu-ok", body: pushed(eu, "wayglass-eu") },
      { id: "m-us-ok", body: pushed(us, "wayglass") },
    ]);
    expect(r.explicitAcks.sort()).toEqual(["m-eu-ok", "m-eu-wrong", "m-unbound", "m-us-ok", "m-us-wrong"]);
    expect(r.retryMessages).toEqual([]);
    expect((await pushEvents(us)).map((e) => e.data.event_id)).toEqual(["m-us-ok"]);
    expect((await pushEvents(eu)).map((e) => e.data.event_id)).toEqual(["m-eu-ok"]);
  });

  it("acknowledges junk, other event types and unknown projects without creating anything", async () => {
    const r = await deliver([
      { id: "j-1", body: "garbage" },
      { id: "j-2", body: { type: "cf.artifacts.repo.token.created", source: { namespace: "wayglass", repoName: "demo" }, payload: { tokenId: "t" } } },
      { id: "j-3", body: pushed("never-made-1") },
      { id: "j-4", body: pushed("Bad Name") },
    ]);
    expect(r.explicitAcks.sort()).toEqual(["j-1", "j-2", "j-3", "j-4"]);
    expect(r.retryMessages).toEqual([]);
    const unknown = await call("GET", "/v1/audit?project=never-made-1");
    expect(unknown.status).toBe(404);
  });

  it("attributes a fork push to its intent only when the intent exists in that project", async () => {
    const project = await createProject();
    await deliver([{ id: "f-1", body: pushed(`${project}--i0000000000`) }]);
    const [ev] = await pushEvents(project);
    expect(ev).toMatchObject({ intent_id: null, data: { repo: `${project}--i0000000000` } });
  });
});
