// Which model reviews a change, and how it is called (docs/ARCHITECTURE.md D5).
//
// Cross-family by construction: the reviewer model is picked so that its
// family is never the author's. A Claude-authored change goes to Workers AI
// (Llama) or an OpenAI model; a Codex-authored change goes to Claude; an
// author with no declared family gets Workers AI. The family recorded on
// the approval is the model vendor's (meta, claude, openai), so the policy
// rule `reviewer: { different_family: true }` holds against the AI exactly as
// against any agent: an Anthropic reviewer cannot approve a Claude-authored
// change, however it is configured.
//
// Every call goes through the AI binding, and third-party models through the
// AI Gateway named in AI_GATEWAY_ID. The gateway holds the provider keys
// (BYOK, `default` alias) or bills Unified Billing; the Worker holds none.
// AI_PROVIDERS lists what this Worker may use ("workers-ai" alone is the
// self-host default; "workers-ai,anthropic" on api.wayglass.dev). A provider
// call that fails falls back to the next enabled provider of another family;
// when none is left the change goes to a person.

import { normalizeFamily } from "./family";

export type ProviderId = "workers-ai" | "anthropic" | "openai";

export const PROVIDER_IDS: readonly ProviderId[] = ["workers-ai", "anthropic", "openai"];

export const DEFAULT_MODEL = "@cf/meta/llama-4-scout-17b-16e-instruct";
export const DEFAULT_ANTHROPIC_MODEL = "anthropic/claude-sonnet-5.5";
export const DEFAULT_OPENAI_MODEL = "openai/gpt-5.4-mini";

export interface Provider {
  id: ProviderId;
  /** The model id as the AI binding takes it: `@cf/vendor/model` or `vendor/model`. */
  model: string;
  /** The model's family (its vendor, normalised): meta, claude, openai, gemini, ... */
  family: string;
}

/** The AI binding, narrowed to the one call made here so tests can hand in a fake. */
export interface AiBinding {
  run(model: string, inputs: Record<string, unknown>, options?: { gateway?: { id: string } }): Promise<unknown>;
}

export function aiBinding(env: Env): AiBinding | null {
  const ai = (env as { AI?: unknown }).AI;
  if (ai && typeof ai === "object" && typeof (ai as { run?: unknown }).run === "function") return ai as AiBinding;
  return null;
}

export function gatewayId(env: Env): string | null {
  return (env.AI_GATEWAY_ID ?? "").trim() || null;
}

/** The provider a model id belongs to, or null when it is not one the Worker can call. */
export function providerOf(model: string): ProviderId | null {
  if (model.startsWith("@cf/")) return "workers-ai";
  if (model.startsWith("anthropic/")) return "anthropic";
  if (model.startsWith("openai/")) return "openai";
  return null;
}

/** The model's family from its vendor segment: `@cf/meta/llama-...` is meta, `anthropic/claude-...` is claude. */
export function modelFamily(model: string): string {
  const parts = model.replace(/^@cf\//, "").split("/");
  const vendor = (parts.length > 1 ? parts[0]! : "").toLowerCase();
  const f = normalizeFamily(vendor);
  return f ? f : vendor || "other";
}

/** Who the AI reviewer is on the record: `workers-ai:@cf/meta/...` or `anthropic:claude-sonnet-5.5`. */
export function aiReviewer(p: Provider): string {
  if (p.id === "workers-ai") return `workers-ai:${p.model}`;
  return `${p.id}:${p.model.slice(p.model.indexOf("/") + 1)}`;
}

/**
 * The providers this Worker may call, in AI_PROVIDERS order. A third-party
 * provider needs the gateway (it holds the keys); without AI_GATEWAY_ID it
 * is dropped with a warning rather than tried and failed on every review.
 */
export function providers(env: Env): Provider[] {
  const raw = ((env as { AI_PROVIDERS?: string }).AI_PROVIDERS ?? "").trim() || "workers-ai";
  const gateway = gatewayId(env);
  const out: Provider[] = [];
  for (const name of raw.split(",").map((s) => s.trim().toLowerCase()).filter(Boolean)) {
    if (!(PROVIDER_IDS as readonly string[]).includes(name)) {
      console.warn(`AI_PROVIDERS names ${JSON.stringify(name)}, which is not one of ${PROVIDER_IDS.join(", ")}; ignored`);
      continue;
    }
    const id = name as ProviderId;
    if (out.some((p) => p.id === id)) continue;
    if (id !== "workers-ai" && !gateway) {
      console.warn(`AI_PROVIDERS names ${id} but AI_GATEWAY_ID is empty; third-party models need the gateway, so ${id} is ignored`);
      continue;
    }
    const model = defaultModel(env, id);
    out.push({ id, model, family: modelFamily(model) });
  }
  return out;
}

function defaultModel(env: Env, id: ProviderId): string {
  const e = env as { AI_REVIEW_MODEL?: string; AI_ANTHROPIC_MODEL?: string; AI_OPENAI_MODEL?: string };
  switch (id) {
    case "workers-ai":
      return (e.AI_REVIEW_MODEL ?? "").trim() || DEFAULT_MODEL;
    case "anthropic":
      return (e.AI_ANTHROPIC_MODEL ?? "").trim() || DEFAULT_ANTHROPIC_MODEL;
    case "openai":
      return (e.AI_OPENAI_MODEL ?? "").trim() || DEFAULT_OPENAI_MODEL;
  }
}

// The order to try providers in, by the author's family. The author's own
// family is filtered out before this order applies, so a Claude author can
// never reach the anthropic entry; it is listed so a declared alias of an
// unknown family still gets a deterministic order.
const PREFERENCE: Record<string, ProviderId[]> = {
  claude: ["workers-ai", "openai", "anthropic"],
  openai: ["anthropic", "workers-ai", "openai"],
};
const DEFAULT_ORDER: ProviderId[] = ["workers-ai", "anthropic", "openai"];

export interface ReviewerPick {
  primary: Provider;
  /** The next provider of another family, tried when the primary call fails. */
  fallback: Provider | null;
  /** Why a policy-pinned model was not used, if it was not. */
  note: string | null;
}

/**
 * The reviewer for an author of `authorFamily` (null: none declared), with a
 * fallback of another family. `pinned` is the policy rule's `ai.model`; it
 * comes first when its provider is enabled and its family is not the
 * author's. Refuses (same_family) when every enabled provider shares the
 * author's family.
 */
export function pickReviewer(env: Env, authorFamily: string | null, pinned?: string | null): ReviewerPick | { refused: { code: "same_family"; message: string } } {
  const enabled = providers(env);
  const order = (authorFamily && PREFERENCE[authorFamily]) || DEFAULT_ORDER;
  const rank = (p: Provider) => {
    const i = order.indexOf(p.id);
    return i < 0 ? order.length : i;
  };
  let candidates = enabled.filter((p) => !authorFamily || p.family !== authorFamily).sort((a, b) => rank(a) - rank(b));
  let note: string | null = null;
  if (pinned) {
    const id = providerOf(pinned);
    const family = modelFamily(pinned);
    if (!id || !enabled.some((p) => p.id === id)) {
      note = `policy pins model ${pinned}, but its provider is not enabled on this Worker (AI_PROVIDERS); picked as usual`;
    } else if (authorFamily && family === authorFamily) {
      note = `policy pins model ${pinned}, which is the author's family (${authorFamily}); picked another`;
    } else {
      candidates = [{ id, model: pinned, family }, ...candidates.filter((p) => p.id !== id)];
    }
  }
  if (candidates.length === 0) {
    const have = enabled.map((p) => `${p.id} (${p.family})`).join(", ") || "none";
    return {
      refused: {
        code: "same_family",
        message: `every AI reviewer this Worker can call is from the author's own family (${authorFamily}); enabled: ${have}. Another reviewer must decide`,
      },
    };
  }
  return { primary: candidates[0]!, fallback: candidates[1] ?? null, note };
}

// ---- calling a model ------------------------------------------------------

export interface ModelCall {
  system: string;
  user: string;
  /** A JSON schema for the answer, where the provider can enforce one. */
  schema?: Record<string, unknown>;
  maxTokens: number;
}

export interface ModelReply {
  /** The answer: a string, or already-parsed JSON when the provider returns it that way. */
  text: unknown;
  tokens: { prompt: number; completion: number } | null;
  latency_ms: number;
}

export type ModelOutcome = { ok: true; reply: ModelReply } | { ok: false; error: string; latency_ms: number };

const num = (v: unknown): number | null => (typeof v === "number" && Number.isFinite(v) ? v : null);

function tokens(prompt: unknown, completion: unknown): ModelReply["tokens"] {
  const p = num(prompt);
  const c = num(completion);
  return p !== null && c !== null ? { prompt: p, completion: c } : null;
}

/**
 * One call to the provider's model through the AI binding, in the request
 * shape that provider takes, with the reply normalised. Never throws: a
 * failure is an outcome the caller turns into a fallback or an escalation.
 */
export async function callModel(ai: AiBinding, p: Provider, gateway: string | null, call: ModelCall): Promise<ModelOutcome> {
  const started = Date.now();
  const opts = gateway ? { gateway: { id: gateway } } : undefined;
  try {
    let raw: unknown;
    switch (p.id) {
      case "workers-ai":
        raw = await ai.run(
          p.model,
          {
            messages: [
              { role: "system", content: call.system },
              { role: "user", content: call.user },
            ],
            ...(call.schema ? { response_format: { type: "json_schema", json_schema: call.schema } } : {}),
            max_tokens: call.maxTokens,
            temperature: 0,
          },
          opts,
        );
        break;
      case "anthropic":
        // Anthropic Messages shape. No sampling parameters: current Claude
        // models refuse non-default values.
        raw = await ai.run(p.model, { max_tokens: call.maxTokens, system: call.system, messages: [{ role: "user", content: call.user }] }, opts);
        break;
      case "openai":
        raw = await ai.run(
          p.model,
          {
            messages: [
              { role: "system", content: call.system },
              { role: "user", content: call.user },
            ],
            ...(call.schema ? { response_format: { type: "json_object" } } : {}),
            max_completion_tokens: call.maxTokens,
          },
          opts,
        );
        break;
    }
    const latency_ms = Date.now() - started;
    const reply = normalise(p.id, raw, latency_ms);
    if ("error" in reply) return { ok: false, error: reply.error, latency_ms };
    return { ok: true, reply };
  } catch (err) {
    // Provider errors can contain request data or credentials.
    console.warn(`ai: ${p.id} call failed`);
    return { ok: false, error: "provider call failed", latency_ms: Date.now() - started };
  }
}

/** The reply in one shape, whichever provider answered. */
export function normalise(id: ProviderId, raw: unknown, latency_ms: number): ModelReply | { error: string } {
  const o = typeof raw === "object" && raw !== null ? (raw as Record<string, unknown>) : null;
  switch (id) {
    case "workers-ai": {
      if (!o) return { text: raw, tokens: null, latency_ms };
      const usage = o.usage as Record<string, unknown> | undefined;
      return { text: o.response, tokens: tokens(usage?.prompt_tokens, usage?.completion_tokens), latency_ms };
    }
    case "anthropic": {
      if (!o || !Array.isArray(o.content)) return { error: "the model's reply had no content" };
      if (o.stop_reason === "refusal") return { error: "the model declined the request (refusal)" };
      const text = (o.content as Record<string, unknown>[])
        .filter((b) => b && b.type === "text" && typeof b.text === "string")
        .map((b) => b.text as string)
        .join("\n");
      const usage = o.usage as Record<string, unknown> | undefined;
      return { text, tokens: tokens(usage?.input_tokens, usage?.output_tokens), latency_ms };
    }
    case "openai": {
      const choice = o && Array.isArray(o.choices) ? (o.choices[0] as Record<string, unknown> | undefined) : undefined;
      const message = choice?.message as Record<string, unknown> | undefined;
      if (!message || typeof message.content !== "string") return { error: "the model's reply had no message content" };
      const usage = o!.usage as Record<string, unknown> | undefined;
      return { text: message.content, tokens: tokens(usage?.prompt_tokens, usage?.completion_tokens), latency_ms };
    }
  }
}

// ---- what it costs --------------------------------------------------------

/** USD per million input and output tokens, from the vendors' published list prices. */
export const AI_PRICING: { per_million: Record<string, { input: number; output: number }>; as_of: string; sources: string[] } = {
  per_million: {
    "@cf/meta/llama-4-scout-17b-16e-instruct": { input: 0.27, output: 0.85 },
    "@cf/openai/gpt-oss-120b": { input: 0.35, output: 0.75 },
    "@cf/openai/gpt-oss-20b": { input: 0.2, output: 0.3 },
    "anthropic/claude-sonnet-5.5": { input: 2, output: 10 },
    "anthropic/claude-haiku-5.5": { input: 0.1, output: 0.5 },
    "anthropic/claude-opus-5.5": { input: 4, output: 20 },
  },
  as_of: "2026-10-14",
  sources: ["https://developers.cloudflare.com/workers-ai/platform/pricing/", "https://developers.cloudflare.com/ai/models/"],
};

/** Estimated USD for one call, or null when the model's price is not on the list. */
export function estimateAiUsd(model: string, t: { prompt: number; completion: number } | null): number | null {
  const price = AI_PRICING.per_million[model];
  if (!price || !t) return null;
  return Math.round(((t.prompt * price.input + t.completion * price.output) / 1e6) * 1e6) / 1e6;
}
