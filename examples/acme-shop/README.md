# Acme Shop: a Wayglass example with real agents

A small Go web shop (`app/`) and eight task briefs (`tasks/`) for Claude Code
and Codex sessions that work on it at the same time through Wayglass. Each
brief is written to produce one moment of the golden path: a docs change
that auto-merges, an auth change that waits for a person, EU personal data
that is quarantined, an infra change that needs two people, a tests-only
change that a reviewer agent checks, a hardcoded key that is blocked, and
two catalog changes that overlap, one stacked on the other with `--on`.

| Path | What it is |
|---|---|
| `app/` | The seed repo: catalog, sign-in, EU invoicing, infra, docs, web page, tests. Stdlib-only Go 1.25. |
| `app/.wayglass/policy.yaml` | The policy the briefs are written against (below). |
| `tasks/PROTOCOL.md` | The rules every agent gets first: declare, change, test, commit, push, propose, follow the exit code. |
| `tasks/01-docs.md` ... `08-category.md` | One brief per agent, each with its exact `intent declare` line and expected route. |
| `tasks/reviewer.md` | The reviewer session (Codex) that decides `agent_review` changes, written by Claude, with `wayglass diff`. |
| `policy_test.go` | Checks the policy routes each brief's change as the brief claims (`go test ./examples/...`). |
| `../../scripts/demo/examples.sh` | Creates the projects, clones and launchers; `--standins` runs scripted agents instead. |

## Run it

```bash
wayglass login --worker https://api.wayglass.dev   # once
scripts/demo/examples.sh
```

It creates two public projects, `demo-acme-<unix>` (us) and
`eu-demo-acme-<unix>` (eu), seeded with `app/`, gives each task its own clone
and `WAYGLASS_AGENT` under `~/wayglass-examples/demo-acme-<unix>/`, and
prints a launcher per task:

```bash
~/wayglass-examples/demo-acme-<unix>/launch/01-docs.sh               # its default CLI, interactive
~/wayglass-examples/demo-acme-<unix>/launch/02-auth.sh claude print  # other CLI, headless
```

Each launcher sets `WAYGLASS_AGENT_FAMILY` from the CLI it starts (`claude`
or `openai`). The `agent_review` changes (05, 07, 08) are Claude's and the
reviewer is Codex, because policy refuses a reviewer from the author's own
model family. Claude Code starts with `--permission-mode acceptEdits` and an allow list
limited to `wayglass`, `go`, `jq` and local `git` commands (no `git push`).
Codex starts with `--sandbox workspace-write`, network on (Wayglass talks to
the Worker and Artifacts) and the Go build cache writable. Start
`08-category` after `07-search` has pushed (its brief waits up to 10
minutes), and the reviewer any time. Decide the person-routed changes
yourself in `wayglass review` or the desktop app.

## Policy

Rules are checked top to bottom; the first enforcing rule that matches
decides. `area` matches when *any* changed file is in the area (strict rules);
`only_area` when *every* file is (permissive rules), so a docs edit cannot
carry billing code past review.

| Rule | When | Action |
|---|---|---|
| `no-secrets` | data class `secrets` (Paylane `pl_live_` keys, private keys, AWS key IDs, `.env` files) | `block` |
| `infra-two-people` | area `infra` | `multi_approve`, 2 people |
| `auth-boundary` | area `auth` | `human_approve` |
| `eu-personal-data` | jurisdiction `eu` and data class `pii` | `quarantine` |
| `personal-data` | data class `pii` | `human_approve` |
| `billing-money` | area `billing` | `human_approve` |
| `docs-only` | every file is docs, risk low or medium | `auto_merge` |
| `trial-tests-auto-merge` | every file is a test, risk low; **`mode: shadow`** | (would `auto_merge`) |
| `tests-only` | every file is a test | `agent_review`, reviewer from another model family |
| `routine-app` | every file is app, web, tests or docs, risk low | `agent_review`, reviewer from another model family |
| default | anything else | `human_approve` |

## Routes, proven on real Artifacts

Dry run on Oct 13, 2026 (projects `demo-test-acme-1791946483` and
`eu-demo-test-acme-1791946483`) with scripted stand-ins that make each
brief's change, against the deployed Worker. A scripted reviewer (declared
family `openai`; the authors declared `claude`) read each `agent_review`
diff with `wayglass diff`, ran the tests and approved; its approval as
family `claude` was refused with `same_family`. The person-routed changes
were left for a person. 8 of 8 tasks routed as their brief says.

| Task | Agent | Tags (area; data class; risk; jurisdiction) | Rule | Action | Status | Exit | Outcome |
|---|---|---|---|---|---|---|---|
| 01 docs | claude-docs | docs; -; low; us | `docs-only` | auto_merge | approved | 0 | landed; `why docs/api.md:3` answers |
| 02 auth | codex-auth | app, auth, tests; -; low; us | `auth-boundary` | human_approve | pending_human | 4 | waits for a person |
| 03 EU PII | claude-billing | app, billing, tests; pii; low; **eu** | `eu-personal-data` | quarantine | quarantined | 4 | held |
| 04 infra | codex-infra | docs, infra; -; low; us | `infra-two-people` | multi_approve (2) | pending_human | 4 | waits for two people |
| 05 tests | claude-tests | app, tests; -; low; us | `tests-only` | agent_review | pending_agent | 0 | reviewer approved, landed; shadow: `trial-tests-auto-merge` would auto-merge |
| 06 secret | codex-payments | app, billing; **secrets**; low; us | `no-secrets` | block | blocked | 3 | stopped |
| 07 search | claude-search | app, tests; -; low; us | `routine-app` | agent_review | pending_agent | 0 | reviewer approved, landed |
| 08 category | claude-category | app, tests, web; -; low; us | `routine-app` | agent_review | pending_agent | 0 | stacked on 07 (`--on`), checkout fast-forwarded to 07's head, landed after 07 |

Overlaps reported to the agents in `push --json`, with the suggested
re-plan: 05 and 07 (07 declared `internal/catalog/**`, 05 wrote
`internal/catalog/price_test.go`: "land 05 before 07"), 07 and 02 (both
edited `internal/server/server.go`: "stack"), and 08 and 02. The auth change
(02) still merges cleanly onto the trunk after 07 and 08 landed (checked
locally with `git merge`). Both projects answer on the public audit view.

Real agents write their own code, so tags can differ: a large diff can lift
risk to `medium` (over 400 lines or 15 files), which takes it out of
`routine-app` and sends it to a person.
