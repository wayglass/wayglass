// JSON envelope shared with the Go CLI (AGENTS.md section 5):
//   {"v":1,"ok":true,"data":...}
//   {"v":1,"ok":false,"error":{"code":"...","message":"..."}}

export type ErrorCode =
  | "bad_request"
  | "unauthorized"
  | "forbidden"
  | "not_author"
  | "needs_human"
  | "needs_agent"
  | "not_found"
  | "method_not_allowed"
  | "conflict"
  | "superseded"
  | "decision_replaced"
  | "policy_changed"
  | "family_required"
  | "same_family"
  | "upstream_error"
  | "not_configured"
  | "ai_unavailable"
  | "ai_budget"
  | "access_not_configured"
  | "residency_pending"
  | "project_moved"
  | "internal";

const STATUS: Record<ErrorCode, number> = {
  bad_request: 400,
  unauthorized: 401,
  forbidden: 403,
  not_author: 403,
  needs_human: 403,
  // Policy said `reviewer: agent`: a reviewer agent decides, not the AI reviewer.
  needs_agent: 403,
  not_found: 404,
  method_not_allowed: 405,
  conflict: 409,
  superseded: 409,
  // The intent was proposed again after this decision; only the latest decision of an intent is decided.
  decision_replaced: 409,
  policy_changed: 409,
  family_required: 403,
  same_family: 403,
  upstream_error: 502,
  not_configured: 503,
  // The Workers AI reviewer is off or has no binding; agent_review keeps waiting for a reviewer agent.
  ai_unavailable: 503,
  // The project's daily AI budget (AI_DAILY_REVIEWS) is used up; tomorrow, or a person.
  ai_budget: 429,
  // Cloudflare Access sign-in is off on this Worker: the routes do not exist.
  access_not_configured: 404,
  // The project exists but its state is not yet (residency_pending) or no
  // longer (project_moved) where its jurisdiction says it must live; an
  // admin runs migrate-residency. Nothing is served from the wrong place.
  residency_pending: 503,
  project_moved: 503,
  internal: 500,
};

export interface Failure {
  ok: false;
  code: ErrorCode;
  message: string;
}
export interface Success<T> {
  ok: true;
  data: T;
}
/** Result type used across the Worker/Durable Object RPC boundary. */
export type Result<T> = Success<T> | Failure;

export const okResult = <T>(data: T): Success<T> => ({ ok: true, data });
export const failResult = (code: ErrorCode, message: string): Failure => ({ ok: false, code, message });

const HEADERS = { "content-type": "application/json; charset=utf-8", "cache-control": "no-store" };

export function ok<T>(data: T, status = 200): Response {
  return new Response(JSON.stringify({ v: 1, ok: true, data }), { status, headers: HEADERS });
}

export function fail(code: ErrorCode, message: string): Response {
  return new Response(JSON.stringify({ v: 1, ok: false, error: { code, message } }), {
    status: STATUS[code],
    headers: HEADERS,
  });
}

export function fromResult<T>(r: Result<T>, status = 200): Response {
  return r.ok ? ok(r.data, status) : fail(r.code, r.message);
}
