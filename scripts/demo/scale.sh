#!/usr/bin/env bash
# shellcheck disable=SC2016 # single quotes hold jq programs and Markdown backticks
# Scale run: N agents (default 10) working one project at the same time,
# through Wayglass, against the deployed Worker and real Artifacts. Nothing
# here is mocked.
#
# It creates a public project <prefix>-<unix> (us) seeded with a small app
# and a policy with named rules, mints one agent key per agent, and starts
# every agent at once. Each agent, as scale-agent-NNN with its own key:
# declares an intent, fetches its fork with the declare token, commits a
# small change with its why, pushes, proposes, and lands its own change when
# policy says auto_merge (retrying a busy land slot or a moved trunk with
# jitter, and running wayglass sync on land_conflict, repropose_required or
# policy_changed, as
# `wayglass land --help` says). A few pairs of agents declare and edit the
# same file on purpose.
#
# Seven agents break a rule, one per kind, and the run checks each is caught:
#   undeclared  pushes a file outside its declared paths -> undeclared_paths flagged, not auto_merge
#   auth        auth change under a docs goal            -> auth-needs-human, pending_human, exit 4
#   pii         adds personal data                       -> pii-quarantine, quarantined, exit 4
#   self        approves its own change                  -> refused (conflict), nothing recorded
#   lgtm        approves another's change with "LGTM"    -> refused by the CLI (usage) and the Worker (bad_request)
#   human       its agent key approves the auth change   -> needs_human, exit 4
#   forge       pushes a forged git note                 -> why says verified: mismatch
# Changes routed to a person stay pending for the person; agent_review
# changes stay pending for a reviewer. No agent uses the admin key, and this
# script never approves or rejects anything.
#
# Prints a summary (and writes summary.json): latency p50/p95/max per step,
# decisions by action and status, overlaps, breakers caught vs expected,
# HTTP errors by status (any 5xx fails the run), wall time, and the Worker's
# Artifacts operations (GET /v1/cost). Writes a run log of the measured
# numbers to docs/scale-run-<date>.md (not for demo-test-* runs).
#
# Usage:
#   scripts/demo/scale.sh                            # 10 agents on demo-scale-<unix>
#   scripts/demo/scale.sh -n 100                     # 100 agents
#   SCALE_PREFIX=demo-test-scale scripts/demo/scale.sh   # throwaway (wayglass project delete can remove it)
#   scripts/demo/scale.sh --json                     # summary JSON on stdout, progress on stderr
#   scripts/demo/scale.sh --revoke-keys <project>    # revoke agent keys a crashed run left
# Environment (all optional):
#   SCALE_PREFIX  project name prefix (default demo-scale; must start with demo-)
#   SCALE_LOG     run log path (default docs/scale-run-<date>.md; "-" for none)
#   SCALE_LAND_TIMEOUT  seconds an agent keeps retrying its land (default 900)
#   WAYGLASS_URL, WAYGLASS_KEY (the admin key)   default: ~/.config/wayglass/credentials.yaml
#
# Cost: 1 trunk + N forks; per agent a fork, a few repo tokens (declare,
# each push, land) and the Worker's binding reads. The run prints the
# Worker's metered operations; client git traffic is not metered by the
# Worker and is counted here as git commands run.
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
n=10
json=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    -n) n=${2:?-n needs a number}; shift 2 ;;
    --json) json=1; shift ;;
    --revoke-keys)
      : "${2:?usage: $0 --revoke-keys <project>}"
      # shellcheck source=agent-keys.sh
      source "$repo_root/scripts/demo/agent-keys.sh"
      ak_revoke_note "scale $2"
      exit ;;
    -h | --help) sed -n '3,50p' "$0"; exit 0 ;;
    *) echo "unknown argument $1 (see --help)" >&2; exit 2 ;;
  esac
done
[[ "$n" =~ ^[0-9]+$ && $n -ge 10 && $n -le 200 ]] || { echo "-n must be 10 to 200 (seven agents are rule-breakers)" >&2; exit 2; }
prefix=${SCALE_PREFIX:-demo-scale}
[[ "$prefix" == demo-* ]] || { echo "SCALE_PREFIX must start with demo- (public view and cleanup rely on it)" >&2; exit 2; }
land_timeout=${SCALE_LAND_TIMEOUT:-900}
ts=$(date +%s)
project="$prefix-$ts"
date_tag=$(date +%Y-%m-%d)
log_path=${SCALE_LOG:-$repo_root/docs/scale-run-$date_tag.md}
[[ "$prefix" == demo-test-* && -z "${SCALE_LOG:-}" ]] && log_path=-
work=$(mktemp -d "${TMPDIR:-/tmp}/wayglass-scale-$ts.XXXX")
out="$work/out"
mkdir -p "$out" "$work/agents" "$work/bin" "$work/cfg"
creds="${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}/credentials.yaml"

# Progress goes to stderr with --json, so stdout is only the summary.
say() { if [[ $json -eq 1 ]]; then printf '%s\n' "$*" >&2; else printf '%s\n' "$*"; fi; }

for t in git go jq curl; do
  command -v "$t" >/dev/null || { echo "$t is required" >&2; exit 1; }
done
[[ -n "${EPOCHREALTIME:-}" ]] || { echo "bash 5 or later is required (EPOCHREALTIME)" >&2; exit 1; }
if [[ -z "${WAYGLASS_URL:-}" ]]; then WAYGLASS_URL=$(sed -n 's/^url: *//p' "$creds" 2>/dev/null || true); fi
: "${WAYGLASS_URL:?not logged in: run wayglass login or set WAYGLASS_URL}"
export WAYGLASS_URL
unset WAYGLASS_INTENT WAYGLASS_PROJECT WAYGLASS_FORK_TOKEN GIT_CONFIG_COUNT

wg="$work/bin/wayglass"
(cd "$repo_root" && go build -o "$wg" ./cmd/wayglass)
export WAYGLASS_BIN=$wg

# shellcheck source=agent-keys.sh
source "$repo_root/scripts/demo/agent-keys.sh"
ak_admin_load
trap ak_revoke_all EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

now_ms() { local t=${EPOCHREALTIME/[.,]/}; echo $((t / 1000)); }

# Strips token values before JSON reaches disk.
redact() { jq 'del(.data.token.value) | walk(if type == "string" then gsub("art_v[0-9]+_[a-z_]*[0-9a-f]{40}[^\" ]*"; "art_REDACTED") else . end)'; }

# Admin GET with the key on stdin, never in argv.
admin_get() { ak_as_admin bash -c 'printf "Authorization: Bearer %s\n" "$WAYGLASS_KEY" | curl -sS -H @- "$WAYGLASS_URL$1"' _ "$1"; }

identity() { # <dir> <name>
  git -C "$1" config user.name "$2"
  git -C "$1" config user.email "$2@agents.wayglass.invalid"
  git -C "$1" config commit.gpgsign false
}

# ------------------------------------------------------------ roles
# Seven breakers, then max(1, n/25) pairs that edit one shared file, then
# routine agents: three in five change docs (auto_merge, land), two in five
# change code (agent_review, wait for a reviewer).
pairs=$((n / 25)); [[ $pairs -ge 1 ]] || pairs=1
roles=(b_undeclared b_auth b_pii b_self b_lgtm b_human b_forge)
for ((p = 1; p <= pairs; p++)); do roles+=("pair:$p" "pair:$p"); done
while [[ ${#roles[@]} -lt $n ]]; do
  if (( ${#roles[@]} % 5 < 3 )); then roles+=(docs); else roles+=(code); fi
done
name_of() { printf 'scale-agent-%03d' "$1"; }
agent_of_role() { local i; for i in "${!roles[@]}"; do [[ ${roles[$i]} == "$1" ]] && { name_of $((i + 1)); return; }; done; }

say "Wayglass scale run against $WAYGLASS_URL"
say "Project $project: $n agents ($pairs overlap pair(s), 7 rule-breakers), workdir $work"

# ------------------------------------------------------------ seed + init
seed="$work/seed"
mkdir -p "$seed/docs/shared" "$seed/docs/scale" "$seed/src/auth" "$seed/.wayglass"
git init -q -b main "$seed"
identity "$seed" scale-owner
cat >"$seed/README.md" <<'EOF'
# Acme notes

A small app that many agents change at once in the Wayglass scale run.
EOF
printf '# Docs\n\nOne page per feature lives in docs/scale/.\n' >"$seed/docs/index.md"
printf '# Scale notes\n\nEach agent adds its own page here.\n' >"$seed/docs/scale/README.md"
for ((p = 1; p <= pairs; p++)); do
  printf '# Shared topic %d\n\nTwo agents add a line here at the same time.\n' "$p" >"$seed/docs/shared/topic-$p.md"
done
cat >"$seed/src/auth/login.ts" <<'EOF'
export function login(user: string, password: string): boolean {
  return user.length > 0 && password.length > 0;
}
EOF
printf 'export const appName = "acme-notes";\n' >"$seed/src/app.ts"
cat >"$seed/.wayglass/policy.yaml" <<'EOF'
# Scale run policy. The first enforcing rule that matches decides; no agent
# is trusted by name, only by what its change touches and contains.
version: 1
classify:
  area:
    docs: ["docs/**", "**/*.md"]
    auth: ["src/auth/**"]
    infra: ["infra/**", ".github/**", "Dockerfile"]
    app: ["src/**"]
  data_class:
    secrets:
      content:
        - "-----BEGIN [A-Z ]*PRIVATE KEY-----"
        - "AKIA[0-9A-Z]{16}"
      paths: ["**/.env", "**/*.pem", "**/.dev.vars"]
    pii:
      content:
        - "(?i)\\bdate_?of_?birth\\b|\\bbirth_?date\\b|\\bdob\\b"
        - "(?i)\\b(iban|passport_?(number|no))\\b|\\bnational_?id\\b"
        - "(?i)\\bphone_?(number)?\\b|\\bstreet_?address\\b"
  risk:
    medium: { lines_changed_over: 400, files_changed_over: 15 }
    high: { lines_changed_over: 1000, files_changed_over: 40 }
rules:
  - name: no-secrets
    when: { data_class: secrets }
    action: block
    reason: "Secrets never land in the repo."
  - name: pii-quarantine
    when: { data_class: pii }
    action: quarantine
    reason: "New personal data waits for a data protection review."
  - name: infra-two-people
    when: { area: infra }
    action: multi_approve
    approvers: 2
    reason: "Infrastructure changes affect everyone; two people approve."
  - name: auth-needs-human
    when: { area: auth }
    action: human_approve
    reason: "Sign-in is a security boundary; a person approves."
  - name: docs-auto
    when: { only_area: docs, risk: [low, medium] }
    action: auto_merge
    reason: "Every file is documentation."
  - name: routine-code-review
    when: { only_area: [app, docs], risk: low }
    action: agent_review
    reason: "Small app change outside sensitive areas; a reviewer agent checks intent against diff."
default: { action: human_approve, reason: "No rule matched; a person decides." }
EOF
git -C "$seed" add -A
git -C "$seed" commit -q -m "feat: seed the scale-run app

A small app with docs, sign-in and code, and the policy that routes the
agents' changes, so every route is exercised at once."
init_rc=0
(cd "$seed" && ak_as_admin env WAYGLASS_AGENT=scale-owner "$wg" init --project "$project" --jurisdiction us --public --json) 2>"$out/init.err" | redact >"$out/init.json" || init_rc=$?
if [[ $init_rc -ne 0 ]] || ! jq -e '.ok and .data.pushed == "main"' "$out/init.json" >/dev/null; then
  echo "init failed: $(jq -r '.error.message // empty' "$out/init.json" 2>/dev/null) $(head -3 "$out/init.err")" >&2
  exit 1
fi
admin_get "/v1/cost?project=$project" >"$out/cost-before.json" || true
say "Trunk $(jq -r .data.trunk.name "$out/init.json") created, public $(jq -r .data.public "$out/init.json")"

# ------------------------------------------------------------ keys
key_note="scale $project"
declare -A keys
t0=$(now_ms)
for ((i = 1; i <= n; i++)); do
  a=$(name_of "$i")
  ak_mint k "$a" "$key_note"
  keys[$a]=$k
done
k=
mint_ms=$(($(now_ms) - t0))
say "Agent keys: $n minted in ${mint_ms} ms (agent kind, one per agent; revoked when this script exits)"

# ------------------------------------------------------------ agent steps
# Each agent writes rows "agent step ms rc code http" to $out/<agent>.tsv and
# its verdict to $out/<agent>.result as JSON.

# http_of <code> <message>: the HTTP status behind an envelope error code.
http_of() {
  case "$1" in
    ok) echo 200 ;;
    bad_request) echo 400 ;;
    unauthorized) echo 401 ;;
    forbidden | not_author | needs_human | family_required | same_family) echo 403 ;;
    not_found) echo 404 ;;
    method_not_allowed) echo 405 ;;
    conflict | superseded) echo 409 ;;
    internal) echo 500 ;;
    upstream_error) echo 502 ;;
    not_configured) echo 503 ;;
    bad_response) [[ "$2" =~ HTTP\ ([0-9]{3}) ]] && echo "${BASH_REMATCH[1]}" || echo bad_response ;;
    *) [[ "$2" == *"worker request failed"* ]] && echo network || echo local ;;
  esac
}

# step <agent> <step> <file> <cmd...>: run, time, record; stdout to <file>.
step() {
  local a=$1 s=$2 f=$3 rc=0 t0 ms code msg
  shift 3
  t0=$(now_ms)
  "$@" >"$f" 2>>"$f.err" || rc=$?
  ms=$(($(now_ms) - t0))
  code=$(jq -r 'if .ok then "ok" else .error.code end' "$f" 2>/dev/null || echo nojson)
  msg=$(jq -r '.error.message // ""' "$f" 2>/dev/null || true)
  printf '%s\t%s\t%d\t%d\t%s\t%s\n' "$a" "$s" "$ms" "$rc" "$code" "$(http_of "$code" "$msg")" >>"$out/$a.tsv"
  return $rc
}
gitc() { printf '1\n' >>"$out/$a.gitops"; git "$@"; }
git_tok() { # <token> <git args...>
  local tok=$1; shift
  printf '1\n' >>"$out/$a.gitops"
  GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: Bearer $tok" \
    GIT_TERMINAL_PROMPT=0 git "$@"
}
result() { # <json fields as jq args...>
  jq -n "$@" >"$out/$a.result"
}

# The edit each role makes, in the agent's clone ($d), and the commit's why.
edit() { # <role> <idx>
  local role=$1 i=$2 f
  case "$role" in
    docs | b_lgtm | b_human)
      f=docs/scale/agent-$i.md
      printf '# Feature %s\n\nHow feature %s works, written by %s.\n\nIt keeps one note per user and syncs it on save.\n' "$i" "$i" "$a" >"$f" ;;
    pair:*)
      printf -- '- %s: notes sync on save, so two devices stay current.\n' "$a" >>"docs/shared/topic-${role#pair:}.md" ;;
    code | b_self | b_forge)
      mkdir -p "src/feature-$i"
      printf 'export function feature%s(note: string): string {\n  return note.trim();\n}\n' "$i" >"src/feature-$i/index.ts" ;;
    b_undeclared)
      printf '# Feature %s\n\nNotes for feature %s.\n' "$i" "$i" >"docs/scale/agent-$i.md"
      mkdir -p "src/feature-$i"
      printf 'export const undeclared%s = true;\n' "$i" >"src/feature-$i/extra.ts" ;;
    b_auth)
      cat >src/auth/login.ts <<'EOF'
export function login(user: string, password: string): boolean {
  // Sign-in wording only, says the goal; the check below changed too.
  return user.length > 0 && password.length >= 8;
}
EOF
      ;;
    b_pii)
      mkdir -p "src/feature-$i"
      cat >"src/feature-$i/customer.ts" <<'EOF'
export interface Customer {
  id: string;
  date_of_birth: string; // ISO date, for an age check
}
EOF
      ;;
  esac
}

# declared_paths <role> <idx>, goal and reason per role.
declared() {
  case "$1" in
    docs | b_lgtm | b_human | b_undeclared | b_auth) echo "docs/scale/agent-$2.md" ;;
    pair:*) echo "docs/shared/topic-${1#pair:}.md" ;;
    code | b_self | b_forge | b_pii) echo "src/feature-$2/**" ;;
  esac
}
goal_of() {
  case "$1" in
    docs | b_lgtm | b_human) echo "docs: describe feature $2" ;;
    b_undeclared) echo "docs: notes for feature $2" ;;
    b_auth) echo "docs: clarify the sign-in wording" ;;
    pair:*) echo "docs: add a sync note to shared topic ${1#pair:}" ;;
    code | b_self | b_forge) echo "feat: trim notes in feature $2" ;;
    b_pii) echo "feat: age check for feature $2" ;;
  esac
}
reason_of() {
  case "$1" in
    docs | b_lgtm | b_human | b_undeclared) echo "Users ask how feature $2 works; the docs have no page for it" ;;
    b_auth) echo "Support says the sign-in page wording confuses users" ;;
    pair:*) echo "Two devices showed different notes; the shared page should say sync happens on save" ;;
    code | b_self | b_forge) echo "Notes saved with trailing spaces break search in feature $2" ;;
    b_pii) echo "Age-restricted content in feature $2 needs the customer's date of birth" ;;
  esac
}

# wait_proposed <agent name> <seconds>: id of that agent's proposed intent.
wait_proposed() {
  local who=$1 deadline=$(($(date +%s) + $2)) id
  while [[ $(date +%s) -lt $deadline ]]; do
    id=$("$wg" intent list --json 2>/dev/null | jq -r --arg w "$who" '.data.intents[]? | select(.agent == $w and .status == "proposed") | .id' | head -1)
    [[ -n "$id" ]] && { echo "$id"; return 0; }
    sleep $((2 + RANDOM % 3))
  done
  return 1
}

# land_loop: land, retrying a busy slot or a moved trunk with jitter, and
# running wayglass sync on land_conflict / repropose_required / policy_changed,
# then pushing and proposing again. Returns 2 when policy then routes it elsewhere.
land_loop() {
  local deadline=$(($(date +%s) + land_timeout)) try=0 code msg t_start merged=0 rc
  t_start=$(now_ms)
  while :; do
    try=$((try + 1))
    rc=0
    step "$a" land "$out/$a.land.json" "$wg" land --json --no-mirror || rc=$?
    [[ $rc -eq 0 ]] && break
    code=$(jq -r '.error.code // "?"' "$out/$a.land.json" 2>/dev/null || echo "?")
    msg=$(jq -r '.error.message // ""' "$out/$a.land.json" 2>/dev/null || true)
    [[ $(date +%s) -lt $deadline ]] || { land_note="gave up after $try tries: $code"; return 1; }
    case "$code" in
      conflict)
        if [[ "$msg" == *"is landing until"* ]]; then
          printf '1\n' >>"$out/$a.busy"
          sleep "$(( (1 + RANDOM % 4) * (try < 4 ? try : 4) / 2 + 1 )).$((RANDOM % 10))"
        else
          land_note="refused: $msg"; return 1
        fi ;;
      trunk_moved) printf '1\n' >>"$out/$a.moved"; sleep "0.$((2 + RANDOM % 8))" ;;
      land_conflict | repropose_required | policy_changed)
        [[ $merged -lt 3 ]] || { land_note="still $code after $merged syncs"; return 1; }
        merged=$((merged + 1))
        printf '%s\n' "$code" >>"$out/$a.merges"
        # The documented recovery: wayglass sync merges the trunk into this clone.
        rc=0
        step "$a" sync "$out/$a.sync.json" "$wg" sync --json || rc=$?
        if [[ $rc -ne 0 ]]; then
          [[ $(jq -r '.error.code // ""' "$out/$a.sync.json") == sync_conflict ]] || { land_note="sync failed: $(jq -r '.error.code // "?"' "$out/$a.sync.json")"; return 1; }
          # Both agents appended a line: keep the trunk's lines, then ours.
          local f mine
          for f in $(git diff --name-only --diff-filter=U); do
            mine=$(git show ":2:$f" | tail -1)
            git show "refs/wayglass/trunk:$f" >"$f"
            printf '%s\n' "$mine" >>"$f"
            git add "$f"
          done
          git commit -q --no-edit
        fi
        step "$a" push "$out/$a.push2.json" "$wg" push --json || { land_note="push after merge failed"; return 1; }
        rc=0
        step "$a" propose "$out/$a.propose2.json" "$wg" propose --json || rc=$?
        if [[ $(jq -r '.data.decision.status // ""' "$out/$a.propose2.json") != approved ]]; then
          land_note="after merging the trunk, policy routed it to $(jq -r '"\(.data.decision.matched_rule // "?") -> \(.data.decision.action // "?")"' "$out/$a.propose2.json")"
          return 2
        fi ;;
      *) land_note="land failed: $code: ${msg:0:160}"; return 1 ;;
    esac
  done
  land_tries=$try
  land_total_ms=$(($(now_ms) - t_start))
  printf '%s\tland_total\t%d\t0\tok\t200\n' "$a" "$land_total_ms" >>"$out/$a.tsv"
  return 0
}

run_agent() { # <idx>
  local i=$1 role=${roles[$1 - 1]} d decl id tok remote rc=0 action status prc
  a=$(name_of "$i")
  d="$work/agents/$a"
  local -a paths
  read -r -a paths <<<"$(declared "$role" "$(printf %03d "$i")")"
  local idx goal reason
  idx=$(printf %03d "$i")
  goal=$(goal_of "$role" "$idx")
  reason=$(reason_of "$role" "$idx")
  export WAYGLASS_AGENT=$a WAYGLASS_AGENT_FAMILY=claude WAYGLASS_KEY=${keys[$a]} WAYGLASS_CONFIG_DIR="$work/cfg/$a"
  mkdir -p "$WAYGLASS_CONFIG_DIR"
  git clone -q "$seed" "$d"
  identity "$d" "$a"
  cd "$d"

  # Declare: the output holds the fork token, so it stays in a variable.
  local t0 ms args=() p
  for p in "${paths[@]}"; do args+=(--path "$p"); done
  t0=$(now_ms)
  decl=$("$wg" intent declare --json --goal "$goal" --reason "$reason" "${args[@]}" 2>>"$out/$a.declare.err") || rc=$?
  ms=$(($(now_ms) - t0))
  printf '%s' "$decl" | redact >"$out/$a.declare.json" 2>/dev/null || printf '%s' "$decl" >"$out/$a.declare.raw.err"
  local dcode dmsg
  dcode=$(printf '%s' "$decl" | jq -r 'if .ok then "ok" else .error.code end' 2>/dev/null || echo nojson)
  dmsg=$(printf '%s' "$decl" | jq -r '.error.message // ""' 2>/dev/null || true)
  printf '%s\tdeclare\t%d\t%d\t%s\t%s\n' "$a" "$ms" "$rc" "$dcode" "$(http_of "$dcode" "$dmsg")" >>"$out/$a.tsv"
  if [[ $rc -ne 0 ]]; then
    decl=
    result --arg a "$a" --arg r "$role" --arg n "declare failed: $dcode ${dmsg:0:160}" '{agent:$a, role:$r, ok:false, note:$n}'
    return 1
  fi
  id=$(printf '%s' "$decl" | jq -r .data.intent.id)
  tok=$(printf '%s' "$decl" | jq -r .data.token.value)
  remote=$(printf '%s' "$decl" | jq -r .data.fork.remote)
  decl=

  # Clone its fork: fetch it with the declare token (memory only) and work on it.
  rc=0
  t0=$(now_ms)
  git_tok "$tok" fetch -q "$remote" "+refs/heads/main:refs/remotes/fork/main" 2>>"$out/$a.git.err" || rc=$?
  tok=
  printf '%s\tfork_fetch\t%d\t%d\t%s\t-\n' "$a" "$(($(now_ms) - t0))" "$rc" "$([[ $rc -eq 0 ]] && echo ok || echo git_error)" >>"$out/$a.tsv"
  [[ $rc -eq 0 ]] && git reset -q --hard refs/remotes/fork/main

  edit "$role" "$idx"
  git add -A
  git commit -q -F - <<EOF
$goal

$reason

Wayglass-Intent: $id
EOF
  if [[ $role == b_forge ]]; then
    # A note claiming policy auto-merged this commit; nobody decided that.
    git notes --ref wayglass add -f -m "$(jq -nc --arg id "$id" --arg g "$goal" --arg r "$reason" --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      '{v:1, intent_id:$id, goal:$g, reason:$r, tags:{areas:["app"], data_classes:[], risk:"low", jurisdiction:"us"}, rule:"docs-auto", action:"auto_merge", decision_id:"dforged00000", status:"approved", decided_by:"policy", decided_at:$at}')" HEAD
  fi
  rc=0
  step "$a" push "$out/$a.push.json" "$wg" push --json || rc=$?
  if [[ $rc -ne 0 ]]; then
    result --arg a "$a" --arg r "$role" --arg id "$id" --arg n "push failed: $(jq -r '.error.code + ": " + .error.message' "$out/$a.push.json" 2>/dev/null | head -c 200)" '{agent:$a, role:$r, intent:$id, ok:false, note:$n}'
    return 1
  fi
  sleep 1
  prc=0
  step "$a" propose "$out/$a.propose.json" "$wg" propose --json || prc=$?
  action=$(jq -r '.data.decision.action // "none"' "$out/$a.propose.json" 2>/dev/null || echo none)
  status=$(jq -r '.data.decision.status // "none"' "$out/$a.propose.json" 2>/dev/null || echo none)
  local rule undeclared
  rule=$(jq -r '.data.decision.matched_rule // "none"' "$out/$a.propose.json" 2>/dev/null || echo none)
  undeclared=$(jq -c '.data.undeclared_paths // []' "$out/$a.propose.json" 2>/dev/null || echo '[]')
  if [[ $action == none ]]; then
    result --arg a "$a" --arg r "$role" --arg id "$id" --arg n "propose failed (exit $prc): $(jq -r '.error.code + ": " + .error.message' "$out/$a.propose.json" 2>/dev/null | head -c 200)" '{agent:$a, role:$r, intent:$id, ok:false, note:$n}'
    return 1
  fi

  local ok=false note="" landed=false rerouted=false land_note="" land_tries=0 land_total_ms=0 lrc
  if [[ $status == approved ]]; then
    lrc=0
    land_loop || lrc=$?
    [[ $lrc -eq 0 ]] && landed=true
    [[ $lrc -eq 2 ]] && { status=$(jq -r '.data.decision.status' "$out/$a.propose2.json"); rerouted=true; }
  fi

  # Expected outcome per role, and the rule-breaking attempt itself.
  local brc bcode target
  case "$role" in
    docs | pair:*)
      [[ $action == auto_merge && $landed == true ]] && ok=true
      note="$rule -> $action, landed $landed${land_note:+ ($land_note)}" ;;
    code)
      [[ $action == agent_review && $status == pending_agent ]] && ok=true
      note="$rule -> $action ($status), left for a reviewer" ;;
    b_undeclared)
      [[ $undeclared != '[]' && $action != auto_merge ]] && ok=true
      note="undeclared $undeclared; $rule -> $action ($status)" ;;
    b_auth)
      [[ $rule == auth-needs-human && $status == pending_human && $prc -eq 4 ]] && ok=true
      note="$rule -> $action ($status), exit $prc; undeclared $undeclared; left for a person" ;;
    b_pii)
      [[ $rule == pii-quarantine && $status == quarantined && $prc -eq 4 ]] && ok=true
      note="$rule -> $action ($status), exit $prc; left for a person" ;;
    b_self)
      brc=0
      step "$a" approve_self "$out/$a.attempt.json" "$wg" approve "$id" --json --reason "I checked my own diff and the trim is right." || brc=$?
      bcode=$(jq -r '.error.code // "approved (not refused!)"' "$out/$a.attempt.json")
      [[ $brc -ne 0 && $bcode == conflict && $(jq -r '.error.message' "$out/$a.attempt.json") == *"cannot decide"* ]] && ok=true
      note="approve own $id -> $bcode, exit $brc" ;;
    b_lgtm)
      target=$(wait_proposed "$(agent_of_role b_self)" 600 || true)
      if [[ -z "$target" ]]; then note="no target change to approve"; else
        brc=0
        step "$a" approve_lgtm "$out/$a.attempt.json" "$wg" approve "$target" --json --reason "LGTM" || brc=$?
        bcode=$(jq -r '.error.code // "approved (not refused!)"' "$out/$a.attempt.json")
        # Past the CLI: the Worker must refuse the rubber stamp too.
        local wcode
        # The key goes on stdin as a header; the body holds no secret.
        wcode=$(printf 'Authorization: Bearer %s\nX-Wayglass-Agent: %s\nContent-Type: application/json\n' "$WAYGLASS_KEY" "$WAYGLASS_AGENT" |
          curl -sS -H @- --data-binary "$(jq -nc --arg p "$project" '{project:$p, verdict:"approve", reason:"LGTM"}')" "$WAYGLASS_URL/v1/intents/$target/decide" 2>/dev/null |
          jq -r '.error.code // "approved (not refused!)"' 2>/dev/null || echo "no answer")
        [[ $brc -eq 2 && $wcode == bad_request ]] && ok=true
        note="approve $target with LGTM -> CLI $bcode (exit $brc), Worker $wcode"
      fi
      [[ $action == auto_merge && $landed != true ]] && { ok=false; note="$note; own docs change not landed: $land_note"; } ;;
    b_human)
      target=$(wait_proposed "$(agent_of_role b_auth)" 600 || true)
      if [[ -z "$target" ]]; then note="no auth change to approve"; else
        brc=0
        step "$a" approve_human "$out/$a.attempt.json" "$wg" approve "$target" --json --reason "Read the diff; the length check is stricter, which the goal did not mention." || brc=$?
        bcode=$(jq -r '.error.code // "approved (not refused!)"' "$out/$a.attempt.json")
        [[ $brc -eq 4 && $bcode == needs_human ]] && ok=true
        note="agent key approves $target (auth) -> $bcode, exit $brc"
      fi
      [[ $action == auto_merge && $landed != true ]] && { ok=false; note="$note; own docs change not landed: $land_note"; } ;;
    b_forge)
      local why wv line
      line=$(grep -n 'note.trim' "src/feature-$idx/index.ts" | cut -d: -f1)
      why=$("$wg" why "src/feature-$idx/index.ts:$line" --json 2>/dev/null || true)
      printf '%s' "$why" >"$out/$a.why.json"
      wv=$(printf '%s' "$why" | jq -r '.data.verified // .error.code // "no answer"' 2>/dev/null || echo "no answer")
      [[ $wv == mismatch ]] && ok=true
      note="forged note pushed; why -> verified: $wv" ;;
  esac
  result --arg a "$a" --arg r "$role" --arg id "$id" --argjson ok "$ok" --arg n "$note" --arg act "$action" --arg st "$status" \
    --arg rule "$rule" --argjson landed "$landed" --argjson tries "$land_tries" --argjson rr "$rerouted" --argjson ov "$(jq '[.data.overlaps[]?] | length' "$out/$a.push.json" 2>/dev/null || echo 0)" \
    '{agent:$a, role:$r, intent:$id, ok:$ok, note:$n, action:$act, status:$st, rule:$rule, landed:$landed, land_tries:$tries, rerouted_after_sync:$rr, overlaps_seen:$ov}'
}

# ------------------------------------------------------------ run
say "Starting $n agents at once"
run_t0=$(now_ms)
pids=()
for ((i = 1; i <= n; i++)); do
  (run_agent "$i") >"$out/$(name_of "$i").log" 2>&1 &
  pids+=($!)
done
done_n=0
for p in "${pids[@]}"; do
  wait "$p" || true
  done_n=$((done_n + 1))
  (( done_n % 10 == 0 || done_n == n )) && say "  $done_n/$n agents finished ($(( ($(now_ms) - run_t0) / 1000 )) s)"
done
wall_ms=$(($(now_ms) - run_t0))

# ------------------------------------------------------------ measure
admin_get "/v1/cost?project=$project" >"$out/cost-after.json" || true
admin_get "/v1/decisions?project=$project&limit=500" >"$out/decisions.json" || true
(cd "$seed" && ak_as_admin "$wg" overlaps --json) >"$out/overlaps.json" 2>/dev/null || true
cat "$out"/*.tsv >"$work/steps.tsv" 2>/dev/null || : >"$work/steps.tsv"
jq -s '.' "$out"/*.result >"$work/results.json" 2>/dev/null || echo '[]' >"$work/results.json"
# Counts lines in out/*.<ext> (files that may not exist).
count() { cat "$out"/*."$1" 2>/dev/null | wc -l | tr -d ' ' || true; }
gitops=$(count gitops)
busy=$(count busy)
moved=$(count moved)
merges=$(count merges)

summary=$(jq -Rn \
  --arg project "$project" --arg url "$WAYGLASS_URL" --argjson n "$n" --argjson wall "$wall_ms" --argjson mint "$mint_ms" \
  --argjson gitops "$gitops" --argjson busy "$busy" --argjson moved "$moved" --argjson merges "$merges" \
  --slurpfile results "$work/results.json" --slurpfile dec "$out/decisions.json" \
  --slurpfile cb "$out/cost-before.json" --slurpfile ca "$out/cost-after.json" --slurpfile ov "$out/overlaps.json" '
  def pct(p): sort | if length == 0 then null else .[((length - 1) * p | floor)] end;
  [inputs | split("\t") | {agent: .[0], step: .[1], ms: (.[2] | tonumber), rc: (.[3] | tonumber), code: .[4], http: .[5]}] as $rows
  | ($results[0]) as $r
  | {
      project: $project, worker: $url, agents: $n, wall_ms: $wall, key_mint_ms: $mint,
      latency_ms: ($rows | group_by(.step) | map({key: .[0].step, value: {n: length, p50: (map(.ms) | pct(0.5)), p95: (map(.ms) | pct(0.95)), max: (map(.ms) | max)}}) | from_entries),
      decisions_by_action: ($dec[0].data.decisions // [] | group_by(.action) | map({key: .[0].action, value: length}) | from_entries),
      decisions_by_status: ($dec[0].data.decisions // [] | group_by(.status) | map({key: .[0].status, value: length}) | from_entries),
      landed: ($r | map(select(.landed)) | length),
      rerouted_after_sync: ($r | map(select(.rerouted_after_sync)) | length),
      land_retries: {busy_slot: $busy, trunk_moved: $moved, trunk_merges: $merges},
      overlaps: {open_pairs_now: ($ov[0].data.overlaps // [] | length), agents_warned_on_push: ($r | map(select((.overlaps_seen // 0) > 0)) | length)},
      breakers: {expected: ($r | map(select(.role | startswith("b_"))) | length), caught: ($r | map(select((.role | startswith("b_")) and .ok)) | length),
                  rows: ($r | map(select(.role | startswith("b_"))) | map({role: .role[2:], agent, ok, note}))},
      agents_ok: ($r | map(select(.ok)) | length), agents_reported: ($r | length),
      failures: ($r | map(select(.ok | not)) | map({agent, role, note})),
      http: ($rows | map(select(.http != "-" and .http != "200")) | group_by(.http) | map({key: .[0].http, value: length}) | from_entries),
      http_5xx: ($rows | map(select(.http | test("^5"))) | length),
      network_errors: ($rows | map(select(.http == "network")) | length),
      artifacts_ops: {before: ($cb[0].data.total.ops // null), after: ($ca[0].data.total.ops // null),
                      run: (if ($ca[0].data.total.ops // null) == null then null else ($ca[0].data.total.ops - ($cb[0].data.total.ops // 0)) end),
                      per_agent: (if ($ca[0].data.total.ops // null) == null then null else ((($ca[0].data.total.ops - ($cb[0].data.total.ops // 0)) / $n) * 10 | round / 10) end),
                      fork_calls: ($ca[0].data.total.by_op.fork // null),
                      scope: "Worker-side Artifacts binding calls (GET /v1/cost); client git traffic is counted separately as git commands"},
      client_git_commands: $gitops
    }' <"$work/steps.tsv" 2>/dev/null) || summary='{"error":"could not build the summary"}'
printf '%s\n' "$summary" >"$work/summary.json"

if [[ $json -eq 1 ]]; then
  printf '%s\n' "$summary"
else
  jq -r '
    "\nAgents: \(.agents) (\(.agents_ok) as expected), wall time \(.wall_ms / 1000) s, keys minted in \(.key_mint_ms) ms",
    "\nLatency (ms)        n    p50    p95    max",
    (.latency_ms | to_entries[] | "  \(.key | . + "                " | .[:14]) \(.value.n | tostring | "    " + . | .[-4:]) \(.value.p50 | tostring | "      " + . | .[-6:]) \(.value.p95 | tostring | "      " + . | .[-6:]) \(.value.max | tostring | "      " + . | .[-6:])"),
    "\nDecisions by action: \(.decisions_by_action | to_entries | map("\(.key) \(.value)") | join(", "))",
    "Decisions by status: \(.decisions_by_status | to_entries | map("\(.key) \(.value)") | join(", "))",
    "Landed: \(.landed); rerouted by policy after a trunk sync: \(.rerouted_after_sync); land retries: busy slot \(.land_retries.busy_slot), trunk moved \(.land_retries.trunk_moved), trunk merges \(.land_retries.trunk_merges)",
    "Overlaps: \(.overlaps.open_pairs_now) open pair(s) now; \(.overlaps.agents_warned_on_push) agent(s) warned on push",
    "\nBreakers caught \(.breakers.caught)/\(.breakers.expected):",
    (.breakers.rows[] | "  \(if .ok then "PASS" else "FAIL" end) \(.role + "          " | .[:11]) \(.agent)  \(.note)"),
    "\nHTTP errors by status: \(if (.http | length) == 0 then "none" else (.http | to_entries | map("\(.key) x\(.value)") | join(", ")) end); 5xx: \(.http_5xx); network: \(.network_errors)",
    "Artifacts ops (Worker meter): \(.artifacts_ops.run) this run (\(.artifacts_ops.per_agent)/agent; \(.artifacts_ops.fork_calls) fork calls for \(.agents) forks); client git commands: \(.client_git_commands)",
    (if (.failures | length) > 0 then "\nNot as expected:", (.failures[] | "  \(.agent) \(.role): \(.note)") else empty end)
  ' <<<"$summary" || printf '%s\n' "$summary"
  say ""
  say "Workdir $work (summary.json, steps.tsv, results.json; token values removed)"
  say "Watch: cd $seed && wayglass watch    Public view: $WAYGLASS_URL/?project=$project"
  say "Leftover keys after a crash: $0 --revoke-keys $project"
fi

# ------------------------------------------------------------ run log
if [[ "$log_path" != - ]]; then
  {
    printf '# Scale run, %s\n\n' "$date_tag"
    printf 'Measured by `scripts/demo/scale.sh -n %s` against %s on project `%s` (public). Numbers below are what this run measured; nothing is estimated.\n\n' "$n" "$WAYGLASS_URL" "$project"
    jq -r '
      "- Agents: \(.agents), started at once, each with its own agent key; \(.agents_ok) ended as expected.",
      "- Wall time: \(.wall_ms / 1000) s (from the first agent start to the last agent exit).",
      "- Landed: \(.landed). Rerouted by policy after merging the trunk (wayglass sync): \(.rerouted_after_sync). Land retries: busy land slot \(.land_retries.busy_slot), trunk moved \(.land_retries.trunk_moved), trunk merged into an intent \(.land_retries.trunk_merges).",
      "- Decisions by action: \(.decisions_by_action | to_entries | map("\(.key) \(.value)") | join(", ")). By status: \(.decisions_by_status | to_entries | map("\(.key) \(.value)") | join(", ")).",
      "- Overlaps: \(.overlaps.agents_warned_on_push) agent(s) warned on push; \(.overlaps.open_pairs_now) open pair(s) at the end.",
      "- HTTP errors by status: \(if (.http | length) == 0 then "none" else (.http | to_entries | map("\(.key) x\(.value)") | join(", ")) end) (4xx are expected refusals and busy land slots); 5xx: \(.http_5xx); network errors: \(.network_errors).",
      "- Artifacts operations, Worker meter (`GET /v1/cost`): \(.artifacts_ops.run) (\(.artifacts_ops.per_agent) per agent; \(.artifacts_ops.fork_calls) fork calls for \(.agents) forks; any extra are retries of INTERNAL_ERROR). Git commands the script ran itself (fork fetches, merges): \(.client_git_commands); the git traffic of wayglass push, sync and land is not counted here, and the Worker cannot see any of it.",
      "",
      "| Step | n | p50 ms | p95 ms | max ms |",
      "|---|---|---|---|---|",
      (.latency_ms | to_entries[] | "| \(.key) | \(.value.n) | \(.value.p50) | \(.value.p95) | \(.value.max) |"),
      "",
      "Rule-breakers caught: \(.breakers.caught) of \(.breakers.expected).",
      "",
      "| Kind | Agent | Caught | What happened |",
      "|---|---|---|---|",
      (.breakers.rows[] | "| \(.role) | \(.agent) | \(if .ok then "yes" else "no" end) | \(.note | gsub("\\|"; "/")) |"),
      (if (.failures | length) > 0 then "", "Agents that did not end as expected:", "", (.failures[] | "- \(.agent) (\(.role)): \(.note)") else empty end)
    ' <<<"$summary"
    printf '\n`land_total` is the time from an agent'"'"'s first land attempt to its landed change, retries included; `land` is one attempt. Changes routed to a person and `agent_review` changes were left pending.\n'
  } >"$log_path"
  say "Run log: $log_path"
fi

jq -e '.http_5xx == 0 and .breakers.caught == .breakers.expected and .agents_ok == .agents' <<<"$summary" >/dev/null
