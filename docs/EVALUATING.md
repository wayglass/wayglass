# Evaluating Wayglass

A test plan for checking Wayglass end to end from a clean machine, using only
this repository and public URLs. It is written so an AI agent can follow it
step by step, and a person can too. Every check gives the command, what it
should print, and what counts as a pass.

There are three tiers. Each one stands on its own; most evaluations can stop
after Tier A.

| Tier | What it proves | Needs | Time |
|---|---|---|---|
| **A** Offline | The code builds, the test suites pass, the TUIs and `--json` work, and the example policy routes each kind of change as documented | No Cloudflare account, no key, no network after downloads | 15 to 25 min (most of it is `go` and `npm` downloads) |
| **B** Self-host | The golden path on your own Worker and real Artifacts: parallel agents, overlap, classification, a person's approval, `why`, session fork, public view | A Cloudflare account on Workers Paid with Artifacts | 45 to 60 min |
| **C** Adversarial | The Zero Trust claims hold when someone tries to get around them | The Tier B deployment | 30 to 45 min |

Conventions used below:

- Commands are Bash. On macOS the system Bash (3.2) is fine for Tiers A to C;
  only the optional `scripts/demo/golden-path.sh` needs Bash 4 or later.
- `exit N` means the process exit code (`echo $?` right after the command).
  Run `set -o pipefail` in each shell first, so that `echo "exit $?"` after
  `wayglass ... | jq ...` reports the wayglass exit code, not jq's.
  Wayglass exit codes: `0` ok, `1` error, `2` usage, `3` blocked by policy,
  `4` a person must act.
- With `--json`, every command prints one envelope:
  `{"v":1,"ok":true,"data":{...}}` or
  `{"v":1,"ok":false,"error":{"code":"...","message":"..."}}`. The public
  HTTP API answers with the same envelope. When the CLI passes on an error
from the Worker, the message starts with `worker: <code>: `.
- Where a check reads fields from a successful response, it goes through one
  of these two helpers. Define them in each shell, with `pipefail`:

  ```bash
  set -o pipefail
  # okc / okr FILTER: FILTER over .data, compact (okc) or raw (okr), only when
  # .ok is true. Otherwise they print the error and exit 5, so a failed call
  # can never pass for a null or empty result.
  okc() { jq -ec "if .ok == true then .data | ($1) else error(\"not ok: \(.error)\") end"; }
  okr() { jq -er "if .ok == true then .data | ($1) else error(\"not ok: \(.error)\") end"; }
  ```

  A line such as `jq: error (at <stdin>:1): not ok: {"code":...}` is a
  failed check, whatever the step expected to print. Checks that expect a
  refusal read `.error` with plain `jq` instead.
- Intent ids (`i` plus 10 hex), commit hashes and timestamps differ on every
  run. Expected outputs name the fields to read, not those values.

## What you need

| Tier | Tools | Accounts |
|---|---|---|
| A | git 2.40+, Go 1.25.8+, Node.js 22.12+ with npm, curl, jq | None |
| B | Tier A tools, plus openssl | Cloudflare account on **Workers Paid** with access to **Artifacts** (beta) and a workers.dev subdomain. An API token with account permissions **Artifacts › Edit**, **Workers Scripts › Edit** and **Queues › Edit** (with `--domain`, also **Zone › Workers Routes › Edit** on that zone). The token is used only by `scripts/selfhost.sh`; the deployed Worker never sees it. Optional: a second token with only **Account › Queues › Edit** (`CF_EVENTS_API_TOKEN`) so trunk pushes that skip `land` are flagged. |
| C | Same as B | Same deployment as B |

## Tier A: no Cloudflare account

### A1. Build

```bash
git clone https://github.com/wayglass/wayglass.git
cd wayglass
go build -o bin/wayglass ./cmd/wayglass
./bin/wayglass version
export PATH="$PWD/bin:$PATH"
export WG_REPO="$PWD"
```

**Expect:** `version` prints `wayglass dev (commit none, go1.x.y)` and exits 0.
`wayglass version --json` prints an ok envelope with `version`, `commit` and
`go`.
**Pass:** the binary builds and runs.

Before launching coding agents, install the binary somewhere your login shell
already searches. For example, if `~/.local/bin` is on that PATH:

```bash
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/wayglass "$HOME/.local/bin/wayglass"
"${SHELL:-/bin/bash}" -lc 'wayglass version --json'
```

If the last command fails, add that directory to PATH in your shell's startup
file and retry. Exporting this checkout's `bin` directory only in the parent
shell may not survive an agent starting a login shell.

### A2. Go tests (CLI, policy, classifier, TUI)

```bash
cd "$WG_REPO"
go test ./...
```

**Expect:** every package prints `ok` or `[no test files]`; no `FAIL`. The
packages with tests are `examples/acme-shop`, `internal/api`,
`internal/classify`, `internal/cli`, `internal/gitx`, `internal/intent`,
`internal/notes`, `internal/policy`, `internal/textsafe` and `internal/tui`.
`internal/cli` takes about 30 to 40 s.
**Pass:** exit 0.

The CLI tests run the real binary's commands against an in-process fake
Worker (`internal/cli/fakeworker_test.go`) and real local git repos. They
prove CLI behaviour, not Cloudflare behaviour; Tier B covers that.

### A3. Worker tests

```bash
cd "$WG_REPO/worker"
npm ci
npx vitest run
node --test ../tests/console-model.test.mjs
```

**Expect:** Vitest ends with `Test Files  18 passed (18)` and
`Tests  347 passed (347)` at the time of writing (counts grow; any `failed`
is a fail). Warnings about AI bindings accessing remote resources are
expected and harmless. The console test prints `pass 4`, `fail 0`.
**Pass:** both exit 0.

The Worker tests run in the Workers runtime (`@cloudflare/vitest-plugin`)
against a fake Artifacts binding (`worker/test/fake-artifacts.ts`), with the
same Go policy evaluator the CLI uses, compiled to Wasm.

### A4. Desktop app tests

```bash
cd "$WG_REPO/desktop"
go test ./...
```

**Expect:** `ok  github.com/wayglass/wayglass/desktop/app` and
`[no test files]` for the root package.
**Pass:** exit 0. These tests use a fake Worker. They were run on macOS; on
Linux, if the Wails root package fails to build for lack of GTK or WebKit
headers, run `go test ./app/...` instead.

### A5. Terminal UIs on sample data

```bash
cd /tmp
wayglass watch --demo-data </dev/null
wayglass review --demo-data </dev/null
wayglass watch --demo-data --json </dev/null | jq '{v, ok, sample: .data.sample, project: .data.project, counters: .data.counters.by_action, overlaps: (.data.overlaps | length)}'
wayglass review --demo-data --json </dev/null | jq -c '[.data.queue[] | {id: .intent.id, action: .decision.action, status: .decision.status, rule: .decision.matched_rule}]'
```

Without a terminal (as above, or any agent shell) the TUIs print one frame
and exit; in a real terminal they are interactive (`q` quits, `?` lists keys).

**Expect, `watch --demo-data`:** a header with `acme-web` and
`SAMPLE DATA  --demo-data, not live`; the line
`decisions 7 · auto-merge 1 · agent review 2 · human 2 · multi-approve 1 · quarantine 1 · block 0`
(the sample includes the AI reviewer: one agent-review change it approved,
one it escalated to a person, and advice on the auth change, so the header's
second line ends `ai 1✓ 1▲ 1adv`); sections `INTENTS`,
`CHANGES  tags as classified: area · data class · risk · jurisdiction`,
`OVERLAPS  suggested re-plans` and `ACTIVITY`; a CHANGES row for `i0c9d8e`
with `pii`, `EU`, `quarantine`, `eu-pii-quarantine`; a row for `i4c5d6e`
with `▲ AI escalated`; the overlap line
`⇄ i3f2a91 ↔ i5a6b7c  web/login.ts  → land i3f2a91 first, i5a6b7c lands after it`;
the footer `▲ 4 changes wait for a person · wayglass review`. Exit 0.

**Expect, `review --demo-data`:** four queue entries,
`i3f2a91c0d1  ▲ needs a person`, `i0c9d8e7f61  ▲ quarantined`,
`i7b4e0d2a55  ▲ needs a person 1/2` and `i4c5d6e7f8a  ▲ AI escalated`, each
with its tags and `rule <name> → <action>: <reason>`, then
`Decide with: wayglass approve|reject <intent-id> --reason "..."`. Exit 0.

**Expect, JSON:** `"v":1`, `"ok":true`, `"sample":true`,
`"project":"acme-web"`, `by_action` of
`{"agent_review":2,"auto_merge":1,"block":0,"human_approve":2,"multi_approve":1,"quarantine":1}`,
one overlap whose `replan` has `"action":"land_first"` and
`"suggested":true`. The review queue is:

```json
[{"id":"i3f2a91c0d1","action":"human_approve","status":"pending_human","rule":"auth-needs-human"},
 {"id":"i0c9d8e7f61","action":"quarantine","status":"quarantined","rule":"eu-pii-quarantine"},
 {"id":"i7b4e0d2a55","action":"multi_approve","status":"pending_human","rule":"infra-two-people"},
 {"id":"i4c5d6e7f8a","action":"human_approve","status":"pending_human","rule":"low-risk-agent-review"}]
```

The exact counts follow the sample data in `internal/tui/demo.go`; if they
drift, the contract that matters is the shape above (sample flag, project,
one overlap with a suggested re-plan, every queue entry routed to a person).

`review --json` also lists the agent-written fields in `data.untrusted`
(for example `queue[].intent.goal`).

**Pass:** all four exit 0, the human output is labelled as sample data, and
the JSON matches. No colour codes appear in the JSON.

### A6. Policy routing with `wayglass policy test`

`policy test` replays commits through a policy file with the same classifier
and evaluator as `propose`. It is offline and records nothing. This builds a
scratch repo with one commit per kind of change, then replays it through the
example policy that `wayglass init` writes (`examples/policy.yaml`).

```bash
rm -rf /tmp/wg-policy && mkdir /tmp/wg-policy && cd /tmp/wg-policy
git init -q -b main
git config user.name "Evaluator"; git config user.email "evaluator@example.invalid"; git config commit.gpgsign false
printf '# Demo\n' > README.md; git add -A; git commit -qm "chore: seed"
mkdir -p docs; printf '# Guide\n\nSign in first.\n' > docs/guide.md; git add -A; git commit -qm "docs: add a guide"
mkdir -p src/auth; printf 'export const MAX_ATTEMPTS = 5;\n' > src/auth/login.ts; git add -A; git commit -qm "feat: limit sign-in attempts"
mkdir -p .github/workflows; printf 'name: ci\non: push\n' > .github/workflows/ci.yml; git add -A; git commit -qm "ci: add a workflow"
mkdir -p src/profile; printf 'export interface Customer { date_of_birth: string }\n' > src/profile/customer.ts; git add -A; git commit -qm "feat: store date of birth"
mkdir -p config; printf 'API_KEY=example-not-a-real-key\n' > config/sample.env; git add -A; git commit -qm "chore: add a config sample"

wayglass policy test --file "$WG_REPO/examples/policy.yaml" --jurisdiction us
wayglass policy test --file "$WG_REPO/examples/policy.yaml" --jurisdiction eu
```

**Expect:** `Replaying 6 commits in HEAD through ...`, one line per commit
(hash, action, rule, subject), a `Rules` table, and
`No shadow rule matched these commits.` Exit 0. The actions:

| Commit | Files | `--jurisdiction us` | `--jurisdiction eu` |
|---|---|---|---|
| `chore: seed` | `README.md` (matches `**/*.md`, so docs) | `auto_merge` `rules[4]` | `auto_merge` `rules[4]` |
| `docs: add a guide` | `docs/guide.md` | `auto_merge` `rules[4]` | `auto_merge` `rules[4]` |
| `feat: limit sign-in attempts` | `src/auth/login.ts` | `human_approve` `rules[2]` | `human_approve` `rules[2]` |
| `ci: add a workflow` | `.github/workflows/ci.yml` | `multi_approve` `rules[1]` | `multi_approve` `rules[1]` |
| `feat: store date of birth` | `date_of_birth` in content (PII) | `agent_review` `rules[5]` | **`quarantine` `rules[3]`** |
| `chore: add a config sample` | `API_KEY=` in content (secret) | `block` `rules[0]` | `block` `rules[0]` |

The same PII change is reviewed by an agent in a US project and quarantined
in an EU one: jurisdiction is a tag, and the rule
`{ jurisdiction: eu, data_class: pii } → quarantine` uses it. (In a real
project the jurisdiction comes from the Artifacts namespace, never from the
client; see C7.)

For machine-readable output:

```bash
wayglass policy test --file "$WG_REPO/examples/policy.yaml" --jurisdiction eu --json \
  | jq -c '[.data.commits[] | {subject, action: .decision.action, rule: .decision.rule.index, tags: .decision.tags | {areas, data_classes, risk, jurisdiction}}]'
```

Each tag set carries `areas`, `data_classes`, `risk` and `jurisdiction`, and
`evidence` names the file, line and pattern that set a data class.

**Shadow rule.** Add a rule in `mode: shadow` at the top of a copy of the
policy and replay again (US):

```bash
awk '{print} /^rules:/{print "  - { name: trial-pii-review, when: { data_class: pii }, action: quarantine, mode: shadow, reason: \"Trial: all personal data gets a DPO look.\" }"}' \
  "$WG_REPO/examples/policy.yaml" > /tmp/wg-shadow.yaml
wayglass policy test --file /tmp/wg-shadow.yaml --jurisdiction us
wayglass policy test --file /tmp/wg-shadow.yaml --jurisdiction us --json | jq -c '.data.summary'
```

**Expect:** every action is unchanged (the enforcing rules are now
`rules[1]` to `rules[6]`); under `feat: store date of birth` the extra line
`shadow: trial-pii-review would have quarantined this`; in the Rules table a
row `trial-pii-review  shadow  quarantine  1  1  1`; the closing line
`Enforcing the shadow rules would change 1 of 6 commits.` The summary is
`{"commits":6,"shadow_matched":1,"would_differ":1,"recorded":0,"changed":0}`.

**Errors:** `cd /tmp && wayglass policy test --json` prints
`{"v":1,"ok":false,"error":{"code":"not_a_repo",...}}`, exit 1.
`wayglass policy test --limit 0` exits 2 (usage).

**Pass:** every action matches the table, the shadow rule changes nothing
but is reported, and the error cases give the stated codes.

### A7. The audit view (moved to Tier B)

The hosted Worker at api.wayglass.dev serves no data to anonymous visitors:
its web app and `/v1/public/*` are behind Cloudflare Access, and the Worker
itself refuses them without a valid Access token. Tier A stays offline. The
audit view is checked on your own Worker in [B9](#b9-step-8-the-public-audit-view),
including the negative checks (a project that did not opt in, a write to the
read-only view, the key API without a key).

### A8. `wayglass why`, through the tests

`why` needs a landed change, which needs a Worker. Offline, its behaviour is
covered by CLI tests against the fake Worker:

```bash
cd "$WG_REPO"
go test ./internal/cli -run 'TestGoldenPathFlow|TestWhyDoesNotTrustForgedNotes|TestCloneThenLandAndWhy|TestLandFlow' -v 2>&1 | grep -E '^(--- |ok|FAIL)'
```

**Expect:** four `--- PASS` lines and `ok`. What they assert:

- `TestGoldenPathFlow`: after a docs change lands, `why docs/guide.md:1`
  answers from the git note (`source: "note"`, `verified: "worker"`, the
  intent id, `action: "auto_merge"`); for an undecided auth change it falls
  back to the Worker's record (`source: "worker"`, `human_approve`); a line
  with no record gives `no_why`, exit 1. It also checks that the authoring
  agent's own approval is refused (`conflict`).
- `TestWhyDoesNotTrustForgedNotes`: a hand-written note on a commit the
  Worker never decided gives `verified: "mismatch"` with a warning; a forged
  note on a decided commit is replaced by the Worker's record
  (`source: "worker"`); human output shows `WARNING:` and never the forged
  goal.
- `TestCloneThenLandAndWhy` and `TestLandFlow`: `why` after `clone` and after
  `land`, including the approver's reason.

**Pass:** all four pass.

### A9. Optional: the Zero Trust checks in the Worker tests

The Tier C refusals also have Worker tests you can run offline:

```bash
cd "$WG_REPO/worker"
npx vitest run test/propose.test.ts test/keys.test.ts test/intents.test.ts test/restack.test.ts
```

**Expect:** `Test Files  4 passed (4)`. Among them: "reads policy from the
trunk base, so a fork cannot relax its own rules", "routes any .wayglass/
change to a person even when policy would auto-merge it", "quarantines EU
personal data in a project created in the eu namespace", "refuses rubber-stamp
reasons like LGTM", "refuses to let the authoring agent decide its own
change", "refuses another agent's key before reserving anything" (land),
"refuses an agent key on multi_approve and on quarantine", "supersedes an
approval by a person ..." and the control-character cases in
`test/intents.test.ts`.

## Tier B: self-host in your own account

This deploys your own Worker and runs the golden path from
[AGENTS.md](../AGENTS.md#golden-path-the-demo-build-this-first-polish-it-last)
on real Artifacts. Nothing is mocked. It creates resources that cost money
once Artifacts billing applies (see [Cost](#cost)), and nothing here deletes
anything until [Cleanup](#cleanup).

### B0. Deploy

Use names that will not collide with anything already in the account, and
keep this login apart from any existing Wayglass login:

```bash
cd "$WG_REPO"
export WAYGLASS_CONFIG_DIR="$HOME/.config/wayglass-eval"   # selfhost.sh logs in here, not in ~/.config/wayglass
export CLOUDFLARE_ACCOUNT_ID="<your account id>"
SH=(--name wayglass-eval --namespace wayglass-eval --eu-namespace wayglass-eval-eu
    --queue wayglass-eval-events --public 'demo-test-*,eu-demo-test-*')

scripts/selfhost.sh "${SH[@]}" --dry-run      # writes worker/wrangler.selfhost.jsonc and bundles; creates nothing
read -r -s CLOUDFLARE_API_TOKEN && export CLOUDFLARE_API_TOKEN
scripts/selfhost.sh "${SH[@]}"                # shows the plan, asks y/N, then creates and deploys
```

An AI evaluator without a terminal cannot answer the prompt, and the script
refuses to create anything (`selfhost: not a terminal; rerun with --yes to
create these resources`, exit 1). If whoever runs the evaluation has
authorized the deployment, they export `CLOUDFLARE_ACCOUNT_ID` and
`CLOUDFLARE_API_TOKEN` in the agent's environment beforehand (the agent never
asks for or prints the token), and the agent passes `--yes`:

```bash
scripts/selfhost.sh "${SH[@]}" --dry-run      # read the plan
scripts/selfhost.sh "${SH[@]}" --yes          # the same plan, without the prompt
```

`--yes` skips only the question. A missing namespace or queue is created; a
refused token, an unreachable API or a namespace with another jurisdiction
stops the script with Cloudflare's error.

**Expect:** the dry run ends with
`Dry run done. Rerun without --dry-run to create the resources and deploy.`
The real run prints `created namespace wayglass-eval (us)`,
`created namespace wayglass-eval-eu (eu)`, `created queue wayglass-eval-events`
and the queue id, wrangler's deploy output with a
`https://wayglass-eval.<subdomain>.workers.dev` URL,
`Logged in to <url> (key stored in $WAYGLASS_CONFIG_DIR/credentials.yaml, mode 0600).`,
then `wayglass doctor`. A rerun prints `namespace ... exists` and creates
nothing.
**Pass:** doctor has no `fail` line for git, login, worker and auth (project
and trunk are checked once you are inside an initialised repo).

A namespace's jurisdiction is fixed when it is created. If a namespace with
that name already exists with another jurisdiction, the script stops; pick
another name. The project names below start with `demo-test-` and
`eu-demo-test-` because those are the only names `wayglass project delete`
will remove, and because `--public` above lets them into the public view.

Optional, so pushes that skip `land` are flagged in the audit log:
`read -r -s CF_EVENTS_API_TOKEN && export CF_EVENTS_API_TOKEN && scripts/selfhost.sh "${SH[@]}" --keep-key`.

```bash
export WG_URL=$(sed -n 's/^url: *//p' "$WAYGLASS_CONFIG_DIR/credentials.yaml")
echo "$WG_URL"
wayglass doctor --json | okc '[.checks[]? | {name, status}]'
```

### B1. Keys: one per agent, one for the person

The login above holds the admin key. Mint a key per actor; each key is bound
to a name the Worker records, whatever the caller claims. The key value is
printed once, so keep it in a shell variable only.

```bash
DOCS_KEY=$(wayglass keys create --agent eval-docs --note evaluation --json | okr .key)
AUTH_KEY=$(wayglass keys create --agent eval-auth --note evaluation --json | okr .key)
PII_KEY=$(wayglass keys create --agent eval-pii --note evaluation --json | okr .key)
SECOND_KEY=$(wayglass keys create --agent eval-second --note evaluation --json | okr .key)
PERSON_KEY=$(wayglass keys create --agent eval-person --human --note evaluation --json | okr .key)
for k in "$DOCS_KEY" "$AUTH_KEY" "$PII_KEY" "$SECOND_KEY" "$PERSON_KEY"; do printf '%s\n' "${k:0:4}"; done
wayglass keys list --json | okc '[.keys[] | {id, agent, kind, note}]'
```

**Expect:** each prefix is `wgk_`; `keys list` shows the five keys, four of
kind agent and `eval-person` of kind human.
**Pass:** five keys exist. Every command below that sets `WAYGLASS_KEY=...`
acts as that key; commands without it act as the admin.

### B1a. Optional: a person signs in through Cloudflare Access

Only if you set up Access ([SELF-HOSTING.md](SELF-HOSTING.md#people-sign-in-through-cloudflare-access));
skip it otherwise. It needs a Zero Trust organisation and the extra Access
token permissions listed there; no custom domain is needed. To set it up on the
B0 deployment, on its workers.dev host (replace the email):

```bash
scripts/selfhost.sh "${SH[@]}" --dry-run --access --allow 'you@example.com'
scripts/selfhost.sh "${SH[@]}" --access --allow 'you@example.com'   # new admin key, logs in again
curl -s -o /dev/null -w '%{http_code} %{redirect_url}\n' "$WG_URL/"         # 302 https://<team>.cloudflareaccess.com/...
curl -s -o /dev/null -w '%{http_code}\n' "$WG_URL/v1/whoami"               # 401: no key
```

**Expect:** the dry run lists the access host (`wayglass-eval.<subdomain>.workers.dev`),
the team domain, the login methods and three Access applications to create; the
real run ends with the same two checks passing ("302 to <team>.cloudflareaccess.com",
"401 without a key"). A rerun says "updated" for all three, never "created".
Without a Zero Trust organisation it stops first with a link to create one in
the dashboard; create it and rerun. With a custom domain add `--domain HOST` to
both commands; the Worker then answers only on that domain, so set `WG_URL=https://HOST`.

Then sign in as a person. Use a separate config directory so the admin login stays.

```bash
curl -s "$WG_URL/v1/human/session" | jq -c .error   # Access off: code access_not_configured
HUMAN_DIR=$(mktemp -d)
WAYGLASS_CONFIG_DIR=$HUMAN_DIR wayglass login --human --worker "$WG_URL" --json | jq -c '.data // .error | {agent, kind, source, expires_at, code}'
WAYGLASS_CONFIG_DIR=$HUMAN_DIR wayglass doctor --json | jq -c '[.data.checks[] | select(.name == "session")]'
```

**Expect:** with Access off, the first command prints the
`access_not_configured` error and the second exits 1 with the same code. With
Access on, a browser opens; after signing in and confirming, `agent` is your
email, `kind` human, `source` access, `expires_at` about 12 hours ahead, and
doctor's `session` check is ok (with Access off it is absent: nothing was stored).
**Pass:** either outcome above; with Access on, the key never appears on screen.

### B2. Golden path step 1: `init` creates the trunk

```bash
export P="demo-test-eval-$(date +%s)" E="eu-demo-test-eval-$(date +%s)"
export EV="$HOME/wg-eval-$P"; mkdir -p "$EV/trunk/docs" "$EV/trunk/src/auth"
cd "$EV/trunk" && git init -q -b main
git config user.name "Eval Owner"; git config user.email owner@example.invalid; git config commit.gpgsign false
printf '# Getting started\n\nInstall the app and sign in.\n' > docs/getting-started.md
printf 'export function login(user: string, password: string): boolean {\n  return user.length > 0 && password.length > 0;\n}\n' > src/auth/login.ts
git add -A && git commit -qm "feat: seed the demo app"
wayglass init --project "$P" --public --json | jq '{ok, trunk: .data.trunk.name, jurisdiction: .data.jurisdiction, public: .data.public, pushed: .data.pushed, warnings: .data.warnings}'
cat .wayglass/policy.yaml
```

**Expect:** `ok: true`, `jurisdiction: "us"`, `public: true`,
`pushed: "main"`, exit 0. Without `CF_EVENTS_API_TOKEN` a warning says trunk
pushes are not watched. `.wayglass/policy.yaml` is the example policy from A6.
**Pass:** the trunk exists (`wayglass doctor` inside `$EV/trunk` now passes
project and trunk).

Optional, an existing repository with real history (public, over HTTPS; a
private one works the same with your own SSH key or credential helper):

```bash
export X="demo-test-from-$(date +%s)"
(cd "$EV" && wayglass init --from https://github.com/spf13/cobra.git cobra --project "$X" --tags --json \
  | jq '{ok, branch: .data.branch, pushed: .data.pushed, renamed: .data.branch_renamed, tags: .data.tags_pushed, remote: .data.remote, warnings: .data.warnings}')
git -C "$EV/cobra" fetch wayglass && git -C "$EV/cobra" log -1 --format=%s
wayglass project delete "$X" --confirm "$X" --json | jq .ok
```

**Expect:** `ok: true`, `pushed: "main"`, `remote: "wayglass"`, the fetch
needs no password (the credential helper mints a read-only token), and the
last commit is `chore: bring this repository under Wayglass`. A file over
32 MB in the history would stop `init` with `file_too_large` before the
project exists. **Pass:** all of that, and `git -C "$EV/cobra" remote -v`
shows origin on GitHub and `wayglass` on Artifacts.

Make one clone per agent:

```bash
for a in docs auth; do
  git clone -q "$EV/trunk" "$EV/$a"
  git -C "$EV/$a" config user.name "eval-$a"; git -C "$EV/$a" config user.email "eval-$a@example.invalid"
  git -C "$EV/$a" config commit.gpgsign false
done
```

### B3. Step 2: two agents declare in parallel

```bash
(cd "$EV/docs" && WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json \
   --goal "docs: explain signing in" \
   --reason "New users ask how to sign in; the getting-started page stops at install" \
   --path "docs/**" | jq 'del(.data.token.value)' > "$EV/docs.declare.json") &
(cd "$EV/auth" && WAYGLASS_KEY=$AUTH_KEY wayglass intent declare --json \
   --goal "feat: lock accounts after five failed sign-ins" \
   --reason "Brute-force attempts on login; a lockout limits them" \
   --path "src/auth/**" --path docs/auth.md | jq 'del(.data.token.value)' > "$EV/auth.declare.json") &
wait
for a in docs auth; do okc '{id: .intent.id, agent: .intent.agent, fork: .fork.name, scope: .token.scope, expires: .token.expires_at, has_token_value: (.token | has("value")), overlaps: [.overlaps[]?.intent_id]}' <"$EV/$a.declare.json"; done
export DOCS_ID=$(okr .intent.id <"$EV/docs.declare.json") AUTH_ID=$(okr .intent.id <"$EV/auth.declare.json")
```

**Expect:** both print (no `not ok`) with different intent ids; `agent` is
`eval-docs` and `eval-auth` (from the keys); `fork` is `<project>--<intent id>`;
`scope: "write"` with `expires_at` about 15 minutes ahead. Whichever declared
second lists the other in `overlaps` (`docs/**` overlaps `docs/auth.md`).
`has_token_value: false`: the `jq 'del(.data.token.value)'` above drops the
fork's write token before anything reaches disk, keeping only the metadata
later steps read. The CLI keeps that token in memory for git; never save the
full `intent declare --json` output.
**Pass:** two forks, each with its own write token.

### B4. Step 3: push, overlap and a suggested re-plan

In a second terminal (same `WAYGLASS_CONFIG_DIR`), `wayglass watch` inside
`$EV/trunk` shows the intents live; it is optional.

```bash
cd "$EV/docs"
printf '\n## Sign in\n\nOpen the app and sign in with your email address.\n' >> docs/getting-started.md
WAYGLASS_KEY=$DOCS_KEY wayglass push -m "docs: explain signing in" --json | jq -c '{ok, files: .data.files, overlaps: [.data.overlaps[]? | .paths]}'

cd "$EV/auth"
printf '\nexport const MAX_FAILED_SIGN_INS = 5;\n' >> src/auth/login.ts
printf '# Sign-in lockout\n\nFive failed sign-ins lock the account.\n' > docs/auth.md
WAYGLASS_KEY=$AUTH_KEY wayglass push -m "feat: lock accounts after five failed sign-ins" --json | jq -c '{ok, files: .data.files}'
WAYGLASS_KEY=$AUTH_KEY wayglass overlaps --json | jq -c '[.data.overlaps[] | {a: .a.intent_id, b: .b.intent_id, paths, replan: .replan | {action, suggested, summary}}]'
cd "$EV/trunk" && wayglass watch --json </dev/null | jq -c '{intents: [.data.intents[] | {id, status, declared_paths, actual_paths}], overlaps: (.data.overlaps | length)}'
```

**Expect:** docs push `files: ["docs/getting-started.md"]`; auth push
`files: ["docs/auth.md","src/auth/login.ts"]` (order may differ); `overlaps`
lists the pair `$DOCS_ID`/`$AUTH_ID` with paths `docs/**` and `docs/auth.md`
and a `replan` with `suggested: true` and `action` one of `land_first`,
`stack` or `split`. `watch --json` shows both intents with declared and
pushed paths.
**Pass:** the overlap is found on real pushes and a re-plan is suggested.
Nothing re-plans automatically (see [Known limits](#known-limits)).

### B5. Step 4: `propose`, policy picks the action

```bash
cd "$EV/docs" && WAYGLASS_KEY=$DOCS_KEY wayglass propose --json > "$EV/docs.propose.json"; echo "exit $?"
cd "$EV/auth" && WAYGLASS_KEY=$AUTH_KEY wayglass propose --json > "$EV/auth.propose.json"; echo "exit $?"
for a in docs auth; do jq -c '{action: .data.decision.action, status: .data.decision.status, rule: .data.decision.matched_rule, exit: .data.exit, tags: .data.preview.tags | {areas, data_classes, risk, jurisdiction}}' "$EV/$a.propose.json"; done
```

**Expect:**

| Change | exit | action | status | rule | tags |
|---|:-:|---|---|---|---|
| docs | `0` | `auto_merge` | `approved` | `rules[4]` | areas `docs`, no data class, risk `low`, jurisdiction `us` |
| auth | `4` | `human_approve` | `pending_human` | `rules[2]` | areas include `auth` (and `docs`), risk `low`, `us` |

Now the EU project, with personal data:

```bash
mkdir -p "$EV/eu/src" && cd "$EV/eu" && git init -q -b main
git config user.name "eval-pii"; git config user.email eval-pii@example.invalid; git config commit.gpgsign false
printf 'package billing\n\ntype Invoice struct {\n\tTotal int\n}\n' > src/invoice.go
git add -A && git commit -qm "feat: seed the billing app"
wayglass init --project "$E" --jurisdiction eu --public --json | jq -c '{ok, jurisdiction: .data.jurisdiction}'
WAYGLASS_KEY=$PII_KEY wayglass intent declare --json --goal "feat: store the customer's date of birth on invoices" \
  --reason "Age-restricted goods need the buyer's age on the invoice" --path "src/**" | jq -c '{ok, id: .data.intent.id}'
printf '\ntype Customer struct {\n\tDateOfBirth string `json:"date_of_birth"`\n}\n' >> src/invoice.go
WAYGLASS_KEY=$PII_KEY wayglass push -m "feat: store date of birth on invoices" --json | jq -c '{ok, files: .data.files}'
WAYGLASS_KEY=$PII_KEY wayglass propose --json > "$EV/pii.propose.json"; echo "exit $?"
jq -c '{action: .data.decision.action, status: .data.decision.status, rule: .data.decision.matched_rule, tags: .data.preview.tags | {data_classes, jurisdiction}}' "$EV/pii.propose.json"
export PII_ID=$(okr .intent_id <"$EV/pii.propose.json")
```

**Expect:** init `jurisdiction: "eu"`; propose exit `4`, `quarantine`,
`quarantined`, `rules[3]`, `data_classes: ["pii"]`, `jurisdiction: "eu"`.
**Pass:** the three changes take three different routes, decided by the
Worker (it re-reads the fork and re-derives the tags; `.data.agrees` is
`true` when the local preview said the same).

The gate on person-routed changes: an agent key may not decide them.

```bash
cd "$EV/docs" && WAYGLASS_KEY=$DOCS_KEY wayglass approve "$AUTH_ID" --json \
  --reason "An agent key tries to decide a change routed to a person" | jq -c .error; echo "exit $?"
```

**Expect:** `"code":"needs_human"`, exit `4`, message starting
`policy routed intent ... to a person, and this key is an agent key`.
Nothing is recorded.

### B6. Step 7 (run it now): session share and fork

Share the auth agent's session while its change waits for a person, then
continue it as a second agent:

```bash
cd "$EV/auth"
SESSION_URL=$(WAYGLASS_KEY=$AUTH_KEY wayglass session share --note "Lockout pushed; waiting for a person to approve" --json | okr .url)
echo "$SESSION_URL"
cd "$EV" && WAYGLASS_KEY=$SECOND_KEY wayglass session fork "$SESSION_URL" --dir "$EV/second" --json \
  | jq -c '{ok, id: .data.intent.id, parent: .data.intent.parent_id, agent: .data.intent.agent, fork: .data.fork.name}'
cd "$EV/second" && WAYGLASS_KEY=$SECOND_KEY wayglass session show
```

**Expect:** a session URL; the fork is `ok: true`, a new intent by
`eval-second` with `parent` equal to `$AUTH_ID` (stacked on the source, so
it proposes only its own change and lands after it); `session show` prints
the source intent's goal, reason and the handoff note. The new checkout has
the auth agent's code.
**Pass:** code and context arrive together. For the "second machine" part,
run the `session fork` line on another machine with the CLI built, the same
`WAYGLASS_URL=$WG_URL` and its own agent key; the URL is the only thing it
needs from this one.

### B7. Step 5: a person reviews and approves with a reason

```bash
cd "$EV/trunk"
WAYGLASS_KEY=$PERSON_KEY wayglass review --json | jq -c '[.data.queue[] | {id: .intent.id, action: .decision.action, rule: .decision.matched_rule}]'
WAYGLASS_KEY=$PERSON_KEY wayglass diff "$AUTH_ID"
WAYGLASS_KEY=$PERSON_KEY wayglass approve "$AUTH_ID" --reason "Lockout matches the intent; five tries is the support policy" --json \
  | jq -c '{status: .data.decision.status, approvals: [.data.approvals[] | {by, reason}]}'; echo "exit $?"
```

In a terminal, `WAYGLASS_KEY=$PERSON_KEY wayglass review` is the interactive
queue: select the change, `a` to approve, type a reason, `enter`. Typing
`LGTM` there shows the Worker's refusal under the form.

**Expect:** the queue lists `$AUTH_ID` (`human_approve`, `rules[2]`) and
`$PII_ID` is not in it (different project); `diff` shows the lockout change
and `docs/auth.md`; approve exits 0 with `status: "approved"` and one
approval by `eval-person` with your reason.
**Pass:** a person, not an agent, decided it, and the reason is recorded.

### B8. Land both, then step 6: `why`

```bash
cd "$EV/docs" && WAYGLASS_KEY=$DOCS_KEY wayglass land --json | jq -c '{ok, commit: .data.commit, verified: .data.verified, note: .data.note_written}'
cd "$EV/auth" && WAYGLASS_KEY=$AUTH_KEY wayglass land --json | jq -c '{ok, commit: .data.commit, verified: .data.verified, note: .data.note_written}'

cd "$EV/docs" && wayglass why docs/getting-started.md:7 --json | jq -c '{verified: .data.verified, source: .data.source, intent: .data.note.intent_id, action: .data.note.action, rule: .data.note.rule, by: .data.note.decided_by}'
cd "$EV/auth" && wayglass why src/auth/login.ts:5
wayglass why src/auth/login.ts:5 --json | jq -c '{verified: .data.verified, intent: .data.note.intent_id, action: .data.note.action, approvals: .data.note.approvals}'
```

**Expect:** both lands `ok: true`, `verified: true`, `note: true` (the
second merges onto the trunk the first one moved). `why` on line 7 ("Open the
app and sign in ...") gives `verified: "worker"`, `source: "note"`, intent
`$DOCS_ID`, `auto_merge`, `rules[4]`, decided by `policy`. `why` on line 5
(`MAX_FAILED_SIGN_INS`) shows intent `$AUTH_ID` with its goal and reason, the
four tags, `human_approve by rule rules[2], decided by eval-person`, and the
approval reason; JSON `verified: "worker"`.
**Pass:** the why travels with the commit and matches the Worker's record.

`wayglass log --limit 20` (inside `$EV/trunk`) shows the same story as audit
events: declared, pushed, overlap, proposed, approve, landed.

### B9. Step 8: the public audit view

```bash
echo "$WG_URL/?project=$P"
curl -sS -o /dev/null -w '%{http_code}\n' "$WG_URL/v1/public/projects/$P/audit"
curl -sS "$WG_URL/v1/public/projects/$P/decisions" | okc '[.decisions[] | {intent_id, action, status}]'
curl -sS -o /dev/null -w '%{http_code}\n' "$WG_URL/v1/public/projects/$E"
curl -sS "$WG_URL/v1/public/projects/$E" | okc '.counts.decisions_by_action'
```

Negative checks (no key, no Access):

```bash
curl -sS "$WG_URL/v1/public/projects/does-not-exist" | jq -c .error     # not opted in
curl -sS -X POST "$WG_URL/v1/public/projects/$P/intents" -d '{}' | jq -c .error
curl -sS "$WG_URL/v1/intents?project=$P" | jq -c .error                 # the key API, no key
```

**Expect:** `200`; decisions for `$DOCS_ID` (`auto_merge`) and `$AUTH_ID`
(`human_approve`, `approved`); `200` again, and the EU project shows one
`quarantine`. A `404` with `not ok: {"code":"not_found",...}` means the EU
project is not in the Worker's public allowlist: B0's `--public` must cover
both `demo-test-*` and `eu-demo-test-*`. The
page at the printed URL shows the same trail, with the approval reason. The
negative checks give `{"code":"not_found","message":"no public project here"}`
(404), `{"code":"method_not_allowed","message":"the public view is read-only"}`
(405) and `{"code":"unauthorized","message":"missing bearer token"}` (401).
If you deployed with `--access` or `--access-team`, the Worker gates the web
app and `/v1/public/*` behind Access: run these in a browser signed in through
Access, or expect `302` (from Access) or `403` (from the Worker) from curl.
**Pass:** the website shows what the CLI showed, and nothing else is readable
or writable without a key.

### B10. Optional: the scripted golden path

`scripts/demo/golden-path.sh` runs steps 1 to 8 with three parallel agents
and prints a PASS/FAIL table and a measured-run report. It needs Bash 4+,
jq and curl, mints and revokes its own agent keys with the admin key, and
never approves anything.

```bash
DEMO_PROJECT_PREFIX=demo-test DEMO_EU_PROJECT=new scripts/demo/golden-path.sh
```

**Expect:** `N passed, 0 failed, 0 pending, 0 skipped`. Push events are
optional ([SELF-HOSTING.md, Push events](SELF-HOSTING.md#push-events-optional)):
when the Worker has no `CF_EVENTS_API_TOKEN`, `wayglass init --json` reports
`trunk_events.reason: "not_configured"`, the script does not wait for events,
and the step `POC before on new ref` reads `SKIP (push events not configured;
optional, see SELF-HOSTING.md)`, so the last line is `N passed, 0 failed,
0 pending, 1 skipped`. With events configured, that step waits up to
`DEMO_EVENT_WAIT` (default 120 s) and fails if no push event arrives. It
creates two more projects, `demo-test-<time>` and `demo-test-eu-<time>`.

### B11. Optional: real coding agents

Install Claude Code and Codex, sign in to their providers, and check that
`wayglass` is available from a login shell as described in A1. In a test
project, run `wayglass agent setup` and add `@AGENTS.md` to `CLAUDE.md` so
both agents read the same instructions. Commit those files before cloning
the project into a separate directory for each agent.

Give each process its own agent key through `WAYGLASS_KEY`, the Worker URL
through `WAYGLASS_URL`, and an empty `WAYGLASS_CONFIG_DIR`. Keep the admin key
in the supervising shell. Set `WAYGLASS_AGENT_FAMILY=claude` or `openai` for
the respective process. In those separate clones, launch concurrently:

```bash
claude -p --permission-mode acceptEdits --allowedTools 'Read Edit Write Glob Grep Bash' -- \
  'Make a small code change with tests and documentation, following the repository instructions.'
codex exec --sandbox workspace-write -c sandbox_workspace_write.network_access=true \
  --add-dir "$(git rev-parse --absolute-git-dir)" \
  'Add a page explaining how to run tests, following the repository instructions.'
```

Claude's print mode cannot prompt for shell permission. `acceptEdits` permits
file edits; `--allowedTools Bash` also permits the shell commands needed for
Wayglass, git and tests. Use a disposable clone with only that agent's key.
Codex also needs write access to this clone's Git directory so Wayglass can
commit and fetch. The `--add-dir` argument grants that access while keeping
the workspace sandbox enabled.

Before starting, add `reviewer: { different_family: true }` to the project's
`agent_review` rule through a separately approved policy change. After the
authors finish, ask Codex to review Claude's pending change. Check that it
reads the diff and runs tests before deciding with a specific reason. An
author's own approval must fail; a reviewer declaring the author's family
must receive `same_family`. The author may land after approval.
Check the latest decision for the intent in `wayglass watch --json` under
`data.decisions`. Rerunning `propose` records a new decision and replaces the
approval. For a staged run, ask authors to return once review is pending,
then start the reviewer.

For the Acme Shop tasks and generated launchers, run
`DEMO_PREFIX=demo-test-acme scripts/demo/examples.sh`. Each launcher mints
and revokes its own agent key; pass `claude print` or `codex print` for
headless use. The script prints clone and launcher paths without starting
the agents. Revoke leftover keys with `examples.sh --revoke-keys <project>`
after an interrupted run, and delete the throwaway projects when finished.

### Cost

Artifacts billing started on October 14, 2026, with 10,000 operations and
1 GB of storage a month included. One Tier B run creates two trunks, four
forks (docs, auth, PII, the session fork) and roughly 10 to 20 short-lived
tokens; Tier C adds about eight forks. Durable Objects, Queues and Worker
requests are billed under Workers Paid like any other Worker. The Worker
counts its own Artifacts calls: `wayglass intent show <id>` and
`$WG_URL/v1/public/projects/$P` (`cost`) show them; client git traffic to
forks is not counted there.

### Cleanup

Run this after Tier C if you do it.

```bash
cd "$EV/trunk"
wayglass project delete "$P" --confirm "$P" --json | jq -c '{ok, repos: .data.repos_deleted, tokens: .data.tokens_revoked}'
wayglass project delete "$E" --confirm "$E" --json | jq -c '{ok, repos: .data.repos_deleted}'
for id in $(wayglass keys list --json | okr '.keys[] | select(.note == "evaluation" and .revoked_at == null) | .id'); do wayglass keys revoke "$id"; done
cd "$WG_REPO"
scripts/selfhost.sh --name wayglass-eval --queue wayglass-eval-events --uninstall   # add --yes without a terminal
rm -rf "$WAYGLASS_CONFIG_DIR" "$EV"
```

`--uninstall` deletes the queue first, because Cloudflare refuses to delete a
Worker that still consumes a queue (`code: 10064`), then the Worker. After
each delete it checks with the API that the resource is gone and prints
`deleted ... (verified: the API no longer finds it)`; on a rerun it prints
`... does not exist; nothing to delete`. Its last lines say the namespaces
were kept.

**Expect:** both `project delete` lines `ok: true` with nonzero counts, the
two `verified` lines, exit 0.

By hand, keep the same order: `npx wrangler queues delete wayglass-eval-events`,
then `npx wrangler delete --config wrangler.selfhost.jsonc` from `worker/`.
With the Tier B token, `wrangler delete` can exit 1 with
`Authentication error [code: 10000]` on `/storage/kv/namespaces` after it
has already deleted the Worker (that lookup needs **Workers KV Storage ›
Read**). Do not read the exit code as "the Worker is still there"; ask the
API:

```bash
printf 'header = "Authorization: Bearer %s"\n' "$CLOUDFLARE_API_TOKEN" |
  curl -sS --config - "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/workers/services/wayglass-eval" | jq -c '{success, errors}'
```

`"success": false` with code `10090` means the Worker is deleted.

`project delete` needs the admin key and works only on names starting with
`demo-test-` or `eu-demo-test-`; any other name is refused (403) and keeps
its repos and audit log. It removes the project's trunk and fork repos, their
tokens and the Worker's state for it. The Artifacts namespaces
(`wayglass-eval`, `wayglass-eval-eu`) are not deleted by anything here;
whether to delete them in the Cloudflare dashboard is the account owner's
call.

## Tier C: adversarial checks

Each check tries to get around one Zero Trust claim on the Tier B
deployment, with the same keys and `$P`, `$E`, `$EV`. Run them in fresh
clones so one check's commits do not leak into the next:

```bash
fresh() { rm -rf "$EV/$1"; git clone -q "$EV/trunk" "$EV/$1"; cd "$EV/$1";
          git config user.name "$1"; git config user.email "$1@example.invalid"; git config commit.gpgsign false; }
```

(`intent declare` fast-forwards a stale clone to the trunk, so cloning the
local `trunk` directory is enough.) Each check also names the offline test
that covers it.

### C1. The author cannot approve its own change

```bash
fresh c1 && WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "feat: add a version constant" \
  --reason "The footer should show the app version" --path "src/**" | okr .intent.id > "$EV/c1.id"
export C1=$(cat "$EV/c1.id")
mkdir -p src && printf 'export const version = "1.0.0";\n' > src/version.ts
WAYGLASS_KEY=$DOCS_KEY wayglass push -m "feat: add a version constant" --json >/dev/null
WAYGLASS_KEY=$DOCS_KEY wayglass propose --json | jq -c '{action: .data.decision.action, status: .data.decision.status, exit: .data.exit}'
WAYGLASS_KEY=$DOCS_KEY wayglass approve "$C1" --reason "Checked my own change carefully" --json | jq -c .error; echo "exit $?"
WAYGLASS_KEY=$AUTH_KEY wayglass approve "$C1" --reason "Constant matches the intent; nothing else changed" --json | okc '.decision.status'
```

**Expect:** propose `agent_review`, `pending_agent`, exit 0 (low risk,
outside every area: `rules[5]`). The author's approval:
`{"code":"conflict","message":"worker: conflict: the agent that wrote a change cannot decide it"}`,
exit 1. Another agent's approval: `"approved"` (`agent_review` is open to
agent keys).
**Pass:** self-approval refused. Test: "refuses to let the authoring agent
decide its own change" (`worker/test/propose.test.ts`), "still refuses a human
key deciding its own change" (`worker/test/keys.test.ts`).

### C2. Filler reasons are refused

```bash
fresh c2 && WAYGLASS_KEY=$AUTH_KEY wayglass intent declare --json --goal "feat: shorten the session timeout" \
  --reason "Shared machines stay signed in too long" --path "src/auth/**" | okr .intent.id > "$EV/c2.id"
export C2=$(cat "$EV/c2.id")
printf '\nexport const SESSION_MINUTES = 15;\n' >> src/auth/login.ts
WAYGLASS_KEY=$AUTH_KEY wayglass push -m "feat: shorten the session timeout" --json >/dev/null
WAYGLASS_KEY=$AUTH_KEY wayglass propose --json >/dev/null; echo "propose exit $?"
WAYGLASS_KEY=$PERSON_KEY wayglass approve "$C2" --reason "LGTM" --json | jq -c .error; echo "exit $?"
printf 'Authorization: Bearer %s\n' "$PERSON_KEY" | curl -sS -H @- -H 'content-type: application/json' \
  -X POST "$WG_URL/v1/intents/$C2/decide" -d "{\"project\":\"$P\",\"verdict\":\"approve\",\"reason\":\"LGTM 👍\"}" | jq -c .error
```

**Expect:** propose exit 4 (auth). The CLI refuses before calling the
Worker: `"code":"usage"`, message `--reason "LGTM" is a rubber stamp, not a why; ...`,
exit 2. The Worker refuses the same on its own (HTTP 400):
`"code":"bad_request"`, message `reason "LGTM 👍" is a rubber stamp, not a why; the audit log keeps it forever, so say what you checked`.
Placeholders such as `<what you checked>`, `todo` or `n/a` are refused the
same way. `LGTM: checked the timeout path` would pass. Leave `$C2` pending.
**Pass:** neither door accepts a rubber stamp. Test: "refuses rubber-stamp
reasons like LGTM" and "refuses placeholder reasons" (`propose.test.ts`).

### C3. An agent key on a person-routed change

```bash
cd "$EV/c2" && WAYGLASS_KEY=$DOCS_KEY wayglass approve "$C2" --reason "Timeout value matches the intent" --json | jq -c .error.code; echo "exit $?"
cd "$EV/eu" && WAYGLASS_KEY=$DOCS_KEY wayglass approve "$PII_ID" --reason "Date of birth is needed for the age check" --json | jq -c .error.code; echo "exit $?"
```

**Expect:** `"needs_human"`, exit 4, for both the `human_approve` change and
the quarantined one. A human key or the admin key can decide them; an agent
key never can, whatever name or family it reports. For `multi_approve`, the
admin key counts as one approver (`admin`) however it names itself, so the
second approval needs another person's key (`wayglass keys create --human`).
**Pass:** both refused. Test: "refuses an agent key on multi_approve and on
quarantine" (`keys.test.ts`), `TestOnlyAPersonDecidesHumanRoutedChange`.

### C4. A push outside the declared paths is flagged

```bash
fresh c4 && WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "docs: add a FAQ" \
  --reason "Support answers the same questions every week" --path "docs/**" >/dev/null
printf '# FAQ\n\nHow do I reset my password?\n' > docs/faq.md
mkdir -p src && printf 'export const faqEnabled = true;\n' > src/flags.ts
WAYGLASS_KEY=$DOCS_KEY wayglass push -m "docs: add a FAQ" --json | jq -c .data.files
WAYGLASS_KEY=$DOCS_KEY wayglass propose --json | jq -c '{undeclared: .data.undeclared_paths, warnings: .data.warnings, action: .data.decision.action}'
```

**Expect:** `undeclared: ["src/flags.ts"]`, a warning
`1 changed file(s) outside the declared paths: src/flags.ts; declare them next time so overlap checks see them early`,
and action `agent_review`, not `auto_merge`: `only_area: docs` needs every
file to be docs. `watch` marks the undeclared path too.
**Pass:** the stray file is reported and costs the change its docs fast
path. It is a warning, not a refusal. Test: `TestProposeWarnsOnUndeclaredPaths`.

### C5. An auth change under a docs goal still goes to a person

```bash
fresh c5 && WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "docs: fix a typo" \
  --reason "A reader reported a typo" --path "docs/**" >/dev/null
printf '\nexport const BYPASS = true;\n' >> src/auth/login.ts
WAYGLASS_KEY=$DOCS_KEY wayglass push -m "docs: fix a typo" --json >/dev/null
WAYGLASS_KEY=$DOCS_KEY wayglass propose --json | jq -c '{action: .data.decision.action, rule: .data.decision.matched_rule, areas: .data.preview.tags.areas, undeclared: .data.undeclared_paths, exit: .data.exit}'; echo "exit $?"
```

**Expect:** `human_approve`, `rules[2]`, areas `["auth"]`, undeclared
`["src/auth/login.ts"]`, exit 4. The goal, reason and commit message do not
affect classification; the changed paths and content do.
**Pass:** routed to a person.

### C6. A fork cannot relax its own policy

An agent rewrites the policy in its fork so that everything auto-merges:

```bash
fresh c6 && WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "chore: simplify the policy" \
  --reason "Fewer rules are easier to read" --path ".wayglass/**" >/dev/null
printf 'version: 1\nrules: []\ndefault: { action: auto_merge, reason: "Everything is fine." }\n' > .wayglass/policy.yaml
WAYGLASS_KEY=$DOCS_KEY wayglass push -m "chore: simplify the policy" --json >/dev/null
WAYGLASS_KEY=$DOCS_KEY wayglass propose --json | jq -c '{action: .data.decision.action, rule: .data.decision.matched_rule, status: .data.decision.status, exit: .data.exit}'; echo "exit $?"
```

**Expect:** `rule: "builtin:protect-policy"`, a person-routed status, exit 4.
Any change under `.wayglass/` goes to a person, and the Worker judges a
change by the trunk's current policy, never by the fork's copy. (The guard
and the trunk read overlap here; the Worker test below checks the trunk read
on its own.)
**Pass:** not auto-merged. Tests: "reads policy from the trunk base, so a
fork cannot relax its own rules" and "routes any .wayglass/ change to a
person even when policy would auto-merge it" (`propose.test.ts`).

### C7. Personal data in the EU is quarantined, and jurisdiction is not the client's to choose

B5 showed the EU change quarantined. Push the same kind of change to the US
project:

```bash
fresh c7 && WAYGLASS_KEY=$PII_KEY wayglass intent declare --json --goal "feat: store date of birth" \
  --reason "Age-restricted content needs an age check" --path "src/**" >/dev/null
mkdir -p src/profile && printf 'export interface Customer { date_of_birth: string }\n' > src/profile/customer.ts
WAYGLASS_KEY=$PII_KEY wayglass push -m "feat: store date of birth" --json >/dev/null
WAYGLASS_KEY=$PII_KEY wayglass propose --json | jq -c '{action: .data.decision.action, jur: .data.preview.tags.jurisdiction, data: .data.preview.tags.data_classes}'
```

**Expect:** `agent_review`, `us`, `["pii"]`: the same data class, routed by
where the project lives. The Worker takes jurisdiction from the project's
Artifacts namespace (`wayglass-eval-eu` for `$E`), not from anything the
client sends.
**Pass:** US `agent_review`, EU `quarantine`. Test: "quarantines EU personal
data in a project created in the eu namespace" (`propose.test.ts`),
`TestPIIQuarantinedInEUProject`.

### C8. A forged git note does not fool `why`

Anyone who can push `refs/notes/wayglass` can write a note. `why` checks it
against the Worker.

```bash
cd "$EV/docs"
C=$(git log -1 --format=%H -- docs/getting-started.md)
git notes --ref=wayglass add -f -m '{"v":1,"intent_id":"i0000000000","goal":"reviewed by security","reason":"approved","rule":"rules[0]","action":"auto_merge","decision_id":"d0000000000","decided_by":"a-person","decided_at":"2026-10-14T00:00:00Z"}' "$C"
wayglass why docs/getting-started.md:7 --json | jq -c '{verified: .data.verified, source: .data.source, warning: .data.warning, worker_intent: .data.worker.intent.id}'
wayglass why docs/getting-started.md:7
```

**Expect:** `verified: "mismatch"`, `source: "worker"`, warning
`the git note on this commit does not match the Worker's record; showing the Worker's record (the note may be forged)`,
`worker_intent` equal to `$DOCS_ID`. The human output starts with
`WARNING:` and never shows "reviewed by security".
**Pass:** the forged note is reported and not believed. The forgery is only
in this checkout; the trunk still holds the real note
(`wayglass clone "$P" "$EV/c8-trunk"` gets it). Test:
`TestWhyDoesNotTrustForgedNotes`.

### C9. Control and escape characters in agent-written text are refused

```bash
fresh c9
WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "$(printf 'docs: tidy\033[2J\033[H')" \
  --reason "Clean up the guide" --path "docs/**" | jq -c .error; echo "exit $?"
WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "docs: tidy" \
  --reason "$(printf 'Clean up \342\200\256the guide')" --path "docs/**" | jq -c .error; echo "exit $?"
```

**Expect:** `{"code":"bad_request","message":"worker: bad_request: goal contains control characters; send plain text (tabs and newlines are fine)"}`,
exit 1; then the same for `reason` (the second command embeds U+202E, a
bidi override). No intent or fork is created.
**Pass:** both refused by the Worker. Test: the control-character cases in
`worker/test/intents.test.ts`; `TestHumanOutputStripsTerminalEscapes` for the
CLI's output filter.

### C10. Only the author lands

Use `$C1` (approved in C1, not landed):

```bash
cd "$EV/c2" && WAYGLASS_KEY=$AUTH_KEY wayglass land "$C1" --json | jq -c .error; echo "exit $?"
```

**Expect:** `"code":"not_author"`, message
`only eval-docs (who declared intent ... ) or the admin key can land it; nothing was pushed. ...`,
exit 1. No trunk token is minted.
**Pass:** an approval does not let another key push the change to trunk.
Test: "refuses another agent's key before reserving anything, and lets the
author's own key land" (`propose.test.ts`), `TestLandOnlyByAuthorOrAdmin`.

### C11. Re-stacking supersedes a stale approval

`$C1` is approved. Stack it on another open intent, so its change now sits
on code the approver never saw:

```bash
fresh c11 && WAYGLASS_KEY=$AUTH_KEY wayglass intent declare --json --goal "docs: add release notes" \
  --reason "Users ask what changed" --path "notes/**" | okr .intent.id > "$EV/c11.id"
export C11=$(cat "$EV/c11.id")
mkdir -p notes && printf '# Release notes\n' > notes/1.0.md
WAYGLASS_KEY=$AUTH_KEY wayglass push -m "docs: add release notes" --json >/dev/null

cd "$EV/c1"
WAYGLASS_KEY=$DOCS_KEY wayglass intent restack "$C1" --on "$C11" --reason "Ship the version constant with the release notes" --json | jq -c '{ok}'
WAYGLASS_KEY=$DOCS_KEY wayglass land --json | jq -c .error; echo "exit $?"
WAYGLASS_KEY=$AUTH_KEY wayglass approve "$C1" --reason "Constant matches the intent; nothing else changed" --json | jq -c .error.code; echo "exit $?"
wayglass log --intent "$C1"
```

**Expect:** restack `ok: true`. Land refuses with `"code":"conflict"` and a
message that the decision `was superseded when intent ... was re-stacked; run wayglass propose again ...`,
exit 1. A new approval gets `"superseded"`, exit 1. `log` shows
`decision.superseded` with "superseded approval by eval-auth". After
`wayglass propose` the change is classified again as a whole.
**Pass:** an approval does not carry over to a different base. Test:
"supersedes an approval by a person, ..." (`worker/test/restack.test.ts`).
The repo's docs record this behaviour under tests; no live run of `restack`
is recorded in them.

### C12. A decision does not survive a change to the trunk's policy

Run this last: it changes the trunk's policy. First approve the pending
session-timeout change from C2, then land a policy edit (a comment only)
through Wayglass, then try to land the C2 change under its old decision.

```bash
cd "$EV/c2" && WAYGLASS_KEY=$PERSON_KEY wayglass approve "$C2" --reason "Fifteen minutes matches the session policy" --json | okc .decision.status

fresh c12 && WAYGLASS_KEY=$DOCS_KEY wayglass intent declare --json --goal "chore: name the policy owner" \
  --reason "People ask who owns the rules" --path ".wayglass/policy.yaml" | okr .intent.id > "$EV/c12.id"
export C12=$(cat "$EV/c12.id")
printf '# Owner: the platform team\n' >> .wayglass/policy.yaml
WAYGLASS_KEY=$DOCS_KEY wayglass push -m "chore: name the policy owner" --json >/dev/null
WAYGLASS_KEY=$DOCS_KEY wayglass propose --json | jq -c '{rule: .data.decision.matched_rule, exit: .data.exit}'
WAYGLASS_KEY=$PERSON_KEY wayglass approve "$C12" --reason "Comment only; no rule changes" --json | okc .decision.status
WAYGLASS_KEY=$DOCS_KEY wayglass land --json | jq -c '{ok, verified: .data.verified}'

cd "$EV/c2" && WAYGLASS_KEY=$AUTH_KEY wayglass land --json | jq -c .error; echo "exit $?"
```

**Expect:** the C2 approval gives `"approved"`. The policy edit routes to a
person (`builtin:protect-policy`, exit 4), is approved and lands
(`ok: true`, `verified: true`). The C2 land is then refused:
`"code":"policy_changed"`, message
`the trunk's .wayglass/policy.yaml changed after decision ... was made, so it no longer covers the change; nothing was issued. Next: merge the trunk into intent ..., push, and run wayglass propose again ...; nothing was pushed (then run: wayglass propose ...)`,
exit 1. Any byte change counts, a comment included. The refused land is
audited as `land.cancelled`.
**Pass:** an approval made under one policy does not land under another.
Proposing again (after merging the trunk into the intent) decides the change
under the current rules. Tests: `worker/test/policy-drift.test.ts`,
`TestLandPolicyChanged`. Added on October 14, 2026; the repo's docs record
no live run of it yet.

## What proves what

| Claim (contest rule or [AGENTS.md](../AGENTS.md)) | Checks |
|---|---|
| Uses Cloudflare Workers | B0 (your own Worker deploys), A7 (the hosted one answers), A3 |
| Uses Cloudflare Artifacts, not mocked | B2 (trunk), B3 (a fork per intent with a scoped write token), B8 (land verified against the trunk), cleanup removes real repos |
| Multiple agents work concurrently | B3 (two declares in parallel, separate forks and keys), B4 (overlap on real pushes), B10 (three agents) |
| Every change is classified by area, data class, risk and jurisdiction | A6 (tags and evidence per commit), B5 (tags in `propose --json`), A5 |
| Policy picks one action per change | A6 (all six actions), B5 (auto_merge, human_approve, quarantine), C1 (agent_review) |
| Jurisdiction comes from the namespace | A6 (same change, US vs EU), B5, C7 |
| Quarantine | A6, B5, C3 (an agent key cannot release it), B9 |
| Shadow mode for new rules | A6 (shadow rule reported, nothing changes) |
| Consequential changes go to a person | B5 gate, B7, C3, C5, C6 |
| No self-approval, no rubber stamps | C1, C2 |
| A decision holds only for the change and policy it was made for | C11, C12 |
| Only the author (or the admin) lands | C10 |
| Every change carries its why | B8 (`why` with `verified: worker`), A8, C8 |
| Append-only audit log | B8 (`log`), B9, A7 |
| Agent-written text is data, not instructions or escapes | C9, A5 (`untrusted` in `review --json`) |
| Terminal-first: TUI for people, `--json` for agents | A5, B4 (`watch`), B7 (`review`), every `--json` check |
| Overlap warnings inside `--json`, with a suggested re-plan | A5, B3, B4 |
| Forkable agent sessions | B6 |
| Short-lived, scoped tokens; no long-lived write token | B3 (15 min fork token), B8 (land uses a 300 s trunk token), C10 |
| MIT licence | `LICENSE` at the repo root |
| Run instructions in the README | A1 and B0 follow the README's commands |

## Known limits

From the README's Status section, as of this writing. Check it for
changes.

Not built:

- LLM-based `agent_review`. A reviewer session (another Claude Code or Codex)
  reads the change with `wayglass diff` and approves through the CLI.
- Automatic re-planning. Overlaps come with a suggestion (`land_first`,
  `stack` or `split`); an agent or person acts on it (`intent restack` carries
  out `stack` in one step when asked).
- Short-lived session keys and project-scoped keys. Per-agent keys are bound
  to a name; the admin key's caller still names itself.
- Branch protection on the trunk. Artifacts has none, so the Worker detects a
  trunk push that skipped `land` (with `CF_EVENTS_API_TOKEN`) but cannot
  prevent it.

Known issues:

- Worker responses mix `defaultBranch` (raw Artifacts) with snake_case
  elsewhere.
- The trunk branch in Artifacts is always `main`. A repository whose default
  branch is `master` keeps that name locally (`init` warns; `init --from`
  renames its own clone), and `land` fast-forwards only a local `main`.
- Refused calls (for example `not_author`) are not written to the audit log.
- Model families are self-reported; nothing proves an agent is the model it
  says it is.
- `internal/gitx` does not guard git arguments against values starting with
  `-`; all revisions and refs come from Worker-validated ids today.

Verified less than the rest:

- `scripts/selfhost.sh` stopped at the queue step on its first fresh-account
  run (Oct 14). That was fixed, and the fixed script then ran Tiers B and C
  on a brand-new account the same day, `--access` and `--uninstall`
  included. `scripts/test-selfhost.sh` covers a fresh account, a rerun,
  `--keep-key`, refused tokens, bad names, Access and `--uninstall` against
  stubbed wrangler and curl. Tier B is the live test; report what breaks.
- `intent abandon` (and re-stacking children) is deployed and covered by
  tests, with no live run recorded.
- The desktop app was tested against a fake Worker and its read views were
  exercised against the live Worker; approving from the window has not been
  done live.
- The Acme Shop kit (`examples/acme-shop/`) routed 8 of 8 tasks with scripted
  stand-ins. A smaller hosted agent run
  verified concurrent Claude Code and Codex work and cross-family review;
  the full kit has not been run with models.
- Artifacts cost figures count only the Worker's own binding calls; client
  git traffic is not counted, and whether binding reads are billed operations
  is unverified.
- No release has been tagged, so the install line in
  [RELEASING.md](RELEASING.md) does not work yet; build from source (A1).
