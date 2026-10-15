import { fail } from "./envelope";
import { hashKey, KEY_PREFIX, type KeyKind, type KeySource, registryStub } from "./keys";

// Two kinds of bearer key:
//   - the admin key (secret WAYGLASS_ADMIN_KEY): may do everything, including
//     minting agent keys. Its caller names itself with X-Wayglass-Agent.
//   - agent keys (`wgk_...`, minted by the admin at POST /v1/keys): bound to
//     one agent name. That name is the caller's identity on every route; the
//     self-reported header and body fields are ignored. A key minted with
//     kind "human" belongs to a person; every other agent key to an agent.
// Only a person (the admin key or a human key) may decide a change policy
// routed to a person.
// A person who signs in through Cloudflare Access gets a human key with
// source "access" and an expiry (src/human.ts); past it the key is refused.

/** Where the caller's identity comes from: the admin key, an admin-minted key, or a Cloudflare Access sign-in. */
export type IdentitySource = "admin" | KeySource;

export type Identity =
  | { role: "admin"; agent: null; key_id: null; kind: "human"; source: "admin"; expires_at: null }
  | { role: "agent"; agent: string; key_id: string; kind: KeyKind; source: KeySource; expires_at: string | null };

/** Whether the caller is a person: the admin key, or a key minted with kind "human". */
export const isPerson = (id: Identity): boolean => id.kind === "human";

const encoder = new TextEncoder();

async function digest(s: string): Promise<ArrayBuffer> {
  return crypto.subtle.digest("SHA-256", encoder.encode(s));
}

/** Who the bearer key belongs to, or an error response. Fails closed. */
export async function authenticate(req: Request, env: Env): Promise<Identity | Response> {
  const key = env.WAYGLASS_ADMIN_KEY;
  if (!key) return fail("not_configured", "WAYGLASS_ADMIN_KEY is not set on the Worker");
  const header = req.headers.get("authorization") ?? "";
  const m = /^Bearer (.+)$/.exec(header);
  if (!m) return fail("unauthorized", "missing bearer token");
  const presented = m[1]!;
  if (presented.startsWith(KEY_PREFIX)) {
    // A SHA-256 of 32 random bytes is the lookup key, so there is nothing to time.
    const found = await registryStub(env).lookup(await hashKey(presented));
    if (!found) return fail("unauthorized", "invalid or revoked agent key");
    if ("expired" in found) return fail("unauthorized", "this key has expired; sign in again (wayglass login --human)");
    return { role: "agent", agent: found.agent, key_id: found.id, kind: found.kind, source: found.source, expires_at: found.expires_at };
  }
  // Hash both sides so the constant-time compare sees equal lengths.
  const [a, b] = await Promise.all([digest(presented), digest(key)]);
  if (!crypto.subtle.timingSafeEqual(a, b)) return fail("unauthorized", "invalid bearer token");
  return { role: "admin", agent: null, key_id: null, kind: "human", source: "admin", expires_at: null };
}
