// Presentation only: the Worker remains authoritative for decisions.
export const active = (i) => ["open", "proposed"].includes(i.status);
export const sameIdentity = (a, b) => a.trim().toLowerCase() === b.trim().toLowerCase();
export const projectName = (s) => /^[a-z0-9][a-z0-9._-]{0,62}$/.test(s || "");
export const backoff = (failures) => Math.min(30000, 2000 * 2 ** Math.min(failures, 4));

// A decision needs the button that was pressed; a form submitted any other way
// records nothing rather than defaulting to approve.
export const verdictOf = (v) => (v === "approve" || v === "reject" ? v : null);

// The stored tab session: a non-empty key, and not past its own expiry (an
// expired Access session is dropped without ever being sent).
export function sessionFrom(raw, now = Date.now()) {
  let s;
  try { s = JSON.parse(raw || "null"); } catch { return null; }
  if (!s || typeof s !== "object" || typeof s.key !== "string" || !s.key) return null;
  if (s.expires_at) {
    const t = Date.parse(s.expires_at);
    if (Number.isNaN(t) || t <= now) return null;
  }
  return s;
}

// Whether the Worker offers Cloudflare Access sign-in, from a GET probe of
// /v1/human/session fetched with redirect: "manual": off answers
// access_not_configured; on answers method_not_allowed, or an opaque redirect
// when the Access application in front of /v1/human sends the probe to the
// login page. Anything else (a network error, a timeout, an unexpected body)
// is "not offered", so a hiccup never sends the person to a login page that
// may not exist.
export function accessOffered({ type, code } = {}) {
  if (type === "opaqueredirect") return true;
  return code === "method_not_allowed";
}

export function snapshot(intents, decisions) {
  const latest = new Map(decisions.map(d => [d.intent_id, d]));
  const waiting = intents.filter(i => active(i) &&
    ["human_approve", "multi_approve", "quarantine"].includes(latest.get(i.id)?.action) &&
    ["pending_human", "quarantined"].includes(latest.get(i.id)?.status))
    .map(i => ({ intent: i, decision: latest.get(i.id) }))
    .sort((a, b) => a.decision.created_at.localeCompare(b.decision.created_at));
  return { intents, decisions, latest, waiting, overlaps: overlaps(intents) };
}

// Mirrors worker/src/paths.ts: glob/glob intersections are conservative.
const glob = p => /[*?]/.test(p);
const prefix = p => p.split(/[*?]/, 1)[0];
function matches(pattern, path) {
  let re = "^";
  for (let i = 0; i < pattern.length; i++) {
    const c = pattern[i];
    if (c === "*") {
      if (pattern[i + 1] === "*") {
        if (pattern[i + 2] === "/") { re += "(?:.*/)?"; i += 2; }
        else { re += ".*"; i++; }
      } else re += "[^/]*";
    } else if (c === "?") re += "[^/]";
    else re += c.replace(/[.+^${}()|[\]\\]/g, "\\$&");
  }
  return new RegExp(re + "$").test(path);
}
export function intersects(a, b) {
  if (!glob(a) && !glob(b)) return a === b;
  if (glob(a) && !glob(b)) return matches(a, b);
  if (!glob(a) && glob(b)) return matches(b, a);
  return prefix(a).startsWith(prefix(b)) || prefix(b).startsWith(prefix(a));
}

// Same order of suggestions as internal/intent/replan.go. Never mutates intents.
export function overlaps(intents) {
  const open = intents.filter(active).slice().sort((a, b) => a.created_at.localeCompare(b.created_at) || a.id.localeCompare(b.id));
  // A stack builds on its ancestors on purpose (declare --on, session fork), at any depth.
  const parent = new Map(open.map(i => [i.id, i.parent_id || null]));
  const inStack = (child, anc) => { for (let p = parent.get(child), n = 0; p && n < 64; p = parent.get(p), n++) if (p === anc) return true; return false; };
  const out = [];
  for (let n = 0; n < open.length; n++) {
    const a = open[n];
    for (const b of open.slice(n + 1)) {
      if (inStack(a.id, b.id) || inStack(b.id, a.id)) continue;
      const pairs = [];
      for (const x of new Set([...(a.declared_paths || []), ...(a.actual_paths || [])]))
        for (const y of new Set([...(b.declared_paths || []), ...(b.actual_paths || [])]))
          if (intersects(x, y)) pairs.push([x, y]);
      if (!pairs.length) continue;
      const paths = [...new Set(pairs.flatMap(([x, y]) => !glob(x) ? [x] : !glob(y) ? [y] : [x, y]))].sort();
      const touched = i => (i.actual_paths || []).some(p => paths.some(q => intersects(p, q)));
      let first = a, then = b, action = "land_first";
      if (a.status === "proposed" || b.status === "proposed") {
        if (a.status !== "proposed") [first, then] = [b, a];
      } else if (touched(a) && touched(b)) action = "stack";
      else if (touched(b)) [first, then] = [b, a];
      else if (!touched(a) && !pairs.some(([x, y]) => !glob(x) && !glob(y))) action = "split";
      const suggestion = action === "stack" ? `Stack ${then.id} on ${first.id}; both edited the contested paths.`
        : action === "split" ? "Split the broad declarations: each agent declares the narrower paths it needs."
        : `Land ${first.id} before ${then.id}; the second intent lands after it, once policy allows.`;
      out.push({ a, b, paths, action, suggestion });
    }
  }
  return out;
}

// ---- the AI reviewer ------------------------------------------------------
// The Workers AI reviewer decides as "workers-ai:<model>" with identity source
// "ai" (worker/src/aireview.ts). Its record on a decision is read defensively:
// every field may be absent on an older Worker, and its words are model output
// shown as text, never as markup.
export const AI_PREFIX = "workers-ai:";
export const aiModel = (by) => {
  if (typeof by !== "string") return "";
  const prefix = [AI_PREFIX, "anthropic:", "openai:"].find((p) => by.startsWith(p));
  return prefix ? by.slice(prefix.length) : "";
};
export const isAI = (a) => !!a && (a.identity_source === "ai" || aiModel(a.by) !== "");
export const aiName = (model) => (model ? `AI reviewer (${model})` : "AI reviewer");
// Who gave a verdict, as the page names them: the AI by its model, a person
// signed in through Cloudflare Access with "(Access)".
export function deciderName(a) {
  if (!a) return "";
  if (isAI(a)) return aiName(aiModel(a.by));
  // The admin key may report a name with its verdict: "admin (john)".
  const by = `${a.by || ""}${a.claimed_by && a.claimed_by !== a.by ? ` (${a.claimed_by})` : ""}`;
  return a.identity_source === "access" ? `${by} (Access)` : by;
}
const strings = (xs) => (Array.isArray(xs) ? xs.filter((x) => typeof x === "string" && x.trim()).map((x) => x.trim()) : []);
const str = (x) => (typeof x === "string" ? x : "");
export function aiRecord(d) {
  const e = d?.detail?.escalation && typeof d.detail.escalation === "object" ? d.detail.escalation : null;
  const adv = d?.detail?.ai_advice && typeof d.detail.ai_advice === "object" ? d.detail.ai_advice : null;
  const approval = (d?.approvals || []).find(isAI) || null;
  const decidedBy = str(d?.decided_by);
  const model = str(e?.ai?.model) || aiModel(e?.by) || aiModel(decidedBy) || aiModel(approval?.by) || str(adv?.model) || str(adv?.ai?.model) || aiModel(adv?.by);
  return {
    decided: !!e || aiModel(decidedBy) !== "" || !!approval, // the AI approved or escalated
    model,
    approval,
    escalation: e ? { reason: str(e.reason), concerns: strings(e.concerns), from: str(e.from_action), model: str(e.ai?.model) || aiModel(e.by) || model } : null,
    advice: adv ? { summary: str(adv.summary), risks: strings(adv.risks), concerns: strings(adv.concerns), questions: strings(adv.questions), model: str(adv.model) || str(adv.ai?.model) || aiModel(adv.by) || model } : null,
  };
}
// What "Ask AI" asks the Worker for: a verdict while a reviewer agent is
// awaited (the AI may approve or escalate), advice once a person must decide
// (the AI never decides that). Nothing for a decided or superseded change.
export function aiAskRoute(d) {
  if (!d) return null;
  if (d.status === "pending_agent") return "ai-review";
  if (d.status === "pending_human" || d.status === "quarantined") return "ai-advice";
  return null;
}
// The AI's work across decisions, for the dashboard.
export function aiCounts(decisions) {
  const c = { approved: 0, escalated: 0, advised: 0 };
  for (const d of decisions || []) {
    const r = aiRecord(d);
    if (r.escalation) c.escalated++;
    else if (r.decided) c.approved++;
    if (r.advice) c.advised++;
  }
  return c;
}

// ---- Project directory -----------------------------------------------------

// The command that checks a listed project out. The CLI mints a short-lived
// read token for it; the page never shows a git remote with a credential.
export const cloneCommand = (name) => `wayglass clone ${name}`;

// Rows of GET /v1/projects whose name contains q (case-insensitive), in the
// order the directory gave them. The Worker applies the same match when the
// page asks it with ?q=; this keeps typing responsive between fetches.
export function filterProjects(rows, q) {
  const needle = (q || "").trim().toLowerCase();
  return needle ? rows.filter((p) => (p.name || "").toLowerCase().includes(needle)) : rows.slice();
}

// What a project row says, from the authenticated directory (counts carry
// pending_human and landed) or the public one (intents, open, agents only).
// A project whose state is not in place (counts null) says why instead.
export function projectSummary(p) {
  if (!p.counts) return { served: false, text: `not served: ${p.residency || "unknown"}`, needsYou: 0 };
  const c = p.counts;
  const parts = [`${c.open ?? 0} open`];
  if (c.pending_human) parts.push(`${c.pending_human} for a person`);
  if (typeof c.landed === "number") parts.push(`${c.landed} landed`);
  if (typeof c.agents === "number") parts.push(`${c.agents} agent${c.agents === 1 ? "" : "s"}`);
  return { served: true, text: parts.join(" · "), needsYou: c.pending_human || 0 };
}
