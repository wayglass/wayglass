#!/usr/bin/env bash
# Waits for a routed change to be approved, then lands it.
#
# For agents whose `wayglass propose` said pending_agent or pending_human:
# retries `wayglass land --json` every 20 s until the Worker lets it land,
# refuses it for good, or the time runs out. It never approves anything.
#
# Usage: wayglass-land-when-approved [intent-id] [max-seconds]
#   intent-id    default: this checkout's current intent
#   max-seconds  default 110 (under the 2-minute default timeout of agent shells)
#
# Prints the last `wayglass land --json` envelope. Exit codes:
#   0   landed
#   3   rejected or blocked: stop
#   4   quarantined: stop, a data protection review decides
#   75  still waiting when time ran out: run it again
#   1   any other error (the envelope says which)
set -uo pipefail

id=${1:-}
max=${2:-110}
wg=${WAYGLASS_BIN:-wayglass}
deadline=$(( $(date +%s) + max ))
args=(land --json)
[[ -n "$id" ]] && args+=("$id")

while :; do
  out=$("$wg" "${args[@]}" 2>/dev/null)
  rc=$?
  if [[ $rc -eq 0 ]]; then
    printf '%s\n' "$out"
    exit 0
  fi
  msg=$(printf '%s' "$out" | jq -r '.error.message // ""' 2>/dev/null)
  case "$msg" in
    *" is rejected"* | *" is blocked"*)
      printf '%s\n' "$out"; exit 3 ;;
    *" is quarantined"*)
      printf '%s\n' "$out"; exit 4 ;;
    *"must be approved before landing"* | *" first"* | *"is landing until"*)
      ;; # not yet: a reviewer, a person, a parent intent or another land
    *)
      printf '%s\n' "$out"; exit 1 ;;
  esac
  if [[ $(date +%s) -ge $deadline ]]; then
    printf '%s\n' "$out"
    echo "still waiting: $msg" >&2
    exit 75
  fi
  echo "waiting: $msg" >&2
  sleep 20
done
