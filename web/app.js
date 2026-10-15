import { h, ago, short } from "./ui.js";
import { client } from "./console/client.js";
import {
  snapshot,
  projectName,
  sameIdentity,
  active,
  backoff,
  verdictOf,
  sessionFrom,
  accessOffered,
  aiRecord,
  aiName,
  aiAskRoute,
  aiCounts,
  deciderName,
  cloneCommand,
  filterProjects,
  projectSummary,
} from "./console/model.js";
import { parseYAML, diffLines } from "./vendor.js";

const $ = (s) => document.querySelector(s);
const replace = (target, ...children) =>
  target.replaceChildren(
    ...children.flat(Infinity).filter((n) => n !== false && n != null),
  );
const sessionKey = "wayglass.console.session";
const draft = new Map();
let session = null,
  identity = null,
  controller = new AbortController(),
  generation = 0;
let project = new URL(location.href).searchParams.get("project") || "";
if (!projectName(project)) project = "";
let data = null,
  events = [],
  auditNext = 0,
  timer,
  failures = 0,
  renderId = 0;
let directoryRows = [],
  directoryNext = null;
const enc = encodeURIComponent;
const errText = (e) => `${e.code || "error"}: ${e.message}`;
const text = (s, cls = "") => h("p", { class: cls }, s);
const muted = (s) => text(s, "text-sm text-base-content/65");
const heading = (title, description, actions) =>
  h(
    "div",
    { class: "flex flex-wrap items-start justify-between gap-4 mb-6" },
    h(
      "div",
      { class: "min-w-0" },
      h(
        "h1",
        { class: "text-2xl font-semibold tracking-tight break-words" },
        title,
      ),
      description && muted(description),
    ),
    actions,
  );
const panel = (title, ...children) =>
  h(
    "section",
    { class: "card bg-base-100 border border-base-300 min-w-0" },
    h(
      "div",
      { class: "card-body gap-4 p-5" },
      title && h("h2", { class: "card-title text-base" }, title),
      children,
    ),
  );
const badge = (value) =>
  h(
    "span",
    {
      class:
        value === "quarantine" ||
        value === "quarantined" ||
        value === "pending_human"
          ? "badge badge-soft badge-warning h-auto py-1 whitespace-normal"
          : value === "block" || value === "blocked" || value === "rejected"
            ? "badge badge-soft badge-error h-auto py-1 whitespace-normal"
            : "badge badge-soft h-auto py-1 whitespace-normal",
    },
    String(value || "unknown").replaceAll("_", " "),
  );
const link = (label, route, cls = "link link-hover") =>
  h("a", { href: `#/${route}`, class: cls }, label);
const button = (label, action, cls = "btn") =>
  h("button", { type: "button", class: cls, onclick: action }, label);
const note = (s) =>
  h("div", { class: "alert alert-soft", role: "status" }, text(s));
const empty = (title, detail) => panel(title, muted(detail));
const code = (s) =>
  h(
    "pre",
    {
      class:
        "max-w-full overflow-x-auto rounded-box bg-base-200 p-4 text-xs leading-6",
    },
    h("code", {}, s),
  );
function tags(t) {
  if (!t) return muted("Classification appears after a proposal.");
  return h(
    "dl",
    {
      class:
        "grid grid-cols-2 xl:grid-cols-4 gap-4 border-y border-base-300 py-4",
    },
    [
      ["Area", t.areas?.join(", ") || "None"],
      ["Data class", t.data_classes?.join(", ") || "None"],
      ["Risk", t.risk || "Unknown"],
      ["Jurisdiction", t.jurisdiction?.toUpperCase() || "Unknown"],
    ].map(([k, v]) =>
      h(
        "div",
        { class: "min-w-0" },
        h("dt", { class: "text-xs text-base-content/65 mb-1" }, k),
        h("dd", { class: "font-semibold text-sm break-words" }, v),
      ),
    ),
  );
}
function table(labels, rows) {
  return h(
    "div",
    { class: "max-w-full overflow-x-auto" },
    h(
      "table",
      { class: "table table-sm" },
      h(
        "thead",
        {},
        h(
          "tr",
          {},
          labels.map((l) => h("th", {}, l)),
        ),
      ),
      h(
        "tbody",
        {},
        rows.map((row) =>
          h(
            "tr",
            {},
            row.map((cell) =>
              h(
                "td",
                { class: "align-top whitespace-normal max-w-sm break-words" },
                cell,
              ),
            ),
          ),
        ),
      ),
    ),
  );
}
function field(id, label, input) {
  return h(
    "div",
    { class: "fieldset min-w-0" },
    h("label", { class: "label", for: id }, label),
    input,
  );
}
const input = (id, attrs = {}) =>
  h("input", { id, class: "input w-full", ...attrs });
const nav = [
  ["Dashboard", "", "icon-[lucide--layout-dashboard]"],
  ["Intents", "intents", "icon-[lucide--git-branch]"],
  ["Review queue", "review", "icon-[lucide--clipboard-check]"],
  ["Code", "code", "icon-[lucide--files]"],
  ["History", "history", "icon-[lucide--history]"],
  ["Audit log", "audit", "icon-[lucide--list-checks]"],
  ["Policy", "policy", "icon-[lucide--shield-check]"],
];
function route() {
  const raw =
    (location.hash.startsWith("#/") ? location.hash.slice(2) : "") ||
    (project ? "" : "projects");
  const [path, query] = raw.split("?");
  return { path, q: new URLSearchParams(query), parts: path.split("/") };
}
function setDrawer(open) {
  $("#drawer").checked = open;
  $("#nav-toggle").setAttribute("aria-expanded", String(open));
}
function shell() {
  const selected = route().parts[0];
  const menu = h(
    "ul",
    { class: "menu w-full gap-1" },
    nav.map(([label, path, icon]) =>
      h(
        "li",
        {},
        h(
          "a",
          {
            href: `#/${path}`,
            class: selected === path ? "menu-active" : null,
            "aria-current": selected === path ? "page" : null,
            onclick: () => {
              setDrawer(false);
            },
          },
          h("span", { class: icon + " size-5", "aria-hidden": true }),
          label,
        ),
      ),
    ),
  );
  const switcher = h(
    "form",
    {
      class: "grid gap-2",
      onsubmit: (e) => {
        e.preventDefault();
        openProject($("#project").value);
      },
    },
    field(
      "project",
      "Project name",
      input("project", {
        value: project,
        placeholder: "demo-example",
        required: true,
        pattern: "[a-z0-9][a-z0-9._-]{0,62}",
      }),
    ),
    h("button", { class: "btn w-full", type: "submit" }, "Open"),
  );
  const sidebar = h(
    "aside",
    {
      class:
        "min-h-full w-64 bg-base-100 border-r border-base-300 flex flex-col p-4",
    },
    h(
      "a",
      {
        href: "/#/projects",
        class: "flex items-center gap-3 px-3 py-4 font-semibold text-xl",
      },
      h("img", { src: "/favicon-32.png", width: 32, height: 32, alt: "" }),
      "Wayglass",
    ),
    h("div", { class: "px-3 py-4" }, switcher),
    project ? menu : null,
    h(
      "div",
      { class: "mt-auto p-3 space-y-3" },
      h(
        "a",
        { href: "/#/projects", class: "btn btn-ghost w-full justify-start" },
        "All projects",
      ),
      muted(
        identity
          ? `${identity.agent} · ${identity.kind === "human" ? "Person" : "Agent"}`
          : "Public view",
      ),
      identity
        ? button("Sign out", signOut, "btn btn-outline w-full")
        : link("Sign in", "signin", "btn btn-outline w-full"),
    ),
  );
  const theme = button(
    "Change theme",
    () => {
      const dark = document.documentElement.dataset.theme
        ? document.documentElement.dataset.theme === "dark"
        : matchMedia("(prefers-color-scheme: dark)").matches;
      document.documentElement.dataset.theme = dark ? "light" : "dark";
      localStorage.setItem("wayglass.theme", dark ? "light" : "dark");
    },
    "btn btn-ghost btn-square",
  );
  theme.setAttribute("aria-label", "Change theme");
  theme.replaceChildren(
    h("span", { class: "icon-[lucide--sun-moon] size-5", "aria-hidden": true }),
  );
  $("#app").replaceChildren(
    h(
      "div",
      { class: "drawer lg:drawer-open" },
      h("input", {
        id: "drawer",
        type: "checkbox",
        class: "drawer-toggle",
        "aria-label": "Navigation",
        onchange: e => $("#nav-toggle").setAttribute("aria-expanded", String(e.target.checked)),
      }),
      h(
        "div",
        { class: "drawer-content min-w-0" },
        h(
          "header",
          {
            class:
              "navbar bg-base-100 border-b border-base-300 px-4 md:px-8 gap-3",
          },
          h(
            "button",
            {
              id: "nav-toggle",
              type: "button",
              "aria-expanded": "false",
              "aria-controls": "navigation",
              onclick: () => setDrawer(!$("#drawer").checked),
              class: "btn btn-ghost btn-square lg:hidden",
              "aria-label": "Open navigation",
            },
            h("span", {
              class: "icon-[lucide--menu] size-5",
              "aria-hidden": true,
            }),
          ),
          h(
            "div",
            { class: "flex-1 min-w-0 text-sm font-semibold truncate" },
            project || "Projects",
          ),
          h(
            "span",
            {
              id: "connection",
              class: "text-xs text-base-content/65",
              role: "status",
            },
            identity ? "Signed in" : "Public",
          ),
          theme,
        ),
        h("main", {
          id: "content",
          tabindex: -1,
          class: "p-4 md:p-8 max-w-screen-2xl mx-auto min-w-0 space-y-6",
        }),
      ),
      h(
        "div",
        { id: "navigation", class: "drawer-side z-40" },
        h("label", {
          for: "drawer",
          class: "drawer-overlay",
          "aria-label": "Close navigation",
        }),
        sidebar,
      ),
    ),
  );
}
const api = (path, body) =>
  client(
    session?.key || "",
    identity?.agent || session?.name || "web-reader",
    controller.signal,
  )(path, body);
function stop() {
  clearTimeout(timer);
  controller.abort();
  controller = new AbortController();
  generation++;
  renderId++;
}
// Set while a person is signed out, so the sign-in page does not send them
// straight back through Cloudflare Access; cleared on the next sign-in.
const signedOutKey = "wayglass.console.signed-out";
function storeFlag(key, on) {
  try {
    if (on) sessionStorage.setItem(key, "1");
    else sessionStorage.removeItem(key);
  } catch {
    // Storage unavailable: the page still signs out.
  }
}
// Forget the key in this tab first, before anything asynchronous: closing the
// tab or navigating away mid sign-out must not leave it in session storage.
function forget(ending) {
  stop();
  session = null;
  identity = null;
  draft.clear();
  data = null;
  events = [];
  auditNext = 0;
  directoryRows = [];
  directoryNext = null;
  try {
    sessionStorage.removeItem(sessionKey);
  } catch {
    // Storage unavailable: nothing was stored.
  }
  storeFlag(signedOutKey, ending?.source !== "access");
}
// Revoke the key itself, so signing out ends the session on the Worker too,
// not only in this tab. The admin key cannot be revoked; that is fine here.
async function revoke(ending, who) {
  if (!ending?.key || !who?.key_id) return;
  try {
    await client(ending.key, who.agent || ending.name, AbortSignal.timeout(5000))("/v1/keys/self/revoke", {});
  } catch {
    // Best effort: the key still expires on its own.
  }
}
// Shown on the sign-in page until the next sign-in: why the last session ended.
let signinNotice = null;
async function signOut(why) {
  const ending = session,
    who = identity;
  forget(ending);
  signinNotice = why || null;
  if (ending?.source === "access") {
    // One sign-out for both layers: end the Cloudflare Access session too, so
    // the next visit asks the identity provider again and then signs straight in.
    shell();
    replace($("#content"), heading("Signing out"), note(why || "Ending your session…"));
    await revoke(ending, who);
    location.assign("/cdn-cgi/access/logout");
    return;
  }
  location.hash = "/signin";
  shell();
  render();
  await revoke(ending, who);
}
function openProject(value) {
  value = value.trim();
  if (!projectName(value)) return;
  stop();
  project = value;
  data = null;
  events = [];
  auditNext = 0;
  draft.clear();
  failures = 0;
  const url = new URL(location.href);
  url.search = "";
  url.searchParams.set("project", project);
  url.hash = "/";
  history.pushState(null, "", url);
  try {
    const saved = JSON.parse(
      localStorage.getItem("wayglass.console.projects") || "[]",
    );
    localStorage.setItem(
      "wayglass.console.projects",
      JSON.stringify(
        [project, ...saved.filter((p) => p !== project)].slice(0, 12),
      ),
    );
  } catch {
    /* storage unavailable */
  }
  shell();
  render();
  refresh();
}
function endpoint(view, query = "") {
  return identity
    ? `/v1/${view}?project=${enc(project)}${query}`
    : `/v1/public/projects/${enc(project)}/${view}?${query.replace(/^&/, "")}`;
}
async function refresh(manual = false) {
  clearTimeout(timer);
  if (!project) return;
  const gen = generation;
  try {
    const [intents, decisions, audit, costs] = await Promise.all([
      api(endpoint("intents")),
      api(endpoint("decisions", "&limit=500")),
      api(endpoint("audit", `&since=${auditNext}&limit=500`)),
      api(identity ? endpoint("cost") : `/v1/public/projects/${enc(project)}`),
    ]);
    if (gen !== generation) return;
    const initial = !data;
    data = {
      ...snapshot(intents.intents || [], decisions.decisions || []),
      cost: identity ? costs : costs.cost,
      jurisdiction:
        costs.project?.jurisdiction || intents.project?.jurisdiction,
    };
    const seen = new Set(events.map((e) => e.seq));
    events.push(...(audit.events || []).filter((e) => !seen.has(e.seq)));
    auditNext = audit.next || auditNext;
    failures = 0;
    $("#connection").textContent =
      "Updated " +
      new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    const path = route().path;
    if (initial || manual || ["", "intents"].includes(path)) render();
  } catch (e) {
    if (gen !== generation) return;
    if (e.code === "unauthorized" && session) {
      signOut(`Your key was refused. Sign in again. (${e.message})`);
      return;
    }
    failures++;
    $("#connection").textContent = "Update failed · retrying";
    if (!data)
      $("#content").replaceChildren(
        heading("Project unavailable", project),
        note(errText(e)),
        button("Retry", () => refresh(true)),
      );
  } finally {
    if (gen === generation)
      timer = setTimeout(() => refresh(), backoff(failures));
  }
}
/** Whether the Worker offers Cloudflare Access sign-in (see accessOffered). */
async function accessAvailable(signal) {
  try {
    const res = await fetch(new URL("/v1/human/session", location.origin), {
      headers: { Accept: "application/json" },
      mode: "same-origin",
      credentials: "omit",
      redirect: "manual",
      cache: "no-store",
      signal: AbortSignal.any([signal, AbortSignal.timeout(15000)]),
    });
    if (res.type === "opaqueredirect") return accessOffered({ type: res.type });
    const envelope = await res.json();
    return accessOffered({ type: res.type, code: envelope?.error?.code });
  } catch {
    return false;
  }
}
function signin() {
  const message = h("div", {
    role: "status",
    "aria-live": "polite",
    class: "text-sm break-words",
  });
  const submit = h(
    "button",
    { class: "btn btn-neutral w-full", type: "submit" },
    "Sign in",
  );
  const form = h(
    "form",
    {
      class: "space-y-4",
      onsubmit: async (e) => {
        e.preventDefault();
        submit.disabled = true;
        const gen = generation;
        const candidate = {
          key: $("#key").value.trim(),
          name: $("#name").value.trim() || "web-reader",
        };
        try {
          const who = await client(
            candidate.key,
            candidate.name,
            controller.signal,
          )("/v1/whoami");
          if (gen !== generation) return;
          stop();
          session = candidate;
          identity = who;
          signinNotice = null;
          sessionStorage.setItem(sessionKey, JSON.stringify(candidate));
          storeFlag(signedOutKey, false);
          data = null;
          events = [];
          auditNext = 0;
          location.hash = "/projects"; // the directory is where a session starts
          shell();
          render();
          if (project) refresh();
        } catch (e) {
          if (gen === generation) replace(message, note(errText(e)));
        } finally {
          submit.disabled = false;
        }
      },
    },
    field(
      "key",
      "Wayglass key",
      input("key", { type: "password", autocomplete: "off", required: true }),
    ),
    field(
      "name",
      "Display name (admin keys only)",
      input("name", { autocomplete: "username", placeholder: "Your name" }),
    ),
    muted(
      "A bound key supplies its own identity. Person-routed changes require a person’s key. The key stays in this tab’s session storage until sign-out.",
    ),
    submit,
    message,
  );
  // People can sign in through Cloudflare Access when the Worker has it on.
  // The sign-in page (/v1/human/login, behind Access) mints a human key that
  // expires and stores it in this tab's session storage, like a pasted key.
  const access = h("div", { class: "space-y-2" });
  accessAvailable(controller.signal).then((on) => {
    if (!on) return;
    const href =
      "/v1/human/login?to=console" +
      (project ? `&project=${encodeURIComponent(project)}` : "");
    // Behind Access the person has already signed in once to reach this page,
    // so sign them in with it rather than asking again. #/signin/key keeps the
    // key form for admin and agent keys; after a sign-out, wait for a click.
    let signedOut = false;
    try {
      signedOut = sessionStorage.getItem(signedOutKey) === "1";
    } catch {
      signedOut = true;
    }
    if (!signedOut && location.hash !== "#/signin/key") {
      location.replace(href);
      return;
    }
    // The hidden attribute, not a class: the stylesheet is built from the
    // classes in use and may not carry one that is only added at runtime.
    if (location.hash !== "#/signin/key") form.hidden = true;
    replace(
      access,
      h(
        "a",
        { class: "btn btn-outline w-full", href },
        "Sign in with Cloudflare Access",
      ),
      muted(
        "For people: sign in with your organisation’s login. The Worker issues a human key bound to your email that expires in 12 hours.",
      ),
      location.hash === "#/signin/key"
        ? ""
        : h("a", { class: "link text-sm", href: "#/signin/key" }, "Use a Wayglass key instead"),
    );
  });
  return [
    heading("Sign in", "Use a key issued by your Wayglass administrator."),
    signinNotice && note(signinNotice),
    h("div", { class: "max-w-lg space-y-4" }, panel("Your identity", form, access)),
  ];
}
// The project directory: every project this key can read (or, signed out,
// the public ones), searched as you type. The search box filters the rows
// already loaded at once and asks the Worker (?q=) a moment later, so a
// project on a later directory page is found too. Text only: names, counts
// and times go through createTextNode (h), never markup.
let directoryQuery = "",
  directoryTimer,
  directoryLoading = false;
function jurisdictionBadge(j) {
  const v = String(j || "").toUpperCase();
  return h(
    "span",
    {
      class:
        v === "EU"
          ? "badge badge-soft badge-info"
          : "badge badge-soft badge-ghost",
      title: v === "EU" ? "Repos and state stay in the EU" : "Jurisdiction",
    },
    v || "—",
  );
}
function copyButton(cmd) {
  const btn = button(
    "Copy",
    async () => {
      try {
        await navigator.clipboard.writeText(cmd);
        btn.textContent = "Copied";
      } catch {
        btn.textContent = "Select and copy";
      }
      setTimeout(() => {
        btn.textContent = "Copy";
      }, 1500);
    },
    "btn btn-xs btn-ghost",
  );
  btn.setAttribute("aria-label", `Copy ${cmd}`);
  return btn;
}
function projectCard(p) {
  const s = projectSummary(p);
  const open = `/?project=${enc(p.name)}#/`;
  const clone = cloneCommand(p.name);
  return h(
    "article",
    {
      class:
        "card bg-base-100 border border-base-300 hover:border-base-content/40 transition-colors min-w-0",
      "data-project": p.name,
    },
    h(
      "div",
      { class: "card-body p-5 gap-3" },
      h(
        "div",
        { class: "flex items-center justify-between gap-3" },
        h("span", {
          class: "icon-[lucide--folder-git-2] size-6 shrink-0",
          "aria-hidden": true,
        }),
        h(
          "div",
          { class: "flex items-center gap-2" },
          s.needsYou
            ? h(
                "span",
                { class: "badge badge-soft badge-warning" },
                `▲ ${s.needsYou} for a person`,
              )
            : null,
          jurisdictionBadge(p.jurisdiction),
        ),
      ),
      h(
        "h2",
        { class: "card-title text-lg break-all" },
        s.served
          ? h("a", { href: open, class: "link link-hover" }, p.name)
          : p.name,
      ),
      s.served
        ? h(
            "p",
            { class: "text-sm text-base-content/65" },
            s.text,
            p.last_activity ? " · active " : "",
            p.last_activity ? ago(p.last_activity) : "",
          )
        : h(
            "p",
            { class: "text-sm text-warning" },
            s.text,
            " (an admin runs wayglass project migrate-residency)",
          ),
      p.default_branch
        ? muted(`branch ${p.default_branch}${p.public ? " · public view on" : ""}`)
        : null,
      h(
        "div",
        { class: "flex items-center gap-2 min-w-0" },
        h(
          "code",
          {
            class:
              "text-xs bg-base-200 rounded-box px-2 py-1 break-all min-w-0",
          },
          clone,
        ),
        copyButton(clone),
      ),
      s.served
        ? h(
            "div",
            { class: "card-actions justify-end" },
            h("a", { href: open, class: "btn btn-sm btn-neutral" }, "Open"),
          )
        : null,
    ),
  );
}
function renderDirectoryList(target, rid) {
  const list = target.querySelector("#project-list");
  if (!list) return;
  const rows = filterProjects(directoryRows, directoryQuery);
  const status = target.querySelector("#project-status");
  if (status)
    status.textContent = directoryLoading
      ? "Searching the directory…"
      : `${rows.length} project${rows.length === 1 ? "" : "s"}${directoryQuery ? ` matching “${directoryQuery}”` : ""}`;
  replace(
    list,
    rows.length
      ? h(
          "div",
          { class: "grid gap-4 sm:grid-cols-2 xl:grid-cols-3" },
          rows.map(projectCard),
        )
      : empty(
          directoryQuery ? "No project matches" : "No projects on this page",
          directoryNext
            ? "Continue to the next directory page."
            : directoryQuery
              ? "Try a shorter part of the name."
              : "Open a project by name, or sign in to see private projects.",
        ),
    directoryNext &&
      button("Load more projects", () => projectsView(target, rid, true)),
    muted(
      "Directory pages also scan agent forks. A page can contain no projects and still have a next page.",
    ),
  );
}
function directoryShell(target, rid) {
  const search = input("project-search", {
    type: "search",
    value: directoryQuery,
    placeholder: "Search projects by name",
    autocomplete: "off",
    spellcheck: false,
    maxlength: 63,
    oninput: (e) => {
      directoryQuery = e.target.value;
      renderDirectoryList(target, rid);
      clearTimeout(directoryTimer);
      directoryTimer = setTimeout(() => projectsView(target, rid), 300);
    },
  });
  replace(
    target,
    heading(
      "Projects",
      identity
        ? "Every project your key can read. Open one, or copy its clone command."
        : "Public projects opted into the audit view.",
      !identity && link("Sign in", "signin", "btn btn-neutral"),
    ),
    h(
      "div",
      { class: "flex flex-wrap items-end gap-3" },
      h("div", { class: "grow max-w-md" }, field("project-search", "Search", search)),
      h("p", { id: "project-status", class: "text-sm text-base-content/65 pb-2" }, "Loading projects…"),
    ),
    h("div", { id: "project-list", class: "grid gap-4" }),
  );
  return search;
}
async function projectsView(target, rid, more = false) {
  const gen = generation,
    q = directoryQuery.trim();
  if (!target.querySelector("#project-list")) directoryShell(target, rid);
  directoryLoading = true;
  renderDirectoryList(target, rid);
  try {
    const params = new URLSearchParams();
    if (q) params.set("q", q);
    if (more && directoryNext) params.set("cursor", directoryNext);
    const result = await api(
      (identity ? "/v1/projects" : "/v1/public/projects") +
        (params.size ? `?${params}` : ""),
    );
    if (rid !== renderId || gen !== generation || q !== directoryQuery.trim())
      return;
    directoryRows = more
      ? [...directoryRows, ...result.projects]
      : result.projects;
    directoryNext = result.next;
    directoryLoading = false;
    renderDirectoryList(target, rid);
  } catch (e) {
    if (rid !== renderId || gen !== generation) return;
    directoryLoading = false;
    const list = target.querySelector("#project-list");
    if (list)
      replace(
        list,
        note(errText(e)),
        button("Retry", () => projectsView(target, rid)),
      );
  }
}
function activity(rows) {
  if (!rows.length) return muted("No events recorded yet.");
  return h(
    "ul",
    { class: "list" },
    rows.map((e) =>
      h(
        "li",
        { class: "list-row px-0 min-w-0" },
        h("span", { class: "status status-neutral mt-2", "aria-hidden": true }),
        h(
          "div",
          { class: "min-w-0" },
          h("div", { class: "text-sm font-semibold break-words" }, e.kind),
          h(
            "div",
            { class: "text-xs text-base-content/65 break-words" },
            e.actor,
            " · ",
            e.intent_id
              ? link(e.intent_id, `intents/${e.intent_id}`)
              : "Project",
          ),
        ),
        h("div", { class: "text-xs text-base-content/65" }, ago(e.at)),
      ),
    ),
  );
}
function dashboard() {
  const ds = data.decisions,
    latest = [...data.latest.values()];
  const metrics = [
    [
      "Agents",
      new Set(data.intents.map((i) => i.agent)).size,
      "Distinct intent authors",
    ],
    ["Open intents", data.intents.filter(active).length, "Open or proposed"],
    ["Needs a person", data.waiting.length, "Pending human review"],
    [
      "Auto-merged",
      ds.filter((d) => d.action === "auto_merge" && d.landed_commit).length,
      "Verified landed changes",
    ],
  ];
  const ai = aiCounts(ds);
  if (ai.approved + ai.escalated + ai.advised > 0)
    metrics.push([
      "AI reviewer",
      `${ai.approved} ✓ · ${ai.escalated} ▲`,
      `${ai.approved} approved, ${ai.escalated} escalated to a person, ${ai.advised} advised`,
    ]);
  const aiCost = data.cost?.ai;
  return [
    heading(
      "Project dashboard",
      "Intent, policy and the decisions that followed.",
      link("Review queue", "review", "btn btn-neutral"),
    ),
    h(
      "div",
      { class: "grid grid-cols-2 gap-4 xl:grid-cols-4" },
      metrics.map(([label, value, detail]) =>
        h(
          "div",
          { class: "stats bg-base-100 border border-base-300 min-w-0" },
          h(
            "div",
            { class: "stat min-w-0 p-4" },
            h("p", { class: "stat-title" }, label),
            h("p", { class: "stat-value text-3xl" }, value),
            h("p", { class: "stat-desc whitespace-normal" }, detail),
          ),
        ),
      ),
    ),
    h(
      "div",
      { class: "grid gap-6 xl:grid-cols-3" },
      h(
        "div",
        { class: "xl:col-span-2 min-w-0 space-y-6" },
        panel(
          "Recent activity",
          activity(events.slice(-8).reverse()),
          link("View audit log", "audit", "btn btn-ghost self-start"),
        ),
        panel(
          "Policy routing",
          table(
            ["Action", "Decisions"],
            [
              "auto_merge",
              "agent_review",
              "human_approve",
              "multi_approve",
              "quarantine",
              "block",
            ].map((a) => [badge(a), ds.filter((d) => d.action === a).length]),
          ),
          muted(
            "Counts cover the most recent 500 decisions, including replaced proposals.",
          ),
        ),
      ),
      h(
        "div",
        { class: "min-w-0 space-y-6" },
        panel(
          "Worker cost",
          h(
            "p",
            { class: "text-3xl font-semibold tabular-nums" },
            data.cost?.total
              ? `$${Number(data.cost.total.est_usd).toFixed(4)}`
              : "Unavailable",
          ),
          muted(`${data.cost?.total?.ops ?? "—"} binding calls`),
          aiCost &&
            muted(
              aiCost.today
                ? `AI reviewer today: ${aiCost.today.calls ?? 0} call(s) ≈ $${Number(aiCost.today.est_usd ?? 0).toFixed(4)}`
                : `AI reviewer: ${aiCost.calls ?? 0} call(s) ≈ $${Number(aiCost.est_usd ?? 0).toFixed(4)}`,
            ),
          muted(data.cost?.scope || "Cost data is unavailable."),
          muted(
            "Estimate before included operations. Time saved is not measured.",
          ),
        ),
        panel(
          "Current holds",
          text(
            `${latest.filter((d) => d.status === "quarantined").length} quarantined`,
          ),
          text(
            `${latest.filter((d) => d.status === "blocked").length} blocked`,
          ),
          text(`${data.overlaps.length} overlaps`),
          link("Inspect intents", "intents", "btn btn-outline"),
        ),
      ),
    ),
  ];
}
function intentList() {
  return [
    heading("Intents", "What each agent plans to change and why."),
    panel(
      null,
      data.intents.length
        ? table(
            ["Goal / intent", "Author", "Status", "Decision", "Updated"],
            data.intents
              .slice()
              .reverse()
              .map((i) => [
                h("div", {}, link(i.goal, `intents/${i.id}`), muted(i.id)),
                i.agent,
                badge(i.status),
                data.latest.has(i.id)
                  ? badge(data.latest.get(i.id).action)
                  : "Not proposed",
                ago(i.updated_at),
              ]),
          )
        : muted("No intents declared yet."),
    ),
    panel(
      "Overlaps and suggested replans",
      data.overlaps.length
        ? data.overlaps.map((o) =>
            h(
              "div",
              {
                class: "border-t first:border-0 border-base-300 pt-4 space-y-2",
              },
              h(
                "div",
                { class: "flex flex-wrap gap-2" },
                link(o.a.id, `intents/${o.a.id}`),
                " / ",
                link(o.b.id, `intents/${o.b.id}`),
              ),
              code(o.paths.join("\n")),
              text(o.suggestion),
              muted("Suggestion only; no intent was changed."),
            ),
          )
        : muted("No contested paths among open intents."),
    ),
  ];
}
function decisionContext(d) {
  const ai = aiRecord(d);
  return [
    tags(d.tags),
    h(
      "div",
      { class: "flex flex-wrap gap-2" },
      badge(d.action),
      badge(d.status),
      aiBadge(ai),
    ),
    h(
      "div",
      {},
      muted(`Matched rule · ${d.matched_rule || "Default"}`),
      text(d.reason, "text-sm leading-6"),
    ),
    muted(`Decision ${d.id} · commit ${short(d.commit_sha)}`),
    escalationBlock(ai),
    adviceBlock(ai),
    shadow(d),
  ];
}
// The AI reviewer on a decision. Its words are model output, written from
// the agent's diff: text nodes only, like everything else on this page.
function aiBadge(ai) {
  if (!ai.decided) return null;
  const what = ai.escalation ? "escalated by" : "approved by";
  return h(
    "span",
    { class: "badge badge-soft h-auto py-1 whitespace-normal", "data-ai": what },
    `◆ ${what} ${aiName(ai.model)}`,
  );
}
function escalationBlock(ai) {
  const e = ai.escalation;
  if (!e) return null;
  return h(
    "div",
    { class: "rounded-box bg-base-200 p-4 space-y-2", role: "status", "data-ai": "escalation" },
    text(`Escalated by ${aiName(e.model)}: ${e.reason}`, "text-sm font-semibold"),
    e.concerns.map((c) => text(`• ${c}`, "text-sm")),
    muted(
      e.from
        ? `Was ${e.from.replaceAll("_", " ")}; the AI could not approve it alone, so a person decides.`
        : "The AI could not approve it alone, so a person decides.",
    ),
  );
}
function adviceBlock(ai) {
  const a = ai.advice;
  if (!a) return null;
  const group = (label, items) =>
    items.length
      ? h(
          "div",
          {},
          h("h4", { class: "text-xs font-semibold mt-2" }, label),
          items.map((x) => text(`• ${x}`, "text-sm")),
        )
      : null;
  return h(
    "div",
    { class: "rounded-box bg-base-200 p-4 space-y-2", "data-ai": "advice" },
    h("h3", { class: "text-sm font-semibold" }, `◆ AI advice · advisory`),
    text(a.summary, "text-sm leading-6"),
    group("Risks", a.risks),
    group("Concerns", a.concerns),
    group("Questions", a.questions),
    muted(`Advice only, from ${aiName(a.model)}: the decision and its reason are yours.`),
  );
}
// "Ask AI": a verdict while a reviewer agent is awaited (the AI approves or
// hands the change to a person), advice once a person must decide. The
// Worker's refusals (reviewer off, only a person may decide) are shown as
// they are; nothing here decides anything.
const aiAnswers = new Map(); // decision id -> the last answer, kept across re-renders
function askAI(i, d) {
  const route = aiAskRoute(d);
  if (!identity || !route) return null;
  const feedback = h(
    "div",
    { role: "status", "aria-live": "polite", class: "break-words" },
    aiAnswers.has(d.id) && note(aiAnswers.get(d.id)),
  );
  const label = route === "ai-review" ? "Ask the AI reviewer to decide" : "Ask AI for advice";
  const btn = button(label, async () => {
    const gen = generation,
      view = renderId;
    btn.disabled = true;
    replace(feedback, muted("Asking the AI reviewer…"));
    try {
      const out = await api(`/v1/intents/${enc(i.id)}/${route}`, { project });
      if (gen !== generation) return;
      const r = out.review,
        a = out.advice;
      const said = r
        ? `${aiName(r.model)} ${r.verdict === "approve" ? "approved" : "escalated to a person"}: ${r.reason}`
        : a
          ? `${aiName(a.model || aiRecord(out.decision || {}).model)} advised; advice only, the decision is yours.`
          : "The AI reviewer answered.";
      aiAnswers.set(d.id, said);
      replace(feedback, note(said));
      await refresh(false);
      if (view === renderId) render();
    } catch (err) {
      if (gen !== generation) return;
      replace(
        feedback,
        note(
          err.code === "ai_unavailable"
            ? `The AI reviewer is not available on this Worker: ${err.message}`
            : err.code === "needs_human"
              ? `Only a person can decide this change: ${err.message}`
              : err.code === "not_found" && /no route/.test(err.message)
                ? "This Worker has no AI advice route yet."
                : errText(err),
        ),
      );
    } finally {
      if (gen === generation) btn.disabled = false;
    }
  }, "btn btn-outline btn-sm");
  return h("div", { class: "space-y-2", "data-ai": "ask" }, btn, feedback);
}
function shadow(d) {
  const matches = d.detail?.shadow_matches || d.shadow_matches || [];
  return matches.length
    ? h(
        "div",
        { class: "space-y-2" },
        h("h3", { class: "text-sm font-semibold" }, "Shadow matches"),
        matches.map((m) =>
          text(`${m.rule}: ${m.action} — ${m.reason}`, "text-sm"),
        ),
        muted(
          d.detail?.would_differ || d.would_differ
            ? "A shadow rule would change the action."
            : "Shadow rules did not change the action.",
        ),
      )
    : null;
}
function intentDetail(id) {
  const i = data.intents.find((i) => i.id === id);
  if (!i)
    return [empty("Intent not found", "It may belong to another project.")];
  const d = data.latest.get(id),
    children = data.intents.filter((x) => x.parent_id === id);
  return [
    heading(i.goal, `${i.id} · ${i.agent}`, badge(i.status)),
    h(
      "div",
      { class: "grid gap-6 xl:grid-cols-3" },
      h(
        "div",
        { class: "xl:col-span-2 space-y-6 min-w-0" },
        panel(
          "Why this change",
          text(i.reason, "leading-7"),
          d && tags(d.tags),
        ),
        panel(
          "Paths",
          h(
            "div",
            { class: "grid gap-4 md:grid-cols-2" },
            h(
              "div",
              {},
              h("h3", { class: "font-semibold text-sm mb-2" }, "Declared"),
              code((i.declared_paths || []).join("\n") || "No declared paths"),
            ),
            h(
              "div",
              {},
              h("h3", { class: "font-semibold text-sm mb-2" }, "Pushed"),
              code((i.actual_paths || []).join("\n") || "No pushed paths"),
            ),
          ),
        ),
        panel(
          "Stack and session",
          i.parent_id
            ? link(`Parent ${i.parent_id}`, `intents/${i.parent_id}`)
            : muted("Forked from trunk."),
          children.map((c) => link(`Child ${c.id}`, `intents/${c.id}`)),
          i.session_id
            ? link(`Session ${i.session_id}`, `sessions/${i.session_id}`)
            : muted("No shared session is exposed for this intent."),
          data.overlaps
            .filter((o) => o.a.id === id || o.b.id === id)
            .map((o) => note(o.suggestion)),
        ),
      ),
      h(
        "div",
        { class: "space-y-6 min-w-0" },
        d
          ? panel(
              "Latest decision",
              decisionContext(d),
              (d.approvals || []).map((a) =>
                text(`${deciderName(a)} · ${a.verdict}: ${a.reason}`, "text-sm"),
              ),
              link("Read change", `changes/${d.id}`, "btn btn-neutral"),
              data.waiting.some((w) => w.intent.id === id) &&
                link("Review this change", `review/${d.id}`, "btn btn-outline"),
              d.status === "pending_agent" && active(i) && askAI(i, d),
            )
          : panel("Decision", muted("This intent has not been proposed.")),
      ),
    ),
  ];
}
function reviewForm(i, d) {
  if (!identity)
    return note("Sign in with a person’s key to review this decision.");
  if (identity.kind !== "human")
    return note("This key belongs to an agent. A person’s key is required.");
  if (sameIdentity(identity.agent || "", i.agent || ""))
    return note("The author cannot approve their own change.");
  if (!["pending_human", "quarantined"].includes(d.status) || !active(i))
    return note("This decision is no longer waiting for a person.");
  const reason = h(
    "textarea",
    {
      id: `reason-${d.id}`,
      class: "textarea w-full min-h-28",
      required: true,
      maxlength: 4000,
      oninput: (e) => draft.set(d.id, e.target.value),
    },
    draft.get(d.id) || "",
  );
  const feedback = h("div", {
    role: "status",
    "aria-live": "polite",
    class: "break-words",
  });
  const approve = h(
    "button",
    { type: "submit", value: "approve", class: "btn btn-neutral" },
    "Approve",
  );
  const reject = h(
    "button",
    { type: "submit", value: "reject", class: "btn btn-outline" },
    "Reject",
  );
  return h(
    "form",
    {
      class: "space-y-4",
      onsubmit: async (e) => {
        e.preventDefault();
        if (!reason.value.trim()) {
          reason.focus();
          return;
        }
        // Only a pressed Approve or Reject button records a verdict.
        const verdict = verdictOf(e.submitter?.value);
        if (!verdict) return;
        const gen = generation,
          view = renderId;
        approve.disabled = reject.disabled = true;
        try {
          await api(`/v1/decisions/${enc(d.id)}`, {
            project,
            verdict,
            reason: reason.value.trim(),
            family: "human",
          });
          if (gen !== generation) return;
          draft.delete(d.id);
          replace(
            feedback,
            note("Decision recorded. Landing is a separate step."),
          );
          await refresh(false);
          if (view === renderId) render();
        } catch (err) {
          if (gen === generation) replace(feedback, note(errText(err)));
        } finally {
          if (gen === generation) approve.disabled = reject.disabled = false;
        }
      },
    },
    field(`reason-${d.id}`, "What did you check?", reason),
    muted("Your reason is recorded in the audit log."),
    h("div", { class: "flex flex-wrap gap-2" }, approve, reject),
    feedback,
  );
}
function review(id) {
  const waiting = data.waiting.filter((w) => !id || w.decision.id === id);
  return [
    heading(
      "Review queue",
      "Read the intent and change, then record your reason.",
      button("Refresh queue", () => refresh(true)),
    ),
    !identity && link("Sign in to review", "signin", "btn btn-neutral"),
    !waiting.length
      ? empty(
          "Nothing waiting on you",
          "No current decisions need a person in this project.",
        )
      : waiting.map(({ intent: i, decision: d }) =>
          panel(
            null,
            h(
              "div",
              { class: "grid gap-6 xl:grid-cols-3" },
              h(
                "div",
                { class: "xl:col-span-2 min-w-0 space-y-4" },
                h(
                  "h2",
                  { class: "text-xl font-semibold" },
                  link(i.goal, `intents/${i.id}`),
                ),
                muted(`${i.agent} · ${i.id}`),
                text(i.reason, "leading-6"),
                decisionContext(d),
                diffView(d),
              ),
              h(
                "div",
                {
                  class:
                    "min-w-0 space-y-4 xl:border-l border-base-300 xl:pl-6",
                },
                h("h3", { class: "font-semibold" }, "Your decision"),
                reviewForm(i, d),
                askAI(i, d),
                (d.approvals || []).map((a) =>
                  h(
                    "div",
                    { class: "border-t border-base-300 pt-3" },
                    text(`${deciderName(a)} · ${a.verdict}`, "text-sm font-semibold"),
                    text(a.reason, "text-sm"),
                  ),
                ),
              ),
            ),
          ),
        ),
  ];
}
function diffView(d) {
  if (!identity)
    return note(
      "Sign in to read file contents. Public views include classification and audit metadata.",
    );
  const paths =
    d.paths ||
    d.detail?.files?.map((f) => f.path) ||
    d.files?.map((f) => f.path) ||
    [];
  if (!paths.length) return muted("No changed paths were recorded.");
  const select = h(
    "select",
    {
      id: `file-${d.id}`,
      class: "select w-full",
      "aria-label": "Changed file",
    },
    paths.map((p) => h("option", { value: p }, p)),
  );
  const output = h("div", { class: "min-w-0" });
  let seq = 0;
  const read = async () => {
    const n = ++seq,
      gen = generation;
    replace(output, muted("Loading change…"));
    try {
      const result = await api(
        `/v1/projects/${enc(project)}/changes/${enc(d.id)}?path=${enc(select.value)}`,
      );
      if (gen !== generation || n !== seq) return;
      const a = result.before,
        b = result.after;
      if (a.binary || b.binary) {
        replace(output, note("Binary content is not displayed."));
        return;
      }
      if (a.truncated || b.truncated) {
        replace(
          output,
          note(
            "This file exceeds the 128 KiB web limit. Use wayglass diff to review the complete change.",
          ),
        );
        return;
      }
      if (
        (a.text || "").split("\n").length > 4000 ||
        (b.text || "").split("\n").length > 4000
      ) {
        replace(
          output,
          note(
            "This file exceeds the 4,000-line web limit. Use wayglass diff.",
          ),
        );
        return;
      }
      const parts = diffLines(a.text || "", b.text || "", {
        timeout: 100,
        maxEditLength: 4000,
      });
      if (!parts) {
        replace(
          output,
          note("This diff is too large for the browser. Use wayglass diff."),
        );
        return;
      }
      let old = 0,
        now = 0;
      const lines = [];
      for (const p of parts) {
        const rows = p.value.split("\n");
        if (rows.at(-1) === "") rows.pop();
        for (const line of rows) {
          const left = p.added ? "" : ++old,
            right = p.removed ? "" : ++now;
          lines.push(
            h(
              "div",
              {
                class: p.added
                  ? "bg-success/10 text-base-content flex min-w-max"
                  : p.removed
                    ? "bg-error/10 text-base-content flex min-w-max"
                    : "flex min-w-max",
              },
              h(
                "span",
                {
                  class:
                    "w-10 shrink-0 text-right pr-2 text-base-content/60 select-none",
                },
                left,
              ),
              h(
                "span",
                {
                  class:
                    "w-10 shrink-0 text-right pr-2 text-base-content/60 select-none",
                },
                right,
              ),
              h(
                "span",
                { class: "w-5 shrink-0 select-none" },
                p.added ? "+" : p.removed ? "−" : " ",
              ),
              h("span", { class: "whitespace-pre pr-4" }, line || " "),
            ),
          );
        }
      }
      replace(
        output,
        muted(
          `${short(result.base)} → ${short(result.commit)} · ${result.path}`,
        ),
        h(
          "div",
          {
            class:
              "max-w-full max-h-96 overflow-auto border border-base-300 rounded-box mt-3 font-mono text-xs leading-6",
            role: "region",
            "aria-label": "File diff",
            tabindex: 0,
          },
          lines,
        ),
      );
    } catch (e) {
      if (gen === generation && n === seq) replace(output, note(errText(e)));
    }
  };
  select.addEventListener("change", read);
  read();
  return h(
    "div",
    { class: "space-y-3 min-w-0" },
    field(`file-${d.id}`, "Changed file", select),
    output,
  );
}
function change(id) {
  const d = data.decisions.find((x) => x.id === id);
  if (!d)
    return [
      empty(
        "Decision not found",
        "Only the most recent 500 decisions are listed.",
      ),
    ];
  const i = data.intents.find((x) => x.id === d.intent_id);
  return [
    heading(
      "Change",
      i?.goal || d.intent_id,
      link("Intent", `intents/${d.intent_id}`, "btn btn-outline"),
    ),
    panel(null, decisionContext(d), diffView(d)),
  ];
}
async function codeView(target, rid, q) {
  if (!identity) {
    replace(
      target,
      heading("Code", "Browse the project’s trunk."),
      note("Sign in to read repository contents."),
      link("Sign in", "signin", "btn btn-neutral"),
    );
    return;
  }
  const path = q.get("path") || "",
    ref = q.get("ref") || "";
  try {
    const result = await api(
      `/v1/projects/${enc(project)}/browse?path=${enc(path)}${ref ? `&ref=${enc(ref)}` : ""}`,
    );
    if (rid !== renderId) return;
    const pathLink = (label, p) =>
      link(
        label,
        `code?path=${enc(p)}${result.ref ? `&ref=${enc(result.ref)}` : ""}`,
      );
    const crumbs = h(
      "div",
      { class: "breadcrumbs text-sm max-w-full" },
      h(
        "ul",
        {},
        h("li", {}, pathLink("Trunk", "")),
        path
          .split("/")
          .filter(Boolean)
          .map((part, n) =>
            h(
              "li",
              {},
              pathLink(
                part,
                path
                  .split("/")
                  .slice(0, n + 1)
                  .join("/"),
              ),
            ),
          ),
      ),
    );
    const items = [
      heading(
        "Code",
        result.ref ? `Commit ${result.ref}` : "The trunk is empty.",
        link("History", "history", "btn btn-outline"),
      ),
      crumbs,
    ];
    if (result.type === "tree")
      items.push(
        panel(
          null,
          result.entries.length
            ? table(
                ["Name", "Type"],
                result.entries.map((e) => [
                  pathLink(e.name, (path ? path + "/" : "") + e.name),
                  e.type,
                ]),
              )
            : muted("No files in this directory."),
          result.truncated && note("Only the first 1,000 entries are shown."),
        ),
      );
    else if (result.type === "gitlink")
      items.push(panel("Submodule", code(result.hash)));
    else if (
      result.binary ||
      result.truncated ||
      (result.text || "").split("\n").length > 4000
    )
      items.push(
        note(
          result.binary
            ? "Binary file. Contents are not displayed."
            : "File exceeds the 128 KiB or 4,000-line web limit. Use a local checkout for the complete file.",
        ),
      );
    else {
      const why = h(
        "div",
        { class: "space-y-4" },
        muted(
          "Select a line to see this commit’s recorded intent. Exact line authorship requires git blame in the CLI.",
        ),
      );
      const lines = (result.text || "").split("\n");
      const file = h(
        "div",
        {
          class:
            "max-w-full max-h-160 overflow-auto font-mono text-xs leading-6",
          role: "region",
          "aria-label": "File contents",
          tabindex: 0,
        },
        lines.map((line, n) =>
          h(
            "div",
            { class: "flex min-w-max" },
            h(
              "button",
              {
                type: "button",
                class: "btn btn-ghost btn-sm w-12 min-h-10 shrink-0 font-mono",
                "aria-label": `Why line ${n + 1}`,
                onclick: async () => {
                  replace(why, muted("Reading commit provenance…"));
                  try {
                    const r = await api(
                      `/v1/why?project=${enc(project)}&commit=${enc(result.ref)}`,
                    );
                    if (rid !== renderId) return;
                    replace(
                      why,
                      text(
                        `Line ${n + 1} · commit provenance`,
                        "font-semibold",
                      ),
                      text(r.intent.goal),
                      text(r.intent.reason, "text-sm"),
                      tags(r.decision.tags),
                      text(r.decision.reason, "text-sm"),
                      muted(
                        "This is the selected commit’s decision, not a blame result for the line.",
                      ),
                      code(`wayglass why ${path}:${n + 1}`),
                    );
                  } catch (e) {
                    if (rid === renderId)
                      replace(
                        why,
                        note(errText(e)),
                        muted(
                          "No recorded decision for this commit. Use the CLI for line-level blame.",
                        ),
                        code(`wayglass why ${path}:${n + 1}`),
                      );
                  }
                },
              },
              n + 1,
            ),
            h("span", { class: "whitespace-pre py-2 pr-4" }, line || " "),
          ),
        ),
      );
      items.push(
        h(
          "div",
          { class: "grid gap-6 xl:grid-cols-3" },
          h("div", { class: "xl:col-span-2 min-w-0" }, panel(path, file)),
          panel("Why is this here?", why),
        ),
      );
    }
    replace(target, ...items);
  } catch (e) {
    if (rid === renderId)
      replace(
        target,
        heading("Code", path || "Trunk"),
        note(errText(e)),
        link("Back to trunk", "code", "btn"),
      );
  }
}
async function historyView(target, rid, cursor = "") {
  if (!identity) {
    replace(
      target,
      heading("History", "First-parent trunk history."),
      note("Sign in to read commit history."),
    );
    return;
  }
  try {
    const r = await api(
      `/v1/projects/${enc(project)}/history${cursor ? `?cursor=${enc(cursor)}` : ""}`,
    );
    if (rid !== renderId) return;
    replace(
      target,
      heading("History", r.scope),
      panel(
        null,
        r.commits.length
          ? table(
              ["Commit", "Message", "Author"],
              r.commits.map((c) => [
                link(short(c.hash), `code?ref=${enc(c.hash)}`),
                c.message || "No message supplied",
                c.author || "Unknown",
              ]),
            )
          : muted("No commits yet."),
      ),
      r.next && button("Older commits", () => historyView(target, rid, r.next)),
    );
  } catch (e) {
    if (rid === renderId) replace(target, heading("History"), note(errText(e)));
  }
}
function download(format, rows) {
  const csv = (v) =>
    '"' +
    String(v ?? "")
      .replace(/^[=+@-]/, "'$&")
      .replaceAll('"', '""') +
    '"';
  const content =
    format === "json"
      ? JSON.stringify(
          { project, scope: "Loaded audit events", events: rows },
          null,
          2,
        )
      : [
          [
            "seq",
            "at",
            "kind",
            "actor",
            "intent_id",
            "decision_id",
            "data",
          ].join(","),
          ...rows.map((e) =>
            [
              e.seq,
              e.at,
              e.kind,
              e.actor,
              e.intent_id,
              e.decision_id,
              JSON.stringify(e.data),
            ]
              .map(csv)
              .join(","),
          ),
        ].join("\r\n");
  const url = URL.createObjectURL(
    new Blob([content], {
      type: format === "json" ? "application/json" : "text/csv",
    }),
  );
  const a = h("a", { href: url, download: `${project}-audit.${format}` });
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
function auditView() {
  const list = h("div", { class: "min-w-0" });
  let rows = events;
  const search = input("audit-filter", {
    type: "search",
    placeholder: "Kind, actor, intent or reason…",
    oninput: (e) => {
      const term = e.target.value.toLowerCase();
      rows = events.filter((x) =>
        JSON.stringify(x).toLowerCase().includes(term),
      );
      draw();
    },
  });
  const draw = () =>
    list.replaceChildren(
      muted(
        `${rows.length} matching events loaded. Exports include these rows only.`,
      ),
      rows.length
        ? table(
            ["Sequence / time", "Event", "Actor / intent", "Detail"],
            rows
              .slice()
              .reverse()
              .map((e) => [
                h("div", {}, text(`#${e.seq}`), ago(e.at)),
                e.kind,
                h(
                  "div",
                  {},
                  e.actor,
                  e.intent_id && text("", "mt-1"),
                  e.intent_id && link(e.intent_id, `intents/${e.intent_id}`),
                ),
                code(JSON.stringify(e.data, null, 2)),
              ]),
          )
        : muted("No matching events."),
    );
  draw();
  return [
    heading(
      "Audit log",
      "The project’s append-only record.",
      h(
        "div",
        { class: "flex gap-2 flex-wrap" },
        button("Export CSV", () => download("csv", rows)),
        button("Export JSON", () => download("json", rows)),
      ),
    ),
    panel(
      null,
      field("audit-filter", "Filter loaded events", search),
      list,
      button("Load newer events", async () => {
        await refresh(false);
        rows = events.filter((x) =>
          JSON.stringify(x).toLowerCase().includes(search.value.toLowerCase()),
        );
        draw();
      }),
    ),
  ];
}
async function policyView(target, rid) {
  const shadows = panel(
    "Observed shadow matches",
    data.decisions.flatMap((d) =>
      (d.detail?.shadow_matches || d.shadow_matches || []).length
        ? [
            h(
              "div",
              { class: "border-t first:border-0 border-base-300 pt-3" },
              link(d.id, `changes/${d.id}`),
              shadow(d),
            ),
          ]
        : [],
    ),
  );
  if (!identity) {
    replace(
      target,
      heading("Policy", "Rules are read from the trunk."),
      note("Sign in to read .wayglass/policy.yaml."),
      shadows,
    );
    return;
  }
  try {
    const r = await api(
      `/v1/projects/${enc(project)}/browse?path=.wayglass%2Fpolicy.yaml`,
    );
    if (rid !== renderId) return;
    if (!r.text || r.truncated || r.binary)
      throw new Error("The policy file cannot be displayed.");
    const p = parseYAML(r.text, { maxAliasCount: 50 });
    const rules = Array.isArray(p?.rules) ? p.rules : [];
    replace(
      target,
      heading("Policy", `Trunk ${short(r.ref)} · first enforcing match wins`),
      panel(
        "Rules",
        rules.length
          ? table(
              ["Order / mode", "When", "Action", "Reason"],
              rules.map((r, n) => [
                h("div", {}, `rules[${n}]`, badge(r.mode || "enforce")),
                code(JSON.stringify(r.when || {}, null, 2)),
                badge(r.action),
                r.reason || "No reason",
              ]),
            )
          : muted("No rules found."),
        p?.default &&
          text(`Default: ${p.default.action} — ${p.default.reason}`),
      ),
      shadows,
      panel("Source YAML", code(r.text)),
    );
  } catch (e) {
    if (rid === renderId)
      replace(target, heading("Policy"), note(errText(e)), shadows);
  }
}
async function sessionView(target, rid, id) {
  if (!identity) {
    replace(
      target,
      heading("Session"),
      note("Sign in to read shared session context."),
    );
    return;
  }
  try {
    const r = await api(`/v1/projects/${enc(project)}/sessions/${enc(id)}`);
    if (rid !== renderId) return;
    replace(
      target,
      heading("Shared session", id),
      panel(
        "Handoff notes",
        text(r.session.notes || "No handoff notes.", "whitespace-pre-wrap"),
      ),
      panel("Continue in a checkout", code(`wayglass session fork ${r.url}`)),
      panel(
        "Recorded context",
        code(JSON.stringify(r.session.snapshot, null, 2)),
      ),
    );
  } catch (e) {
    if (rid === renderId) replace(target, heading("Session"), note(errText(e)));
  }
}
function render() {
  const rid = ++renderId,
    { path, parts, q } = route(),
    target = $("#content");
  if (!target) return;
  const titles = {
    "": "Dashboard",
    projects: "Projects",
    intents: "Intents",
    review: "Review queue",
    changes: "Change",
    code: "Code",
    history: "History",
    audit: "Audit log",
    policy: "Policy",
    signin: "Sign in",
    sessions: "Session",
  };
  document.title = `Wayglass · ${titles[parts[0]] || "Page not found"}`;
  document.querySelectorAll(".menu a").forEach((a) => {
    const on = a.getAttribute("href") === `#/${parts[0]}`;
    a.classList.toggle("menu-active", on);
    if (on) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  });
  if (parts[0] === "signin") {
    replace(target, ...signin());
    return;
  }
  if (path === "projects" || !project) {
    replace(target, heading("Projects"), muted("Loading projects…"));
    projectsView(target, rid);
    return;
  }
  if (!data) {
    replace(
      target,
      heading("Reading project", project),
      h("div", {
        class: "skeleton h-32 w-full",
        "aria-label": "Loading project",
      }),
    );
    return;
  }
  if (parts[0] === "code") {
    codeView(target, rid, q);
    return;
  }
  if (parts[0] === "history") {
    historyView(target, rid);
    return;
  }
  if (parts[0] === "policy") {
    policyView(target, rid);
    return;
  }
  if (parts[0] === "sessions") {
    sessionView(target, rid, parts[1]);
    return;
  }
  const views = {
    "": () => dashboard(),
    intents: () => (parts[1] ? intentDetail(parts[1]) : intentList()),
    review: () => review(parts[1]),
    changes: () => change(parts[1]),
    audit: auditView,
  };
  replace(
    target,
    ...(views[parts[0]]
      ? views[parts[0]]()
      : [empty("Page not found", "Choose a view from the navigation.")]),
  );
}
try {
  const theme = localStorage.getItem("wayglass.theme");
  if (["light", "dark"].includes(theme))
    document.documentElement.dataset.theme = theme;
  const stored = sessionStorage.getItem(sessionKey);
  session = sessionFrom(stored);
  if (stored && !session) {
    // An expired Access session: wait for a click rather than signing in again
    // by itself, and never send the stale key.
    sessionStorage.removeItem(sessionKey);
    storeFlag(signedOutKey, true);
    signinNotice = "Your session has expired. Sign in again.";
  }
} catch {
  session = null;
}
shell();
if (session) {
  try {
    identity = await api("/v1/whoami");
  } catch (e) {
    // A refused or unreachable key is dropped. The sign-in page then waits for
    // a click: a key the Access login page just minted must never loop through
    // the identity provider on its own.
    forget(session);
    storeFlag(signedOutKey, true);
    signinNotice = `Your key was refused. Sign in again. (${errText(e)})`;
  }
  shell();
}
// Signed out on a Worker with Access on: go to sign-in, which signs a person
// in through Access instead of showing pages that need a key.
if (!session && !route().parts[0].startsWith("signin") && (await accessAvailable(controller.signal)))
  location.hash = "/signin";
render();
if (project) refresh();
addEventListener("hashchange", () => {
  render();
  $("#content")?.focus({ preventScroll: true });
});
addEventListener("keydown", (e) => {
  if (e.key === "Escape" && $("#drawer")?.checked) {
    setDrawer(false);
    $("#nav-toggle").focus();
  }
});
addEventListener("popstate", () => {
  const value = new URL(location.href).searchParams.get("project") || "";
  if (value !== project) {
    stop();
    project = projectName(value) ? value : "";
    data = null;
    events = [];
    auditNext = 0;
    shell();
    render();
    if (project) refresh();
  }
});
