#!/usr/bin/env node
// Writes worker/wrangler.selfhost.jsonc (git-ignored) from worker/wrangler.jsonc
// for someone deploying Wayglass to their own Cloudflare account, so nobody
// edits the committed config by hand. Everything account-specific comes from
// the environment; bindings, migrations and assets come from wrangler.jsonc,
// so the two never drift.
//
//   WAYGLASS_WORKER_NAME      Worker name (default "wayglass")
//   WAYGLASS_DOMAIN           custom domain on a zone in your account; unset = workers.dev
//   WAYGLASS_NAMESPACE        main Artifacts namespace (default "wayglass")
//   WAYGLASS_JURISDICTION     its jurisdiction: us, eu or "" for unrestricted (default "us")
//   WAYGLASS_EU_NAMESPACE     optional EU namespace for `wayglass init --jurisdiction eu`
//   WAYGLASS_QUEUE            queue for Artifacts events (default "artifacts-events")
//   WAYGLASS_QUEUE_ID         that queue's id (from `wrangler queues info`); "" = trunks are not subscribed
//   WAYGLASS_PUBLIC_PROJECTS  names or prefix* allowed in the public audit view (default none)
//   WAYGLASS_AI_REVIEW        the Workers AI reviewer for agent_review changes: off (default), manual or auto
//   WAYGLASS_AI_GATEWAY_ID    AI Gateway the model calls go through; "" = straight to Workers AI
//   WAYGLASS_AI_REVIEW_MODEL  Workers AI model; "" = the Worker's default (src/aiproviders.ts)
//   WAYGLASS_AI_PROVIDERS     providers the reviewer may use (workers-ai, anthropic, openai); "" = workers-ai
//   WAYGLASS_AI_DAILY_REVIEWS AI model calls per project per day; "" = 200
//   WAYGLASS_ACCESS_TEAM_DOMAIN  Zero Trust team domain (<team>.cloudflareaccess.com) for people
//                             signing in through Cloudflare Access; unset = off
//   WAYGLASS_ACCESS_AUD       that Access application's audience (AUD) tag; set both or neither
//   WAYGLASS_ACCESS_WEB_AUD   audience tag(s), comma list, of the Access app covering the web app;
//                             unset = ACCESS_AUD. Needs the two above
//
// Usage: node scripts/selfhost-config.mjs [output path]   (run from worker/)

import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const workerDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const out = resolve(process.argv[2] ?? join(workerDir, "wrangler.selfhost.jsonc"));
if (dirname(out) !== workerDir) {
  // main, assets and $schema are relative paths; they only resolve next to wrangler.jsonc.
  fail(`output must be in ${workerDir} (got ${out})`);
}

/** JSONC to JSON: drops // and /* *\/ comments outside strings, and trailing commas. */
function stripJsonc(src) {
  let res = "";
  for (let i = 0; i < src.length; i++) {
    const c = src[i];
    if (c === '"') {
      let j = i + 1;
      while (j < src.length && src[j] !== '"') j += src[j] === "\\" ? 2 : 1;
      res += src.slice(i, j + 1);
      i = j;
    } else if (c === "/" && src[i + 1] === "/") {
      while (i < src.length && src[i] !== "\n") i++;
      res += "\n";
    } else if (c === "/" && src[i + 1] === "*") {
      i = src.indexOf("*/", i + 2) + 1;
      if (i === 0) fail("unterminated comment in wrangler.jsonc");
    } else {
      res += c;
    }
  }
  return res.replace(/,(\s*[}\]])/g, "$1");
}

function fail(msg) {
  console.error(`selfhost-config: ${msg}`);
  process.exit(1);
}

const env = process.env;
const pick = (k, d) => (env[k] === undefined ? d : env[k].trim());
const NAME = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;
const NS = /^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$/;

const name = pick("WAYGLASS_WORKER_NAME", "wayglass");
const domain = pick("WAYGLASS_DOMAIN", "");
const ns = pick("WAYGLASS_NAMESPACE", "wayglass");
const jurisdiction = pick("WAYGLASS_JURISDICTION", "us").toLowerCase();
const euNs = pick("WAYGLASS_EU_NAMESPACE", "");
const queue = pick("WAYGLASS_QUEUE", "artifacts-events");
const queueId = pick("WAYGLASS_QUEUE_ID", "");
const publicProjects = pick("WAYGLASS_PUBLIC_PROJECTS", "");
const aiReview = pick("WAYGLASS_AI_REVIEW", "off").toLowerCase();
const aiGateway = pick("WAYGLASS_AI_GATEWAY_ID", "");
const aiModel = pick("WAYGLASS_AI_REVIEW_MODEL", "");
const aiProviders = pick("WAYGLASS_AI_PROVIDERS", "workers-ai").toLowerCase().split(",").map((p) => p.trim()).filter(Boolean);
const aiBudget = pick("WAYGLASS_AI_DAILY_REVIEWS", "200") || "200";
const accessTeam = pick("WAYGLASS_ACCESS_TEAM_DOMAIN", "").toLowerCase().replace(/^https:\/\//, "").replace(/\/+$/, "");
const accessAud = pick("WAYGLASS_ACCESS_AUD", "");
const accessWebAud = pick("WAYGLASS_ACCESS_WEB_AUD", "").split(",").map((a) => a.trim()).filter(Boolean).join(",");

if (!NAME.test(name)) fail(`WAYGLASS_WORKER_NAME ${JSON.stringify(name)} must be lowercase letters, digits and inner hyphens`);
if (!NS.test(ns)) fail(`WAYGLASS_NAMESPACE ${JSON.stringify(ns)} is not a valid namespace name`);
if (euNs && !NS.test(euNs)) fail(`WAYGLASS_EU_NAMESPACE ${JSON.stringify(euNs)} is not a valid namespace name`);
if (euNs && euNs === ns) fail("WAYGLASS_EU_NAMESPACE must differ from WAYGLASS_NAMESPACE");
if (!["us", "eu", ""].includes(jurisdiction)) fail(`WAYGLASS_JURISDICTION must be us, eu or empty (got ${jurisdiction})`);
if (domain && !/^(?!-)[a-z0-9.-]+\.[a-z]{2,}$/i.test(domain)) fail(`WAYGLASS_DOMAIN ${JSON.stringify(domain)} is not a host name`);
if (queueId && !/^[0-9a-f]{32}$/.test(queueId)) fail(`WAYGLASS_QUEUE_ID ${JSON.stringify(queueId)} is not a queue id`);
if (Boolean(accessTeam) !== Boolean(accessAud)) fail("set both WAYGLASS_ACCESS_TEAM_DOMAIN and WAYGLASS_ACCESS_AUD, or neither");
if (accessTeam && !/^(?!-)[a-z0-9.-]+\.[a-z]{2,}$/.test(accessTeam)) fail(`WAYGLASS_ACCESS_TEAM_DOMAIN ${JSON.stringify(accessTeam)} is not a host name`);
if (accessAud && !/^[A-Za-z0-9._-]{1,256}$/.test(accessAud)) fail(`WAYGLASS_ACCESS_AUD ${JSON.stringify(accessAud)} is not an audience tag`);
if (accessWebAud && !accessTeam) fail("WAYGLASS_ACCESS_WEB_AUD needs WAYGLASS_ACCESS_TEAM_DOMAIN and WAYGLASS_ACCESS_AUD");
if (accessWebAud && !accessWebAud.split(",").every((a) => /^[A-Za-z0-9._-]{1,256}$/.test(a))) fail(`WAYGLASS_ACCESS_WEB_AUD ${JSON.stringify(accessWebAud)} is not a list of audience tags`);
if (!["off", "manual", "auto"].includes(aiReview)) fail(`WAYGLASS_AI_REVIEW must be off, manual or auto (got ${aiReview})`);
if (aiGateway && !/^[A-Za-z0-9._-]{1,64}$/.test(aiGateway)) fail(`WAYGLASS_AI_GATEWAY_ID ${JSON.stringify(aiGateway)} is not a gateway id`);
if (aiModel && !/^@?[A-Za-z0-9._/-]{1,128}$/.test(aiModel)) fail(`WAYGLASS_AI_REVIEW_MODEL ${JSON.stringify(aiModel)} is not a model name`);
for (const p of aiProviders) if (!["workers-ai", "anthropic", "openai"].includes(p)) fail(`WAYGLASS_AI_PROVIDERS names ${JSON.stringify(p)}; want workers-ai, anthropic or openai`);
if (aiProviders.some((p) => p !== "workers-ai") && !aiGateway) fail("WAYGLASS_AI_PROVIDERS anthropic or openai needs WAYGLASS_AI_GATEWAY_ID: the gateway holds their keys");
if (!/^[0-9]{1,6}$/.test(aiBudget)) fail(`WAYGLASS_AI_DAILY_REVIEWS ${JSON.stringify(aiBudget)} is not a whole number`);
if (!NAME.test(queue)) fail(`WAYGLASS_QUEUE ${JSON.stringify(queue)} is not a valid queue name`);

const cfg = JSON.parse(stripJsonc(readFileSync(join(workerDir, "wrangler.jsonc"), "utf8")));

cfg.name = name;
delete cfg.account_id; // wrangler reads CLOUDFLARE_ACCOUNT_ID
if (domain) {
  cfg.routes = [{ pattern: domain, custom_domain: true }];
  cfg.workers_dev = false;
} else {
  delete cfg.routes;
  cfg.workers_dev = true;
}

const main = cfg.artifacts.find((a) => a.binding === "ARTIFACTS");
if (!main) fail("wrangler.jsonc has no ARTIFACTS binding");
main.namespace = ns;
cfg.artifacts = cfg.artifacts.filter((a) => a.binding !== "ARTIFACTS_EU");
// An EU namespace only adds anything when the main one is not already EU (src/artifacts.ts).
const withEu = Boolean(euNs) && jurisdiction !== "eu";
if (withEu) cfg.artifacts.push({ binding: "ARTIFACTS_EU", namespace: euNs, remote: true });

cfg.vars = {
  ...cfg.vars,
  ARTIFACTS_JURISDICTION: jurisdiction,
  ARTIFACTS_NAMESPACE: ns,
  ARTIFACTS_EVENTS_QUEUE_ID: queueId,
  PUBLIC_PROJECTS: publicProjects,
  // The AI reviewer is a cost: off unless asked for, and never the hosted gateway.
  AI_REVIEW: aiReview,
  AI_GATEWAY_ID: aiGateway,
  AI_REVIEW_MODEL: aiModel,
  AI_PROVIDERS: aiProviders.join(",") || "workers-ai",
  AI_DAILY_REVIEWS: aiBudget,
  // Never inherit wrangler.jsonc's Access settings: they name another team.
  ACCESS_TEAM_DOMAIN: accessTeam,
  ACCESS_AUD: accessAud,
  ACCESS_WEB_AUD: accessWebAud,
};
if (withEu) cfg.vars.ARTIFACTS_EU_NAMESPACE = euNs;
else delete cfg.vars.ARTIFACTS_EU_NAMESPACE;

const consumers = cfg.queues?.consumers ?? [];
if (consumers.length !== 1) fail("expected exactly one queue consumer in wrangler.jsonc");
consumers[0].queue = queue;

const header =
  `// GENERATED by worker/scripts/selfhost-config.mjs from wrangler.jsonc. Do not edit;\n` +
  `// rerun scripts/selfhost.sh (or the generator) instead. Git-ignored.\n`;
writeFileSync(out, header + JSON.stringify(cfg, null, 2) + "\n");
console.log(relative(process.cwd(), out) || out);
