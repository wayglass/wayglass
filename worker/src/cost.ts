// What Wayglass costs in Artifacts operations, counted where the Worker can
// see them: every call it makes on the Artifacts binding, attributed to the
// intent (or project) the request was for.
//
// What this is NOT: the client's own git traffic (clone, fetch, push to a
// fork) never passes through the Worker. Trunk pushes show up as
// `artifacts.repo.pushed` events and are counted separately; fork traffic is
// invisible. Cloudflare does not document which binding calls are billed
// operations, so every call is counted (including the `get` that opens a
// repo stub): for the Worker's side this is an upper bound, never a guess
// dressed as a measurement.

import type { RepoHost, RepoReader } from "./artifacts";

/** Published Artifacts pricing (developers.cloudflare.com/artifacts/platform/pricing/, checked Oct 11, 2026). */
export const PRICING = {
  usd_per_1000_ops: 0.15,
  included_ops_per_month: 10_000,
  source: "https://developers.cloudflare.com/artifacts/platform/pricing/",
  as_of: "2026-10-11",
} as const;

export const COST_SCOPE =
  "Worker-side Artifacts binding calls only (every call counted, so an upper bound for the Worker); " +
  "client git clone/fetch/push to forks is not visible to the Worker. Trunk pushes are counted from Artifacts events.";

/** Estimated USD for n operations at the marginal price, before the monthly included operations. */
export function estimateUsd(ops: number): number {
  return Math.round(((ops * PRICING.usd_per_1000_ops) / 1000) * 1e6) / 1e6;
}

/** Counts binding calls by operation name for one request. */
export class Meter {
  readonly ops: Record<string, number> = {};

  add(...names: string[]): void {
    for (const n of names) this.ops[n] = (this.ops[n] ?? 0) + 1;
  }

  total(): number {
    return Object.values(this.ops).reduce((a, b) => a + b, 0);
  }
}

/**
 * Wraps a RepoHost so each method counts the binding calls BindingRepoHost
 * makes for it (test/binding-host.test.ts pins the two together). Calls are
 * counted when made, whether or not they succeed: a failed call may still be
 * billed.
 */
export function metered(host: RepoHost, m: Meter): RepoHost {
  return {
    listRepos(limit, cursor) {
      m.add("list");
      return host.listRepos(limit, cursor);
    },
    createTrunk(name, description) {
      m.add("create", "get", "revokeToken", "createToken");
      return host.createTrunk(name, description);
    },
    fork(source, name, description) {
      m.add("get", "fork", "get", "revokeToken");
      return host.fork(source, name, description);
    },
    mintToken(repo, scope, ttl) {
      m.add("get", "createToken");
      return host.mintToken(repo, scope, ttl);
    },
    revokeToken(repo, id) {
      m.add("get", "revokeToken");
      return host.revokeToken(repo, id);
    },
    async reader(repo) {
      m.add("get");
      return meteredReader(await host.reader(repo), m);
    },
    deleteRepo(repo) {
      m.add("delete");
      return host.deleteRepo(repo);
    },
  };
}

function meteredReader(r: RepoReader, m: Meter): RepoReader {
  return {
    head() {
      m.add("log");
      return r.head();
    },
    readCommit(hash) {
      m.add("readCommit");
      return r.readCommit(hash);
    },
    readTree(hash) {
      m.add("readTree");
      return r.readTree(hash);
    },
    readBlob(hash) {
      m.add("readBlob");
      return r.readBlob(hash);
    },
    readFile(ref, path) {
      m.add("readFile");
      return r.readFile(ref, path);
    },
    [Symbol.dispose]() {
      r[Symbol.dispose]();
    },
  };
}

export interface CostLine {
  ops: number;
  by_op: Record<string, number>;
  /** Short-lived repo tokens minted (createToken calls). */
  tokens_minted: number;
  est_usd: number;
}

export function costLine(byOp: Record<string, number>): CostLine {
  const ops = Object.values(byOp).reduce((a, b) => a + b, 0);
  return { ops, by_op: byOp, tokens_minted: byOp.createToken ?? 0, est_usd: estimateUsd(ops) };
}

export interface CostSummary {
  scope: string;
  pricing: typeof PRICING;
  total: CostLine;
  /** Calls not tied to one intent: creating the trunk, read tokens for doctor. */
  project: CostLine;
  intents: Record<string, CostLine>;
  /** Git operations seen through Artifacts events (trunk only), by event kind. */
  events: Record<string, number>;
  /** Average Worker-side cost of the intents that landed. */
  per_landed_change: { landed: number; ops: number; est_usd: number } | null;
  /** The AI reviewer and adviser: calls, outcomes, tokens and the daily budget (src/aiproviders.ts). */
  ai: AiCost;
}

// ---- the AI reviewer's budget and numbers ---------------------------------

/** Model calls a project may make per UTC day when AI_DAILY_REVIEWS is unset or not a number. */
export const DEFAULT_AI_DAILY_CALLS = 200;

/** AI_DAILY_REVIEWS as a non-negative integer; the default when it is not one. */
export function aiDailyLimit(env: { AI_DAILY_REVIEWS?: string }): number {
  const raw = (env.AI_DAILY_REVIEWS ?? "").trim();
  if (!/^[0-9]{1,9}$/.test(raw)) return DEFAULT_AI_DAILY_CALLS;
  return Number(raw);
}

/** The UTC day a call counts against. */
export const utcDay = (at = new Date()) => at.toISOString().slice(0, 10);

export interface AiBudget {
  day: string;
  /** Model calls allowed per project per UTC day (AI_DAILY_REVIEWS). */
  daily_calls: number;
  used_today: number;
  remaining: number;
}

/** One line of AI usage: a review or advice call that reached a model. */
export interface AiUsageInput {
  kind: "review" | "advice";
  provider: string;
  model: string;
  /** approve, escalate or guard for a review; advice or guard for advice. */
  outcome: string;
  /** Model calls made (2 when a fallback provider was tried). */
  calls: number;
  tokens: { prompt: number; completion: number } | null;
  /** Estimated USD from the vendor's list price, or null when the model is unpriced. */
  est_usd: number | null;
}

export interface AiCostLine {
  calls: number;
  prompt_tokens: number;
  completion_tokens: number;
  est_usd: number;
  /** Calls to models whose price is not on the list; their cost is not in est_usd. */
  unpriced_calls: number;
}

export interface AiCost {
  /** Model calls since the project was created. */
  calls: number;
  /** Reviews the AI made, approved, or sent to a person (the model escalated, or a guard did). */
  reviews: number;
  approvals: number;
  escalations: number;
  /** Advice written for people. */
  advice: number;
  /** Prompt and completion tokens together. */
  tokens: number;
  est_usd: number;
  budget: AiBudget;
  /** Every call since the project was created, by outcome. */
  total: AiCostLine & { reviews: number; approves: number; escalations: number; guards: number; advice: number };
  /** Today's calls (UTC). */
  today: AiCostLine & { tokens: number };
  by_model: Record<string, AiCostLine>;
  /** What the dollar figures are: list prices, as of when, and what is not counted. */
  basis: string;
}

export const AI_COST_BASIS =
  "estimate from the vendors' published list prices (src/aiproviders.ts AI_PRICING) per token the model reported; " +
  "Workers AI is billed in neurons and third-party models by their vendor or Unified Billing, so the bill may differ; " +
  "calls to models not on the price list are counted but not priced.";

export function aiCostLine(): AiCostLine {
  return { calls: 0, prompt_tokens: 0, completion_tokens: 0, est_usd: 0, unpriced_calls: 0 };
}

export function addAiCost(line: AiCostLine, r: { calls: number; prompt_tokens: number; completion_tokens: number; est_usd: number; unpriced: number }): void {
  line.calls += r.calls;
  line.prompt_tokens += r.prompt_tokens;
  line.completion_tokens += r.completion_tokens;
  line.est_usd = Math.round((line.est_usd + r.est_usd) * 1e6) / 1e6;
  line.unpriced_calls += r.unpriced;
}
