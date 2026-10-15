#!/usr/bin/env bash
# Runs scripts/selfhost.sh against stubbed `npx wrangler`, curl and wayglass
# (tests/selfhost/stubs), in a throwaway copy of the repo layout. Never
# contacts Cloudflare. Needs bash, node and openssl.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fails=0
pass() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; sed 's/^/      | /' "$work/out" | tail -n 25; fails=$((fails + 1)); }

# A copy of only what selfhost.sh reads, so it cannot pick up a real
# bin/wayglass or write into the checkout.
root=$work/root
mkdir -p "$root/scripts" "$root/worker/scripts" "$root/worker/node_modules/.bin" "$root/bin" "$work/path"
: >"$root/worker/node_modules/.bin/wrangler"   # the pinned toolchain is "installed", so npm ci never runs
cp "$repo/scripts/selfhost.sh" "$root/scripts/"
cp "$repo/worker/wrangler.jsonc" "$root/worker/"
cp "$repo/worker/scripts/selfhost-config.mjs" "$repo/worker/scripts/selfhost-access.mjs" "$root/worker/scripts/"
cp "$repo/tests/selfhost/stubs/wayglass" "$root/bin/"
cp "$repo/tests/selfhost/stubs/npx" "$repo/tests/selfhost/stubs/curl" "$work/path/"
chmod +x "$root/bin/wayglass" "$work/path/npx" "$work/path/curl"

export STUB_STATE=$work/state
export CLOUDFLARE_ACCOUNT_ID=0123456789abcdef0123456789abcdef
export CLOUDFLARE_API_TOKEN=stub-token-must-not-leak
export WAYGLASS_CONFIG_DIR=$work/config
export WAYGLASS_SELFHOST_POLL=0
export PATH="$work/path:$PATH"
unset CF_EVENTS_API_TOKEN WAYGLASS_QUEUE_ID STUB_FAIL

args=(--name wg-test --namespace wg-test --eu-namespace wg-test-eu --queue wg-test-events --public 'demo-test-*,eu-demo-test-*')

# run [ENV=VALUE...] -- ARGS...: runs selfhost.sh with stdin closed; sets rc.
run() {
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  rc=0
  env ${envs[@]+"${envs[@]}"} "$root/scripts/selfhost.sh" "$@" </dev/null >"$work/out" 2>&1 || rc=$?
  if grep -q "$CLOUDFLARE_API_TOKEN" "$work/out"; then rc=99; echo "API token printed" >>"$work/out"; fi
}
has() { grep -qF -- "$1" "$work/out"; }
none() { local g; for g in "$@"; do ! compgen -G "$STUB_STATE/$g" >/dev/null || return 1; done; }
called() { grep -qF -- "$1" "$STUB_STATE/calls"; }
reset() { rm -rf "$STUB_STATE" "$WAYGLASS_CONFIG_DIR"; mkdir -p "$STUB_STATE"; : >"$STUB_STATE/calls"; }

# 1. Dry run: no token needed, nothing created.
reset
run CLOUDFLARE_API_TOKEN= -- "${args[@]}" --dry-run
if [ "$rc" -eq 0 ] && has "Dry run done." && none 'ns-*'; then pass "dry run"; else fail "dry run (exit $rc)"; fi

# 2. Fresh account: no namespace, queue or Worker exists yet.
reset
run -- "${args[@]}" --yes
if [ "$rc" -eq 0 ] && has "created namespace wg-test (us)" && has "created namespace wg-test-eu (eu)" &&
   has "created queue wg-test-events" && grep -qE "queue wg-test-events: [0-9a-f]{32}$" "$work/out" &&
   has "Logged in to https://wg-test.test.workers.dev (key stored in $WAYGLASS_CONFIG_DIR/credentials.yaml" &&
   [ -f "$WAYGLASS_CONFIG_DIR/credentials.yaml" ] && [ -f "$STUB_STATE/worker-wg-test" ]; then
  pass "fresh account: creates namespaces and queue, deploys, logs in"
else fail "fresh account (exit $rc)"; fi
# The admin key reaches wrangler's secrets file and `wayglass login` on stdin only:
# never the output, the generated config, or anything left in the checkout.
admin_key=$(sed -n 's/^key: *//p' "$WAYGLASS_CONFIG_DIR/credentials.yaml")
gen=$root/worker/wrangler.selfhost.jsonc
if [ "${#admin_key}" -eq 64 ] && ! grep -q "$admin_key" "$work/out" && ! grep -q "$admin_key" "$gen" &&
   ! grep -rq "$admin_key" "$root" && [ "$(stat -c %a "$WAYGLASS_CONFIG_DIR/credentials.yaml" 2>/dev/null || stat -f %Lp "$WAYGLASS_CONFIG_DIR/credentials.yaml")" = 600 ]; then
  pass "fresh account: the admin key is never printed or written outside credentials.yaml"
else fail "admin key exposure (key length ${#admin_key})"; fi

# 3. Rerun: everything exists; nothing is created again.
: >"$STUB_STATE/calls"
run -- "${args[@]}" --yes
if [ "$rc" -eq 0 ] && has "namespace wg-test exists (us)" && has "namespace wg-test-eu exists (eu)" &&
   ! has "created" && ! called "POST /artifacts/namespaces" && ! called "wrangler queues create" &&
   called "secrets admin_key=new" && [ "$(sed -n 's/^key: *//p' "$WAYGLASS_CONFIG_DIR/credentials.yaml")" != "$admin_key" ]; then
  pass "rerun: reuses namespaces and queue, rotates the admin key"
else fail "rerun (exit $rc)"; fi

# 3a. --keep-key: redeploys with no admin key in the secrets, skips login.
admin_key=$(sed -n 's/^key: *//p' "$WAYGLASS_CONFIG_DIR/credentials.yaml")
: >"$STUB_STATE/calls"
run -- "${args[@]}" --keep-key --yes
if [ "$rc" -eq 0 ] && called "secrets admin_key=kept" && ! called "wayglass login" && has "Admin key unchanged" &&
   [ "$(sed -n 's/^key: *//p' "$WAYGLASS_CONFIG_DIR/credentials.yaml")" = "$admin_key" ]; then
  pass "--keep-key: deploys without rotating the key and keeps the login"
else fail "--keep-key (exit $rc)"; fi

# 3b. --keep-key on an account with no Worker: the key was never set, so stop.
reset
run -- "${args[@]}" --keep-key --yes
if [ "$rc" -eq 1 ] && has "no Worker named wg-test exists" && ! called "POST" && ! called "wrangler deploy"; then
  pass "--keep-key without a Worker refused before anything is created"
else fail "--keep-key without a Worker (exit $rc)"; fi

# 3c. A bad name is refused by the config generator before any resource carries it.
reset
run -- --name Wg_Test --namespace wg-test --queue wg-test-events --yes
r1=$rc; o1=$(cat "$work/out")
run -- --name wg-test --namespace 'wg test' --queue wg-test-events --yes
if [ "$r1" -eq 1 ] && grep -qF 'WAYGLASS_WORKER_NAME "Wg_Test"' <<<"$o1" && [ "$rc" -eq 1 ] && has "is not a valid namespace name" &&
   has "nothing was created" && ! called "POST" && ! called "GET" && none 'ns-*' 'queue-*' 'worker-*'; then
  pass "bad --name or --namespace stops before any API call"
else fail "bad names (exit $r1/$rc)"; fi

# 4. A namespace with the wrong jurisdiction stops the run.
reset; printf us >"$STUB_STATE/ns-wg-test-eu"
run -- "${args[@]}" --yes
if [ "$rc" -eq 1 ] && has "exists with jurisdiction us, not eu"; then pass "jurisdiction mismatch refused"; else fail "jurisdiction mismatch (exit $rc)"; fi

# 5. Refused API token: a loud diagnostic, nothing created.
reset
run STUB_FAIL=api-auth -- "${args[@]}" --yes
if [ "$rc" -eq 1 ] && has "looking up namespace wg-test failed: HTTP 403 (the API token was refused" &&
   ! called "POST" && ! called "wrangler queues create"; then
  pass "auth failure on the API: stops with a diagnostic"
else fail "auth failure on the API (exit $rc)"; fi

# 6. Refused token on the queue lookup: not mistaken for a missing queue.
reset
run STUB_FAIL=queue-auth -- "${args[@]}" --yes
if [ "$rc" -eq 1 ] && has "Authentication error [code: 10000]" && has "could not look up queue wg-test-events" &&
   ! called "wrangler queues create" && ! called "wrangler deploy"; then
  pass "auth failure on the queue lookup: stops, creates nothing"
else fail "auth failure on the queue lookup (exit $rc)"; fi

# 7. No terminal and no --yes: refuses before creating anything.
reset
run -- "${args[@]}"
if [ "$rc" -eq 1 ] && has "rerun with --yes" && ! called "POST"; then pass "no terminal without --yes refused"; else fail "no terminal without --yes (exit $rc)"; fi

# 8. Uninstall: queue before Worker, verified by GETs, namespaces kept.
reset
run -- "${args[@]}" --yes
: >"$STUB_STATE/calls"
run -- --name wg-test --queue wg-test-events --uninstall --yes
qline=$(grep -n '^DELETE /queues/' "$STUB_STATE/calls" | cut -d: -f1 | head -n1 || true)
wline=$(grep -n '^DELETE /workers/scripts/wg-test' "$STUB_STATE/calls" | cut -d: -f1 | head -n1 || true)
if [ "$rc" -eq 0 ] && [ -n "$qline" ] && [ -n "$wline" ] && [ "$qline" -lt "$wline" ] &&
   has "deleted queue wg-test-events (verified" && has "deleted Worker wg-test (verified" &&
   has "Artifacts namespaces were not touched" && [ -f "$STUB_STATE/ns-wg-test" ] && [ -f "$STUB_STATE/ns-wg-test-eu" ] &&
   none 'queue-*' 'worker-*' && ! called "DELETE /artifacts"; then
  pass "uninstall: queue, then Worker; namespaces kept"
else fail "uninstall (exit $rc)"; fi

# 9. Uninstall again: nothing left, still succeeds.
run -- --name wg-test --queue wg-test-events --uninstall --yes
if [ "$rc" -eq 0 ] && has "queue wg-test-events does not exist" && has "Worker wg-test does not exist"; then
  pass "uninstall rerun: nothing to delete"
else fail "uninstall rerun (exit $rc)"; fi

# 10. Worker delete reports an error after the Worker is gone: the GET decides.
reset
run -- "${args[@]}" --yes
run STUB_FAIL=delete-late -- --name wg-test --queue wg-test-events --uninstall --yes
if [ "$rc" -eq 0 ] && has "deleted Worker wg-test (verified"; then pass "uninstall trusts the API state over the delete's error"; else fail "uninstall after a late error (exit $rc)"; fi

# 11. Uninstall without a terminal or --yes deletes nothing.
reset
run -- "${args[@]}" --yes
: >"$STUB_STATE/calls"
run -- --name wg-test --queue wg-test-events --uninstall
if [ "$rc" -eq 1 ] && has "rerun with --yes" && ! called "DELETE"; then pass "uninstall without --yes refused"; else fail "uninstall without --yes (exit $rc)"; fi

# 12. Cloudflare Access: both flags set the vars; one alone is refused; none leaves it off.
reset
run CLOUDFLARE_API_TOKEN= -- "${args[@]}" --dry-run --access-team https://acme.cloudflareaccess.com/ --access-aud abc123
if [ "$rc" -eq 0 ] && grep -q '"ACCESS_TEAM_DOMAIN": "acme.cloudflareaccess.com"' "$gen" && grep -q '"ACCESS_AUD": "abc123"' "$gen"; then
  pass "access flags set the Worker vars"
else fail "access flags (exit $rc)"; fi
reset
run CLOUDFLARE_API_TOKEN= -- "${args[@]}" --dry-run --access-team acme.cloudflareaccess.com --access-aud abc123 --access-web-aud 'web1, web2'
if [ "$rc" -eq 0 ] && grep -q '"ACCESS_WEB_AUD": "web1,web2"' "$gen" && grep -q '"run_worker_first": true' "$gen"; then
  pass "--access-web-aud sets the web gate audience"
else fail "access web aud (exit $rc)"; fi

run CLOUDFLARE_API_TOKEN= -- "${args[@]}" --dry-run --access-web-aud web1
if [ "$rc" -eq 1 ] && has "--access-web-aud needs --access-team" && none 'ns-*'; then pass "web aud without team refused"; else fail "web aud alone (exit $rc)"; fi

run CLOUDFLARE_API_TOKEN= -- "${args[@]}" --dry-run --access-team acme.cloudflareaccess.com
if [ "$rc" -eq 1 ] && has "--access-team and --access-aud go together" && none 'ns-*'; then pass "access team without aud refused"; else fail "access team alone (exit $rc)"; fi
reset
run CLOUDFLARE_API_TOKEN= -- "${args[@]}" --dry-run
if [ "$rc" -eq 0 ] && grep -q '"ACCESS_TEAM_DOMAIN": ""' "$gen" && grep -q '"ACCESS_AUD": ""' "$gen" && grep -q '"ACCESS_WEB_AUD": ""' "$gen"; then
  pass "no access flags: Access stays off"
else fail "access off by default (exit $rc)"; fi

# 13-21. --access: Cloudflare Access set up from the API token.
acc=(--name wg-test --namespace wg-test --queue wg-test-events --domain wg.example.com --access --allow 'you@example.com, @example.com')
# app EXPR: evaluates EXPR over the stub's Access apps (array `a`), prints the result.
app() { node -e 'const fs=require("fs"),d=process.argv[1];const a=fs.readdirSync(d).filter(f=>f.startsWith("app-")).map(f=>JSON.parse(fs.readFileSync(d+"/"+f,"utf8")));const by=n=>a.find(x=>x.name===n);process.stdout.write(String('"$1"'))' "$STUB_STATE"; }
idp() { local id; id=$(openssl rand -hex 16); printf '{"id":"%s","name":"%s","type":"%s"}' "$id" "$1" "$2" >"$STUB_STATE/idp-$id"; printf %s "$id"; }

reset
run -- "${acc[@]}" --yes
if [ "$rc" -eq 1 ] && has "has no Zero Trust organisation yet" && has "https://one.dash.cloudflare.com/" &&
   ! called "POST /access" && ! called "POST /artifacts" && ! called "wrangler deploy"; then
  pass "access: no Zero Trust org stops before anything is created"
else fail "access without an org (exit $rc)"; fi

reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run -- "${acc[@]}" --yes
if [ "$rc" -eq 0 ] && has "added the One-time PIN login" && [ "$(cat "$STUB_STATE"/idp-* | grep -c onetimepin)" = 1 ] &&
   [ "$(app 'a.length')" = 3 ] && has 'created Access app "wg-test web"' &&
   [ "$(app 'by("wg-test key API").policies[0].decision')" = bypass ] &&
   [ "$(app 'by("wg-test human sign-in").session_duration')" = 12h ] &&
   [ "$(app 'by("wg-test web").destinations.map(d=>d.uri).join(" ")')" = "wg.example.com wg.example.com/v1/public" ] &&
   [ "$(app 'JSON.stringify(by("wg-test web").policies[0].include)')" = '[{"email":{"email":"you@example.com"}},{"email_domain":{"domain":"example.com"}}]' ] &&
   grep -q "\"ACCESS_TEAM_DOMAIN\": \"acme.cloudflareaccess.com\"" "$gen" &&
   grep -q "\"ACCESS_AUD\": \"$(app 'by("wg-test human sign-in").aud')\"" "$gen" &&
   grep -q "\"ACCESS_WEB_AUD\": \"$(app 'by("wg-test web").aud')\"" "$gen" &&
   has "302 to acme.cloudflareaccess.com" && has "401 without a key"; then
  pass "access: no login method yet falls back to one-time PIN, creates three apps, deploys their AUDs, checks the redirect"
else fail "access with PIN fallback (exit $rc)"; fi

# Rerun: the same three apps are updated in place, policies included.
pol_before=$(app 'a.map(x=>x.policies.map(p=>p.id).join()).sort().join()')
aud_before=$(app 'by("wg-test web").aud')
: >"$STUB_STATE/calls"
run -- "${acc[@]}" --yes
if [ "$rc" -eq 0 ] && [ "$(app 'a.length')" = 3 ] && has 'updated Access app "wg-test web"' &&
   has 'updated Access app "wg-test human sign-in"' && has 'updated Access app "wg-test key API"' &&
   ! called "POST /access/apps" && ! called "POST /access/identity_providers" &&
   [ "$(app 'a.map(x=>x.policies.map(p=>p.id).join()).sort().join()')" = "$pol_before" ] &&
   [ "$(app 'a.every(x=>x.policies.length===1)')" = true ] && [ "$(app 'by("wg-test web").aud')" = "$aud_before" ]; then
  pass "access: rerun updates the apps and their policies instead of duplicating them"
else fail "access rerun (exit $rc)"; fi

# Existing login methods are reused; --idp narrows them; group: becomes per-IdP rules.
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
okta=$(idp Okta okta); gh=$(idp GitHub github)
run -- "${acc[@]}" --allow 'group:eng' --yes
if [ "$rc" -eq 0 ] && ! called "POST /access/identity_providers" &&
   [ "$(app 'by("wg-test web").allowed_idps.sort().join()')" = "$(printf '%s\n%s\n' "$okta" "$gh" | sort | paste -sd, -)" ] &&
   [ "$(app 'by("wg-test web").auto_redirect_to_identity')" = false ] &&
   [ "$(app 'JSON.stringify(by("wg-test human sign-in").policies[0].include.filter(r=>r.okta||r["github-organization"]).map(r=>Object.keys(r)[0]).sort())')" = '["github-organization","okta"]' ]; then
  pass "access: reuses every existing login method; group: matches each IdP's groups"
else fail "access reusing IdPs (exit $rc)"; fi
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
okta=$(idp Okta okta); idp GitHub github >/dev/null
run -- "${acc[@]}" --idp okta --yes
if [ "$rc" -eq 0 ] && [ "$(app 'by("wg-test human sign-in").allowed_idps.join()')" = "$okta" ] &&
   [ "$(app 'by("wg-test human sign-in").auto_redirect_to_identity')" = true ]; then
  pass "access: --idp picks one login method and redirects straight to it"
else fail "access --idp (exit $rc)"; fi
run -- "${acc[@]}" --idp 'Azure' --yes
if [ "$rc" -eq 1 ] && has '--idp "Azure" matches no login method' && has "Okta [okta]"; then pass "access: unknown --idp lists the login methods"; else fail "access unknown idp (exit $rc)"; fi

# Bad input is refused before anything is created.
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run -- "${acc[@]}" --allow 'not-an-email' --yes
r1=$rc; o1=$(cat "$work/out")
run -- "${acc[@]}" --allow 'group:eng' --yes
if [ "$r1" -eq 1 ] && grep -qF 'is not an email' <<<"$o1" && [ "$rc" -eq 1 ] && has "has groups" &&
   ! called "POST"; then
  pass "access: bad --allow and group: without a group-capable IdP are refused before any change"
else fail "access bad --allow (exit $r1/$rc)"; fi

# workers.dev: no --domain, so the apps protect <name>.<subdomain>.workers.dev.
wd=(--name wg-test --namespace wg-test --queue wg-test-events --access --allow you@example.com)
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run -- "${wd[@]}" --yes
if [ "$rc" -eq 0 ] && called "GET /workers/subdomain" && has "access host    wg-test.test.workers.dev (workers.dev)" &&
   [ "$(app 'by("wg-test web").destinations.map(d=>d.uri).join(" ")')" = "wg-test.test.workers.dev wg-test.test.workers.dev/v1/public" ] &&
   [ "$(app 'by("wg-test human sign-in").domain')" = "wg-test.test.workers.dev/v1/human" ] &&
   [ "$(app 'by("wg-test key API").domain')" = "wg-test.test.workers.dev/v1" ] &&
   grep -q "\"ACCESS_AUD\": \"$(app 'by("wg-test human sign-in").aud')\"" "$gen" &&
   grep -q '"workers_dev": true' "$gen" && has "302 to acme.cloudflareaccess.com"; then
  pass "access: workers.dev without --domain protects the Worker's workers.dev host"
else fail "access on workers.dev (exit $rc)"; fi
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run STUB_FAIL=no-subdomain -- "${wd[@]}" --yes
if [ "$rc" -eq 1 ] && has "no workers.dev subdomain yet" && has "--domain HOST" && ! called "POST"; then
  pass "access: no workers.dev subdomain stops with the fix"
else fail "access without a workers.dev subdomain (exit $rc)"; fi
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run STUB_FAIL=org-auth -- "${acc[@]}" --yes
if [ "$rc" -eq 1 ] && has "cannot read the Zero Trust organisation" && has "Organizations, Identity" &&
   has "Providers, and Groups > Read" && ! called "POST"; then
  pass "access: a token that cannot read the org names the missing permission"
else fail "access org read refused (exit $rc)"; fi
reset
run STUB_FAIL=org-disabled -- "${acc[@]}" --yes
if [ "$rc" -eq 1 ] && has "has no Zero Trust organisation yet" && has "https://one.dash.cloudflare.com/" &&
   ! has "Providers, and Groups > Read" && ! called "POST"; then
  pass "access: Access not enabled on the account says to create the org, not to fix the token"
else fail "access not enabled (exit $rc)"; fi

# Dry run with a token: reads and plans, writes nothing.
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run -- "${acc[@]}" --dry-run
if [ "$rc" -eq 0 ] && has 'access app     create "wg-test web"' && has "adds One-time PIN" &&
   ! called "POST" && ! called "PUT" && [ "$(app 'a.length')" = 0 ]; then
  pass "access: dry run prints the plan and changes nothing"
else fail "access dry run (exit $rc)"; fi

# The redirect check fails loudly when Access does not answer for the host.
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
run STUB_FAIL=no-redirect -- "${acc[@]}" --yes
if [ "$rc" -eq 1 ] && has "the Access check failed" && has "Expected 302 to https://acme.cloudflareaccess.com/"; then
  pass "access: a missing redirect fails the run"
else fail "access check failure (exit $rc)"; fi

# Uninstall deletes only the three apps; the org, login methods and other apps stay.
reset; printf acme.cloudflareaccess.com >"$STUB_STATE/org"
idp Okta okta >/dev/null
printf '{"id":"other","name":"someone else","aud":"x","policies":[],"destinations":[{"uri":"other.example.com"}]}' >"$STUB_STATE/app-other"
run -- "${acc[@]}" --yes
: >"$STUB_STATE/calls"
run -- --name wg-test --queue wg-test-events --uninstall --yes
if [ "$rc" -eq 0 ] && [ "$(app 'a.map(x=>x.name).join()')" = "someone else" ] &&
   has 'deleted Access app "wg-test web" (verified' && has 'deleted Access app "wg-test key API" (verified' &&
   [ -f "$STUB_STATE/org" ] && compgen -G "$STUB_STATE/idp-*" >/dev/null && ! called "DELETE /access/identity_providers" &&
   ! called "DELETE /access/organizations"; then
  pass "access: uninstall deletes only its three apps"
else fail "access uninstall (exit $rc)"; fi
reset
run -- "${args[@]}" --yes
run STUB_FAIL=access-auth -- --name wg-test --queue wg-test-events --uninstall --yes
if [ "$rc" -eq 0 ] && has "the token has no Access permission" && has "deleted Worker wg-test (verified"; then
  pass "access: uninstall without Access permissions still removes the Worker"
else fail "access uninstall without permission (exit $rc)"; fi

if [ "$fails" -gt 0 ]; then echo "$fails selfhost check(s) failed" >&2; exit 1; fi
echo "Selfhost checks passed"
