import { env } from "cloudflare:workers";
import { setRepoHostForTests } from "../src/artifacts";
import worker from "../src/index";
import { FakeRepoHost } from "./fake-artifacts";

export const KEY = "test-admin-key";

export interface Envelope<T = any> {
  v: number;
  ok: boolean;
  data?: T;
  error?: { code: string; message: string };
}

export function useFakeArtifacts(): FakeRepoHost {
  const fake = new FakeRepoHost();
  setRepoHostForTests(fake);
  return fake;
}

export async function call<T = any>(
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = { authorization: `Bearer ${KEY}` },
  envOverride: Partial<Env> = {},
): Promise<{ status: number; json: Envelope<T> }> {
  const init: RequestInit = { method, headers: { ...headers } };
  if (body !== undefined) {
    init.body = typeof body === "string" ? body : JSON.stringify(body);
    (init.headers as Record<string, string>)["content-type"] = "application/json";
  }
  const req = new Request(`https://wayglass.test${path}`, init) as Request<unknown, IncomingRequestCfProperties>;
  const res = await worker.fetch(req, { ...env, ...envOverride });
  return { status: res.status, json: (await res.json()) as Envelope<T> };
}

let counter = 0;
/** A project name unique to this test run. */
export function uniqueProject(prefix = "p"): string {
  counter += 1;
  return `${prefix}${counter}-${crypto.randomUUID().slice(0, 6)}`;
}

export async function createProject(name = uniqueProject()): Promise<string> {
  const r = await call("POST", "/v1/projects", { name });
  if (r.status !== 201) throw new Error(`createProject failed: ${JSON.stringify(r.json)}`);
  return name;
}

/** A project in a given jurisdiction's namespace (e.g. "eu"). */
export async function createProjectIn(jurisdiction: string, name = uniqueProject()): Promise<string> {
  const r = await call("POST", "/v1/projects", { name, jurisdiction });
  if (r.status !== 201) throw new Error(`createProjectIn failed: ${JSON.stringify(r.json)}`);
  return name;
}

export async function declare(project: string, paths: string[], goal = "do a thing", agent = "agent-a") {
  return call("POST", "/v1/intents", { project, goal, reason: "because", paths, agent });
}
