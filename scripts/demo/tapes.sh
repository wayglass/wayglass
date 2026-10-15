#!/usr/bin/env bash
# Source for the tape helpers, or execute to render a fresh suite.
set -euo pipefail
set +x
TAPE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
export PATH="$TAPE_ROOT/bin:$PATH"

# Filter before writing to disk or the terminal, including command errors.
tape_redact() {
  jq 'walk(if type == "object" then del(.token, .key, .remote, .fork_remote, .trunk_remote)
    elif type == "string" then
      gsub("art_v[0-9]+_[a-z_]*[0-9a-f]{40}[^\\s\"]*"; "[repo token removed]")
      | gsub("[a-fA-F0-9]{32}\\.artifacts\\.cloudflare\\.net"; "[account].artifacts.cloudflare.net")
      | gsub("wgk_[0-9a-f]+"; "[key removed]")
      | if (env.WAYGLASS_KEY // "") != "" then split(env.WAYGLASS_KEY) | join("[key removed]") else . end
    else . end)'
}

# Each output shown is selected from a successful real CLI response.
# Policy exit 4 is expected for human approval and quarantine.
safe() {
  local raw rc=0
  raw=$("$@" --json 2>/dev/null) || rc=$?
  if [[ $rc != 0 && $rc != 4 ]]; then
    printf '%s' "$raw" | tape_redact >&2
    return "$rc"
  fi
  printf '%s' "$raw" | tape_redact > "$TAPE_WORK/$TAPE_SLOT.json"
  jq -e '.ok == true' "$TAPE_WORK/$TAPE_SLOT.json" >/dev/null
  case "$TAPE_SLOT" in
    init) jq '.data | {project: .project.name, jurisdiction: .project.jurisdiction, public, pushed}' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    *.declare) jq -r '.data.intent | "intent: \(.id)  agent: \(.agent)", "goal: \(.goal)", "paths: \(.declared_paths | join(", "))"' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    *.push) jq '.data | {intent_id, files, overlaps: [.overlaps[]? | {intent_id, paths, replan}]}' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    *.propose) jq -r '.data | "area: \(.preview.tags.areas | join(", "))", "data class: \(.preview.tags.data_classes | join(", "))", "risk: \(.preview.tags.risk)   jurisdiction: \(.preview.tags.jurisdiction)", "rule: \(.decision.matched_rule) -> \(.decision.action) (\(.decision.status))", "reason: \(.decision.reason)"' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    *.land) jq '.data | {commit, verified, note_written}' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    share) jq '.data | {url, notes: .session.notes}' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    fork) jq '.data | {intent: (.intent | {id, agent, parent_id}), notes_fetched, context_file}' "$TAPE_WORK/$TAPE_SLOT.json" ;;
    *) jq '.data' "$TAPE_WORK/$TAPE_SLOT.json" ;;
  esac
}

# Each tape agent acts with its own agent key, minted by tape_render with the
# admin key and passed down through the environment (never a file or argv);
# the admin key stays with the person's steps (init, watch, review, audit).
tape_key() {
  local v="TAPE_KEY_${1^^}"
  [[ -n "${!v:-}" ]] || { echo "no agent key for tape-$1: run scripts/demo/tapes.sh render" >&2; return 1; }
  printf '%s' "${!v}"
}

tape_identity() {
  git -C "$1" config user.name "$2"
  git -C "$1" config user.email "$2@agents.wayglass.invalid"
  git -C "$1" config commit.gpgsign false
}

tape_init() {
  cd "$TAPE_WORK/seed"
  export WAYGLASS_AGENT=tape-owner
  TAPE_SLOT=init
  printf '+ wayglass init --project %s --jurisdiction eu --public\n' "$TAPE_PROJECT"
  safe wayglass init --project "$TAPE_PROJECT" --jurisdiction eu --public
}

tape_declare() (
  local agent=$1 goal reason paths
  cd "$TAPE_WORK/$agent"
  local actor="tape-$agent"
  TAPE_SLOT="$agent.declare"
  case "$agent" in
    docs) goal='Document sign-in'; reason='The getting-started page needs sign-in instructions'; paths=(--path 'docs/**') ;;
    auth) goal='Reject empty logins'; reason='An empty username must not pass the login check'; paths=(--path 'src/auth/**' --path docs/auth.md) ;;
    pii) goal='Store profile birth dates'; reason='Age checks would need a date of birth; policy must review this'; paths=(--path 'src/profile/**') ;;
    *) return 1 ;;
  esac
  printf '+ [%s] wayglass intent declare --goal "%s"\n' "$actor" "$goal"
  WAYGLASS_AGENT="$actor" WAYGLASS_KEY="$(tape_key "$agent")" safe wayglass intent declare --goal "$goal" --reason "$reason" "${paths[@]}"
)

tape_push() (
  local agent=$1
  cd "$TAPE_WORK/$agent"
  local actor="tape-$agent"
  case "$agent" in
    docs) printf '\nOpen the app and sign in with your username.\n' >> docs/getting-started.md ;;
    auth) printf 'export const login = (user) => user.trim().length > 0;\n' > src/auth/login.mjs
          printf '# Authentication\n\nEmpty usernames are rejected.\n' > docs/auth.md ;;
    pii) mkdir -p src/profile; printf 'export const profile = { date_of_birth: "2000-01-01" };\n' > src/profile/customer.mjs ;;
  esac
  git add docs src
  git commit -q -m "feat: $agent fixture for a repeatable policy recording"
  TAPE_SLOT="$agent.push"
  WAYGLASS_AGENT="$actor" WAYGLASS_KEY="$(tape_key "$agent")" safe wayglass push
)

tape_propose() (
  local agent=$1 expected
  cd "$TAPE_WORK/$agent"
  local actor="tape-$agent"
  TAPE_SLOT="$agent.propose"
  printf '+ [%s] wayglass propose\n' "$actor"
  WAYGLASS_AGENT="$actor" WAYGLASS_KEY="$(tape_key "$agent")" safe wayglass propose
  case "$agent" in docs) expected=auto_merge ;; auth) expected=human_approve ;; pii) expected=quarantine ;; esac
  jq -e --arg action "$expected" '.data.decision.action == $action' "$TAPE_WORK/$TAPE_SLOT.json" >/dev/null
)

tape_land_docs() (
  cd "$TAPE_WORK/docs"
  local actor=tape-docs
  TAPE_SLOT=docs.land
  printf '+ wayglass land --no-mirror\n'
  WAYGLASS_AGENT="$actor" WAYGLASS_KEY="$(tape_key docs)" safe wayglass land --no-mirror
  jq -e '.data.verified and .data.note_written' "$TAPE_WORK/docs.land.json" >/dev/null
)

tape_share() (
  cd "$TAPE_WORK/auth"
  local actor=tape-auth
  TAPE_SLOT=share
  WAYGLASS_AGENT="$actor" WAYGLASS_KEY="$(tape_key auth)" safe wayglass session share --note 'Empty usernames are rejected. Continue with whitespace input tests.'
)

tape_fork() (
  cd "$TAPE_WORK"
  local actor=tape-second
  TAPE_SLOT=fork
  WAYGLASS_AGENT="$actor" WAYGLASS_KEY="$(tape_key second)" safe wayglass session fork "$SESSION_URL" --dir "$TAPE_WORK/continued"
  jq -e --arg parent "$AUTH_ID" '.data.intent.parent_id == $parent' "$TAPE_WORK/fork.json" >/dev/null
)

tape_audit() {
  printf 'Website: %s/?project=%s\n' "$WAYGLASS_URL" "$TAPE_PROJECT"
  # Behind Cloudflare Access (api.wayglass.dev) there is no anonymous view: read the trail with the key, sent on stdin.
  printf '+ curl project audit (the trail the website shows)\n'
  printf 'authorization: Bearer %s\n' "$WAYGLASS_KEY" | curl -fsS -H @- "$WAYGLASS_URL/v1/audit?project=$TAPE_PROJECT" | tape_redact > "$TAPE_WORK/audit.json"
  jq -e '.ok and (.data.events | length > 0)' "$TAPE_WORK/audit.json" >/dev/null
  jq -r '.data.events[-8:][] | "\(.kind)  \(.actor)\n  intent: \(.intent_id // "-")"' "$TAPE_WORK/audit.json"
}

tape_need_init() {
  [[ -f "$TAPE_WORK/init.json" ]] || tape_init
  for agent in docs auth pii; do
    if [[ ! -d "$TAPE_WORK/$agent" ]]; then
      git clone -q "$TAPE_WORK/seed" "$TAPE_WORK/$agent"
      tape_identity "$TAPE_WORK/$agent" "tape-$agent"
    fi
  done
}

tape_need_push() {
  tape_need_init
  local agent p1 p2
  if [[ ! -f "$TAPE_WORK/docs.declare.json" || ! -f "$TAPE_WORK/auth.declare.json" ]]; then
    tape_declare docs & p1=$!
    tape_declare auth & p2=$!
    wait "$p1"; wait "$p2"
  fi
  [[ -f "$TAPE_WORK/pii.declare.json" ]] || tape_declare pii
  for agent in docs auth pii; do
    [[ -f "$TAPE_WORK/$agent.push.json" ]] || tape_push "$agent"
  done
}

tape_need_proposals() {
  tape_need_push
  local agent
  for agent in docs auth pii; do
    [[ -f "$TAPE_WORK/$agent.propose.json" ]] || tape_propose "$agent"
  done
}

tape_setup() {
  local step=$1 creds a
  export PS1='$ '
  unset PROMPT_COMMAND WAYGLASS_INTENT WAYGLASS_PROJECT WAYGLASS_FORK_TOKEN GIT_CONFIG_COUNT
  if [[ $step == watch-demo || $step == review-demo ]]; then return; fi
  : "${TAPE_WORK:?Run scripts/demo/tapes.sh render first}"
  # Only this fresh run's project is eligible for any mutation or approval.
  TAPE_PROJECT=$(cat "$TAPE_WORK/project")
  [[ $TAPE_PROJECT == eu-demo-test-tape-* ]] || return 1
  # Without its own key an agent step would fall back to the person's login.
  for a in docs auth pii second; do tape_key "$a" >/dev/null || return 1; done
  creds="${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}/credentials.yaml"
  export WAYGLASS_KEY=${WAYGLASS_KEY:-$(sed -n 's/^key: *//p' "$creds")}
  export WAYGLASS_URL=${WAYGLASS_URL:-$(sed -n 's/^url: *//p' "$creds")}
  : "${WAYGLASS_KEY:?Login first}" "${WAYGLASS_URL:?Login first}"
  case "$step" in
    init) cd "$TAPE_WORK/seed" ;;
    declare) tape_need_init ;;
    propose) tape_need_push ;;
    watch|review|why|session|audit) tape_need_proposals ;;
    *) return 1 ;;
  esac
  case "$step" in
    review)
      AUTH_ID=$(jq -r .data.intent.id "$TAPE_WORK/auth.declare.json")
      cd "$TAPE_WORK/auth"
      node --input-type=module -e 'import {login} from "./src/auth/login.mjs"; if (login("") || login("   ") || !login("alice")) process.exit(1)'
      # A person decides a change routed to a person: the admin key, not an agent's.
      export WAYGLASS_AGENT=codex-tape-tester ;;
    why) [[ -f "$TAPE_WORK/docs.land.json" ]] || tape_land_docs; cd "$TAPE_WORK/docs" ;;
    session)
      [[ -f "$TAPE_WORK/share.json" ]] || tape_share
      SESSION_URL=$(jq -r .data.url "$TAPE_WORK/share.json")
      AUTH_ID=$(jq -r .data.intent.id "$TAPE_WORK/auth.declare.json") ;;
    watch) cd "$TAPE_WORK/auth"; export WAYGLASS_AGENT=codex-tape-tester ;;
  esac
}

# Only the disposable project created by this render may be deleted.
tape_cleanup() {
  local rc=$? project
  trap - EXIT
  ak_revoke_all
  project=$(cat "$TAPE_WORK/project")
  if [[ $project == eu-demo-test-tape-* ]]; then
    if ak_as_admin _ak_wg project delete "$project" --confirm "$project" --json | jq -e '.ok' >/dev/null; then
      printf 'Deleted recording project %s; agent keys revoked.\n' "$project"
    else
      printf 'Could not delete recording project %s; retry project delete with --confirm.\n' "$project" >&2
      rc=1
    fi
  else
    echo 'Refusing cleanup of a project outside this recording run.' >&2
    rc=1
  fi
  exit "$rc"
}

tape_render() {
  local selection=${1:-all} resolution=${2:-720} name generated
  case "$resolution" in 720|1080) ;; *) echo 'Resolution must be 720 or 1080' >&2; return 2 ;; esac
  case "$selection" in
    all) names=(init declare propose watch review why session audit watch-demo review-demo ai-review) ;;
    live) names=(init declare propose watch review why session audit) ;;
    offline) names=(watch-demo review-demo ai-review) ;;
    init|declare|watch|propose|review|why|session|audit|watch-demo|review-demo|ai-review) names=("$selection") ;;
    *) echo 'Unknown tape selection' >&2; return 2 ;;
  esac
  cd "$TAPE_ROOT"
  for tool in vhs jq node git; do command -v "$tool" >/dev/null || return 1; done
  make build
  mkdir -p docs/tapes/out
  TAPE_WORK=$(mktemp -d "$TAPE_ROOT/docs/tapes/out/run.XXXXXX")
  export TAPE_WORK
  printf 'eu-demo-test-tape-%s-%s\n' "$(date +%s)" "${TAPE_WORK##*.}" | tr '[:upper:]' '[:lower:]' > "$TAPE_WORK/project"
  mkdir -p "$TAPE_WORK/seed/docs" "$TAPE_WORK/seed/src/auth"
  git init -q -b main "$TAPE_WORK/seed"
  tape_identity "$TAPE_WORK/seed" tape-owner
  printf '# Getting started\n\nInstall the app.\n' > "$TAPE_WORK/seed/docs/getting-started.md"
  printf 'export const login = (user) => true;\n' > "$TAPE_WORK/seed/src/auth/login.mjs"
  git -C "$TAPE_WORK/seed" add .
  git -C "$TAPE_WORK/seed" commit -q -m 'chore: seed a disposable terminal-recording project'
  if [[ $selection != offline && $selection != watch-demo && $selection != review-demo && $selection != ai-review ]]; then
    # shellcheck source=agent-keys.sh
    source "$TAPE_ROOT/scripts/demo/agent-keys.sh"
    ak_admin_load
    trap tape_cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM HUP
    local a k=""
    for a in docs auth pii second; do
      ak_mint k "tape-$a" "tapes $(cat "$TAPE_WORK/project")"
      export "TAPE_KEY_${a^^}=$k"
    done
    k=
  fi
  for name in "${names[@]}"; do
    generated="$TAPE_WORK/$name.tape"
    if [[ $resolution == 1080 ]]; then
      sed -e 's/Set Width 1280/Set Width 1920/' -e 's/Set Height 720/Set Height 1080/' \
        -e 's/Set FontSize 18/Set FontSize 27/' -e 's/Set Padding 16/Set Padding 24/' \
        -e "s|out/$name\.|out/$name-1080.|g" "docs/tapes/$name.tape" > "$generated"
    else
      cp "docs/tapes/$name.tape" "$generated"
    fi
    vhs validate "$generated"
    # Record the terminal palette even when the calling agent disables colour.
    env -u VHS_PUBLISH -u NO_COLOR -u CLICOLOR TERM=xterm-256color COLORTERM=truecolor vhs "$generated"
  done
  printf 'Rendered %s at %sp. Local state: %s\n' "$selection" "$resolution" "$TAPE_WORK"
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  case ${1:-} in
    render) shift; tape_render "$@" ;;
    validate) cd "$TAPE_ROOT"; vhs validate docs/tapes/*.tape ;;
    *) echo 'Usage: scripts/demo/tapes.sh render [all|offline|tape-name] [720|1080] | validate' >&2; exit 2 ;;
  esac
fi
