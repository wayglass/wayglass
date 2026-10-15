// Wayglass desktop window. Plain ES module, no build step.
//
// Everything shown here was written by agents, so it is untrusted: the DOM is
// built with textContent only (h() below), never innerHTML. A script injected
// through an intent's goal could otherwise call the bound Approve method.

const go = () => window.go?.app?.App;
const rt = () => window.runtime;

const ACTIONS = [
  { id: "auto_merge", label: "Auto-merge", glyph: "✓" },
  { id: "agent_review", label: "Agent review", glyph: "◇" },
  { id: "human_approve", label: "Human approval", glyph: "▲" },
  { id: "multi_approve", label: "Multi-approve", glyph: "▲▲" },
  { id: "quarantine", label: "Quarantine", glyph: "◼" },
  { id: "block", label: "Block", glyph: "✕" },
];
const ACTION = Object.fromEntries(ACTIONS.map((a) => [a.id, a]));

const state = {
  session: null,
  snap: null,
  filter: "active",
  open: new Set(), // expanded swarm rows
  drafts: new Map(), // decision id -> typed reason (survives re-renders)
  done: new Map(), // decision id -> {verdict, reason} just decided here
  seenEvents: 0,
  sigs: {},
};

// ---------- DOM helpers ----------

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "style") el.style.cssText = v;
    else el.setAttribute(k, v === true ? "" : String(v));
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid === undefined || kid === null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

const $ = (id) => document.getElementById(id);

function changed(key, value) {
  const sig = JSON.stringify(value);
  if (state.sigs[key] === sig) return false;
  state.sigs[key] = sig;
  return true;
}

// ---------- Formatting ----------

function ago(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${String(Math.round((s % 3600) / 60)).padStart(2, "0")}m`;
  return `${Math.floor(s / 86400)}d`;
}

function clock(iso) {
  if (!iso) return "--:--";
  const d = new Date(iso);
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}

const short = (id) => (id || "").slice(0, 11);

function actionPill(action) {
  const a = ACTION[action] || { label: action, glyph: "·" };
  return h("span", { class: `pill a-${action}`, title: action }, h("span", { class: "g" }, a.glyph), a.label);
}

function statusPill(status, d) {
  const map = {
    approved: ["approved", "good"],
    rejected: ["rejected", "stopped"],
    blocked: ["blocked", "stopped"],
    pending_agent: ["agent reviewing", ""],
    pending_human: [d && d.action === "multi_approve" ? `needs a person ${approvedCount(d)}/${d.approvals_needed}` : "needs a person", "attn"],
    quarantined: ["quarantined", "attn"],
    superseded: ["superseded (re-stacked)", ""],
  };
  const [text, cls] = map[status] || [status.replaceAll("_", " "), ""];
  return h("span", { class: `pill status ${cls}` }, text);
}

const approvedCount = (d) => (d.approvals || []).filter((a) => a.verdict === "approve").length;

// ---------- The AI reviewer ----------
// Its decisions must read as the AI's: a pill naming the model where it
// approved or escalated, the escalation's reason and concerns, and its advice
// labelled as advice at both ends. Model-written text goes through h() like an
// agent's: text nodes only.

const aiName = (model) => (model ? `AI reviewer (${model})` : "AI reviewer");

function aiPill(d) {
  if (!d.ai_model || (!d.escalation && !(d.approvals || []).some((a) => a.ai) && !d.decided_by)) return null;
  const what = d.escalation ? "escalated by" : "approved by";
  return h("span", { class: "pill ai", title: `${what} ${aiName(d.ai_model)}` }, h("span", { class: "g" }, "◆"), `${what} ${aiName(d.ai_model)}`);
}

function escalationBlock(d) {
  const e = d.escalation;
  if (!e) return null;
  return h("div", { class: "ai-block escalation" },
    h("div", { class: "t" }, h("span", { class: "g" }, "◆"), ` Escalated by ${aiName(e.model)}: `, h("span", { class: "r" }, e.reason)),
    (e.concerns || []).map((c) => h("div", { class: "li" }, `• ${c}`)),
    h("div", { class: "note" }, e.from_action
      ? `Was ${e.from_action.replaceAll("_", " ")}; the AI could not approve it alone, so a person decides.`
      : "The AI could not approve it alone, so a person decides."));
}

function adviceBlock(d) {
  const a = d.ai_advice;
  if (!a) return null;
  const group = (label, items) => (items && items.length ? [h("div", { class: "lbl" }, label), items.map((x) => h("div", { class: "li" }, `• ${x}`))] : null);
  return h("div", { class: "ai-block advice" },
    h("div", { class: "t" }, h("span", { class: "g" }, "◆"), " AI advice ", h("span", { class: "adv" }, "advisory")),
    h("p", {}, a.summary),
    group("Risks", a.risks), group("Concerns", a.concerns), group("Questions", a.questions),
    h("div", { class: "note" }, `Advice only, from ${aiName(a.model)}: the decision and its reason are yours.`));
}

// "Ask AI": advice on a change this person decides (the AI never decides it),
// a verdict on one still waiting for a reviewer agent. The Worker's refusals
// (reviewer off, only a person may decide) are shown in its words.
function askAIButton(it, d, onDone) {
  const advice = d.awaits_human;
  const btn = h("button", { class: "ghost ask-ai", title: advice ? "Ask the AI reviewer for advice; it decides nothing" : "Ask the AI reviewer to decide" },
    advice ? "Ask AI for advice" : "Ask the AI reviewer to decide");
  const msg = h("span", { class: "msg ai-msg" });
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    msg.className = "msg ai-msg";
    msg.textContent = "Asking the AI reviewer…";
    let out;
    try {
      out = await go().AskAI(it.id);
    } catch (e) {
      out = { ok: false, code: "error", message: String(e) };
    }
    btn.disabled = false;
    if (!out.ok) {
      msg.className = "msg ai-msg err";
      msg.textContent = out.code === "ai_unavailable"
        ? `The AI reviewer is not available: ${out.message}`
        : out.code === "needs_human" ? `Only a person can decide this: ${out.message}` : out.message || "The Worker refused this.";
      return;
    }
    msg.className = "msg ai-msg ok";
    msg.textContent = out.kind === "advice"
      ? `${aiName(out.model)} advised; advice only, the decision is yours.`
      : `${aiName(out.model)} ${out.verdict === "approve" ? "approved" : "escalated to a person"}: ${out.reason}`;
    if (onDone) onDone(out);
  });
  return h("div", { class: "ask", "data-id": d.id }, btn, msg);
}

const verdictLine = (a) => h("div", { class: `a ${a.ai ? "ai" : ""}` }, h("b", {}, a.ai ? `◆ ${a.who || aiName("")}` : a.by), ` ${a.verdict === "approve" ? "approved" : "rejected"}: ${a.reason}`);

const HOT = new Set(["pii", "secrets", "auth", "infra", "payment", "credentials"]);

function tagRow(t) {
  const area = t.areas.length ? t.areas.join(", ") : "–";
  const data = t.data_classes.length ? t.data_classes.join(", ") : "none";
  return h("div", { class: "tags" },
    h("span", { class: "tag" }, h("span", { class: "k" }, "Area"), h("span", { class: "v" }, area)),
    h("span", { class: `tag ${t.data_classes.some((c) => HOT.has(c)) ? "hot" : ""}` }, h("span", { class: "k" }, "Data"), h("span", { class: "v" }, data)),
    h("span", { class: `tag risk-${t.risk || "none"}` }, h("span", { class: "k" }, "Risk"), h("span", { class: "v" }, t.risk || "–")),
    h("span", { class: "tag" }, h("span", { class: "k" }, "Region"), h("span", { class: "v" }, (t.jurisdiction || "–").toUpperCase())),
  );
}

function compactTags(t) {
  if (!t) return h("span", { class: "route-cell" }, h("span", { class: "none" }, "not classified yet"));
  const chips = [];
  for (const a of t.areas) chips.push(h("span", { class: "chip" }, a));
  for (const c of t.data_classes) chips.push(h("span", { class: `chip ${HOT.has(c) ? "hot" : ""}` }, c));
  if (t.risk) chips.push(h("span", { class: `chip risk-${t.risk}` }, t.risk));
  if (t.jurisdiction) chips.push(h("span", { class: "chip" }, t.jurisdiction.toUpperCase()));
  return h("div", { class: "tags compact" }, chips);
}

function diffStat(d, limit = 6) {
  const files = d.files || [];
  if (!files.length) return h("span", { class: "diff-total" }, "no file list recorded");
  const rows = files.slice(0, limit).map((f) => {
    const total = f.added + f.deleted;
    const cells = 5;
    const plus = total ? Math.round((f.added / Math.max(total, 1)) * Math.min(cells, Math.ceil(total / 4) || 1)) : 0;
    const minus = total ? Math.min(cells - plus, Math.round((f.deleted / Math.max(total, 1)) * Math.min(cells, Math.ceil(total / 4) || 1))) : 0;
    const mini = [];
    for (let i = 0; i < cells; i++) mini.push(h("i", { class: i < plus ? "p" : i < plus + minus ? "m" : "" }));
    return h("div", { class: "f" },
      h("span", { class: "path", title: f.path }, f.path),
      h("span", { class: "add mono" }, total ? `+${f.added}` : ""),
      h("span", { class: "del mono" }, total ? `−${f.deleted}` : ""),
      d.lines_unknown ? "" : h("span", { class: "mini" }, mini));
  });
  if (files.length > limit) rows.push(h("div", { class: "more" }, `and ${files.length - limit} more file(s)`));
  return h("div", { class: "diff" }, rows);
}

// ---------- Counters ----------

function renderCounters(s) {
  const c = s.counters;
  if (!changed("counters", [c, state.blind])) return;
  const total = Math.max(1, ACTIONS.reduce((n, a) => n + (c.by_action[a.id] || 0), 0));
  const counter = (k, v, sub, cls = "") =>
    h("div", { class: `counter ${cls}` }, h("div", { class: "k" }, k), h("div", { class: "v" }, String(v), sub ? h("small", {}, sub) : null));
  $("counters").replaceChildren(
    counter("Agents active", c.active_agents, `of ${c.agents}`),
    counter("Intents open", c.open_intents, `of ${c.intents}`),
    counter("Needs you", state.blind ? "–" : c.needs_you, state.blind ? "unavailable" : null, c.needs_you ? "attn" : ""),
    counter("Landed", c.landed, c.overlaps ? `${c.overlaps} overlap${c.overlaps === 1 ? "" : "s"}` : null),
    h("div", { class: "counter by-action" },
      h("div", { class: "k" }, state.blind ? "Decisions by action · unavailable on this Worker" : `Decisions by action · ${c.decisions}`),
      h("div", { class: "bar", role: "img", "aria-label": "Decisions by action" },
        ACTIONS.filter((a) => c.by_action[a.id]).map((a) =>
          h("span", { class: `a-${a.id}`, style: `flex-grow:${(c.by_action[a.id] / total) * 100}`, title: `${a.label}: ${c.by_action[a.id]}` }))),
      h("div", { class: "legend" },
        ACTIONS.map((a) => h("span", { class: `item ${c.by_action[a.id] ? "" : "zero"}` },
          h("span", { class: `sw a-${a.id}` }), a.label, h("b", {}, String(c.by_action[a.id] || 0)))))),
  );
}

// ---------- Needs you ----------

function renderNeeds(s) {
  const items = s.needs_you;
  const mine = items.filter((n) => !n.own).length;
  $("needs-count").textContent = items.length ? `${mine} for you${items.length > mine ? ` · ${items.length - mine} yours, for someone else` : ""}` : "";
  const list = $("needs");
  const keep = new Map([...list.querySelectorAll(".card[data-id]")].map((el) => [el.dataset.id, el]));
  const doneCards = [...state.done.keys()].filter((id) => !items.some((n) => n.decision.id === id));

  const blind = (s.warnings || []).length > 0 && !s.decisions.length;
  if (!items.length && !doneCards.length && blind) {
    if (changed("needs", "blind")) {
      list.replaceChildren(h("div", { class: "calm unknown" },
        h("span", { class: "check" }, "?"),
        h("div", {}, h("b", {}, "Decisions are unavailable."), "This Worker cannot list decisions yet, so changes routed to a person cannot be shown here. Use 'wayglass review' meanwhile.")));
    }
    return;
  }
  if (!items.length && !doneCards.length) {
    if (changed("needs", "calm")) {
      list.replaceChildren(h("div", { class: "calm" },
        h("span", { class: "check" }, "✓"),
        h("div", {}, h("b", {}, "Nothing needs you."), "Agents are working inside policy. Changes routed to a person will appear here.")));
    }
    return;
  }
  state.sigs.needs = null;
  list.querySelector(".calm")?.remove();

  const want = [];
  for (const n of items) {
    const sig = JSON.stringify([n.decision.status, n.decision.approvals, n.own, n.decision.escalation, n.decision.ai_advice]);
    let el = keep.get(n.decision.id);
    if (!el || el.dataset.sig !== sig) {
      const fresh = needCard(n);
      fresh.dataset.sig = sig;
      if (el) {
        el.replaceWith(fresh);
        fresh.style.animation = "none";
      }
      el = fresh;
    }
    want.push(el);
  }
  for (const id of doneCards) {
    const el = keep.get(id);
    if (el) want.push(el);
  }
  for (const [id, el] of keep) if (!want.includes(el)) el.remove();
  want.forEach((el, i) => {
    if (list.children[i] !== el) list.insertBefore(el, list.children[i] || null);
  });
}

function needCard(n) {
  const d = n.decision;
  const it = n.intent;
  const card = h("article", { class: `card a-${d.action} ${n.own ? "own" : ""}`, "data-id": d.id });

  const facts = h("div", { class: "facts" },
    h("div", { class: "k" }, "Tags"), tagRow(d.tags),
    h("div", { class: "k" }, "Rule"),
    h("div", { class: "rule" }, h("code", { class: "mono" }, d.matched_rule), " ", h("span", {}, d.reason)),
    h("div", { class: "k" }, "Diff"),
    h("div", {},
      h("div", { class: "diff-total", style: "margin-bottom:4px" },
        `${d.files.length} file${d.files.length === 1 ? "" : "s"} · `,
        ...(d.lines_unknown
          ? ["line counts not recorded"]
          : [h("span", { class: "add" }, `+${d.added}`), " ", h("span", { class: "del" }, `−${d.deleted}`)]),
        d.commit ? ` · ${d.commit.slice(0, 7)}` : ""),
      diffStat(d)),
  );
  if (d.approvals.length || d.action === "multi_approve") {
    facts.append(
      h("div", { class: "k" }, "Approvals"),
      h("div", { class: "approvals" },
        d.action === "multi_approve" ? h("div", { class: "a" }, `${approvedCount(d)} of ${d.approvals_needed} approvals`) : null,
        d.approvals.map(verdictLine)),
    );
  }
  if (d.escalation) facts.append(h("div", { class: "k" }, "AI reviewer"), escalationBlock(d));
  if (d.ai_advice) facts.append(h("div", { class: "k" }, "AI advice"), adviceBlock(d));

  card.append(
    h("div", { class: "card-head" },
      actionPill(d.action),
      aiPill(d),
      h("span", { class: "mono" }, it.id),
      h("span", { class: "sep" }, "·"),
      h("span", { class: "agent" }, it.agent),
      h("span", { class: "wait", "data-since": d.created_at }, `waiting ${ago(d.created_at)}`)),
    h("h3", { class: "goal" }, it.goal),
    h("p", { class: "why" }, it.reason),
    facts,
  );

  if (n.own) {
    card.append(h("div", { class: "own-note" },
      `You (${state.session.identity}) wrote this change, so you cannot decide it. Policy routed it to someone else.`));
    return card;
  }

  const ta = h("textarea", {
    placeholder: "Why do you approve or reject this? The reason is kept in the audit log for good.",
    "aria-label": `Reason for ${it.id}`,
    maxlength: "2000",
  });
  ta.value = state.drafts.get(d.id) || "";
  const msg = h("span", { class: "msg" }, `Recorded as ${state.session.identity}.`);
  const approve = h("button", { class: "approve" }, "Approve");
  const reject = h("button", { class: "reject" }, "Reject");
  const sync = () => {
    const empty = !ta.value.trim();
    approve.disabled = reject.disabled = empty;
  };
  ta.addEventListener("input", () => {
    state.drafts.set(d.id, ta.value);
    msg.className = "msg";
    msg.textContent = `Recorded as ${state.session.identity}.`;
    sync();
  });
  const act = async (verdict) => {
    approve.disabled = reject.disabled = ta.disabled = true;
    msg.className = "msg";
    msg.textContent = verdict === "approve" ? "Approving…" : "Rejecting…";
    let out;
    try {
      // The decision this card shows: the verdict is bound to it, so a
      // proposal made while reading is refused, not approved unseen.
      out = await (verdict === "approve" ? go().Approve(it.id, d.id, ta.value) : go().Reject(it.id, d.id, ta.value));
    } catch (e) {
      out = { ok: false, code: "error", message: String(e) };
    }
    if (out.ok) {
      state.drafts.delete(d.id);
      state.done.set(d.id, { verdict });
      card.classList.add("done", verdict === "approve" ? "approved" : "rejected");
      msg.className = "msg ok";
      msg.textContent = verdict === "approve"
        ? (out.decision && out.decision.status === "pending_human" ? "Approval recorded. More approvals needed." : "Approved. The author can land it now.")
        : "Rejected. The reason is in the audit log.";
      approve.remove();
      reject.remove();
      setTimeout(() => { state.done.delete(d.id); card.remove(); if (state.snap) renderNeeds(state.snap); }, 4000);
      return;
    }
    ta.disabled = false;
    sync();
    msg.className = "msg err";
    msg.textContent = out.message || "The Worker refused this.";
    ta.focus();
  };
  approve.addEventListener("click", () => act("approve"));
  reject.addEventListener("click", () => act("reject"));
  sync();

  card.append(h("div", { class: "decide" }, ta, h("div", { class: "row" }, msg, reject, approve)));
  // Advice arrives through the next snapshot (AskAI refreshes it); the card's
  // signature then changes and it is rebuilt with the advice block.
  card.append(askAIButton(it, d));
  return card;
}

// ---------- Overlaps ----------

const OVERLAPS_SHOWN = 3;

function renderOverlaps(s) {
  if (!changed("overlaps", [s.overlaps, state.allOverlaps])) return;
  $("overlaps-section").hidden = !s.overlaps.length;
  $("overlaps-count").textContent = s.overlaps.length || "";
  const shown = state.allOverlaps ? s.overlaps : s.overlaps.slice(0, OVERLAPS_SHOWN);
  const more = s.overlaps.length - shown.length;
  const toggle = s.overlaps.length > OVERLAPS_SHOWN
    ? h("button", { class: "ghost more-btn", onclick: () => { state.allOverlaps = !state.allOverlaps; renderOverlaps(state.snap); } },
      more ? `Show ${more} more` : "Show fewer")
    : null;
  $("overlaps").replaceChildren(...shown.map((o) =>
    h("div", { class: "overlap" },
      h("span", { class: "icon" }, "⧉"),
      h("div", { class: "pair" }, o.older_goal, h("span", { class: "mono" }, ` ${short(o.older)} · ${o.older_agent}`), "  and  ",
        o.newer_goal, h("span", { class: "mono" }, ` ${short(o.newer)} · ${o.newer_agent}`)),
      h("div", { class: "paths mono" }, o.paths.join("  ")),
      h("div", { class: "sugg" }, `Suggested: ${o.suggestion}`))), toggle || "");
}

// ---------- Swarm ----------

function intentDot(it) {
  if (it.latest && it.latest.awaits_human && it.active) return "attn";
  return it.stale ? "stale" : it.status;
}

// Status in words: "stale" is an active intent with no push for a while.
const intentStatusText = (it) => (it.stale ? `${it.status} · stale` : it.status);

function renderSwarm(s) {
  const rows = s.intents.filter((it) => state.filter === "all" || it.active);
  $("swarm-count").textContent = `${s.counters.open_intents} active · ${s.counters.intents} total`;
  if (!changed("swarm", [rows, [...state.open], state.session.identity, state.blind])) return;
  const box = $("swarm");
  if (!rows.length) {
    box.replaceChildren(h("div", { class: "empty" }, state.filter === "active" ? "No active intents. Agents declare one with 'wayglass intent declare'." : "No intents yet."));
    return;
  }
  box.replaceChildren(...rows.map(swarmRow));
}

function swarmRow(it) {
  const d = it.latest;
  const open = state.open.has(it.id);
  const row = h("div", { class: `row ${open ? "open" : ""}`, id: `row-${it.id}` });
  const main = h("div", { class: "row-main", role: "button", tabindex: "0", "aria-expanded": String(open) },
    h("span", { class: `st ${intentDot(it)}`, title: intentStatusText(it) }),
    h("div", { class: "intent-cell" },
      h("div", { class: "goal-line", title: it.goal }, it.goal),
      h("div", { class: "sub" },
        h("span", { class: "mono" }, it.id),
        h("span", { class: it.stale || it.status === "abandoned" ? "muted" : "" }, intentStatusText(it)),
        it.overlaps_with.length ? h("span", { class: "ov", title: it.overlaps_with.join(", ") },
          it.overlaps_with.length === 1 ? `⧉ overlaps ${short(it.overlaps_with[0])}` : `⧉ overlaps ${it.overlaps_with.length} intents`) : null)),
    h("div", { class: `agent-cell ${it.mine ? "me" : ""}` }, it.agent),
    d ? compactTags(d.tags) : h("span", { class: "route-cell" }, h("span", { class: "none" }, state.blind ? "unavailable" : "not classified yet")),
    h("div", { class: "route-cell" }, d ? actionPill(d.action) : h("span", { class: "none" },
      it.status !== "proposed" ? "not proposed" : state.blind ? "unavailable" : "awaiting decision")),
    h("div", { class: "time-cell", "data-ago": it.updated_at || it.created_at }, ago(it.updated_at || it.created_at)),
  );
  const toggle = () => {
    if (state.open.has(it.id)) state.open.delete(it.id); else state.open.add(it.id);
    state.sigs.swarm = null;
    renderSwarm(state.snap);
  };
  main.addEventListener("click", toggle);
  main.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); } });
  row.append(main);
  if (open) row.append(intentDetail(it));
  return row;
}

function intentDetail(it) {
  const d = it.latest;
  const declared = it.declared_paths;
  const extra = it.actual_paths.filter((p) => !declared.some((g) => globMatch(g, p)));
  return h("div", { class: "detail" },
    h("div", { class: "full" }, h("h4", {}, "Why"), h("p", {}, it.reason)),
    it.status === "abandoned" && it.abandoned_reason
      ? h("div", { class: "full" }, h("h4", {}, "Abandoned"), h("p", { class: "muted" }, `${it.abandoned_by}: ${it.abandoned_reason}`)) : null,
    it.stale
      ? h("div", { class: "full" }, h("h4", {}, "Stale"), h("p", { class: "muted" }, `No push for a while. Its author can push again, or run 'wayglass intent abandon ${it.id} --reason "…"'.`)) : null,
    h("div", {}, h("h4", {}, "Declared paths"),
      h("div", { class: "paths" }, declared.length ? declared.map((p) => h("span", { class: "path" }, p)) : h("span", { class: "muted" }, "none"))),
    h("div", {}, h("h4", {}, "Changed paths"),
      h("div", { class: "paths" }, it.actual_paths.length
        ? it.actual_paths.map((p) => h("span", { class: `path ${extra.includes(p) ? "extra" : ""}`, title: extra.includes(p) ? "not declared" : null }, extra.includes(p) ? `${p}  (undeclared)` : p))
        : h("span", { class: "muted" }, "nothing pushed yet"))),
    d ? h("div", { class: "full" }, h("h4", {}, "Decision"),
      h("div", { class: "tags", style: "align-items:center" }, actionPill(d.action), statusPill(d.status, d), aiPill(d),
        h("span", { class: "muted" }, ` ${d.matched_rule}: ${d.reason}`))) : null,
    d && d.escalation ? h("div", { class: "full" }, escalationBlock(d)) : null,
    d && d.approvals.length ? h("div", { class: "full" }, h("h4", {}, "Verdicts"),
      d.approvals.map((a) => h("p", { class: a.ai ? "ai" : "" }, h("b", {}, a.ai ? `◆ ${a.who}` : a.by), ` ${a.verdict === "approve" ? "approved" : "rejected"}: ${a.reason}`))) : null,
    d && d.ai_advice ? h("div", { class: "full" }, adviceBlock(d)) : null,
    d && d.status === "pending_agent" && it.active ? h("div", { class: "full" }, askAIButton(it, d)) : null,
    d ? h("div", { class: "full" }, h("h4", {}, "Diff"), diffStat(d, 12)) : null,
  );
}

// A small glob for highlighting undeclared paths; the Worker's check is
// authoritative. Globs are agent-written: one with many stars could make the
// regex backtrack for a long time, so long globs are only compared as
// prefixes and the path is then shown as declared rather than freezing the window.
function globMatch(glob, path) {
  const stars = (glob.match(/\*/g) || []).length;
  if (glob.length > 256 || stars > 16) return path.startsWith(glob.replace(/\/?\*.*$/, "") + "/");
  const re = "^" + glob.split("**").map((part) =>
    part.split("*").map((x) => x.replace(/[.+?^${}()|[\]\\]/g, "\\$&")).join("[^/]*")).join(".*") + "$";
  return new RegExp(re).test(path) || path.startsWith(glob.replace(/\/?\*\*$/, "") + "/");
}

// ---------- Audit ----------

function eventClass(kind) {
  if (kind === "decision.approve") return "k-approve";
  if (kind === "decision.reject") return "k-reject";
  if (kind.startsWith("decision.")) return "k-decision";
  if (kind === "intent.landed") return "k-landed";
  if (kind === "intent.overlap") return "k-overlap";
  if (kind === "land.mismatch" || kind === "land.cancelled") return "k-bad";
  if (kind.startsWith("intent.")) return "k-intent";
  return "";
}

function renderAudit(s) {
  const evs = s.events.slice(0, 200);
  if (!changed("audit", evs.map((e) => e.seq))) return;
  const newest = state.seenEvents;
  $("audit").replaceChildren(...(evs.length ? evs.map((e) =>
    h("li", { class: `ev ${eventClass(e.kind)} ${newest && e.seq > newest ? "new" : ""}` },
      h("span", { class: "d" }),
      h("div", {},
        h("div", { class: "meta" },
          h("span", { class: "mono" }, clock(e.at)),
          h("span", { class: "actor" }, e.actor),
          e.intent_id ? h("a", { onclick: () => reveal(e.intent_id) }, short(e.intent_id)) : null),
        h("div", { class: "sum" }, e.summary)))) : [h("li", { class: "empty" }, "No events yet.")]));
  state.seenEvents = evs.length ? evs[0].seq : 0;
}

function reveal(id) {
  if (!state.snap.intents.some((i) => i.id === id && i.active) && state.filter === "active") setFilter("all");
  state.open.add(id);
  state.sigs.swarm = null;
  renderSwarm(state.snap);
  const row = $(`row-${id}`);
  if (row) {
    row.scrollIntoView({ behavior: "smooth", block: "center" });
    row.classList.remove("flash");
    void row.offsetWidth;
    row.classList.add("flash");
  }
}

// ---------- Session, project picker, chrome ----------

function render(s) {
  if (!s || !state.session || s.project !== state.session.current) return;
  state.snap = s;
  $("error-banner").hidden = true;
  // Without decisions the Needs-you panel explains it; the banner would repeat it.
  const blind = (s.warnings || []).length > 0 && !s.decisions.length;
  const warn = $("warn-banner");
  warn.textContent = (s.warnings || []).join(" ");
  warn.hidden = blind || !(s.warnings || []).length;
  state.blind = blind;
  renderCounters(s);
  renderNeeds(s);
  renderOverlaps(s);
  renderSwarm(s);
  renderAudit(s);
}

function applySession(sess) {
  state.session = sess;
  const initial = (sess.identity || "?").slice(0, 1);
  $("identity").replaceChildren(h("span", { class: "avatar" }, initial), "Deciding as ", h("b", {}, sess.identity));
  document.querySelector(".brand").title = `Wayglass ${sess.version || "dev"}`;
  const sel = $("project");
  sel.replaceChildren(...sess.projects.map((p) => h("option", { value: p, selected: p === sess.current }, p)));
  sel.hidden = !sess.projects.length;
  renderNotify();
  $("name-input").placeholder = sess.default_identity || "";
  $("name-hint").textContent = `Leave empty to use ${sess.default_identity || "your CLI identity"} (the CLI's login, $WAYGLASS_AGENT or user@host). Saved in desktop.yaml.`;

  if (!sess.logged_in) {
    gate("Not connected", sess.problem || "Log in with the Wayglass CLI first.", false);
    return;
  }
  if (!sess.current) {
    gate("Open a project", "Name a Wayglass project this Worker knows. It is remembered for next time.", true);
    return;
  }
  $("gate").hidden = true;
  $("main").hidden = false;
}

// An update is a link, never a download: the Go side checked the version
// and keeps the only URL that can be opened (OpenUpdate takes none from here).
function showUpdate(u) {
  const b = $("update");
  if (!u || !u.available) { b.hidden = true; return; }
  b.replaceChildren(h("span", { class: "dot" }), `Update ${u.latest}`);
  b.hidden = false;
}

function gate(title, text, form) {
  $("main").hidden = true;
  $("gate").hidden = false;
  $("gate-title").textContent = title;
  $("gate-text").textContent = text;
  $("gate-form").hidden = !form;
  if (form) $("gate-name").focus();
}

async function switchProject(name) {
  state.sigs = {};
  state.open.clear();
  state.seenEvents = 0;
  applySession(await go().SelectProject(name));
  await refresh();
}

async function refresh() {
  if (!state.session || !state.session.logged_in || !state.session.current) return;
  try {
    render(await go().Snapshot());
  } catch (e) {
    showError(String(e));
  }
}

function showError(text) {
  const b = $("error-banner");
  b.textContent = text;
  b.hidden = false;
}

function setLive(st) {
  const live = $("live");
  live.className = `live ${st.ok ? "ok" : "err"}`;
  $("live-text").textContent = st.ok
    ? `Live · every ${Math.round(state.session.poll_ms / 100) / 10}s`
    : `Retrying in ${Math.round(st.next_ms / 1000)}s`;
  live.title = st.ok ? `Last poll ${clock(st.at)}` : st.error;
  if (!st.ok && st.failures >= 2) {
    showError(`Cannot reach the Worker: ${st.error}`);
    state.pollError = true;
  } else if (st.ok && state.pollError) {
    // An idle poll after recovery emits no snapshot, so render() would not
    // clear the banner for up to 30 s; the poll itself says the Worker is back.
    state.pollError = false;
    $("error-banner").hidden = true;
  }
}

function toggleNameForm(show) {
  const form = $("name-form");
  form.hidden = !show;
  if (show) {
    $("name-error").textContent = "";
    $("name-input").value = (state.session && state.session.display_name) || "";
    $("name-input").focus();
  }
}

async function saveDisplayName(name) {
  $("name-error").textContent = "";
  try {
    state.sigs = {};
    applySession(await go().SetDisplayName(name));
    toggleNameForm(false);
    if (state.snap) render(state.snap);
  } catch (e) {
    $("name-error").textContent = String(e);
  }
}

async function addProject(name, errEl) {
  errEl.textContent = "";
  try {
    state.sigs = {};
    applySession(await go().AddProject(name));
    $("add-form").hidden = true;
    await refresh();
  } catch (e) {
    errEl.textContent = String(e);
  }
}

// Theme: auto (follows macOS), light, dark.
const THEMES = ["auto", "light", "dark"];
const THEME_ICON = {
  auto: '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="6" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M8 2a6 6 0 0 1 0 12z" fill="currentColor"/></svg>',
  light: '<svg viewBox="0 0 16 16"><circle cx="8" cy="8" r="3" fill="currentColor"/><g stroke="currentColor" stroke-width="1.4" stroke-linecap="round"><path d="M8 1.5v1.6M8 12.9v1.6M1.5 8h1.6M12.9 8h1.6M3.4 3.4l1.1 1.1M11.5 11.5l1.1 1.1M3.4 12.6l1.1-1.1M11.5 4.5l1.1-1.1"/></g></svg>',
  dark: '<svg viewBox="0 0 16 16"><path d="M13.5 9.8A5.8 5.8 0 0 1 6.2 2.5a5.8 5.8 0 1 0 7.3 7.3z" fill="currentColor"/></svg>',
};
const BELL_ON = '<svg viewBox="0 0 16 16"><path d="M8 1.8a4 4 0 0 0-4 4v2.6L2.8 11h10.4L12 8.4V5.8a4 4 0 0 0-4-4z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round"/><path d="M6.4 13a1.7 1.7 0 0 0 3.2 0" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>';
const BELL_OFF = '<svg viewBox="0 0 16 16"><path d="M8 1.8a4 4 0 0 0-4 4v2.6L2.8 11h10.4L12 8.4V5.8a4 4 0 0 0-4-4z" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linejoin="round" opacity=".5"/><path d="M2.5 2.5l11 11" stroke="currentColor" stroke-width="1.4" stroke-linecap="round"/></svg>';

// The icons above are fixed strings from this file, never data; they are the
// only markup set with innerHTML.
function setTheme(t) {
  document.documentElement.dataset.theme = t;
  $("theme").innerHTML = THEME_ICON[t];
  $("theme").title = `Theme: ${t}`;
  try { localStorage.setItem("wayglass.theme", t); } catch { /* private mode */ }
  if (rt()) {
    if (t === "dark") rt().WindowSetDarkTheme();
    else if (t === "light") rt().WindowSetLightTheme();
    else rt().WindowSetSystemDefaultTheme();
  }
}

function renderNotify() {
  const on = state.session && state.session.notifications;
  $("notify").innerHTML = on ? BELL_ON : BELL_OFF;
  $("notify").title = on ? "Notifications on: you are told when a change needs you" : "Notifications off";
}

function setFilter(f) {
  state.filter = f;
  for (const b of document.querySelectorAll(".seg button")) b.classList.toggle("on", b.dataset.filter === f);
  state.sigs.swarm = null;
  if (state.snap) renderSwarm(state.snap);
}

// ---------- Projects ----------
//
// The directory of every project the login can read (GET /v1/projects via
// the Go side). Search filters the loaded rows at once and asks the Worker a
// moment later. Rows are text only; the clone command is copied by Go.

const projects = { open: false, rows: [], q: "", timer: null, loading: false };

function projectRow(p) {
  const stats = p.served
    ? [`${p.open} open`, p.pending_human ? `▲ ${p.pending_human} for a person` : null, `${p.landed} landed`, `${p.agents} agent${p.agents === 1 ? "" : "s"}`]
        .filter(Boolean).join(" · ")
    : `not served: ${p.residency} (an admin runs wayglass project migrate-residency)`;
  const copy = h("button", { type: "button", class: "ghost", title: p.clone }, "Copy clone command");
  copy.addEventListener("click", async () => {
    try {
      await go().CopyCloneCommand(p.name);
      copy.textContent = "Copied";
    } catch (e) {
      copy.textContent = String(e);
    }
    setTimeout(() => { copy.textContent = "Copy clone command"; }, 1800);
  });
  const open = h("button", { type: "button", class: p.current ? "ghost" : "primary", disabled: !p.served || p.current },
    p.current ? "Showing" : "Open");
  open.addEventListener("click", () => openListedProject(p.name));
  return h("div", { class: `project-row ${p.current ? "current" : ""}`, "data-name": p.name },
    h("div", { class: "project-main" },
      h("div", { class: "project-name" },
        h("span", { class: "mono" }, p.name),
        h("span", { class: `pill status ${p.jurisdiction === "eu" ? "attn" : ""}`, title: p.jurisdiction === "eu" ? "Repos and state stay in the EU" : "Jurisdiction" },
          (p.jurisdiction || "?").toUpperCase()),
        p.last_activity ? h("span", { class: "project-when", "data-ago": p.last_activity }, ago(p.last_activity)) : null),
      h("div", { class: p.served ? "project-stats" : "project-stats form-error" }, stats),
      h("code", { class: "mono project-clone" }, p.clone)),
    h("div", { class: "project-actions" }, copy, open));
}

function renderProjects() {
  const q = projects.q.trim().toLowerCase();
  const rows = q ? projects.rows.filter((p) => p.name.includes(q)) : projects.rows;
  $("projects-count").textContent = projects.loading ? "searching…" : `${rows.length}`;
  $("projects-list").replaceChildren(
    ...(rows.length ? rows.map(projectRow)
      : [h("div", { class: "empty" }, projects.loading ? "Listing projects…" : q ? `No project name contains “${projects.q.trim()}”.` : "No projects on this Worker yet; 'wayglass init' creates one.")]),
  );
}

async function loadProjects() {
  const q = projects.q.trim();
  projects.loading = true;
  $("projects-error").textContent = "";
  renderProjects();
  try {
    const v = await go().Projects(q);
    if (q !== projects.q.trim()) return; // typed on; a newer search is on its way
    projects.rows = v.projects || [];
  } catch (e) {
    $("projects-error").textContent = String(e);
  } finally {
    if (q === projects.q.trim()) {
      projects.loading = false;
      renderProjects();
    }
  }
}

function showProjects(open) {
  projects.open = open;
  $("projects").hidden = !open;
  $("projects-btn").setAttribute("aria-expanded", String(open));
  if (open) {
    $("projects-search").value = projects.q;
    $("projects-search").focus();
    loadProjects();
  }
}

async function openListedProject(name) {
  try {
    state.sigs = {};
    state.open.clear();
    state.seenEvents = 0;
    applySession(await go().AddProject(name)); // verifies with the Worker, remembers it, switches
    showProjects(false);
    await refresh();
  } catch (e) {
    $("projects-error").textContent = String(e);
  }
}

function tick() {
  for (const el of document.querySelectorAll("[data-ago]")) el.textContent = ago(el.dataset.ago);
  for (const el of document.querySelectorAll("[data-since]")) el.textContent = `waiting ${ago(el.dataset.since)}`;
}

async function boot() {
  let theme = "auto";
  try { theme = localStorage.getItem("wayglass.theme") || "auto"; } catch { /* private mode */ }
  setTheme(THEMES.includes(theme) ? theme : "auto");

  $("theme").addEventListener("click", () => {
    setTheme(THEMES[(THEMES.indexOf(document.documentElement.dataset.theme) + 1) % THEMES.length]);
  });
  $("notify").addEventListener("click", async () => {
    applySession(await go().SetNotifications(!state.session.notifications));
  });
  $("project").addEventListener("change", (e) => switchProject(e.target.value));
  $("add-project").addEventListener("click", () => {
    $("add-form").hidden = !$("add-form").hidden;
    if (!$("add-form").hidden) $("add-name").focus();
  });
  $("add-cancel").addEventListener("click", () => { $("add-form").hidden = true; });
  $("identity").addEventListener("click", () => toggleNameForm($("name-form").hidden));
  $("name-cancel").addEventListener("click", () => toggleNameForm(false));
  $("name-form").addEventListener("submit", (e) => { e.preventDefault(); saveDisplayName($("name-input").value); });
  $("add-form").addEventListener("submit", (e) => { e.preventDefault(); addProject($("add-name").value, $("add-error")); });
  $("gate-form").addEventListener("submit", (e) => { e.preventDefault(); addProject($("gate-name").value, $("gate-error")); });
  for (const b of document.querySelectorAll(".seg button")) b.addEventListener("click", () => setFilter(b.dataset.filter));
  $("projects-btn").addEventListener("click", () => showProjects(!projects.open));
  $("projects-close").addEventListener("click", () => showProjects(false));
  $("projects").addEventListener("click", (e) => { if (e.target === $("projects")) showProjects(false); });
  $("projects-search").addEventListener("input", (e) => {
    projects.q = e.target.value;
    renderProjects();
    clearTimeout(projects.timer);
    projects.timer = setTimeout(loadProjects, 300);
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") { $("add-form").hidden = true; $("name-form").hidden = true; if (projects.open) showProjects(false); }
    if ((e.metaKey || e.ctrlKey) && e.key === "p") { e.preventDefault(); showProjects(!projects.open); }
    if ((e.metaKey || e.ctrlKey) && e.key === "r") { e.preventDefault(); refresh(); }
  });

  if (!go()) {
    gate("Wayglass", "This page runs inside the Wayglass desktop app.", false);
    return;
  }
  rt().EventsOn("snapshot", (s) => render(s));
  rt().EventsOn("poll", (st) => setLive(st));
  rt().EventsOn("update", (u) => showUpdate(u));
  $("update").addEventListener("click", () => go().OpenUpdate().catch(() => {}));
  applySession(await go().Session());
  await refresh();
  setInterval(tick, 30000);
}

boot();
