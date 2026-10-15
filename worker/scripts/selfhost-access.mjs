#!/usr/bin/env node
// JSON helpers for `scripts/selfhost.sh --access`: they read Cloudflare API
// responses on stdin, build Access request bodies, and print the result. They
// never call the network; the shell script does every request.
//
//   node scripts/selfhost-access.mjs names                 the three app names for WG_NAME
//   node scripts/selfhost-access.mjs team                  <org response> -> auth_domain, or exit 3 when there is no org
//   node scripts/selfhost-access.mjs idps                  <IdP list> -> selected IdPs as JSON; WG_IDP is a newline list
//                                                          of names, ids or types (empty = every IdP); exit 3 when none
//                                                          exist, exit 4 when a named one-time PIN login is missing
//   node scripts/selfhost-access.mjs find NAME             <app list> -> the app with exactly this name, as JSON, or nothing
//   node scripts/selfhost-access.mjs body KIND             <existing app JSON or nothing> -> request body for KIND
//                                                          (web, human or api); needs WG_NAME, WG_DOMAIN, WG_IDPS, WG_ALLOW
//   node scripts/selfhost-access.mjs field NAME            <JSON> -> one top-level field of `result` (or of the object)
//
// The three applications, matched by name so a rerun updates them in place:
//   "<worker> web"            the whole host plus /v1/public: the web app and the public view, for --allow
//   "<worker> human sign-in"  /v1/human: `wayglass login --human` and the console's sign-in, for --allow, 12 h
//   "<worker> key API"        /v1: Bypass for everyone, because the Worker checks keys on every /v1 route itself
// Access applies the most specific path, so /v1/human and /v1/public stay behind
// sign-in while the rest of /v1 reaches the Worker, which refuses it without a key.

import { readFileSync } from "node:fs";

const KINDS = ["web", "human", "api"];

function fail(msg, code = 1) {
  process.stderr.write(`selfhost-access: ${msg}\n`);
  process.exit(code);
}

function stdinJson() {
  let s = "";
  try {
    s = readFileSync(0, "utf8");
  } catch {
    return null;
  }
  if (!s.trim()) return null;
  try {
    return JSON.parse(s);
  } catch {
    fail("the Cloudflare API answered with something that is not JSON");
  }
}

function appName(worker, kind) {
  return { web: `${worker} web`, human: `${worker} human sign-in`, api: `${worker} key API` }[kind];
}

/** Turns one --allow entry into Access include rules (one per IdP for a group). */
function rulesFor(entry, idps) {
  const e = entry.trim();
  if (/^[^\s@,]+@[^\s@,]+\.[^\s@,]+$/.test(e)) return [{ email: { email: e.toLowerCase() } }];
  if (/^@[^\s@,]+\.[^\s@,]+$/.test(e)) return [{ email_domain: { domain: e.slice(1).toLowerCase() } }];
  if (e.startsWith("group:")) {
    const g = e.slice("group:".length).trim();
    if (!g) fail(`--allow ${e}: name the group after group:`);
    const rules = [];
    for (const p of idps) {
      const id = p.id;
      switch (p.type) {
        case "okta":
          rules.push({ okta: { name: g, identity_provider_id: id } });
          break;
        case "azureAD":
          // Entra ID matches the group's Object ID, not its display name.
          rules.push({ azureAD: { id: g, identity_provider_id: id } });
          break;
        case "google-apps":
          // Google Workspace matches the group's email address.
          rules.push({ gsuite: { email: g, identity_provider_id: id } });
          break;
        case "github": {
          // GitHub: an organisation, or org/team.
          const [org, ...team] = g.split("/");
          const rule = { name: org, identity_provider_id: id };
          if (team.length) rule.team = team.join("/");
          rules.push({ "github-organization": rule });
          break;
        }
        case "saml":
          rules.push({ saml: { attribute_name: "groups", attribute_value: g, identity_provider_id: id } });
          break;
        case "oidc":
          rules.push({ oidc: { claim_name: "groups", claim_value: g, identity_provider_id: id } });
          break;
        default:
          // One-time PIN, Cloudflare, social logins: no groups to match.
          break;
      }
    }
    if (!rules.length) {
      fail(`--allow ${e}: none of the login methods in use (${idps.map((p) => `${p.name} [${p.type}]`).join(", ") || "none"}) has groups; use Okta, Entra ID, Google Workspace, GitHub, SAML or OIDC, or allow emails instead`);
    }
    return rules;
  }
  fail(`--allow ${JSON.stringify(e)} is not an email (you@example.com), an email domain (@example.com) or group:<name>`);
}

function allowRules(idps) {
  const entries = (process.env.WG_ALLOW ?? "").split(/[,\n]/).map((s) => s.trim()).filter(Boolean);
  if (!entries.length) fail("--allow needs at least one email, @domain or group:<name>");
  const seen = new Set();
  const out = [];
  for (const entry of entries) {
    for (const r of rulesFor(entry, idps)) {
      const k = JSON.stringify(r);
      if (!seen.has(k)) {
        seen.add(k);
        out.push(r);
      }
    }
  }
  return out;
}

function body(kind, existing) {
  const worker = process.env.WG_NAME ?? "";
  const domain = (process.env.WG_DOMAIN ?? "").toLowerCase();
  if (!worker || !domain) fail("WG_NAME and WG_DOMAIN are required");
  let idps = [];
  try {
    idps = JSON.parse(process.env.WG_IDPS || "[]");
  } catch {
    fail("WG_IDPS is not JSON");
  }
  const name = appName(worker, kind);
  const oldPolicy = (policyName) => (existing?.policies ?? []).find((p) => p && p.name === policyName)?.id;
  const policy = (policyName, decision, include) => {
    const p = { name: policyName, decision, include, precedence: 1 };
    const id = oldPolicy(policyName);
    if (id) p.id = id; // update the app's own policy instead of adding a second one
    return p;
  };
  const common = { name, type: "self_hosted" };
  if (kind === "api") {
    return {
      ...common,
      domain: `${domain}/v1`,
      destinations: [{ type: "public", uri: `${domain}/v1` }],
      app_launcher_visible: false,
      session_duration: "24h",
      policies: [policy(`${worker} agents use keys`, "bypass", [{ everyone: {} }])],
    };
  }
  if (!idps.length) fail("no login method selected");
  const people = policy(`${worker} reviewers`, "allow", allowRules(idps));
  const signIn = {
    allowed_idps: idps.map((p) => p.id),
    auto_redirect_to_identity: idps.length === 1,
  };
  if (kind === "web") {
    return {
      ...common,
      ...signIn,
      domain,
      destinations: [
        { type: "public", uri: domain },
        { type: "public", uri: `${domain}/v1/public` },
      ],
      app_launcher_visible: true,
      session_duration: "24h",
      policies: [people],
    };
  }
  return {
    ...common,
    ...signIn,
    domain: `${domain}/v1/human`,
    destinations: [{ type: "public", uri: `${domain}/v1/human` }],
    app_launcher_visible: false,
    session_duration: "12h",
    policies: [people],
  };
}

const [cmd, arg] = process.argv.slice(2);
switch (cmd) {
  case "names": {
    const worker = process.env.WG_NAME ?? "";
    process.stdout.write(KINDS.map((k) => appName(worker, k)).join("\n") + "\n");
    break;
  }
  case "team": {
    const d = stdinJson();
    const team = d?.success && d.result && typeof d.result.auth_domain === "string" ? d.result.auth_domain.trim().toLowerCase() : "";
    if (!team) process.exit(3);
    process.stdout.write(team);
    break;
  }
  case "idps": {
    const d = stdinJson();
    const all = (Array.isArray(d?.result) ? d.result : []).filter((p) => p && typeof p.id === "string").map((p) => ({ id: p.id, name: String(p.name ?? ""), type: String(p.type ?? "") }));
    const want = (process.env.WG_IDP ?? "").split("\n").map((s) => s.trim()).filter(Boolean);
    if (!want.length) {
      if (!all.length) process.exit(3);
      process.stdout.write(JSON.stringify(all));
      break;
    }
    const picked = [];
    for (const w of want) {
      const lw = w.toLowerCase();
      const hit = all.find((p) => p.id === w) ?? all.find((p) => p.name.toLowerCase() === lw) ?? all.find((p) => p.type.toLowerCase() === lw);
      // A one-time PIN login that does not exist yet is created by the caller (exit 4).
      if (!hit && /^(onetimepin|one-time pin|otp)$/.test(lw)) process.exit(4);
      if (!hit) fail(`--idp ${JSON.stringify(w)} matches no login method in your Zero Trust organisation (have: ${all.map((p) => `${p.name} [${p.type}]`).join(", ") || "none"})`);
      if (!picked.some((p) => p.id === hit.id)) picked.push(hit);
    }
    process.stdout.write(JSON.stringify(picked));
    break;
  }
  case "find": {
    const d = stdinJson();
    const hit = (Array.isArray(d?.result) ? d.result : []).find((a) => a && a.name === arg);
    if (hit) process.stdout.write(JSON.stringify(hit));
    break;
  }
  case "body": {
    if (!KINDS.includes(arg)) fail(`body needs one of ${KINDS.join(", ")}`);
    process.stdout.write(JSON.stringify(body(arg, stdinJson())));
    break;
  }
  case "field": {
    const d = stdinJson();
    const o = d && typeof d === "object" && "result" in d ? d.result : d;
    const v = o && typeof o === "object" ? o[arg] : undefined;
    if (v !== undefined && v !== null) process.stdout.write(String(v));
    break;
  }
  default:
    fail("usage: selfhost-access.mjs names|team|idps|find NAME|body KIND|field NAME");
}
