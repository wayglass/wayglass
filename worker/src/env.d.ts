// Secrets are not visible to `wrangler types` (they live in .dev.vars or
// `wrangler secret`), so they are declared here.
interface __WayglassSecrets {
  /** Placeholder admin key until `wayglass login` issues per-agent session tokens. */
  WAYGLASS_ADMIN_KEY?: string;
  /** Account that owns the namespace and queue; used only for the event-subscription REST call. */
  CF_ACCOUNT_ID?: string;
  /** API token limited to Account > Queues > Edit. Creates per-trunk `pushed` subscriptions (D12). */
  CF_EVENTS_API_TOKEN?: string;
}
interface Env extends __WayglassSecrets {}
declare namespace Cloudflare {
  interface Env extends __WayglassSecrets {}
}
