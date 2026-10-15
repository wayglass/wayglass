// Subscribes each new project trunk to cf.artifacts.repo.pushed.
//
// `pushed` only exists on the per-repo `artifacts.repo` event source, so one
// account-level subscription cannot cover every trunk; each trunk needs its own.
// There is no binding for event subscriptions, so this is the one place the
// Worker calls the Cloudflare REST API. It uses CF_EVENTS_API_TOKEN, a token
// limited to Account > Queues > Edit (ARCHITECTURE D12). Request shape follows
// `wrangler queues subscription create` (wrangler 4.149.0).

const API = "https://api.cloudflare.com/client/v4";

export type SubscribeResult =
  | { subscribed: true; id: string }
  | { subscribed: false; reason: "not_configured" | "api_error"; detail: string };

let fetchOverride: typeof fetch | null = null;

/** Test hook. Only test files call this. */
export function setSubscriptionFetchForTests(f: typeof fetch | null): void {
  fetchOverride = f;
}

/** Names of the settings that are missing, empty when all are set. */
export function missingSubscriptionConfig(env: Env): string[] {
  const need: [string, unknown][] = [
    ["CF_ACCOUNT_ID", env.CF_ACCOUNT_ID],
    ["CF_EVENTS_API_TOKEN", env.CF_EVENTS_API_TOKEN],
    ["ARTIFACTS_EVENTS_QUEUE_ID", env.ARTIFACTS_EVENTS_QUEUE_ID],
  ];
  return need.filter(([, v]) => typeof v !== "string" || v === "").map(([k]) => k);
}

/**
 * Creates a `pushed` subscription for one repo, delivered to the artifacts-events
 * queue. Never throws: project creation must not fail because events could not
 * be wired up; the caller records the result in the audit log instead.
 */
export async function subscribeRepoPushes(env: Env, namespace: string, repo: string): Promise<SubscribeResult> {
  const missing = missingSubscriptionConfig(env);
  if (missing.length > 0) return { subscribed: false, reason: "not_configured", detail: `missing ${missing.join(", ")}` };
  const body = {
    name: `wayglass ${repo} pushed`,
    enabled: true,
    source: { type: "artifacts.repo", namespace, repo_name: repo },
    destination: { type: "queues.queue", queue_id: env.ARTIFACTS_EVENTS_QUEUE_ID },
    events: ["pushed"],
  };
  try {
    const res = await (fetchOverride ?? fetch)(`${API}/accounts/${env.CF_ACCOUNT_ID}/event_subscriptions/subscriptions`, {
      method: "POST",
      headers: { authorization: `Bearer ${env.CF_EVENTS_API_TOKEN}`, "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    const json = (await res.json().catch(() => null)) as {
      success?: boolean;
      result?: { id?: unknown };
      errors?: { code?: unknown; message?: unknown }[];
    } | null;
    const id = json?.result?.id;
    if (res.ok && json?.success !== false && typeof id === "string" && id !== "") return { subscribed: true, id };
    // Only Cloudflare's error codes and messages go to the audit log, never the request.
    const errs = (json?.errors ?? []).map((e) => `${String(e.code ?? "")} ${String(e.message ?? "")}`.trim()).filter(Boolean);
    return { subscribed: false, reason: "api_error", detail: `HTTP ${res.status}${errs.length ? `: ${errs.join("; ")}` : ""}`.slice(0, 500) };
  } catch (err) {
    return { subscribed: false, reason: "api_error", detail: (err instanceof Error ? err.message : String(err)).slice(0, 500) };
  }
}

export type UnsubscribeResult = { removed: true; already_gone: boolean } | { removed: false; detail: string };

/**
 * Removes one push subscription (DELETE, as `wrangler queues subscription
 * delete` does). A 404 means it is already gone. Never throws: a project
 * delete must not stop because an event subscription could not be removed.
 */
export async function unsubscribeRepoPushes(env: Env, subscriptionId: string): Promise<UnsubscribeResult> {
  const missing = missingSubscriptionConfig(env);
  if (missing.length > 0) return { removed: false, detail: `missing ${missing.join(", ")}` };
  try {
    const res = await (fetchOverride ?? fetch)(
      `${API}/accounts/${env.CF_ACCOUNT_ID}/event_subscriptions/subscriptions/${encodeURIComponent(subscriptionId)}`,
      { method: "DELETE", headers: { authorization: `Bearer ${env.CF_EVENTS_API_TOKEN}` } },
    );
    if (res.status === 404) return { removed: true, already_gone: true };
    const json = (await res.json().catch(() => null)) as { success?: boolean; errors?: { code?: unknown; message?: unknown }[] } | null;
    if (res.ok && json?.success !== false) return { removed: true, already_gone: false };
    const errs = (json?.errors ?? []).map((e) => `${String(e.code ?? "")} ${String(e.message ?? "")}`.trim()).filter(Boolean);
    return { removed: false, detail: `HTTP ${res.status}${errs.length ? `: ${errs.join("; ")}` : ""}`.slice(0, 500) };
  } catch (err) {
    return { removed: false, detail: (err instanceof Error ? err.message : String(err)).slice(0, 500) };
  }
}
