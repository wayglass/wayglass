#!/usr/bin/env bash
# Golden-path harness (AGENTS.md section 2) against the deployed Worker and
# real Artifacts. Nothing here is mocked.
#
# In a temp dir it:
#   1. runs `wayglass init` for a fresh US project demo-<unix time>
#   2. runs three agents in parallel, each in its own clone:
#        demo-docs  docs change          -> expected auto_merge (landed here)
#        demo-auth  auth change          -> expected human_approve (left for a person)
#        demo-pii   EU personal data     -> expected quarantine (left for a person)
#      demo-docs and demo-auth declare overlapping paths on purpose.
#   3. lands the auto_merge change, asks `why` about it, checks watch, the
#      suggested re-plan and the review queue, shares the auth agent's
#      session and forks it as a second agent, and probes the public view
#   4. runs proofs of concept: notes after a fork, `before` on new refs,
#      shallow and blobless clones, all with Worker-minted fork tokens
# and prints a PASS/FAIL table, a labelled report of the run (MEASURED:
# timed or counted in this run; SCRIPTED: fixed by this script), and a JSON
# summary with both (`.measured`).
#
# It never approves or rejects anything: changes routed to a person are
# the person's demo moment. Each agent acts with its own agent key, minted
# here with the admin key and revoked on exit; one agent tries to approve the
# auth change and must be refused (needs_human, exit 4). The admin key is
# used only for the person's steps (init, review) and audit reads.
# Tokens and keys stay in shell variables and reach git or wayglass only via
# environment variables; saved JSON has token values removed.
#
# Cost per run: 1 US trunk + 3 US forks (docs, auth, probe) + 1 EU fork
# (or 1 EU trunk + 1 fork with DEMO_EU_PROJECT=new). The run prints the
# exact repos and tokens it created.
#
# Usage:
#   scripts/demo/golden-path.sh
# Environment (all optional):
#   WAYGLASS_KEY       the admin key: the person's steps, audit reads, minting and revoking the
#                      agents' keys (default: from ~/.config/wayglass/credentials.yaml)
#   WAYGLASS_URL       Worker URL (default: from the same file)
#   DEMO_PROJECT_PREFIX  project name prefix (default demo; demo-test for a throwaway run)
#   DEMO_EU_PROJECT    existing EU project to add the PII intent to (default eu-demo-1791797572),
#                      or "new" to create one, or "skip"
#   DEMO_POCS          1 (default) to run the proofs of concept, 0 to skip them
#   DEMO_EVENT_WAIT    seconds to wait for trunk push events (default 120)
#   DEMO_WORKER_REV    Worker revision for the report (default: newest main commit whose
#                      Workers Builds check succeeded, via gh; else "unknown")
#   DEMO_SUMMARY       where to write the JSON summary (default <workdir>/summary.json)
#   DEMO_KEEP          1 to keep the temp dir (default 1; it holds no secrets)
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
ts=$(date +%s)
prefix=${DEMO_PROJECT_PREFIX:-demo}
[[ "$prefix" == demo* ]] || { echo "DEMO_PROJECT_PREFIX must start with demo (the public view relies on it)" >&2; exit 2; }
project="${prefix}-${ts}"
eu_project=${DEMO_EU_PROJECT:-eu-demo-1791797572}
pocs=${DEMO_POCS:-1}
event_wait=${DEMO_EVENT_WAIT:-120}
work=$(mktemp -d "${TMPDIR:-/tmp}/wayglass-demo-${ts}.XXXX")
summary=${DEMO_SUMMARY:-$work/summary.json}
creds="${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}/credentials.yaml"

for t in git go jq curl; do
  command -v "$t" >/dev/null || { echo "$t is required" >&2; exit 1; }
done
if [[ -z "${WAYGLASS_KEY:-}" ]]; then
  WAYGLASS_KEY=$(sed -n 's/^key: *//p' "$creds" 2>/dev/null || true)
fi
if [[ -z "${WAYGLASS_URL:-}" ]]; then
  WAYGLASS_URL=$(sed -n 's/^url: *//p' "$creds" 2>/dev/null || true)
fi
: "${WAYGLASS_KEY:?not logged in: run wayglass login or set WAYGLASS_KEY}"
: "${WAYGLASS_URL:?not logged in: run wayglass login or set WAYGLASS_URL}"
export WAYGLASS_URL WAYGLASS_KEY
# Keep a stray intent or project from the caller's shell out of the agents.
unset WAYGLASS_INTENT WAYGLASS_PROJECT WAYGLASS_FORK_TOKEN GIT_CONFIG_COUNT

wg="$work/bin/wayglass"
(cd "$repo_root" && go build -o "$wg" ./cmd/wayglass)

# One agent key per agent, so the Worker binds each name and the gate on
# person-routed changes applies to them. Revoked when the script exits.
# shellcheck source=agent-keys.sh
source "$repo_root/scripts/demo/agent-keys.sh"
export WAYGLASS_BIN=$wg
ak_admin_load
trap ak_revoke_all EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
declare -A agent_keys
for a in demo-docs demo-auth demo-pii demo-second demo-probe; do
  ak_mint k "$a" "golden-path $project"
  agent_keys[$a]=$k
done
k=

# Wall-clock milliseconds, for the MEASURED rows of the run report.
now_ms() { local t=${EPOCHREALTIME/[.,]/}; echo $(( ${t:-$(date +%s)000000} / 1000 )); }
run_t0=$(now_ms)

steps="$work/steps.tsv"
: >"$steps"
# step <id> <PASS|FAIL|PENDING|SKIP> <detail>
step() { printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$steps"; printf '  %-7s %-26s %s\n' "$2" "$1" "$3"; }
log() { printf '\n== %s\n' "$*"; }

# Worker GET with the key on stdin, never in argv.
wget_api() { printf 'Authorization: Bearer %s\n' "$WAYGLASS_KEY" | curl -sS -H @- "$WAYGLASS_URL$1"; }

# git with a bearer token, passed the same way gitx does (env, not argv or disk).
git_tok() {
  local tok=$1; shift
  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: Bearer $tok" \
    GIT_TERMINAL_PROMPT=0 git "$@"
}

# Strips every token value before JSON reaches disk.
redact() { jq 'del(.data.token.value) | walk(if type == "string" then gsub("art_v[0-9]+_[a-z_]*[0-9a-f]{40}[^\" ]*"; "art_REDACTED") else . end)'; }

git_identity() {
  git -C "$1" config user.name "$2"
  git -C "$1" config user.email "$2@agents.wayglass.invalid"
  git -C "$1" config commit.gpgsign false
}

# The Worker revision for the report, read before anything runs: the newest
# main commit whose Workers Builds deploy succeeded. A build still running
# means the Worker may change mid-run, so the report says so.
worker_rev=${DEMO_WORKER_REV:-}
if [[ -z "$worker_rev" ]] && command -v gh >/dev/null; then
  for sha in $(gh api 'repos/wayglass/wayglass/commits?per_page=20' --jq '.[].sha' 2>/dev/null); do
    c=$(gh api "repos/wayglass/wayglass/commits/$sha/check-runs" --jq '[.check_runs[] | select(.name | startswith("Workers Builds")) | .status + ":" + (.conclusion // "")] | first // ""' 2>/dev/null || true)
    case "$c" in
      completed:success) worker_rev=${worker_rev:-}${sha:0:7}; break ;;
      queued:* | in_progress:*) worker_rev="(deploy of ${sha:0:7} in progress) " ;;
    esac
  done
fi

echo "Wayglass golden path against $WAYGLASS_URL"
echo "Project $project, EU project $eu_project, workdir $work"
echo "$(git version), $("$wg" version 2>/dev/null | head -1)"

# ---------------------------------------------------------------- 1. init
log "1. wayglass init (us)"
seed="$work/seed"
mkdir -p "$seed/docs" "$seed/src/auth"
git init -q -b main "$seed"
git_identity "$seed" demo-owner
cat >"$seed/README.md" <<'EOF'
# Acme notes app

A tiny app the demo agents work on.
EOF
cat >"$seed/docs/getting-started.md" <<'EOF'
# Getting started

Install the app and sign in.
EOF
cat >"$seed/src/auth/login.ts" <<'EOF'
export function login(user: string, password: string): boolean {
  return user.length > 0 && password.length > 0;
}
EOF
git -C "$seed" add -A
git -C "$seed" commit -q -m "feat: seed the demo app

A small app so the demo agents have real files to change."
init_rc=0
(cd "$seed" && WAYGLASS_AGENT=demo-owner "$wg" init --project "$project" --jurisdiction us --public --json) >"$work/init.json.raw" 2>"$work/init.err" || init_rc=$?
redact <"$work/init.json.raw" >"$work/init.json"; rm -f "$work/init.json.raw"
if [[ $init_rc -eq 0 ]] && jq -e '.ok and .data.pushed == "main"' "$work/init.json" >/dev/null; then
  step "1 init" PASS "trunk $(jq -r .data.trunk.name "$work/init.json"), events subscribed: $(jq -r '.data.trunk_events.subscribed' "$work/init.json")"
else
  step "1 init" FAIL "exit $init_rc: $(jq -r '.error.message // empty' "$work/init.json" 2>/dev/null)"
  echo "init failed; nothing else can run" >&2
  cat "$work/init.err" >&2
  exit 1
fi

# ---------------------------------------------------------------- 2. agents
# agent <name> <dir> <goal> <reason> <edit-fn> <paths...>
# Declares, edits, commits, pushes, proposes. Writes <dir>.declare/push/propose.json
# and <dir>.rc (exit codes) next to the clone.
agent() {
  local name=$1 dir=$2 goal=$3 reason=$4 edit=$5; shift 5
  local args=() p out id rc=0
  for p in "$@"; do args+=(--path "$p"); done
  export WAYGLASS_AGENT=$name WAYGLASS_KEY=${agent_keys[$name]}
  cd "$dir"
  out=$("$wg" intent declare --json --goal "$goal" --reason "$reason" "${args[@]}") || rc=$?
  printf '%s' "$out" | redact >"$dir.declare.json"
  [[ $rc -eq 0 ]] || { echo "declare=$rc" >"$dir.rc"; return 1; }
  id=$(printf '%s' "$out" | jq -r .data.intent.id)
  # A clone without the trunk's history (the EU agent) starts from the fork.
  if [[ -n "${AGENT_FETCH_FORK:-}" ]]; then
    local tok remote
    tok=$(printf '%s' "$out" | jq -r .data.token.value)
    remote=$(printf '%s' "$out" | jq -r .data.fork.remote)
    git_tok "$tok" fetch -q "$remote" "+refs/heads/main:refs/remotes/fork/main"
    git reset -q --hard refs/remotes/fork/main
    git branch -q --set-upstream-to=fork/main main 2>/dev/null || true
    tok=
  fi
  "$edit"
  git add -A
  git commit -q -F - <<EOF
$goal

$reason

Wayglass-Intent: $id
EOF
  local t0 t1
  t0=$(now_ms); rc=0; "$wg" push --json >"$dir.push.json" 2>"$dir.push.err" || rc=$?; t1=$(now_ms)
  echo "push_ms=$((t1 - t0))" >"$dir.timing"
  echo "push=$rc" >"$dir.rc"
  [[ $rc -eq 0 ]] || return 1
  # Give the other agents' pushes a moment so overlaps on actual paths show.
  sleep 2
  t0=$(now_ms); rc=0; "$wg" propose --json >"$dir.propose.json" 2>"$dir.propose.err" || rc=$?; t1=$(now_ms)
  echo "propose_ms=$((t1 - t0))" >>"$dir.timing"
  echo "propose=$rc" >>"$dir.rc"
  return 0
}

edit_docs() {
  cat >>docs/getting-started.md <<'EOF'

## Sign in

Open the app and sign in with your email address. If you forget your
password, use "Reset password" on the sign-in screen.
EOF
}

edit_auth() {
  cat >src/auth/login.ts <<'EOF'
const MAX_ATTEMPTS = 5;
const attempts = new Map<string, number>();

export function login(user: string, password: string): boolean {
  const n = attempts.get(user) ?? 0;
  if (n >= MAX_ATTEMPTS) return false; // locked until a reset
  const ok = user.length > 0 && password.length >= 12;
  attempts.set(user, ok ? 0 : n + 1);
  return ok;
}
EOF
  mkdir -p docs
  cat >docs/auth.md <<'EOF'
# Sign-in limits

After five failed sign-ins an account is locked until its password is reset.
EOF
}

edit_pii() {
  mkdir -p src/profile
  cat >src/profile/customer.ts <<'EOF'
export interface Customer {
  id: string;
  email: string;
  date_of_birth: string; // ISO date, used for age checks
}

export function isAdult(c: Customer, today = new Date()): boolean {
  const dob = new Date(c.date_of_birth);
  const age = today.getFullYear() - dob.getFullYear();
  return age >= 18;
}
EOF
}

log "2. three agents in parallel (docs, auth on $project; pii on the EU project)"
for a in docs auth; do
  git clone -q "$seed" "$work/$a"
  git_identity "$work/$a" "demo-$a"
done

eu_dir=""
if [[ "$eu_project" == "new" ]]; then
  eu_project="${prefix}-eu-${ts}"
  eu_dir="$work/pii"
  git clone -q "$seed" "$eu_dir"
  git_identity "$eu_dir" demo-pii
  rm -f "$eu_dir/.wayglass/config.yaml" "$eu_dir/.wayglass/policy.yaml" "$eu_dir/.wayglass/.gitignore"
  git -C "$eu_dir" rm -q -r --cached .wayglass >/dev/null 2>&1 || true
  git -C "$eu_dir" commit -q -m "chore: start the EU demo from the seed" || true
  (cd "$eu_dir" && WAYGLASS_AGENT=demo-owner "$wg" init --project "$eu_project" --jurisdiction eu --public --json | redact >"$work/init-eu.json")
  eu_fetch=""
elif [[ "$eu_project" != "skip" ]]; then
  # Join an existing project with nothing but the Worker: the fork token from
  # `intent declare` is the only credential this clone ever sees.
  eu_dir="$work/pii"
  git init -q -b main "$eu_dir"
  git_identity "$eu_dir" demo-pii
  eu_fetch=1
fi

pids=()
(agent demo-docs "$work/docs" "docs: explain signing in" \
  "New users ask how to sign in; the getting-started page stops at install" \
  edit_docs "docs/**") >"$work/docs.log" 2>&1 &
pids+=($!)
(agent demo-auth "$work/auth" "feat: lock accounts after five failed sign-ins" \
  "Brute-force attempts on login; a lockout limits them" \
  edit_auth "src/auth/**" "docs/auth.md") >"$work/auth.log" 2>&1 &
pids+=($!)
if [[ -n "$eu_dir" ]]; then
  (AGENT_FETCH_FORK=$eu_fetch WAYGLASS_PROJECT=$([[ -n "$eu_fetch" ]] && echo "$eu_project") \
    agent demo-pii "$eu_dir" "feat: age check from the customer's date of birth" \
    "Age-restricted content needs an age check; this stores date of birth" \
    edit_pii "src/profile/**") >"$work/pii.log" 2>&1 &
  pids+=($!)
fi
agents_rc=0
for p in "${pids[@]}"; do wait "$p" || agents_rc=1; done

for a in docs auth pii; do
  [[ -f "$work/$a.rc" ]] || continue
  id=$(jq -r '.data.intent.id // "none"' "$work/$a.declare.json" 2>/dev/null)
  if jq -e '.ok' "$work/$a.push.json" >/dev/null 2>&1; then
    step "2 declare+push $a" PASS "intent $id, $(jq -r '.data.files | length' "$work/$a.push.json") file(s) pushed to $(jq -r '.data.fork.name' "$work/$a.declare.json")"
  else
    step "2 declare+push $a" FAIL "$(tr '\n' ' ' <"$work/$a.rc") $(cat "$work/$a.push.err" 2>/dev/null | head -3 | tr '\n' ' ')"
  fi
done
[[ $agents_rc -eq 0 ]] || echo "(an agent failed; see $work/*.log)"

# ---------------------------------------------------------------- 3. overlap
log "3. overlap between demo-docs and demo-auth"
docs_id=$(jq -r '.data.intent.id // ""' "$work/docs.declare.json" 2>/dev/null || true)
auth_id=$(jq -r '.data.intent.id // ""' "$work/auth.declare.json" 2>/dev/null || true)
pii_id=$(jq -r '.data.intent.id // ""' "$work/pii.declare.json" 2>/dev/null || true)
ov=$(cd "$work/docs" && WAYGLASS_AGENT=demo-docs WAYGLASS_KEY=${agent_keys[demo-docs]} "$wg" overlaps --json 2>/dev/null || true)
pair=$(printf '%s' "$ov" | jq -c --arg a "$docs_id" --arg b "$auth_id" \
  '[.data.overlaps[]? | select((.a.intent_id==$a and .b.intent_id==$b) or (.a.intent_id==$b and .b.intent_id==$a)) | .paths[] | "\(.mine) ~ \(.theirs)"]' 2>/dev/null || echo '[]')
seen_live=$({ cat "$work/docs.declare.json" "$work/auth.declare.json" "$work/docs.push.json" "$work/auth.push.json" 2>/dev/null || true; } |
  jq -s --arg a "$docs_id" --arg b "$auth_id" '[.[] | .data.overlaps[]? | select(.intent_id==$a or .intent_id==$b)] | length')
if [[ "$pair" != "[]" && "$seen_live" -gt 0 ]]; then
  step "3 overlap detected" PASS "$(printf '%s' "$pair" | jq -r 'join(", ")'); reported in declare/push --json ($seen_live)"
else
  step "3 overlap detected" FAIL "pair=$pair, in agent output=$seen_live"
fi
watch_out=$(cd "$work/docs" && "$wg" watch --json 2>/dev/null) && watch_rc=0 || watch_rc=$?
if [[ $watch_rc -eq 0 ]] && printf '%s' "$watch_out" | jq -e '.ok' >/dev/null 2>&1; then
  step "3 watch (live)" PASS "watch --json returned the live swarm"
else
  step "3 watch (live)" PENDING "$(printf '%s' "$watch_out" | jq -r '.error.code // "error"' 2>/dev/null): another stream wires watch to the Worker"
fi
# The re-plan is a client-side suggestion (intent.SuggestReplan), labelled as one.
rp=$(printf '%s' "$watch_out" | jq -r '[.data.overlaps[]? | select(.replan.suggested) | "\(.replan.action): \(.replan.summary)"] | first // empty' 2>/dev/null)
if [[ -n "$rp" ]]; then
  step "3 overlap re-plan" PASS "suggested $rp"
else
  step "3 overlap re-plan" FAIL "watch --json has no overlap with a suggested replan"
fi

# ---------------------------------------------------------------- 4. decisions
log "4. propose: policy routes each change"
check_decision() { # <agent> <want action> <want status> <want exit>
  local f="$work/$1.propose.json" act st ex rule
  if [[ ! -f "$f" ]]; then step "4 propose $1" FAIL "no propose output"; return; fi
  act=$(jq -r '.data.decision.action // "?"' "$f"); st=$(jq -r '.data.decision.status // "?"' "$f")
  ex=$(jq -r '.data.exit // "?"' "$f"); rule=$(jq -r '.data.decision.matched_rule // .data.preview.rule // "?"' "$f")
  local tags; tags=$(jq -r '.data.preview.tags | "area=\(.areas // [] | join("+") | if . == "" then "none" else . end) data=\(.data_classes // [] | join("+") | if . == "" then "none" else . end) risk=\(.risk) jur=\(.jurisdiction)"' "$f")
  if [[ "$act" == "$2" && "$st" == "$3" && "$ex" == "$4" ]]; then
    step "4 propose $1" PASS "$act/$st exit $ex, $rule; $tags"
  else
    step "4 propose $1" FAIL "got $act/$st exit $ex (want $2/$3 exit $4), $rule; $tags"
  fi
}
check_decision docs auto_merge approved 0
check_decision auth human_approve pending_human 4
[[ -n "$eu_dir" ]] && check_decision pii quarantine quarantined 4

# ---------------------------------------------------------------- 5. review
log "5. review (a person decides; this harness never approves)"
rv=$(cd "$work/auth" && WAYGLASS_AGENT=demo-owner "$wg" review --json 2>/dev/null) && rv_rc=0 || rv_rc=$?
if [[ $rv_rc -eq 0 ]] && printf '%s' "$rv" | jq -e --arg id "$auth_id" '[.. | strings | select(. == $id)] | length > 0' >/dev/null 2>&1; then
  step "5 review queue" PASS "review --json lists $auth_id for a person"
else
  step "5 review queue" PENDING "$(printf '%s' "$rv" | jq -r '.error.code // "error"' 2>/dev/null): approve with: wayglass approve $auth_id --reason ..."
fi
# The gate: another agent's key tries to approve the auth change. The Worker
# must refuse it (needs_human, exit 4) and record nothing.
gate_rc=0
gate=$(cd "$work/auth" && WAYGLASS_AGENT=demo-docs WAYGLASS_KEY=${agent_keys[demo-docs]} "$wg" approve "$auth_id" --json \
  --reason "An agent key tries to decide a change routed to a person; the Worker must refuse it." 2>/dev/null) || gate_rc=$?
gate_code=$(printf '%s' "$gate" | jq -r '.error.code // "approved (not refused!)"' 2>/dev/null || echo "no answer")
if [[ $gate_rc -eq 4 && "$gate_code" == needs_human ]]; then
  step "5 agent key refused" PASS "demo-docs (agent key) approve $auth_id -> needs_human, exit 4"
else
  step "5 agent key refused" FAIL "demo-docs (agent key) approve $auth_id -> $gate_code, exit $gate_rc (want needs_human, exit 4)"
fi

# ---------------------------------------------------------------- land + 6. why
log "Land the auto_merge change, then 6. why"
land_rc=0; land_t0=$(now_ms)
(cd "$work/docs" && WAYGLASS_AGENT=demo-docs WAYGLASS_KEY=${agent_keys[demo-docs]} "$wg" land --json --no-mirror) >"$work/docs.land.json" 2>"$work/docs.land.err" || land_rc=$?
land_ms=$(( $(now_ms) - land_t0 ))
if [[ $land_rc -eq 0 ]] && jq -e '.data.verified and .data.note_written' "$work/docs.land.json" >/dev/null; then
  step "4 land (auto_merge)" PASS "trunk at $(jq -r '.data.commit[:12]' "$work/docs.land.json"), verified, note pushed"
else
  step "4 land (auto_merge)" FAIL "exit $land_rc: $(jq -r '.error.message // empty' "$work/docs.land.json" 2>/dev/null | head -c 200)"
fi
why_line=$(grep -n "Open the app" "$work/docs/docs/getting-started.md" | cut -d: -f1)
why=$(cd "$work/docs" && "$wg" why "docs/getting-started.md:$why_line" --json 2>/dev/null) && why_rc=0 || why_rc=$?
printf '%s' "$why" >"$work/docs.why.json"
if [[ $why_rc -eq 0 ]] && printf '%s' "$why" | jq -e --arg id "$docs_id" '.data.note.intent_id == $id' >/dev/null 2>&1; then
  step "6 why <file>:<line>" PASS "docs/getting-started.md:$why_line -> $(printf '%s' "$why" | jq -r '"\(.data.note.intent_id) \(.data.note.action) by \(.data.note.decided_by) (source \(.data.source))"')"
else
  step "6 why <file>:<line>" FAIL "exit $why_rc: $(printf '%s' "$why" | jq -r '.error.message // empty' 2>/dev/null)"
fi

# ---------------------------------------------------------------- 7, 8
# The auth agent shares its session; a second agent forks it into its own intent.
sh=$(cd "$work/auth" && WAYGLASS_AGENT=demo-auth WAYGLASS_KEY=${agent_keys[demo-auth]} "$wg" session share --note "Lockout pushed; waiting for a person to approve" --json 2>/dev/null) && sh_rc=0 || sh_rc=$?
printf '%s' "$sh" >"$work/auth.share.json"
session_url=$(printf '%s' "$sh" | jq -r '.data.url // empty' 2>/dev/null)
if [[ $sh_rc -ne 0 || -z "$session_url" ]]; then
  step "7 session fork" FAIL "share exit $sh_rc: $(printf '%s' "$sh" | jq -r '.error.message // empty' 2>/dev/null | head -c 200)"
else
  sf=$(cd "$work" && WAYGLASS_AGENT=demo-second WAYGLASS_KEY=${agent_keys[demo-second]} "$wg" session fork "$session_url" --dir "$work/auth-fork" --json 2>/dev/null) && sf_rc=0 || sf_rc=$?
  printf '%s' "$sf" >"$work/auth.fork.json"
  if [[ $sf_rc -eq 0 ]] && printf '%s' "$sf" | jq -e --arg id "$auth_id" '.data.intent.parent_intent_id == $id and .data.intent.parent_id == $id' >/dev/null 2>&1; then
    # Stacked on the source like `declare --on`: its diff is only its own work, and it lands after $auth_id.
    step "7 session fork" PASS "$(printf '%s' "$sf" | jq -r '.data.intent.id') continues $auth_id in its own fork, stacked on it"
  else
    step "7 session fork" FAIL "fork exit $sf_rc: $(printf '%s' "$sf" | jq -r '.error.message // empty' 2>/dev/null | head -c 200)"
  fi
fi
site_code=$(curl -sS -o /dev/null -w '%{http_code}' "$WAYGLASS_URL/" || echo 000)
pub_code=$(curl -sS -o /dev/null -w '%{http_code}' "$WAYGLASS_URL/v1/public/projects/$project/audit" || echo 000)
# The key reaches curl on stdin (-H @-), never argv.
key_code=$(printf 'authorization: Bearer %s\n' "$WAYGLASS_KEY" | curl -sS -o /dev/null -w '%{http_code}' -H @- "$WAYGLASS_URL/v1/audit?project=$project" || echo 000)
if [[ "$pub_code" == "200" ]]; then
  step "8 website audit view" PASS "public audit route answers 200"
elif [[ "$site_code" =~ ^(302|403)$ && "$key_code" == "200" ]]; then
  # The web app is behind Cloudflare Access: a signed-in person sees the trail at /?project=, read with their session key.
  step "8 website audit view" PASS "web app behind Cloudflare Access (GET / -> $site_code); the audit trail it reads answers 200 with a key; open $WAYGLASS_URL/?project=$project signed in"
else
  step "8 website audit view" PENDING "GET / -> $site_code, /v1/public/projects/$project/audit -> $pub_code, /v1/audit with key -> $key_code; for an anonymous view, init ran with --public and the Worker's PUBLIC_PROJECTS must match $project"
fi

# ---------------------------------------------------------------- audit + events
# Push events are optional (SELF-HOSTING.md "Push events (optional)"). The
# Worker says so at init: trunk_events.reason is "not_configured" when it has
# no CF_EVENTS_API_TOKEN (or account id / queue id). Only then is the push-event
# POC skipped; an API error or a missing field still runs it and can fail.
events_off=$(jq -r 'if .data.trunk_events.subscribed == false and .data.trunk_events.reason == "not_configured" then .data.trunk_events.detail // "not_configured" else "" end' "$work/init.json" 2>/dev/null || true)
if [[ -n "$events_off" ]]; then
  log "Audit trail (push events not configured: $events_off)"
  event_wait=0
else
  log "Audit trail and trunk push events (waiting up to ${event_wait}s)"
fi
deadline=$(( $(date +%s) + event_wait ))
while :; do
  wget_api "/v1/audit?project=$project&limit=500" >"$work/audit.json"
  n_ev=$(jq '[.data.events[]? | select(.kind=="artifacts.repo.pushed")] | length' "$work/audit.json")
  # init pushes main (new ref); land pushes main and refs/notes/wayglass (new ref).
  [[ "$n_ev" -ge 3 || $(date +%s) -ge $deadline ]] && break
  sleep 10
done
kinds=$(jq -r '[.data.events[]?.kind] | group_by(.) | map("\(.[0])=\(length)") | join(" ")' "$work/audit.json")
if jq -e '[.data.events[]? | select(.kind=="decision.proposed")] | length >= 2' "$work/audit.json" >/dev/null; then
  step "8 audit log (API)" PASS "$(jq '.data.events | length' "$work/audit.json") events: $kinds"
else
  step "8 audit log (API)" FAIL "$kinds"
fi
new_refs=$(jq -r '[.data.events[]? | select(.kind=="artifacts.repo.pushed" and (.data.before | test("^0{40}$"))) | .data.ref] | join(", ")' "$work/audit.json")
if [[ -n "$events_off" ]]; then
  step "POC before on new ref" SKIP "push events not configured; optional, see SELF-HOSTING.md (Worker: $events_off)"
elif [[ "$n_ev" -gt 0 ]]; then
  step "POC before on new ref" PASS "$n_ev pushed event(s); before = 40 zeros on new ref(s): ${new_refs:-none seen}"
else
  step "POC before on new ref" FAIL "no artifacts.repo.pushed event within ${event_wait}s"
fi

# ---------------------------------------------------------------- POCs
probe_id=""
if [[ "$pocs" == "1" ]]; then
  log "Proofs of concept on a fresh fork of the trunk (after the land)"
  out=$(cd "$work/docs" && WAYGLASS_AGENT=demo-probe WAYGLASS_KEY=${agent_keys[demo-probe]} "$wg" intent declare --json \
    --goal "probe: what an Artifacts fork carries" \
    --reason "Check notes, shallow and blobless clones on a real fork before the demo" \
    --path "probe/**") || out=""
  probe_id=$(printf '%s' "$out" | jq -r '.data.intent.id // ""')
  printf '%s' "$out" | redact >"$work/probe.declare.json"
  tok=$(printf '%s' "$out" | jq -r '.data.token.value // ""')
  remote=$(printf '%s' "$out" | jq -r '.data.fork.remote // ""')
  out=
  if [[ -n "$tok" ]]; then
    refs=$({ git_tok "$tok" ls-remote "$remote" 2>&1 || true; } | awk '{print $2}' | tr '\n' ' ')
    sh_rc=0; git_tok "$tok" clone -q --depth 1 "$remote" "$work/probe-shallow" 2>"$work/probe-shallow.err" || sh_rc=$?
    if [[ $sh_rc -eq 0 ]]; then
      step "POC shallow clone" PASS "--depth 1: $(git -C "$work/probe-shallow" rev-list --count HEAD) commit(s), shallow=$(git -C "$work/probe-shallow" rev-parse --is-shallow-repository)"
    else
      step "POC shallow clone" FAIL "$(head -2 "$work/probe-shallow.err" | tr '\n' ' ')"
    fi
    bl_rc=0; git_tok "$tok" clone -q --filter=blob:none "$remote" "$work/probe-blobless" 2>"$work/probe-blobless.err" || bl_rc=$?
    if [[ $bl_rc -eq 0 ]]; then
      # Old blobs stay on the server only if the filter was honoured; checkout
      # fetched the current ones lazily (with the same token).
      missing=$(GIT_NO_LAZY_FETCH=1 git -C "$work/probe-blobless" rev-list --objects --all --missing=print | grep -c '^?' || true)
      if [[ "$missing" -gt 0 ]]; then
        step "POC blobless clone" PASS "--filter=blob:none honoured: $missing historical blob(s) left on the server, checkout fetched the rest lazily"
      else
        step "POC blobless clone" FAIL "clone worked but every blob arrived: the filter was ignored"
      fi
    else
      step "POC blobless clone" FAIL "$(head -2 "$work/probe-blobless.err" | tr '\n' ' ')"
    fi
    # The why note the land pushed to the trunk, read back from the new fork.
    fork_note=""
    if [[ $sh_rc -eq 0 ]]; then
      git_tok "$tok" -C "$work/probe-shallow" fetch -q "$remote" "+refs/notes/wayglass:refs/notes/wayglass" 2>/dev/null || true
      fork_note=$(git -C "$work/probe-shallow" notes --ref=wayglass show HEAD 2>/dev/null | jq -r '.intent_id // empty' 2>/dev/null || true)
    fi
    if [[ "$refs" == *refs/notes/wayglass* && -n "$docs_id" && "$fork_note" == "$docs_id" ]]; then
      step "POC notes survive fork" PASS "fork has refs/notes/wayglass; note on the landed commit names $fork_note"
    else
      step "POC notes survive fork" FAIL "fork refs: ${refs:-none}; note on HEAD: ${fork_note:-none} (want $docs_id)"
    fi
    tok=
  else
    step "POC probe fork" FAIL "could not declare the probe intent"
  fi
fi

# ---------------------------------------------------------------- cost
log "Artifacts usage created by this run"
wget_api "/v1/audit?project=$project&limit=500" >"$work/audit.json"
count_kind() { jq --arg k "$1" '[.data.events[]? | select(.kind==$k)] | length' "$2"; }
us_repos=$(( 1 + $(count_kind intent.declared "$work/audit.json") ))
us_tokens=$(( $(count_kind project.created "$work/audit.json") + $(count_kind intent.declared "$work/audit.json") + $(count_kind token.refreshed "$work/audit.json") + $(count_kind land.token_issued "$work/audit.json") ))
eu_repos=0; eu_tokens=0
if [[ -n "$pii_id" ]]; then
  wget_api "/v1/audit?project=$eu_project&limit=500" >"$work/audit-eu-all.json"
  jq --arg id "$pii_id" '{data: {events: [.data.events[]? | select(.intent_id == $id)]}}' "$work/audit-eu-all.json" >"$work/audit-eu.json"
  eu_repos=$(count_kind intent.declared "$work/audit-eu.json")
  eu_tokens=$(( $(count_kind intent.declared "$work/audit-eu.json") + $(count_kind token.refreshed "$work/audit-eu.json") ))
  if [[ "$eu_project" == "${prefix}-eu-${ts}" ]]; then eu_repos=$((eu_repos + 1)); eu_tokens=$((eu_tokens + 1)); fi
  rm -f "$work/audit-eu-all.json"
fi
echo "  namespace wayglass (us):    $us_repos repo(s) [trunk $project + forks], $us_tokens token(s) minted"
echo "  namespace wayglass-eu (eu): $eu_repos repo(s), $eu_tokens token(s) minted"
echo "  (plus one creation token per repo, which the Worker revokes at once)"

# ---------------------------------------------------------------- measured
# One labelled report of this run. MEASURED: timed or counted in this run
# (counts come from the Worker's audit log, times from this script's clock or
# the Worker's event timestamps). SCRIPTED: fixed by how this script is
# written. Artifacts operations appear only when the Worker meters them
# (GET /v1/cost), and then only its own binding calls; nothing is estimated.
log "Measured run"
run_ms=$(( $(now_ms) - run_t0 ))
timings='{}'
timing() { local v; v=$(sed -n "s/^$2=//p" "$work/$1.timing"); echo "${v:-null}"; }
for a in docs auth pii; do
  [[ -f "$work/$a.timing" ]] || continue
  timings=$(jq --arg a "$a" --argjson push "$(timing "$a" push_ms)" --argjson prop "$(timing "$a" propose_ms)" \
    '.[$a] = {push_ms: $push, propose_ms: ($prop // null)}' <<<"$timings")
done
# This run's events: the whole US project, plus only this run's intent on a shared EU project.
audit_files=("$work/audit.json")
[[ -f "$work/audit-eu.json" ]] && audit_files+=("$work/audit-eu.json")
jq -s '[.[] | .data.events[]?]' "${audit_files[@]}" >"$work/run-events.json"
# Worker-side Artifacts binding calls, if the Worker meters them (GET /v1/cost).
# Client git traffic to forks never reaches the Worker, so it is not in here.
wget_api "/v1/cost?project=$project" >"$work/cost.json" 2>/dev/null || true
ops_row=null
if jq -e '.ok' "$work/cost.json" >/dev/null 2>&1; then
  eu_ops=0
  if [[ -n "$pii_id" ]]; then
    wget_api "/v1/cost?project=$eu_project" >"$work/cost-eu.json" 2>/dev/null || true
    eu_ops=$(jq --arg id "$pii_id" '.data.intents[$id].ops // 0' "$work/cost-eu.json" 2>/dev/null || echo 0)
  fi
  ops_row=$(jq --argjson eu "$eu_ops" '{metric: "Artifacts binding calls (Worker side)", value: (.data.total.ops + $eu),
    label: "MEASURED", how: "the Worker'\''s meter, GET /v1/cost (us \(.data.total.ops), eu \($eu)); client git clone/fetch/push to forks is not seen by the Worker and not counted"}' "$work/cost.json")
fi
measured=$(jq -n --argjson ops_row "$ops_row" --slurpfile ev "$work/run-events.json" --argjson t "$timings" \
  --argjson land "${land_ms:-null}" --argjson run "$run_ms" --argjson usr "$us_repos" --argjson ust "$us_tokens" \
  --argjson eur "$eu_repos" --argjson eut "$eu_tokens" --arg rev "${worker_rev:-unknown}" '
  def median: sort | if length == 0 then null elif length % 2 == 1 then .[length / 2 | floor] else ((.[length / 2 - 1] + .[length / 2]) / 2 | round) end;
  def ms: sub("\\.[0-9]+Z$"; "Z") as $s | (($s | fromdateiso8601) * 1000) + ((capture("\\.(?<f>[0-9]{3})") // {f: "0"}).f | tonumber);
  ($ev[0]) as $e
  | [$e[] | select(.kind == "decision.proposed")] as $dec
  | [$dec[] | .intent_id as $id
      | ([$e[] | select(.kind == "intent.pushed" and .intent_id == $id)] | last) as $p
      | select($p != null) | ((.at | ms) - ($p.at | ms))] as $p2d
  | [$t[] | .propose_ms | select(. != null)] as $prop
  | [$t[] | .push_ms] as $push
  | {worker_revision: $rev,
     rows: [
       {metric: "Agents working in parallel", value: ($t | length), label: "SCRIPTED", how: "docs, auth and PII agents, each in its own clone"},
       {metric: "Agents in the audit log", value: ([$e[] | select(.kind == "intent.declared") | .actor] | unique | length), label: "MEASURED", how: "distinct actors that declared an intent (adds the POC probe and the session-fork agent)"},
       {metric: "Intents declared", value: ([$e[] | select(.kind == "intent.declared")] | length), label: "MEASURED", how: "intent.declared events"},
       {metric: "Decisions", value: ($dec | length), label: "MEASURED", how: "decision.proposed events"},
       ($dec | group_by(.data.action) | map({metric: "Decisions routed to \(.[0].data.action)", value: length, label: "MEASURED", how: "rule chosen by the Worker; the script wrote one change per route"}) | .[]),
       {metric: "Changes landed", value: ([$e[] | select(.kind == "intent.landed")] | length), label: "MEASURED", how: "intent.landed events (the script lands only auto_merge)"},
       {metric: "Median push round-trip", value: ($push | median), unit: "ms", n: ($push | length), label: "MEASURED", how: "wall clock around wayglass push (git push to the fork + Worker overlap check)"},
       {metric: "Median propose round-trip", value: ($prop | median), unit: "ms", n: ($prop | length), label: "MEASURED", how: "wall clock around wayglass propose (classify, Worker policy decision, note)"},
       {metric: "Median push-to-decision", value: ($p2d | median), unit: "ms", n: ($p2d | length), label: "MEASURED", how: "Worker timestamps, intent.pushed to decision.proposed; includes the script'\''s fixed 2 s pause before propose"},
       {metric: "Land round-trip", value: $land, unit: "ms", n: 1, label: "MEASURED", how: "wall clock around wayglass land (merge, push, verify, note)"},
       {metric: "Artifacts repos created", value: ($usr + $eur), label: "MEASURED", how: "trunk + one fork per declared intent, counted from the audit log (us \($usr), eu \($eur))"},
       {metric: "Repo tokens minted", value: ($ust + $eut), label: "MEASURED", how: "init, declares, refreshes and land tokens in the audit log (us \($ust), eu \($eut)); excludes the creation token Artifacts makes per repo, which the Worker revokes at once"},
       ($ops_row | select(. != null)),
       {metric: "Whole run", value: $run, unit: "ms", n: 1, label: "MEASURED", how: "script wall clock, including the wait for queue events"}
     ]}')
md=$(jq -r '"| Metric | Value | Label | How |", "|---|---|---|---|",
  (.rows[] | "| \(.metric) | \(if .value == null then "n/a" elif .unit == "ms" then (if .value >= 10000 then "\(.value / 1000 | floor) s" else "\(.value) ms" end) else .value end)\(if (.n // 1) > 1 then " (n=\(.n))" else "" end) | \(.label) | \(.how) |")' <<<"$measured")
printf '%s\n' "$md" >"$work/measured.md"
printf '%s\n' "$md"
if [[ "$ops_row" == null ]]; then
  echo "Worker revision: $(jq -r .worker_revision <<<"$measured"). Artifacts operations: not reported (this Worker has no /v1/cost meter; never estimated)."
else
  echo "Worker revision: $(jq -r .worker_revision <<<"$measured"). Artifacts operations: Worker-side binding calls only; client git traffic is not counted."
fi

# ---------------------------------------------------------------- summary
pass=$(awk -F'\t' '$2=="PASS"' "$steps" | wc -l | tr -d ' ')
fail=$(awk -F'\t' '$2=="FAIL"' "$steps" | wc -l | tr -d ' ')
pend=$(awk -F'\t' '$2=="PENDING"' "$steps" | wc -l | tr -d ' ')
skip=$(awk -F'\t' '$2=="SKIP"' "$steps" | wc -l | tr -d ' ')
jq -R -s --arg project "$project" --arg eu "$eu_project" --arg url "$WAYGLASS_URL" --arg at "$(date -u +%FT%TZ)" \
  --arg docs "$docs_id" --arg auth "$auth_id" --arg pii "$pii_id" --arg probe "$probe_id" --arg work "$work" \
  --argjson usr "$us_repos" --argjson ust "$us_tokens" --argjson eur "$eu_repos" --argjson eut "$eu_tokens" \
  --argjson measured "$measured" '
  {v: 1, measured: $measured, at: $at, worker: $url, project: $project, eu_project: $eu, workdir: $work,
   intents: {docs: $docs, auth: $auth, pii: $pii, probe: $probe},
   artifacts: {us: {repos: $usr, tokens: $ust}, eu: {repos: $eur, tokens: $eut}},
   steps: [split("\n")[] | select(length > 0) | split("\t") | {step: .[0], result: .[1], detail: .[2]}]}
  | .totals = (.steps | group_by(.result) | map({key: .[0].result, value: length}) | from_entries)' "$steps" >"$summary"

echo
echo "RESULT  STEP                       DETAIL"
awk -F'\t' '{ printf "%-7s %-26s %s\n", $2, $1, $3 }' "$steps"
echo
echo "$pass passed, $fail failed, $pend pending, $skip skipped. Summary: $summary"
echo "Left for a person: wayglass approve $auth_id (in $work/auth); quarantined $pii_id on $eu_project."
[[ "${DEMO_KEEP:-1}" == "1" ]] || rm -rf "$work"
[[ "$fail" -eq 0 ]]
