#!/usr/bin/env bash
# Acme Shop examples: real Claude Code and Codex sessions working on one
# sample app at the same time, through Wayglass, against the deployed Worker
# and real Artifacts.
#
# It creates two public demo projects seeded with examples/acme-shop/app:
#   <prefix>-<unix>      jurisdiction us  (tasks 01, 02, 04-08)
#   eu-<prefix>-<unix>   jurisdiction eu  (task 03, personal data)
# gives every task in examples/acme-shop/tasks its own clone, git identity
# and WAYGLASS_AGENT, and writes one launcher per task plus a reviewer.
# Then it prints how to start each agent. It never starts an LLM itself.
#
# With --standins it instead runs scripted stand-ins that make the same
# changes as the briefs (scripts/demo/acme-standins.sh), plus a scripted
# reviewer for agent_review changes, and checks every task routes the way its
# brief says. Stand-ins only run on throwaway projects (prefix demo-test-*):
# the scripted reviewer approves agent_review changes, which must never
# happen on a project a person will demo. Changes routed to a person are
# never approved by this script.
#
# Keys: every agent (each launcher, each stand-in, the reviewer) gets its
# own agent key, minted with the person's admin key when it starts and
# revoked when it exits, so the Worker refuses it on changes routed to a
# person (needs_human, exit 4). The admin key stays with the person for
# init, review and approve. Keys never reach a file, argv or a log. If a
# launcher was killed before it could revoke its key:
#   scripts/demo/examples.sh --revoke-keys <us-project>
#
# Usage:
#   scripts/demo/examples.sh                         # demo-acme-<unix>, print launch commands
#   DEMO_PREFIX=demo-test-acme scripts/demo/examples.sh --standins   # dry run
#   scripts/demo/examples.sh --revoke-keys demo-acme-<unix>          # revoke leftover agent keys
# Environment (all optional):
#   DEMO_PREFIX   project name prefix (default demo-acme; must start with demo-)
#   DEMO_DIR      where clones, prompts and launchers go (default ~/wayglass-examples/<project>)
#   WAYGLASS_URL, WAYGLASS_KEY   default: from ~/.config/wayglass/credentials.yaml
#
# Cost: 2 trunks + 8 forks (one per task), about 30 Worker-minted tokens
# with stand-ins (fewer with real agents that do not land), and a few hundred
# Artifacts operations. Billing starts Oct 14, 2026.
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
kit="$repo_root/examples/acme-shop"
standins=0
[[ "${1:-}" == "--standins" ]] && standins=1
if [[ "${1:-}" == "--revoke-keys" ]]; then
  : "${2:?usage: $0 --revoke-keys <us-project>}"
  command -v wayglass >/dev/null || export PATH="$HOME/wayglass-examples/$2/bin:$PATH"
  # shellcheck source=agent-keys.sh
  source "$repo_root/scripts/demo/agent-keys.sh"
  ak_revoke_note "examples $2"
  exit
fi
prefix=${DEMO_PREFIX:-demo-acme}
ts=$(date +%s)
us="$prefix-$ts"
eu="eu-$prefix-$ts"
dir=${DEMO_DIR:-$HOME/wayglass-examples/$us}
creds="${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}/credentials.yaml"

[[ "$prefix" == demo-* ]] || { echo "DEMO_PREFIX must start with demo- (public view and cleanup rely on it)" >&2; exit 2; }
if [[ $standins -eq 1 && "$prefix" != demo-test-* ]]; then
  echo "--standins approves agent_review changes as a scripted reviewer; use DEMO_PREFIX=demo-test-... for it" >&2
  exit 2
fi
for t in git go jq python3; do
  command -v "$t" >/dev/null || { echo "$t is required" >&2; exit 1; }
done
# Agents may start login shells, which rebuild PATH. Check before creating
# projects, while the temporary demo bin directory is not on PATH yet.
if [[ $standins -eq 0 ]] && ! "${SHELL:-/bin/bash}" -lc 'wayglass version --json' >/dev/null 2>&1; then
  echo 'Install wayglass on your login-shell PATH before launching agents (see docs/EVALUATING.md, A1).' >&2
  exit 1
fi
if [[ -z "${WAYGLASS_KEY:-}" ]]; then WAYGLASS_KEY=$(sed -n 's/^key: *//p' "$creds" 2>/dev/null || true); fi
if [[ -z "${WAYGLASS_URL:-}" ]]; then WAYGLASS_URL=$(sed -n 's/^url: *//p' "$creds" 2>/dev/null || true); fi
: "${WAYGLASS_KEY:?not logged in: run wayglass login or set WAYGLASS_KEY}"
: "${WAYGLASS_URL:?not logged in: run wayglass login or set WAYGLASS_URL}"
export WAYGLASS_URL WAYGLASS_KEY
unset WAYGLASS_INTENT WAYGLASS_PROJECT WAYGLASS_FORK_TOKEN GIT_CONFIG_COUNT

mkdir -p "$dir"/{bin,agents,prompts,launch,out}
bin="$dir/bin"
(cd "$repo_root" && go build -o "$bin/wayglass" ./cmd/wayglass)
install -m 0755 "$repo_root/scripts/demo/land-when-approved.sh" "$bin/wayglass-land-when-approved"
export PATH="$bin:$PATH"

# Tasks: id, agent name, default CLI, project (us|eu), expected action/status/exit/rule.
# The expectations are what each brief promises; --standins checks them.
tasks=(
  "01-docs     claude-docs      claude us auto_merge    approved      0 docs-only"
  "02-auth     codex-auth       codex  us human_approve pending_human 4 auth-boundary"
  "03-eu-pii   claude-billing   claude eu quarantine    quarantined   4 eu-personal-data"
  "04-infra    codex-infra      codex  us multi_approve pending_human 4 infra-two-people"
  "05-tests    claude-tests     claude us agent_review  pending_agent 0 tests-only"
  "06-secret   codex-payments   codex  us block         blocked       3 no-secrets"
  "07-search   claude-search    claude us agent_review  pending_agent 0 routine-app"
  "08-category claude-category  claude us agent_review  pending_agent 0 routine-app"
)

# Strips token values before JSON reaches disk.
redact() { jq 'del(.data.token.value) | walk(if type == "string" then gsub("art_v[0-9]+_[a-z_]*[0-9a-f]{40}[^\" ]*"; "art_REDACTED") else . end)'; }

identity() { # <dir> <name>
  git -C "$1" config user.name "$2"
  git -C "$1" config user.email "$2@agents.wayglass.invalid"
  git -C "$1" config commit.gpgsign false
}

seed() { # <dir> <project> <jurisdiction>
  local d=$1
  mkdir -p "$d"
  cp -R "$kit/app/." "$d/"
  git init -q -b main "$d"
  identity "$d" acme-seed
  git -C "$d" add -A
  git -C "$d" commit -q -m "feat: seed Acme Shop

The sample shop the demo agents change: catalog, sign-in, EU invoicing,
infra and docs, each with tests, and the policy that routes their changes."
  (cd "$d" && "$bin/wayglass" init --project "$2" --jurisdiction "$3" --public --json) | redact >"$dir/out/init-$2.json"
  if ! jq -e '.ok and .data.pushed == "main"' "$dir/out/init-$2.json" >/dev/null; then
    echo "init $2 failed:" >&2
    jq -r '.error.message // .' "$dir/out/init-$2.json" >&2
    exit 1
  fi
  printf '  %-28s %s, public %s\n' "$2" "$(jq -r .data.jurisdiction "$dir/out/init-$2.json")" "$(jq -r .data.public "$dir/out/init-$2.json")"
}

echo "Acme Shop examples on $WAYGLASS_URL"
echo "Directory $dir"
echo
echo "Projects:"
seed "$dir/trunk-us" "$us" us
seed "$dir/trunk-eu" "$eu" eu

claude_tools='Read Edit Write Glob Grep Bash(wayglass:*) Bash(wayglass-land-when-approved:*) Bash(go:*) Bash(gofmt:*) Bash(jq:*) Bash(sleep:*) Bash(git status:*) Bash(git diff:*) Bash(git log:*) Bash(git add:*) Bash(git commit:*) Bash(git merge:*) Bash(git show:*) Bash(git worktree:*) Bash(mktemp:*)'
gocache=$(go env GOCACHE)
# The person's login stays out of the agents' reach in Claude Code too.
creds_dir=$(dirname "$creds")
claude_deny="Read(/${creds_dir}/**) Edit(/${creds_dir}/**)"
key_note="examples $us"

# Each launcher mints its agent's own key when it starts, from the person's
# login, and revokes it when the agent exits. The key is never in the
# launcher file: it lives in the launcher's environment only, and the agent's
# wayglass reads it from WAYGLASS_KEY with an empty config dir, so it never
# falls back to the person's admin login.
launcher() { # <id> <agent> <cli> <clone dir> <prompt file>
  local f="$dir/launch/$1.sh"
  cat >"$f" <<EOF
#!/usr/bin/env bash
# $1: $2. Usage: $1.sh [claude|codex] [interactive|print]
set -euo pipefail
cd "$4"
export PATH="$bin:\$PATH" WAYGLASS_AGENT="$2" WAYGLASS_URL="$WAYGLASS_URL"
unset WAYGLASS_INTENT WAYGLASS_PROJECT WAYGLASS_FORK_TOKEN
cli=\${1:-$3}
mode=\${2:-interactive}
case "\$cli/\$mode" in claude/interactive | claude/print | codex/interactive | codex/print) ;; *)
  echo "usage: \$0 [claude|codex] [interactive|print]" >&2; exit 2 ;; esac
# The agent's own key: minted now with the person's admin key, revoked on exit.
# shellcheck source=/dev/null
source "$repo_root/scripts/demo/agent-keys.sh"
ak_admin_load
trap ak_revoke_all EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
ak_mint agent_key "$2" "$key_note"
echo "$2: own agent key minted (agent kind; revoked when this session ends)" >&2
export WAYGLASS_KEY="\$agent_key" WAYGLASS_CONFIG_DIR="$dir/agents/.config-$1"
unset agent_key
mkdir -p "\$WAYGLASS_CONFIG_DIR"
# The model family travels with every declare and approval, so policy can
# require a reviewer from another family than the author.
case "\$cli" in claude) export WAYGLASS_AGENT_FAMILY=claude ;; codex) export WAYGLASS_AGENT_FAMILY=openai ;; esac
prompt=\$(cat "$5")
# Wayglass commits and fetches inside this clone; workspace-write otherwise
# protects .git even though source files are writable.
git_dir=\$(git rev-parse --absolute-git-dir)
# Not exec: this shell stays to revoke the key when the agent exits.
case "\$cli/\$mode" in
  claude/interactive) claude --permission-mode acceptEdits --allowedTools "$claude_tools" --disallowedTools "$claude_deny" -- "\$prompt" ;;
  # Print mode cannot ask permission for shell commands composed by the agent.
  claude/print) claude -p --permission-mode acceptEdits --allowedTools "Read Edit Write Glob Grep Bash" --disallowedTools "$claude_deny" -- "\$prompt" ;;
  codex/interactive) codex --sandbox workspace-write -c sandbox_workspace_write.network_access=true --add-dir "$gocache" --add-dir "\$git_dir" "\$prompt" ;;
  codex/print) codex exec --sandbox workspace-write -c sandbox_workspace_write.network_access=true --add-dir "$gocache" --add-dir "\$git_dir" "\$prompt" ;;
esac
EOF
  chmod +x "$f"
}

for line in "${tasks[@]}"; do
  read -r id agent cli where _ <<<"$line"
  src="$dir/trunk-$where"
  git clone -q "$src" "$dir/agents/$id"
  identity "$dir/agents/$id" "$agent"
  { cat "$kit/tasks/PROTOCOL.md"; printf '\n---\n\n'; cat "$kit/tasks/$id.md"; } >"$dir/prompts/$id.md"
  launcher "$id" "$agent" "$cli" "$dir/agents/$id" "$dir/prompts/$id.md"
done
git clone -q "$dir/trunk-us" "$dir/agents/reviewer"
identity "$dir/agents/reviewer" codex-reviewer
cp "$kit/tasks/reviewer.md" "$dir/prompts/reviewer.md"
launcher reviewer codex-reviewer codex "$dir/agents/reviewer" "$dir/prompts/reviewer.md"

cat >"$dir/env.sh" <<EOF
# source $dir/env.sh
export PATH="$bin:\$PATH"
export ACME_US="$us" ACME_EU="$eu" ACME_DIR="$dir"
EOF
cat >"$dir/watch-us.sh" <<EOF
#!/usr/bin/env bash
cd "$dir/trunk-us" && exec "$bin/wayglass" watch "\$@"
EOF
cat >"$dir/watch-eu.sh" <<EOF
#!/usr/bin/env bash
cd "$dir/trunk-eu" && exec "$bin/wayglass" watch "\$@"
EOF
chmod +x "$dir/watch-us.sh" "$dir/watch-eu.sh"

if [[ $standins -eq 0 ]]; then
  cat <<EOF

Ready. Each agent has its own clone in $dir/agents/<task> and its own name.
Each launcher mints the agent's own key when it starts (agent kind, so it is
refused on changes routed to a person) and revokes it when the agent exits.
Leftover keys after a crash: $repo_root/scripts/demo/examples.sh --revoke-keys $us

Watch (one pane each):
  $dir/watch-us.sh                 # the US swarm; enter opens an intent, ? shows keys
  $dir/watch-eu.sh                 # the EU project
  Desktop app: add projects $us and $eu with +
  Public view: $WAYGLASS_URL/?project=$us
               $WAYGLASS_URL/?project=$eu

Start the agents (interactive by default; add 'print' for headless, or
swap the CLI: $dir/launch/02-auth.sh claude):

  task         agent            cli     expected route
EOF
  for line in "${tasks[@]}"; do
    read -r id agent cli where action status exit rule <<<"$line"
    printf '  %-12s %-16s %-7s %s -> %s (exit %s)%s\n' "$id" "$agent" "$cli" "$rule" "$action" "$exit" \
      "$([[ $where == eu ]] && echo ", EU project")"
  done
  cat <<EOF
  reviewer     codex-reviewer   codex   decides agent_review changes (05, 07, 08), written by Claude

  $dir/launch/01-docs.sh
  ...
  $dir/launch/reviewer.sh

Start 08-category after 07-search has pushed (its brief waits up to 10 minutes).
Decide the person-routed changes yourself, with your own login (the admin key
or a key minted with --human), as yourself, not an agent name:
  cd $dir/trunk-us && $bin/wayglass review          # 02 auth, 04 infra (needs two people)
  cd $dir/trunk-eu && $bin/wayglass review          # 03 quarantined
EOF
  exit 0
fi

# ------------------------------------------------------------ stand-ins
# shellcheck source=acme-standins.sh
source "$repo_root/scripts/demo/acme-standins.sh"
out="$dir/out"

# Every stand-in and the scripted reviewer act with their own agent key, as
# the launchers do; the admin key stays with this script's person steps.
# shellcheck source=agent-keys.sh
source "$repo_root/scripts/demo/agent-keys.sh"
ak_admin_load
trap ak_revoke_all EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
declare -A agent_keys
for a in $(printf '%s\n' "${tasks[@]}" | awk '{print $2}') claude-examples-tester; do
  ak_mint k "$a" "$key_note"
  agent_keys[$a]=$k
done
k=
echo
echo "Agent keys: ${#agent_keys[@]} minted (agent kind, one per stand-in and the reviewer); revoked when this script exits"

goal_of() { sed -n 's/^  --goal "\(.*\)" \\$/\1/p' "$kit/tasks/$1.md" | head -1; }
reason_of() { sed -n 's/^  --reason "\(.*\)" \\$/\1/p' "$kit/tasks/$1.md" | head -1; }
paths_of() { grep -o -- '--path "[^"]*"' "$kit/tasks/$1.md" | sed 's/--path "\(.*\)"/\1/'; }

# standin <id> <agent> <family> [--on <parent>]: declare, edit, test, commit, push, propose.
standin() {
  local id=$1 agent=$2 family=$3; shift 3
  local d="$dir/agents/$id" args=() p iid rc=0
  while IFS= read -r p; do args+=(--path "$p"); done < <(paths_of "$id")
  cd "$d"
  export WAYGLASS_AGENT=$agent WAYGLASS_AGENT_FAMILY=$family WAYGLASS_KEY=${agent_keys[$agent]}
  "$bin/wayglass" intent declare --json --goal "$(goal_of "$id")" --reason "$(reason_of "$id")" "${args[@]}" "$@" \
    | redact >"$out/$id.declare.json" || { echo "declare failed" >"$out/$id.err"; return 1; }
  iid=$(jq -r .data.intent.id "$out/$id.declare.json")
  "edit_$id"
  if [[ "$id" != 01-docs && "$id" != 04-infra ]]; then
    if ! { go vet ./... >"$out/$id.test.log" 2>&1 && go test ./... >>"$out/$id.test.log" 2>&1; }; then
      echo "tests failed" >"$out/$id.err"; return 1
    fi
  fi
  git add -A
  git commit -q -F - <<EOF
$(goal_of "$id")

$(reason_of "$id")

Wayglass-Intent: $iid
EOF
  "$bin/wayglass" push --json >"$out/$id.push.json" 2>"$out/$id.push.err" || { echo "push failed" >"$out/$id.err"; return 1; }
  sleep 2
  "$bin/wayglass" propose --json >"$out/$id.propose.json" 2>"$out/$id.propose.err" || rc=$?
  echo "$rc" >"$out/$id.propose.rc"
  if [[ $rc -eq 0 && $(jq -r .data.decision.status "$out/$id.propose.json") == approved ]]; then
    "$bin/wayglass" land --json >"$out/$id.land.json" 2>/dev/null || true
  fi
}

echo
echo "Stand-ins: 7 agents in parallel, then 08 stacked on 07"
pids=()
family_of() { [[ $1 == codex ]] && echo openai || echo "$1"; }
for line in "${tasks[@]}"; do
  read -r id agent cli _ <<<"$line"
  [[ "$id" == 08-category ]] && continue
  (standin "$id" "$agent" "$(family_of "$cli")") >"$out/$id.log" 2>&1 &
  pids+=($!)
done

# 08 waits for 07's push, then stacks on it, as its brief says.
(
  cd "$dir/agents/08-category"
  # shellcheck disable=SC2030 # this subshell only
  export WAYGLASS_KEY=${agent_keys[claude-category]}
  parent=""
  for _ in $(seq 60); do
    parent=$("$bin/wayglass" intent list --json | jq -r '.data.intents[] | select((.goal | startswith("catalog: search")) and .head_commit != null) | .id' | head -1)
    [[ -n "$parent" ]] && break
    sleep 5
  done
  [[ -n "$parent" ]] || { echo "no pushed search intent" >"$out/08-category.err"; exit 1; }
  standin 08-category claude-category claude --on "$parent"
) >"$out/08-category.log" 2>&1 &
pids+=($!)
for p in "${pids[@]}"; do wait "$p" || true; done

# The scripted reviewer: reads each agent_review diff, checks it stays in the
# declared paths and its tests pass, then approves with what it checked.
echo "Scripted reviewer (claude-examples-tester, declared family openai) decides agent_review changes"
rv="$dir/agents/reviewer"
cd "$rv"
# shellcheck disable=SC2031 # the reviewer's key, from here on
export WAYGLASS_AGENT=claude-examples-tester WAYGLASS_AGENT_FAMILY=openai WAYGLASS_KEY=${agent_keys[claude-examples-tester]}

# The gate: an agent key never decides a change policy routed to a person.
# The reviewer tries the auth change and the quarantined EU change; the
# Worker must refuse both with needs_human (exit 4) and record nothing.
gate_rows=()
for id in 02-auth 03-eu-pii; do
  iid=$(jq -r '.data.intent.id // empty' "$out/$id.declare.json" 2>/dev/null || true)
  [[ -n "$iid" ]] || { gate_rows+=("$id FAIL no intent"); continue; }
  rc=0
  (cd "$dir/agents/$id" && "$bin/wayglass" approve "$iid" --json --reason "An agent key tries to decide a change routed to a person; the Worker must refuse it.") \
    >"$out/gate-$id.json" 2>/dev/null || rc=$?
  code=$(jq -r '.error.code // "approved (not refused!)"' "$out/gate-$id.json" 2>/dev/null || echo "no answer")
  if [[ $rc -eq 4 && $code == needs_human ]]; then
    gate_rows+=("$id PASS claude-examples-tester approve $iid -> $code, exit $rc")
  else
    gate_rows+=("$id FAIL claude-examples-tester approve $iid -> $code, exit $rc (want needs_human, exit 4)")
  fi
done

"$bin/wayglass" review --for agent --json >"$out/review-queue.json"
family_checked=""
for iid in $(jq -r '.data.queue[].intent.id' "$out/review-queue.json"); do
  "$bin/wayglass" diff "$iid" --json >"$out/diff-$iid.json"
  if [[ -z "$family_checked" ]]; then
    # The authors declared family claude; the Worker must refuse a claude reviewer.
    WAYGLASS_AGENT_FAMILY=claude "$bin/wayglass" approve "$iid" --json --reason "Same-family approval attempt; the Worker must refuse it." >"$out/same-family-$iid.json" 2>/dev/null || true
    family_checked="$iid: $(jq -r '.error.code // "approved (not refused!)"' "$out/same-family-$iid.json")"
  fi
  head=$(jq -r .data.head "$out/diff-$iid.json")
  files=$(jq -r '.data.files | join(", ")' "$out/diff-$iid.json")
  wt=$(mktemp -d "${TMPDIR:-/tmp}/acme-review.XXXX")
  git worktree add -q --detach "$wt/tree" "$head"
  if (cd "$wt/tree" && go vet ./... && go test ./...) >"$out/review-$iid.log" 2>&1; then
    "$bin/wayglass" approve "$iid" --json --reason "Read the diff ($files): it does what the goal says and stays in the declared paths; go vet and go test pass at ${head:0:12}." >"$out/approve-$iid.json" || true
  else
    "$bin/wayglass" reject "$iid" --json --reason "go test fails at ${head:0:12}; see the test output." >"$out/approve-$iid.json" || true
  fi
  git worktree remove --force "$wt/tree"
  rm -rf "$wt"
done

echo "Agents with approved reviews land (08 waits for 07)"
for id in 05-tests 07-search 08-category; do
  [[ -f "$out/$id.propose.json" ]] || continue
  a=$(printf '%s\n' "${tasks[@]}" | awk -v t="$id" '$1==t {print $2}')
  (cd "$dir/agents/$id" && WAYGLASS_AGENT_FAMILY=claude WAYGLASS_AGENT=$a WAYGLASS_KEY=${agent_keys[$a]} \
    "$bin/wayglass-land-when-approved" "" 300 >"$out/$id.land.json" 2>"$out/$id.land.err") || true
done

# ------------------------------------------------------------ route table
echo
printf '%-12s %-6s %-36s %-34s %-6s %s\n' task result "tags (area / data class / risk / jur.)" "rule -> action (status)" exit "landed / shadow"
pass=0 fail=0
table="$out/routes.md"
{
  echo "| Task | Project | Tags (area; data class; risk; jurisdiction) | Rule | Action | Status | Exit | Landed | Shadow |"
  echo "|---|---|---|---|---|---|---|---|---|"
} >"$table"
for line in "${tasks[@]}"; do
  read -r id agent cli where action status exit rule <<<"$line"
  f="$out/$id.propose.json"
  if [[ ! -f "$f" ]]; then
    printf '%-12s %-6s %s\n' "$id" FAIL "no propose output: $(cat "$out/$id.err" 2>/dev/null)"
    fail=$((fail + 1)); continue
  fi
  got_action=$(jq -r '.data.decision.action // "?"' "$f")
  got_status=$(jq -r '.data.decision.status // "?"' "$f")
  got_rule=$(jq -r '.data.decision.matched_rule // "?"' "$f")
  got_exit=$(cat "$out/$id.propose.rc")
  tags=$(jq -r '.data.preview.tags | "\(.areas | join("+") | if . == "" then "-" else . end) / \(.data_classes | join("+") | if . == "" then "-" else . end) / \(.risk) / \(.jurisdiction)"' "$f")
  shadow=$(jq -r '[.data.decision.detail.shadow_matches[]? | select(.preempts) | "\(.rule) would \(.action)"] | join("; ")' "$f")
  landed=$(jq -r 'if .ok then "landed " + (.data.commit[:12]) else "" end' "$out/$id.land.json" 2>/dev/null || true)
  ok=PASS
  [[ "$got_action/$got_status/$got_exit/$got_rule" == "$action/$status/$exit/$rule" ]] || ok=FAIL
  if [[ $ok == PASS ]]; then pass=$((pass + 1)); else fail=$((fail + 1)); fi
  printf '%-12s %-6s %-36s %-34s %-6s %s\n' "$id" "$ok" "$tags" "$got_rule -> $got_action ($got_status)" "$got_exit" "${landed:-not landed}${shadow:+; shadow: $shadow}"
  [[ $ok == FAIL ]] && echo "             want $rule -> $action ($status), exit $exit"
  echo "| $id | $where | $tags | \`$got_rule\` | $got_action | $got_status | $got_exit | ${landed:-no} | ${shadow:--} |" >>"$table"
done
echo
echo "Overlaps the agents were told about, with the suggested re-plan (from push --json):"
for line in "${tasks[@]}"; do
  read -r id _ <<<"$line"
  jq -r --arg t "$id" '.data.overlaps[]? | select(.replan) | "  \($t): with \(.intent_id): \(.replan.summary)"' "$out/$id.push.json" 2>/dev/null || true
done
stack=$(jq -r '"08 stacked on \(.data.intent.parent_id // "nothing"), fast-forwarded to \(.data.fast_forwarded // "-" | .[:12])"' "$out/08-category.declare.json" 2>/dev/null || true)
echo "Stack: $stack"
echo "Same-family approval of $family_checked"
echo "Agent keys on changes routed to a person:"
for row in "${gate_rows[@]}"; do
  read -r gid gres gdetail <<<"$row"
  printf '  %-12s %-6s %s\n' "$gid" "$gres" "$gdetail"
  if [[ $gres == PASS ]]; then pass=$((pass + 1)); else fail=$((fail + 1)); fi
done
why=$(cd "$dir/agents/01-docs" && "$bin/wayglass" why docs/api.md:3 --json 2>/dev/null | jq -r '.data.note | "\(.intent_id) \(.action) by \(.decided_by): \(.goal)"' || true)
echo "why docs/api.md:3 -> ${why:-no answer}"
for p in "$us" "$eu"; do
  echo "public audit view $p: HTTP $(curl -sS -o /dev/null -w '%{http_code}' "$WAYGLASS_URL/v1/public/projects/$p/audit" || echo 000) (302 or 403: the web app is behind Cloudflare Access; sign in to see it)"
done
echo
echo "$pass pass, $fail fail. Route table: $table. Outputs: $out"
echo "Public view: $WAYGLASS_URL/?project=$us"
[[ $fail -eq 0 ]]
