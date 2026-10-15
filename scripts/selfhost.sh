#!/usr/bin/env bash
# Deploys your own Wayglass Worker to your own Cloudflare account (Workers Paid).
# See docs/SELF-HOSTING.md for what it creates and what it costs.
#
#   1. creates the Artifacts namespace(s), or checks existing ones' jurisdiction
#   2. creates the queue for Artifacts push events, or reuses it
#   3. writes worker/wrangler.selfhost.jsonc (git-ignored) from worker/wrangler.jsonc
#   3a. with --access, creates or updates the Cloudflare Access apps and takes their AUD tags
#   4. generates an admin key locally and deploys the Worker with its secrets
#   5. logs this machine in and runs `wayglass doctor` (with --access, then checks the redirect)
#
# With --uninstall it deletes the queue, then the Worker (a queue's consumer
# cannot be deleted first), checks with the API that both are gone, and leaves
# the Artifacts namespaces alone: deleting those is the account owner's call.
#
# The admin key goes from openssl into a 0600 temp file for wrangler and into
# `wayglass login` on stdin. It is never printed or passed in argv.
#
# Usage: scripts/selfhost.sh [options]
#   --name NAME            Worker name (default: wayglass)
#   --domain HOST          serve on a custom domain in one of your zones (default: workers.dev)
#   --namespace NS         main Artifacts namespace (default: wayglass)
#   --jurisdiction J       main namespace jurisdiction: us, eu or unrestricted (default: us)
#   --eu-namespace NS      also create an EU namespace for `wayglass init --jurisdiction eu`
#   --queue NAME           queue for push events (default: artifacts-events)
#   --public PATTERNS      projects allowed in the public audit view, e.g. 'demo-*,eu-demo-*' (default: none)
#   --ai-review MODE       the Workers AI reviewer for agent_review changes: off (default), manual
#                          (`wayglass ai-review <intent-id>`) or auto (right after every propose that
#                          lands in agent_review). Workers AI bills your account per token (neurons)
#   --ai-gateway ID        with --ai-review: an AI Gateway in your account the model calls go through
#                          (logs, caching, rate limits); default: none, calls go straight to Workers AI
#   --ai-model NAME        with --ai-review: the Workers AI model (default: @cf/meta/llama-4-scout-17b-16e-instruct)
#   --ai-providers LIST    with --ai-review: providers the reviewer may use, comma-separated from
#                          workers-ai, anthropic, openai (default: workers-ai). anthropic and openai go
#                          through --ai-gateway, which must hold their key (BYOK, alias default) or
#                          Unified Billing credit; the model is picked so it is never the author's family
#   --ai-budget N          with --ai-review: AI model calls per project per day; over it, reviews go to
#                          a person and advice is skipped (default: 200)
#   --access               set up Cloudflare Access for people: reads your Zero Trust org, picks its
#                          login methods (or adds one-time PIN), creates or updates three Access
#                          apps named after --name, deploys with their AUD tags and checks the
#                          redirect. Needs --allow. Works on workers.dev (<name>.<subdomain>.workers.dev)
#                          or on --domain. See docs/SELF-HOSTING.md
#   --allow LIST           with --access: who may sign in, comma-separated or repeated:
#                          you@example.com, @example.com (email domain), group:<name> (IdP group)
#   --idp NAME|ID          with --access: login method to offer (repeatable; default: all of them)
#   --access-team DOMAIN   set up Access by hand instead of --access: your Zero Trust team domain,
#                          e.g. acme.cloudflareaccess.com (default: off; see docs/SELF-HOSTING.md)
#   --access-aud TAG       the Access application's audience (AUD) tag; needs --access-team
#   --access-web-aud TAGS  audience tag(s), comma list, of an Access app covering the web app
#                          (/, /console/, /v1/public/*); the Worker then refuses those routes without
#                          a valid token for it. Default: --access-aud. Recommended whenever you
#                          expose the web app; with Access on, the web app is always gated
#   --keep-key             rerun without rotating WAYGLASS_ADMIN_KEY (skips login); the Worker must exist
#   --dry-run              generate the config and bundle locally; create and deploy nothing
#   --uninstall            delete this deployment's queue, Worker and the Access apps --access
#                          made (never the namespaces, the Zero Trust org or its login methods)
#   --yes                  do not ask before creating or deleting resources
#
# Environment:
#   CLOUDFLARE_ACCOUNT_ID  required
#   CLOUDFLARE_API_TOKEN   required (not for --dry-run). Account permissions:
#                          Artifacts Edit, Workers Scripts Edit, Queues Edit; with
#                          --domain also Zone > Workers Routes Edit on that zone.
#                          With --access also Account > Access: Apps and Policies Edit and
#                          Access: Organizations, Identity Providers, and Groups Read (Edit
#                          if the org has no login method yet and one-time PIN is added).
#                          Used here only; the deployed Worker never sees it.
#   WAYGLASS_CONFIG_DIR    where `wayglass login` stores the key (default ~/.config/wayglass)
#   CF_EVENTS_API_TOKEN    optional; a token with only Account > Queues > Edit. The Worker
#                          uses it to subscribe each new trunk to push events.

set -euo pipefail

die() { printf 'selfhost: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }

name=wayglass domain="" namespace=wayglass jurisdiction=us eu_namespace=""
queue=artifacts-events public="" ai_review=off ai_gateway="" ai_model="" ai_providers="" ai_budget="" access_team="" access_aud="" access_web_aud="" access=0 allow="" idp="" keep_key=0 dry_run=0 yes=0 uninstall=0

while [ $# -gt 0 ]; do
  case "$1" in
    --name) name=${2:?--name needs a value}; shift 2 ;;
    --domain) domain=${2:?--domain needs a value}; shift 2 ;;
    --namespace) namespace=${2:?--namespace needs a value}; shift 2 ;;
    --jurisdiction) jurisdiction=${2:?--jurisdiction needs a value}; shift 2 ;;
    --eu-namespace) eu_namespace=${2:?--eu-namespace needs a value}; shift 2 ;;
    --queue) queue=${2:?--queue needs a value}; shift 2 ;;
    --public) public=${2:?--public needs a value}; shift 2 ;;
    --ai-review) ai_review=${2:?--ai-review needs a value}; shift 2 ;;
    --ai-gateway) ai_gateway=${2:?--ai-gateway needs a value}; shift 2 ;;
    --ai-model) ai_model=${2:?--ai-model needs a value}; shift 2 ;;
    --ai-providers) ai_providers=${2:?--ai-providers needs a value}; shift 2 ;;
    --ai-budget) ai_budget=${2:?--ai-budget needs a value}; shift 2 ;;
    --access) access=1; shift ;;
    --allow) allow=${allow:+$allow,}${2:?--allow needs a value}; shift 2 ;;
    --idp) idp=${idp:+$idp$'\n'}${2:?--idp needs a value}; shift 2 ;;
    --access-team) access_team=${2:?--access-team needs a value}; shift 2 ;;
    --access-aud) access_aud=${2:?--access-aud needs a value}; shift 2 ;;
    --access-web-aud) access_web_aud=${2:?--access-web-aud needs a value}; shift 2 ;;
    --keep-key) keep_key=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    --uninstall) uninstall=1; shift ;;
    --yes|-y) yes=1; shift ;;
    -h|--help) sed -n '2,/^$/s/^# \{0,1\}//p' "$0"; exit 0 ;;
    *) die "unknown option $1 (see --help)" ;;
  esac
done

if { [ -n "$access_team" ] && [ -z "$access_aud" ]; } || { [ -z "$access_team" ] && [ -n "$access_aud" ]; }; then
  die "--access-team and --access-aud go together (see docs/SELF-HOSTING.md, Cloudflare Access)"
fi
if [ -n "$access_web_aud" ] && [ -z "$access_team" ]; then
  die "--access-web-aud needs --access-team and --access-aud (see docs/SELF-HOSTING.md, Cloudflare Access)"
fi
case "$ai_review" in off|manual|auto) ;; *) die "--ai-review must be off, manual or auto" ;; esac
if [ -n "$ai_gateway$ai_model$ai_providers$ai_budget" ] && [ "$ai_review" = off ]; then
  die "--ai-gateway, --ai-model, --ai-providers and --ai-budget go with --ai-review manual|auto"
fi
case "$ai_providers" in
  *anthropic*|*openai*) [ -n "$ai_gateway" ] || die "--ai-providers anthropic or openai needs --ai-gateway: the gateway holds their keys" ;;
esac
if [ "$access" -eq 1 ] && [ "$uninstall" -eq 0 ]; then
  [ -n "$allow" ] || die "--access needs --allow: who may sign in (you@example.com, @example.com or group:<name>)"
  [ -z "$access_team$access_aud$access_web_aud" ] ||
    die "--access finds the team and AUD tags itself; drop --access-team, --access-aud and --access-web-aud"
elif [ "$uninstall" -eq 0 ] && [ -n "$allow$idp" ]; then
  die "--allow and --idp go with --access"
fi

case "$jurisdiction" in
  us|eu) worker_jurisdiction=$jurisdiction ;;
  unrestricted) worker_jurisdiction="" ;;
  *) die "--jurisdiction must be us, eu or unrestricted" ;;
esac
if [ -n "$eu_namespace" ] && [ "$jurisdiction" = eu ]; then
  die "--eu-namespace is only for a main namespace outside the EU; yours is already eu"
fi

root=$(cd "$(dirname "$0")/.." && pwd)
worker_dir=$root/worker

for tool in node npx curl openssl git; do
  command -v "$tool" >/dev/null || die "$tool is required"
done
: "${CLOUDFLARE_ACCOUNT_ID:?set CLOUDFLARE_ACCOUNT_ID to your Cloudflare account id}"
if [ "$dry_run" -eq 0 ]; then
  : "${CLOUDFLARE_API_TOKEN:?set CLOUDFLARE_API_TOKEN (see --help for the permissions it needs)}"
fi
[ "$uninstall" -eq 1 ] && [ "$dry_run" -eq 1 ] && die "--uninstall and --dry-run do not go together"

tmp=$(mktemp -d)
chmod 700 "$tmp"
trap 'rm -rf "$tmp"' EXIT

api="https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID"
# Same lookup as the CLI (internal/api.ConfigDir), for messages only.
config_dir=${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}

# cf METHOD PATH [JSON]: Cloudflare REST call. Sets cf_status (HTTP code) and
# cf_body. The token goes to curl on stdin (--config -), so it never shows up
# in the process list. Only a failure to reach the API stops the script here;
# callers decide what each status means.
cf_status="" cf_body=""
cf() {
  local method=$1 path=$2 data=${3:-} out
  local args=(-sS -X "$method" "$api$path" -H "Content-Type: application/json" -w '\n%{http_code}')
  [ -n "$data" ] && args+=(--data "$data")
  out=$(printf 'header = "Authorization: Bearer %s"\n' "$CLOUDFLARE_API_TOKEN" | curl --config - "${args[@]}") ||
    die "could not reach the Cloudflare API ($method $path); check your network and retry"
  cf_status=${out##*$'\n'}
  cf_body=${out%$'\n'*}
}

# json EXPR: evaluates a JS expression over the JSON document on stdin (bound to `d`).
json() { node -e 'let s="";process.stdin.on("data",c=>s+=c).on("end",()=>{let d;try{d=JSON.parse(s)}catch{d=null};const v=('"$1"');process.stdout.write(v==null?"":String(v))})'; }

wrangler() { (cd "$worker_dir" && npx wrangler "$@"); }

# cf_errors: the Cloudflare errors in cf_body, for a diagnostic.
cf_errors() { json 'd&&d.errors&&d.errors.length?JSON.stringify(d.errors):"no Cloudflare error envelope"' <<<"$cf_body"; }

# cf_auth_hint: explains a refused token, or prints nothing.
cf_auth_hint() {
  case "$cf_status" in
    401|403) printf ' (the API token was refused; it needs %s)' "$perms" ;;
    *) [ "$(json 'd&&d.errors&&d.errors.some(e=>e.code===10000||e.code===9109)' <<<"$cf_body")" = true ] &&
         printf ' (the API token was refused; it needs %s)' "$perms" ;;
  esac
  return 0
}
perms="Account > Artifacts Edit, Workers Scripts Edit and Queues Edit"

# cf_not_found: true when the last call said the resource does not exist, as
# opposed to any other failure (auth, rate limit, outage), which must stop us.
cf_not_found() {
  [ "$cf_status" = 404 ] && return 0
  [ "$(json 'd&&d.errors&&d.errors.some(e=>/not.?found|does not exist/i.test(e.message||""))' <<<"$cf_body")" = true ]
}

# ensure_namespace NAME JURISDICTION(us|eu|unrestricted)
ensure_namespace() {
  local ns=$1 want=$2 have body
  cf GET "/artifacts/namespaces/$ns"
  if [ "$(json 'd&&d.success' <<<"$cf_body")" = true ]; then
    have=$(json 'd.result&&(d.result.jurisdiction||"unrestricted")' <<<"$cf_body")
    if [ -n "$have" ] && [ "$have" != "$want" ]; then
      die "namespace $ns exists with jurisdiction $have, not $want (a jurisdiction cannot change; pick another name)"
    fi
    say "namespace $ns exists (${have:-jurisdiction not reported})"
    return
  fi
  cf_not_found ||
    die "looking up namespace $ns failed: HTTP $cf_status$(cf_auth_hint): $(cf_errors)"
  body="{\"namespace\":\"$ns\"}"
  [ "$want" != unrestricted ] && body="{\"namespace\":\"$ns\",\"jurisdiction\":\"$want\"}"
  cf POST /artifacts/namespaces "$body"
  [ "$(json 'd&&d.success' <<<"$cf_body")" = true ] ||
    die "creating namespace $ns failed: HTTP $cf_status$(cf_auth_hint): $(cf_errors)"
  say "created namespace $ns ($want)"
}

# queue_lookup: prints the queue's id and returns 0; returns 3 when the queue
# does not exist (an expected state on a fresh account); for any other failure
# (auth, network, an output we cannot read) prints wrangler's output to stderr
# and returns 1. Call it as `qid=$(queue_lookup) || rc=$?`, never bare under
# errexit.
queue_lookup() {
  local out rc=0 id
  out=$(wrangler queues info "$queue" 2>&1) || rc=$?
  if [ "$rc" -eq 0 ]; then
    id=$(sed -n 's/^Queue ID: *//p' <<<"$out" | head -n1)
    if [ -n "$id" ]; then printf '%s' "$id"; return 0; fi
  elif grep -qi 'does not exist' <<<"$out"; then
    return 3
  fi
  printf '%s\n' "$out" >&2
  return 1
}

# ensure_toolchain: installs worker/'s pinned wrangler, so npx never fetches
# another. A node_modules without wrangler (a broken or partial install) is
# installed again rather than trusted.
ensure_toolchain() {
  [ -e "$worker_dir/node_modules/.bin/wrangler" ] && return 0
  step "Installing the Worker's pinned toolchain (npm ci)"
  (cd "$worker_dir" && npm ci --no-audit --no-fund) || die "npm ci failed in worker/ (see above)"
  [ -e "$worker_dir/node_modules/.bin/wrangler" ] || die "npm ci did not install wrangler in worker/node_modules"
}

# write_config: writes worker/wrangler.selfhost.jsonc from the current
# settings and prints its name. The generator validates every name, so it
# also runs once before anything is created: a bad --name or --namespace
# must stop the run before a namespace, queue or Access app carries it.
write_config() {
  (cd "$worker_dir" &&
    WAYGLASS_WORKER_NAME=$name WAYGLASS_DOMAIN=$domain WAYGLASS_NAMESPACE=$namespace \
    WAYGLASS_JURISDICTION=$worker_jurisdiction WAYGLASS_EU_NAMESPACE=$eu_namespace \
    WAYGLASS_QUEUE=$queue WAYGLASS_QUEUE_ID=${qid:-} WAYGLASS_PUBLIC_PROJECTS=$public \
    WAYGLASS_AI_REVIEW=$ai_review WAYGLASS_AI_GATEWAY_ID=$ai_gateway WAYGLASS_AI_REVIEW_MODEL=$ai_model \
    WAYGLASS_AI_PROVIDERS=$ai_providers WAYGLASS_AI_DAILY_REVIEWS=$ai_budget \
    WAYGLASS_ACCESS_TEAM_DOMAIN=${access_aud:+$access_team} WAYGLASS_ACCESS_AUD=$access_aud \
    WAYGLASS_ACCESS_WEB_AUD=$access_web_aud \
    node scripts/selfhost-config.mjs)
}

# confirm PROMPT: asks y/N unless --yes; refuses without a terminal.
confirm() {
  [ "$yes" -eq 1 ] && return 0
  [ -t 0 ] || die "not a terminal; rerun with --yes to $1"
  local answer
  read -r -p "$2 [y/N] " answer
  case "$answer" in y|Y|yes) ;; *) die "stopped; nothing was changed" ;; esac
}

# worker_state: sets wstate to present or absent, checked with the API rather
# than inferred from an earlier command's exit code.
wstate=""
worker_state() {
  cf GET "/workers/services/$name"
  if [ "$cf_status" = 200 ]; then wstate=present; return; fi
  cf_not_found || die "looking up Worker $name failed: HTTP $cf_status$(cf_auth_hint): $(cf_errors)"
  wstate=absent
}

# --- Cloudflare Access (--access) -------------------------------------------
# Wayglass never talks to an identity provider: Access does the login with
# whatever methods the Zero Trust organisation has, and the Worker verifies the
# Access token it receives. The JSON work is in worker/scripts/selfhost-access.mjs.

access_perms="Account > Access: Apps and Policies Edit and Account > Access: Organizations, Identity Providers, and Groups Read"
# access_host: the hostname the Access apps protect: --domain, or the Worker's
# workers.dev host. Access applies hostname-based self-hosted apps (with paths)
# to workers.dev hostnames the same way as to a zone's hostnames.
access_host=""
access_js() { (cd "$worker_dir" && node scripts/selfhost-access.mjs "$@"); }
access_ok() { [ "$(json 'd&&d.success' <<<"$cf_body")" = true ]; }
# access_die WHAT: stops with Cloudflare's error and the Access permissions hint.
access_die() {
  access_not_enabled && access_no_org_die
  perms=$access_perms
  die "$1 failed: HTTP $cf_status$(cf_auth_hint): $(cf_errors)"
}
# access_not_enabled: Cloudflare says Zero Trust (Access) is not enabled on the
# account. It can answer 403 for this, which is not a token permission problem.
access_not_enabled() {
  [ "$(json 'd&&d.errors&&d.errors.some(e=>/not[ _.-]?enabled/i.test(String(e.message||"")))' <<<"$cf_body")" = true ]
}
access_no_org_die() {
  die "account $CLOUDFLARE_ACCOUNT_ID has no Zero Trust organisation yet (Cloudflare: $(cf_errors)), so there is no team domain to sign in at.
The API cannot create one for you. Creating one is a one-time step in the dashboard: open
https://one.dash.cloudflare.com/, choose a team name (it becomes <team>.cloudflareaccess.com) and the
Free plan (Cloudflare may ask for a payment method; Free costs nothing), then rerun this command.
See docs/SELF-HOSTING.md, \"People sign in through Cloudflare Access\"."
}
access_refused() {
  case "$cf_status" in 401|403) return 0 ;; esac
  [ "$(json 'd&&d.errors&&d.errors.some(e=>e.code===10000||e.code===9109)' <<<"$cf_body")" = true ]
}

# access_resolve_host: sets access_host from --domain or the account's
# workers.dev subdomain (GET /workers/subdomain), or stops.
access_resolve_host() {
  local sub
  if [ -n "$domain" ]; then access_host=$domain; return 0; fi
  cf GET /workers/subdomain
  sub=$(json 'd&&d.success&&d.result&&d.result.subdomain||""' <<<"$cf_body")
  if [ -n "$sub" ]; then
    access_host=$(printf '%s.%s.workers.dev' "$name" "$sub" | tr '[:upper:]' '[:lower:]')
    return 0
  fi
  if cf_not_found || access_ok; then
    die "account $CLOUDFLARE_ACCOUNT_ID has no workers.dev subdomain yet, so there is no hostname for Access to protect.
Register one once: in the dashboard open Workers & Pages (it offers <you>.workers.dev on first visit), or run
'npx wrangler subdomain <you>' from worker/. Or pass --domain HOST for a hostname in one of your zones. Then rerun."
  fi
  perms="Account > Workers Scripts Edit"
  die "reading the account's workers.dev subdomain failed: HTTP $cf_status$(cf_auth_hint): $(cf_errors)"
}

# access_read_org: sets access_team to the org's team domain, or stops.
access_read_org() {
  local rc=0
  cf GET /access/organizations
  access_team=$(access_js team <<<"$cf_body") || rc=$?
  [ "$rc" -eq 0 ] && return 0
  [ "$rc" -eq 3 ] || exit 1
  access_not_enabled && access_no_org_die
  if access_refused; then
    die "the API token cannot read the Zero Trust organisation: HTTP $cf_status: $(cf_errors).
Cloudflare answers 403 (code 10000) here when the token lacks Account > Access: Organizations, Identity
Providers, and Groups > Read, even if it can list Access apps. Edit the token at
https://dash.cloudflare.com/profile/api-tokens, add that permission (Edit if the org has no login method
yet, so one-time PIN can be added) next to Access: Apps and Policies > Edit, then rerun."
  fi
  if cf_not_found || access_ok; then access_no_org_die; fi
  access_die "reading the Zero Trust organisation"
}

# access_pick_idps: sets access_idps (JSON) and access_need_pin (1 when the
# one-time PIN login must be created first).
access_idps="" access_need_pin=0
access_pick_idps() {
  local rc=0
  cf GET "/access/identity_providers?per_page=100"
  access_ok || access_die "listing the login methods (identity providers)"
  access_idps=$(WG_IDP=$idp access_js idps <<<"$cf_body") || rc=$?
  case "$rc" in
    0) access_need_pin=0 ;;
    3|4) access_need_pin=1; access_idps='[{"id":"new-one-time-pin","name":"One-time PIN","type":"onetimepin"}]' ;;
    *) exit 1 ;;
  esac
}

access_create_pin() {
  local id
  cf POST /access/identity_providers '{"name":"One-time PIN","type":"onetimepin","config":{}}'
  access_ok || { access_perms="Account > Access: Organizations, Identity Providers, and Groups Edit"; access_die "adding the One-time PIN login"; }
  id=$(access_js field id <<<"$cf_body")
  [ -n "$id" ] || die "adding the One-time PIN login returned no id"
  access_idps="[{\"id\":\"$id\",\"name\":\"One-time PIN\",\"type\":\"onetimepin\"}]"
  access_need_pin=0
  say "added the One-time PIN login (Access emails a code to each allowed address)"
}

# access_find NAME: prints the existing app with exactly this name as JSON, or nothing.
access_find() {
  cf GET "/access/apps?name=${1// /%20}&exact=true&per_page=100"
  access_ok || access_die "looking up the Access application \"$1\""
  access_js find "$1" <<<"$cf_body"
}

access_kinds=(web human api)
access_label() { case "$1" in web) echo "web app, / and /v1/public" ;; human) echo "sign-in, /v1/human" ;; api) echo "key API, /v1, bypass" ;; esac; }
# Per-app state lives in access_<field>_<kind> variables (Bash 3.2 has no
# associative arrays, and macOS ships 3.2). get FIELD KIND / put FIELD KIND VALUE.
get() { local v="access_$1_$2"; printf '%s' "${!v:-}"; }
put() { printf -v "access_$1_$2" '%s' "$3"; }

# access_plan: reads the org, login methods and existing apps, builds the three
# request bodies (validating --allow) and prints what would change. Read-only.
access_plan() {
  local k n i=0 names ex
  access_resolve_host
  access_read_org
  access_pick_idps
  names=$(WG_NAME=$name access_js names)
  while IFS= read -r n; do
    k=${access_kinds[$i]}; i=$((i + 1))
    put name "$k" "$n"
    ex=$(access_find "$n") || exit 1
    put existing "$k" "$ex"
  done <<<"$names"
  access_build
  say "access host    $access_host$([ -z "$domain" ] && echo ' (workers.dev)')"
  say "access team    $access_team"
  say "login methods  $(json 'd.map(p=>p.name+" ["+p.type+"]").join(", ")' <<<"$access_idps")$([ "$access_need_pin" -eq 1 ] && echo ' (none yet: adds One-time PIN)')"
  say "allowed        $allow"
  for k in "${access_kinds[@]}"; do
    say "access app     $([ -n "$(get existing "$k")" ] && echo update || echo create) \"$(get name "$k")\" ($(access_label "$k"))"
  done
}

access_build() {
  local k b
  for k in "${access_kinds[@]}"; do
    b=$(WG_NAME=$name WG_DOMAIN=$access_host WG_IDPS=$access_idps WG_ALLOW=$allow access_js body "$k" <<<"$(get existing "$k")") || exit 1
    put body "$k" "$b"
  done
}

# access_apply: creates the one-time PIN login if needed, then creates or
# updates the three apps, and sets access_aud / access_web_aud.
access_apply() {
  local k id verb aud
  if [ "$access_need_pin" -eq 1 ]; then access_create_pin; access_build; fi
  for k in "${access_kinds[@]}"; do
    if [ -n "$(get existing "$k")" ]; then
      id=$(access_js field id <<<"$(get existing "$k")")
      cf PUT "/access/apps/$id" "$(get body "$k")"; verb=updated
    else
      cf POST /access/apps "$(get body "$k")"; verb=created
    fi
    access_ok || access_die "saving the Access application \"$(get name "$k")\""
    aud=$(access_js field aud <<<"$cf_body")
    [ -n "$aud" ] || die "Access application \"$(get name "$k")\" came back without an AUD tag"
    put aud "$k" "$aud"
    say "$verb Access app \"$(get name "$k")\" ($(access_label "$k"))"
  done
  access_aud=$(get aud human)
  access_web_aud=$(get aud web)
}

# access_verify URL: browser routes must redirect to the team's login, and the
# key API must answer 401 itself (Access lets it through; the Worker wants a key).
access_verify() {
  local base=$1 out code loc human whoami
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    out=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "$base/" </dev/null 2>/dev/null || true)
    code=${out%% *} loc=${out#* }
    human=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "$base/v1/human/login" </dev/null 2>/dev/null || true)
    whoami=$(curl -sS -o /dev/null -w '%{http_code}' "$base/v1/whoami" </dev/null 2>/dev/null || true)
    if [ "$code" = 302 ] && [[ $loc == "https://$access_team/"* ]] &&
       [ "${human%% *}" = 302 ] && [[ ${human#* } == "https://$access_team/"* ]] && [ "$whoami" = 401 ]; then
      say "GET $base/              302 to $access_team (sign-in required)"
      say "GET $base/v1/human/login 302 to $access_team (sign-in required)"
      say "GET $base/v1/whoami      401 without a key (agents reach the Worker, which checks keys)"
      return 0
    fi
    sleep "${WAYGLASS_SELFHOST_POLL:-6}"
  done
  die "deployed, but the Access check failed: / answered ${code:-nothing} ${loc:-}, /v1/human/login ${human:-nothing}, /v1/whoami ${whoami:-nothing}.
Expected 302 to https://$access_team/... for the first two and 401 for the last. New Access apps and a
new custom domain can take a few minutes; rerun with --keep-key to check again. See docs/SELF-HOSTING.md, Troubleshooting."
}

# access_uninstall: deletes the three apps --access made, found by name; never
# the org or its login methods. A token without Access permissions is not an
# error here (the deployment may never have used --access).
access_uninstall() {
  local n hit id
  while IFS= read -r n; do
    cf GET "/access/apps?name=${n// /%20}&exact=true&per_page=100"
    if access_refused; then
      say "could not read Access applications (the token has no Access permission); if this deployment used --access, delete \"$n\" in Zero Trust > Access controls > Applications"
      continue
    fi
    access_ok || access_die "looking up the Access application \"$n\""
    hit=$(access_js find "$n" <<<"$cf_body")
    if [ -z "$hit" ]; then say "Access app \"$n\" does not exist; nothing to delete"; continue; fi
    id=$(access_js field id <<<"$hit")
    cf DELETE "/access/apps/$id"
    access_ok || cf_not_found || access_die "deleting the Access application \"$n\""
    cf GET "/access/apps/$id"
    cf_not_found || die "Access application \"$n\" ($id) still exists after the delete (HTTP $cf_status); check the dashboard"
    say "deleted Access app \"$n\" (verified: the API no longer finds it)"
  done < <(WG_NAME=$name access_js names)
}

if [ "$uninstall" -eq 1 ]; then
  step "Uninstall plan"
  say "account        $CLOUDFLARE_ACCOUNT_ID"
  say "delete queue   $queue (first: a Worker that consumes a queue cannot be deleted)"
  say "delete worker  $name, with its Durable Objects (intents, decisions, audit log)"
  say "delete access  the Access apps \"$name web\", \"$name human sign-in\" and \"$name key API\", if they exist"
  say "keep           the Zero Trust organisation and its login methods"
  say "keep           Artifacts namespaces and their repos (delete them yourself if you want)"
  confirm "delete these resources" "Delete the queue and the Worker?"
  ensure_toolchain

  step "Queue"
  rc=0; qid=$(queue_lookup) || rc=$?
  if [ "$rc" -eq 3 ]; then
    say "queue $queue does not exist; nothing to delete"
  elif [ "$rc" -eq 0 ]; then
    cf DELETE "/queues/$qid"
    case "$cf_status" in
      2??) ;;
      *) cf_not_found || die "deleting queue $queue failed: HTTP $cf_status$(cf_auth_hint): $(cf_errors)" ;;
    esac
    cf GET "/queues/$qid"
    cf_not_found || die "queue $queue ($qid) still exists after the delete (HTTP $cf_status); check the dashboard"
    say "deleted queue $queue (verified: the API no longer finds it)"
  else
    die "could not look up queue $queue (wrangler output above)"
  fi

  step "Worker"
  worker_state
  if [ "$wstate" = absent ]; then
    say "Worker $name does not exist; nothing to delete"
  else
    # REST, not `wrangler delete`: wrangler also lists KV namespaces afterwards,
    # which the documented token cannot do, and fails after the Worker is gone.
    cf DELETE "/workers/scripts/$name?force=true"
    del_status=$cf_status del_hint=$(cf_auth_hint) del_errors=$(cf_errors)
    for _ in 1 2 3; do
      worker_state
      [ "$wstate" = absent ] && break
      sleep "${WAYGLASS_SELFHOST_POLL:-2}"
    done
    [ "$wstate" = absent ] || die "deleting Worker $name failed: HTTP $del_status$del_hint: $del_errors"
    say "deleted Worker $name (verified: the API no longer finds it)"
  fi

  step "Cloudflare Access"
  access_uninstall

  step "Kept"
  say "Artifacts namespaces were not touched; list or delete them in the dashboard."
  say "The login in $config_dir/credentials.yaml points at the deleted Worker; remove it when you are done."
  exit 0
fi

step "Plan"
# Names are checked before any resource exists with them (see write_config).
qid=${WAYGLASS_QUEUE_ID:-}
write_config >/dev/null || die "fix the options above; nothing was created"
say "account        $CLOUDFLARE_ACCOUNT_ID"
say "worker         $name on ${domain:-workers.dev}"
say "namespaces     $namespace ($jurisdiction)${eu_namespace:+, $eu_namespace (eu)}"
say "queue          $queue"
say "public view    ${public:-off}"
if [ "$access" -eq 0 ]; then say "access sign-in ${access_team:-off}"; fi
say "admin key      $([ "$keep_key" -eq 1 ] && echo 'kept from the last deploy' || echo 'new, generated here')"
say "events token   $([ -n "${CF_EVENTS_API_TOKEN:-}" ] && echo 'set (trunks get push subscriptions)' || echo 'not set (pushes that skip land go unflagged)')"
if [ "$access" -eq 1 ]; then
  if [ -n "${CLOUDFLARE_API_TOKEN:-}" ]; then
    access_plan
  else
    say "access sign-in on for ${domain:-$name.<subdomain>.workers.dev}, allowed: $allow (set CLOUDFLARE_API_TOKEN to read your Zero Trust org into the plan)"
  fi
fi
if [ "$dry_run" -eq 1 ]; then
  say "mode           dry run: nothing is created or deployed"
else
  if [ "$keep_key" -eq 1 ]; then
    # --keep-key deploys without WAYGLASS_ADMIN_KEY; a Worker that was never
    # deployed would come up with no admin key and nothing could log in.
    worker_state
    [ "$wstate" = present ] ||
      die "--keep-key keeps the admin key of an existing Worker, but no Worker named $name exists in this account; rerun without --keep-key"
  fi
  confirm "create these resources" "Create these in your account and deploy?"
fi

ensure_toolchain

if [ "$dry_run" -eq 0 ]; then
  step "Artifacts namespaces"
  ensure_namespace "$namespace" "$jurisdiction"
  [ -n "$eu_namespace" ] && ensure_namespace "$eu_namespace" eu

  step "Queue"
  rc=0; qid=$(queue_lookup) || rc=$?
  if [ "$rc" -eq 3 ]; then
    out=$(wrangler queues create "$queue" 2>&1) || { printf '%s\n' "$out" >&2; die "creating queue $queue failed (wrangler output above)"; }
    say "created queue $queue"
    rc=0; qid=$(queue_lookup) || rc=$?
  fi
  [ "$rc" -eq 0 ] || die "could not look up queue $queue (wrangler output above; check the token has Queues Edit)"
  say "queue $queue: $qid"

  if [ "$access" -eq 1 ]; then
    step "Cloudflare Access"
    access_apply
  fi
fi

step "Config"
config=$(write_config) || die "could not write worker/wrangler.selfhost.jsonc (see above)"
say "wrote worker/$config"
[ "$access" -eq 1 ] && [ "$dry_run" -eq 1 ] &&
  say "(dry run: the Access AUD tags are only known once the apps exist, so this config has Access off)"

if [ "$dry_run" -eq 1 ]; then
  step "Dry run (bundle only)"
  wrangler deploy --config "$config" --dry-run --outdir "$tmp/bundle"
  say ""
  say "Dry run done. Rerun without --dry-run to create the resources and deploy."
  exit 0
fi

step "Deploy"
secrets=$tmp/secrets.json
key=""
if [ "$keep_key" -eq 0 ]; then
  key=$(openssl rand -hex 32) || die "openssl could not generate the admin key"
fi
# Secrets go through the environment of one node process into a 0600 file.
WG_ADMIN_KEY=$key WG_ACCOUNT=$CLOUDFLARE_ACCOUNT_ID WG_EVENTS=${CF_EVENTS_API_TOKEN:-} WG_OUT=$secrets node -e '
  const s = { CF_ACCOUNT_ID: process.env.WG_ACCOUNT };
  if (process.env.WG_ADMIN_KEY) s.WAYGLASS_ADMIN_KEY = process.env.WG_ADMIN_KEY;
  if (process.env.WG_EVENTS) s.CF_EVENTS_API_TOKEN = process.env.WG_EVENTS;
  require("fs").writeFileSync(process.env.WG_OUT, JSON.stringify(s), { mode: 0o600 });
' || die "could not write the secrets file for wrangler"
wrangler deploy --config "$config" --secrets-file "$secrets" 2>&1 | tee "$tmp/deploy.log" ||
  die "wrangler deploy failed (output above); nothing was logged in"
rm -f "$secrets"

if [ -n "$domain" ]; then
  url=https://$domain
else
  url=$(grep -Eo 'https://[A-Za-z0-9.-]+\.workers\.dev' "$tmp/deploy.log" | head -n1 || true)
  [ -n "$url" ] || die "deployed, but could not find the workers.dev URL in wrangler's output; run 'wayglass login --worker <url>' yourself"
fi

step "Login"
wg=""
if [ -x "$root/bin/wayglass" ]; then
  wg=$root/bin/wayglass
elif command -v wayglass >/dev/null; then
  wg=$(command -v wayglass)
elif command -v go >/dev/null; then
  (cd "$root" && go build -o bin/wayglass ./cmd/wayglass)
  wg=$root/bin/wayglass
fi

if [ "$keep_key" -eq 1 ]; then
  say "Admin key unchanged; your existing login still works."
elif [ -z "$wg" ]; then
  say "No wayglass binary and no Go to build one. The admin key was set on the Worker but"
  say "this machine is not logged in; build the CLI and rerun with a new key, or set it by hand."
  exit 1
else
  # A new secret takes a few seconds to reach every request.
  logged_in=0
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if printf '%s\n' "$key" | "$wg" login --worker "$url" --key-stdin >/dev/null 2>"$tmp/login.err"; then
      logged_in=1
      break
    fi
    sleep 3
  done
  [ "$logged_in" -eq 1 ] || die "deployed to $url, but login kept failing: $(cat "$tmp/login.err")"
  say "Logged in to $url (key stored in $config_dir/credentials.yaml, mode 0600)."
fi
key=""

if [ -n "$wg" ]; then
  step "Doctor"
  "$wg" doctor || true
fi

if [ "$access" -eq 1 ]; then
  step "Access check"
  [ "${url#https://}" = "$access_host" ] ||
    die "deployed to $url, but the Access apps protect $access_host; rerun with --domain ${url#https://} or check the account's workers.dev subdomain"
  access_verify "$url"
fi

step "Next"
say "In any git repository:  wayglass init"
say "Give an agent its own key:  wayglass keys create --agent <name>"
[ "$access" -eq 1 ] &&
  say "People sign in (browser, through Access):  wayglass login --human --worker $url   or open $url/console/"
[ -z "${CF_EVENTS_API_TOKEN:-}" ] &&
  say "Optional: rerun with CF_EVENTS_API_TOKEN set (Queues Edit only) and --keep-key to flag trunk pushes that skip land."
exit 0
