# Demo scripts

`golden-path.sh` runs the golden path (the README describes it) against the deployed Worker and real Artifacts, then prints a PASS/FAIL/PENDING table and writes a JSON summary. It never approves or rejects anything: changes routed to a person are left for the person. Last run: `docs/demo-run-2026-10-13.md`.

```bash
scripts/demo/golden-path.sh                       # needs `wayglass login` (or WAYGLASS_URL and WAYGLASS_KEY)
DEMO_EU_PROJECT=new scripts/demo/golden-path.sh   # fresh EU project instead of eu-demo-1791797572
DEMO_POCS=0 scripts/demo/golden-path.sh           # skip the probe fork and clone checks
```

Exit 0 when nothing failed (pending rows do not fail the run). `DEMO_PROJECT_PREFIX=demo-test DEMO_EU_PROJECT=new` makes it a throwaway run that `wayglass project delete` can remove. Cost per run: 1 US trunk, 3 US forks, 1 EU fork, about 10 Worker-minted tokens; the run prints the exact numbers. Billing starts Oct 14, so run it when it tells you something new.

`examples.sh` sets up the Acme Shop example (`examples/acme-shop/`): two public projects (`demo-acme-<unix>` us, `eu-demo-acme-<unix>` eu) seeded with the sample app, one clone, name and launcher per task brief, and a reviewer session, for real Claude Code and Codex agents. `land-when-approved.sh` is installed next to `wayglass` as `wayglass-land-when-approved` for agents waiting on a reviewer or a person.

```bash
scripts/demo/examples.sh                                         # prints how to start each agent
DEMO_PREFIX=demo-test-acme scripts/demo/examples.sh --standins   # scripted dry run, checks every route
```

`--standins` runs only on `demo-test-*` projects, because its scripted reviewer approves `agent_review` changes. Cost per run: 2 trunks, 8 forks, about 30 tokens.

`scale.sh` starts N agents (default 10, `-n 100`) on one fresh public project `demo-scale-<unix>`, seven of them breaking a rule each (undeclared paths, auth under a docs goal, personal data, self-approval, a rubber-stamp approval, an agent key approving a change routed to a person, a forged git note), and checks every breaker is caught. It prints latency p50/p95/max per step, decisions by action, overlaps, HTTP errors by status and the Worker's Artifacts operations, and writes `docs/scale-run-<date>.md`. It never approves anything; changes routed to a person stay pending.

```bash
SCALE_PREFIX=demo-test-scale scripts/demo/scale.sh   # 10-agent dry run; wayglass project delete removes it
scripts/demo/scale.sh -n 100 --json                  # summary JSON on stdout
scripts/demo/scale.sh --revoke-keys <project>        # agent keys a crashed run left
```

Cost: measured on the Oct 14 dry run, about 37 Worker-metered Artifacts operations per agent (a landed docs change about 43, a change left pending about 24), plus each agent's git fetch and push.

**Keys.** Every agent in these scripts (golden-path agents, each launcher, each stand-in, the reviewer, the tape agents, the second machine) gets its own agent key, minted with your admin key when it starts and revoked when it exits (`agent-keys.sh`). The Worker binds the agent's name to its key and refuses it on changes routed to a person (`needs_human`, exit 4); your admin key stays with you for `init`, `review` and `approve`. Keys live only in environment variables, never in a launcher file, argv or a log. If a launcher is killed before it can revoke its key: `scripts/demo/examples.sh --revoke-keys <us-project>` (or `wayglass keys list` and `wayglass keys revoke <id>`).

Output lives in a temp dir it prints (`summary.json`, each agent's clone and its `--json` output with token values removed). No token is written to disk.

**Bash.** `golden-path.sh`, `examples.sh` and `tapes.sh` need Bash 4 or later (associative arrays); `scale.sh` needs Bash 5 (`EPOCHREALTIME`). macOS ships Bash 3.2, so install a newer one (Homebrew or Nix) and put it first on `PATH`; the scripts start with `#!/usr/bin/env bash`. `second-machine/run.sh` and `land-when-approved.sh` run on Bash 3.2.

## Video sequence

The order of the golden path. Run `golden-path.sh` once before recording to warm everything up and to have a pending auth change and a quarantined PII change ready; or run the agents live in split panes as below. Two terminals: **left** for agents, **right** for the person.

Setup, off camera:

```bash
go build -o bin/wayglass ./cmd/wayglass && export PATH=$PWD/bin:$PATH
mkdir -p /tmp/acme && cd /tmp/acme && git init -b main     # add a few files: docs/, src/auth/login.ts
git add -A && git commit -m "feat: seed the demo app"
```

| # | Type | What appears | Status |
|---|---|---|---|
| 1 | `wayglass init --project acme-demo` | "Created project acme-demo (trunk acme-demo, jurisdiction us)", `.wayglass/policy.yaml` written and pushed to the trunk. Show the policy file for five seconds: data classes, jurisdiction, the quarantine rule. | Works |
| 2 | Left, two panes: `git clone /tmp/acme docs-agent` and `auth-agent`; in each `WAYGLASS_AGENT=demo-docs wayglass intent declare --goal ... --reason ... --path "docs/**" --json` and `WAYGLASS_AGENT=demo-auth ... --path "src/auth/**" --path docs/auth.md --json` | Each gets its own fork and a short-lived write token scoped to that fork. The second declare prints `overlaps` naming the first intent and `docs/auth.md ~ docs/**`. | Works |
| 3 | Right: `wayglass watch` | The swarm live: both intents, the overlap, each change's tags as it is classified, counters. | **Pending** (another stream wires watch to the Worker; today only `--demo-data`, labelled sample) |
| 3 | Left: edit, `git commit`, `wayglass push --json` in both | `files` and `overlaps` in the JSON. `wayglass overlaps` shows the pair. | Works (detection). The suggested re-plan in `watch --json` is checked too; not yet run live |
| 4 | Left: `wayglass propose` in both, then the EU agent | Docs: `rules[4] -> auto_merge`, exit 0. Auth: `rules[2] -> human_approve`, exit 4 "Waiting for a person". PII on `eu-demo-1791797572`: `rules[3] -> quarantine`, jurisdiction `eu`, exit 4. Each shows area, data class, risk, jurisdiction. | Works |
| 4 | Left: `wayglass land` in the docs agent | "Landed ... (fast-forward). Worker verified the trunk holds exactly the approved change; trunk token revoked. Why note pushed with it." | Works |
| 5 | Right: `wayglass review` | The auth change with its intent, tags and matched rule; approve with a reason. | **Pending** (TUI on sample data). Works today without the TUI: `wayglass approve <auth-intent> --reason "Lockout matches the intent; tests cover the reset path"`, then `wayglass land` in the auth agent |
| 6 | `wayglass why docs/getting-started.md:7` | Intent, why, tags, `auto_merge by rule rules[4], decided by policy`, read from the git note. For the auth line after approval it shows the decider; the approver's reason is not shown yet (another stream). | Works |
| 7 | Second machine (Linux VM): `wayglass session fork <url>` | The agent's code plus its intent, notes and context; the agent carries on. | Harness shares the auth session and forks it as `demo-second`; not yet run live (routes not deployed). Proven underneath: notes survive an Artifacts fork, and an agent can join a project with only its fork token |
| 8 | Browser: the audit view | The same trail as `wayglass` shows: intents, decisions, reasons. | On api.wayglass.dev the web app is behind Cloudflare Access: sign in, then open `/?project=<name>`. On a self-hosted Worker deployed with `--public 'demo-*'`, projects made with `wayglass init --public` are also readable anonymously. |
| - | Desktop app | Monitor agents and approve or reject changes routed to a person. | **Pending** (another stream, Wails) |

Talking points, each true as of the Oct 13 run: the docs change landed without a person; the auth change waits for one; the EU personal data is stored only in the EU namespace and is quarantined; every decision names the rule that matched; agents write only to their own fork, and touch the trunk only through `land`, with a 300-second token, for a change policy approved.
