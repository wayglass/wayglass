#!/usr/bin/env bash
# Starts a linux/arm64 container as "a second machine" and, given a session
# URL, continues that session there: session fork, session show, an edit,
# push and propose. Without a URL it opens a shell with wayglass on PATH.
#
#   scripts/demo/second-machine/run.sh                 # interactive shell
#   scripts/demo/second-machine/run.sh <session-url>   # fork, show, edit, push, propose
#
# The person's login stays on the host. The agent in the container gets its
# own agent key, minted here with the person's admin key (from
# ~/.config/wayglass/credentials.yaml or WAYGLASS_KEY) and revoked when the
# container exits, so the Worker refuses it on changes routed to a person.
# WAYGLASS_URL and the agent key are forwarded by name (docker -e NAME), so
# no key appears in argv or the image. Leftover keys after a crash:
#   wayglass keys list   then   wayglass keys revoke <id>
#
# Environment (optional):
#   SECOND_MACHINE_NAME  container hostname, part of the default identity (default second-machine)
#   WAYGLASS_AGENT       the agent name its key is bound to (default agent@<hostname>)
#   PLATFORM             docker platform (default linux/arm64)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$here/../../.." && pwd)
platform=${PLATFORM:-linux/arm64}
name=${SECOND_MACHINE_NAME:-second-machine}
image=wayglass-second-machine
creds="${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}/credentials.yaml"

command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }

ctx=$(mktemp -d "${TMPDIR:-/tmp}/wayglass-second-machine.XXXX")
trap 'rm -rf "$ctx"' EXIT
cp "$here/Dockerfile" "$ctx/"
goarch=${platform#linux/}
(cd "$repo_root" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) -X main.commit=$(git rev-parse --short HEAD)" \
  -o "$ctx/wayglass" ./cmd/wayglass)
docker build -q --platform "$platform" -t "$image" "$ctx" >/dev/null

# The host's wayglass mints the container agent's key.
(cd "$repo_root" && go build -o "$ctx/host-wayglass" ./cmd/wayglass)
export WAYGLASS_BIN="$ctx/host-wayglass"
if [[ -z "${WAYGLASS_URL:-}" ]]; then WAYGLASS_URL=$(sed -n 's/^url: *//p' "$creds" 2>/dev/null || true); fi
: "${WAYGLASS_URL:?not logged in: run wayglass login or set WAYGLASS_URL and WAYGLASS_KEY}"
export WAYGLASS_URL
WAYGLASS_AGENT=${WAYGLASS_AGENT:-agent@$name}
# shellcheck source=SCRIPTDIR/../agent-keys.sh
source "$here/../agent-keys.sh"
ak_admin_load
trap 'ak_revoke_all; rm -rf "$ctx"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP
ak_mint agent_key "$WAYGLASS_AGENT" "second-machine $name"
export WAYGLASS_KEY="$agent_key" WAYGLASS_AGENT
agent_key=
echo "$WAYGLASS_AGENT: own agent key minted (agent kind; revoked when the container exits)" >&2

args=(--rm --platform "$platform" --hostname "$name" --name "$name-$$" -e WAYGLASS_URL -e WAYGLASS_KEY -e WAYGLASS_AGENT)

if [[ $# -eq 0 ]]; then
  # Not exec: this shell stays to revoke the key when the container exits.
  docker run -it "${args[@]}" "$image"
  exit
fi

# Non-interactive continuation of a shared session. Every step prints what
# it runs; tokens never reach the output (fork tokens go to git via env).
docker run -i "${args[@]}" -e SESSION_URL="$1" "$image" bash -s <<'EOF'
set -euo pipefail
run() { printf '\n$ %s\n' "$*"; "$@"; }
run uname -sm
run wayglass version
run git --version
printf '\n$ wayglass session fork %s --dir work --json\n' "$SESSION_URL"
wayglass session fork "$SESSION_URL" --dir work --json >/tmp/fork.json
jq '{ok, intent: .data.intent.id, parent: .data.parent, stacked_on: .data.intent.parent_id, base: .data.base, fork: .data.fork.name, head: .data.head,
     notes_fetched: .data.notes_fetched, token_scope: .data.token_scope, token_expires_at: .data.token_expires_at}' /tmp/fork.json
id=$(jq -r .data.intent.id /tmp/fork.json)
# Edit the first declared path that is a plain file, not a glob.
f=$(jq -r '.data.intent.declared_paths[] | select(test("[*?]") | not)' /tmp/fork.json | head -1)
f=${f:-HANDOFF.md}
cd work
git config user.name "$(id -un)@$(hostname)"
git config user.email "$(id -un)@$(hostname).invalid"
run wayglass session show
mkdir -p "$(dirname "$f")"
printf '\n<!-- continued on %s (%s) -->\n' "$(hostname)" "$(uname -m)" >>"$f"
run git add -A
run git commit -q -m "chore: continue the shared session on a second machine" \
  -m "Proves the code and the context move together: forked from a session URL, edited here." \
  -m "Wayglass-Intent: $id"
run wayglass push
rc=0; run wayglass propose || rc=$?
echo "propose exit $rc (4 = waiting for a person)"
EOF
