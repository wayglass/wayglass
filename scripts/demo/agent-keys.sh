# shellcheck shell=bash
# Per-agent keys for the demo scripts. Source it; do not run it.
#
# Every scripted or launched agent gets its own agent key (`wayglass keys
# create`, agent kind, never --human), so the Worker binds its name and
# refuses it on changes policy routed to a person (needs_human, exit 4).
# The admin key stays with the person: it mints and revokes the agent keys
# and is used for init, review, approve and audit reads.
#
# Keys live only in shell variables. They reach `wayglass` through the
# WAYGLASS_KEY environment variable of the one command or subshell that needs
# them, never through argv, a URL, a log or a file.
#
#   ak_admin_load                 read the admin key (WAYGLASS_KEY or credentials.yaml) into a private variable
#   ak_mint <var> <agent> <note>  mint an agent key for <agent>, store it in <var>, remember its id
#   ak_as_admin <cmd...>          run one command with the admin key
#   ak_revoke_all                 revoke every key this shell minted (call it from an EXIT trap)
#   ak_revoke_note <note>         revoke every live key whose note is <note> (cleanup after a crash)
#
# Needs: wayglass on PATH (or WAYGLASS_BIN), jq, WAYGLASS_URL set or a stored login.

_ak_admin=""
_ak_ids=()

_ak_wg() { "${WAYGLASS_BIN:-wayglass}" "$@"; }

ak_admin_load() {
  local creds
  creds="${WAYGLASS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/wayglass}/credentials.yaml"
  _ak_admin=${WAYGLASS_KEY:-$(sed -n 's/^key: *//p' "$creds" 2>/dev/null || true)}
  if [[ -z "$_ak_admin" ]]; then
    echo "agent keys: not logged in; run wayglass login (the admin key mints the agents' keys)" >&2
    return 1
  fi
  if [[ "$_ak_admin" == wgk_* ]]; then
    echo "agent keys: WAYGLASS_KEY is an agent key; minting agent keys needs the admin key" >&2
    return 1
  fi
}

ak_as_admin() { WAYGLASS_KEY="$_ak_admin" "$@"; }

ak_mint() { # <var> <agent> <note>
  local out key id
  [[ -n "$_ak_admin" ]] || ak_admin_load || return 1
  out=$(WAYGLASS_KEY="$_ak_admin" _ak_wg keys create --agent "$2" --note "$3" --json 2>/dev/null) || {
    echo "agent keys: could not mint a key for $2: $(printf '%s' "$out" | jq -r '.error.message // "no answer"' 2>/dev/null)" >&2
    return 1
  }
  key=$(printf '%s' "$out" | jq -r '.data.key // empty')
  id=$(printf '%s' "$out" | jq -r '.data.info.id // empty')
  out=
  if [[ "$key" != wgk_* || -z "$id" ]]; then
    echo "agent keys: unexpected answer minting a key for $2" >&2
    return 1
  fi
  _ak_ids+=("$id")
  printf -v "$1" '%s' "$key"
}

ak_revoke_all() {
  local id n=0
  # The ${arr[@]+...} form keeps an empty array from tripping `set -u` on
  # bash 3.2 (macOS), where the EXIT trap can run before anything was minted.
  for id in ${_ak_ids[@]+"${_ak_ids[@]}"}; do
    if WAYGLASS_KEY="$_ak_admin" _ak_wg keys revoke "$id" --json >/dev/null 2>&1; then
      n=$((n + 1))
    else
      echo "agent keys: could not revoke $id; revoke it with: wayglass keys revoke $id" >&2
    fi
  done
  [[ ${#_ak_ids[@]} -eq 0 ]] || echo "agent keys: revoked $n of ${#_ak_ids[@]}" >&2
  _ak_ids=()
}

ak_revoke_note() { # <note>
  local id n=0
  [[ -n "$_ak_admin" ]] || ak_admin_load || return 1
  for id in $(WAYGLASS_KEY="$_ak_admin" _ak_wg keys list --json | jq -r --arg n "$1" '.data.keys[] | select(.note == $n and .revoked_at == null) | .id'); do
    WAYGLASS_KEY="$_ak_admin" _ak_wg keys revoke "$id" --json >/dev/null && n=$((n + 1))
  done
  echo "agent keys: revoked $n live key(s) noted \"$1\""
}
