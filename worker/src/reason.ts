// A decision's reason is kept forever in the audit log and travels with the
// commit, so the Worker refuses reasons that say nothing. Shared by the
// decide routes (index.ts) and the AI reviewer (aireview.ts), whose reason is
// held to the same bar. Mirrored by placeholderReason and fillerReason in
// internal/cli/review.go.

// "<what you checked>" copied from a usage example is not a reason.
export const PLACEHOLDER = /^<[^<>]*>$|^(todo|tbd|n\/a|none|-|\.+)$/i;

// "LGTM", "ok", "+1", "👍": a rubber stamp, not a why. Matched on the reason
// lower-cased with punctuation and emoji dropped, so "LGTM!" and "ship it 🚀"
// count too.
const FILLER =
  /^(?:(?:lgtm|lg|looks good(?: to me)?|ok|okay|k|approved?|approving|ship ?it|yes|yep|yeah|y|fine|good|great|nice|sure|done|ack|nack|no|nope|nah|rejected?|thanks|ty|1)(?: |$))*$/;

/** True when the reason, stripped of punctuation and emoji, is only rubber-stamp words. */
export function isFiller(v: string): boolean {
  const words = v.toLowerCase().replace(/[^a-z0-9]+/g, " ").trim();
  return FILLER.test(words);
}
