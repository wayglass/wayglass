// Shared text-only rendering for the public audit view and console.
export const ACTIONS = [
  ["auto_merge", "auto-merge", "good"],
  ["agent_review", "agent review", "muted"],
  ["human_approve", "human", "attention"],
  ["multi_approve", "multi-approve", "attention"],
  ["quarantine", "quarantine", "attention"],
  ["block", "block", "stopped"],
];
export const ACTION = Object.fromEntries(ACTIONS.map(([k, label, tone]) => [k, { label, tone, person: tone === "attention" }]));

export function actionChip(a) {
  const m = ACTION[a] ?? { label: a, tone: "muted", person: false };
  return h("span", { class: `chip c-${m.tone}`, title: a }, m.person ? "▲ " : "", m.label);
}

export function statusOf(d) {
  const approved = (d.approvals ?? []).filter((x) => x.verdict === "approve").length;
  switch (d.status) {
    case "approved":
      return d.landed_commit ? ["landed", "good"] : ["approved", "good"];
    case "rejected":
      return ["rejected", "stopped"];
    case "pending_agent":
      return ["agent reviewing", "muted"];
    case "pending_human":
      return d.action === "multi_approve" ? [`▲ needs a person ${approved}/${d.approvals_needed}`, "attention"] : ["▲ needs a person", "attention"];
    case "quarantined":
      return ["▲ quarantined", "attention"];
    case "blocked":
      return ["blocked", "stopped"];
    case "superseded":
      return ["superseded (re-stacked)", "muted"];
  }
  return [d.status, "muted"];
}

export const statusText = (d) => {
  const [label, tone] = statusOf(d);
  return h("span", { class: `c-${tone} small` }, label);
};

export const intentTone = (s) => (s === "landed" ? "good" : s === "abandoned" ? "muted" : "text");

/** Area, data class, risk, jurisdiction, always in that order. A dash means "classified, none found". */
export function tagList(t) {
  if (!t) return null;
  const join = (xs) => (Array.isArray(xs) && xs.length ? xs.join(",") : "-");
  return h(
    "div",
    { class: "tags" },
    tag("area", join(t.areas)),
    tag("data", join(t.data_classes)),
    tag("risk", t.risk || "-"),
    tag("jur", (t.jurisdiction || "-").toUpperCase()),
  );
}
const tag = (k, v) => h("span", { class: "tag" }, h("i", {}, k), v);

// ---- DOM helpers ----------------------------------------------------------

export function h(tagName, attrs = {}, ...kids) {
  const el = document.createElement(tagName);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false || kid === "") continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

export const short = (sha) => (sha ? sha.slice(0, 7) : "—");
export const mono = (s) => h("span", { class: "mono" }, s);

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
export function ago(iso) {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return h("span", {}, "—");
  const s = Math.round((t - Date.now()) / 1000);
  const [n, unit] =
    Math.abs(s) < 60 ? [s, "second"] : Math.abs(s) < 3600 ? [Math.round(s / 60), "minute"] : Math.abs(s) < 86400 ? [Math.round(s / 3600), "hour"] : [Math.round(s / 86400), "day"];
  return h("time", { datetime: iso, title: new Date(t).toLocaleString() }, rtf.format(n, unit));
}

