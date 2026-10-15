#!/usr/bin/env bash
# Day-one spike against a deployed Worker.
#
# Proves, on real Artifacts:
#   1. the Worker creates a trunk and returns remote + short-lived token
#   2. the Worker forks it for an intent with a write token for the fork only
#   3. git clones, commits and pushes with the token as an extra header
#   4. refs/notes/wayglass can be pushed and fetched back
#   5. a trunk push reaches the queue consumer as cf.artifacts.repo.pushed
#
# It creates one project (trunk repo) and one fork, so it costs Artifacts
# operations. Run it by hand; CI never does.
#
# Usage:
#   WAYGLASS_URL=https://<worker-host> WAYGLASS_KEY=<WAYGLASS_ADMIN_KEY> scripts/spike.sh
# Optional:
#   WAYGLASS_SPIKE_PROJECT=spike-1   project/trunk name (default spike-<unix time>)
#   WAYGLASS_SPIKE_EVENT_WAIT=5m     how long step 5 waits for the event
set -euo pipefail

cd "$(dirname "$0")/.."

: "${WAYGLASS_URL:?set WAYGLASS_URL to the deployed Worker URL}"
: "${WAYGLASS_KEY:?set WAYGLASS_KEY to the Worker admin key (never commit it)}"

command -v git >/dev/null || { echo "git is required" >&2; exit 1; }
command -v go >/dev/null || { echo "go is required" >&2; exit 1; }

echo "Spike against ${WAYGLASS_URL} with $(git version)"
echo "Copy the step results from the log into docs/ARCHITECTURE.md."
exec go test -tags spike -run '^TestSpike$' -count=1 -v -timeout 20m ./internal/api/
