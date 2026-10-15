# Architecture decisions

Decisions the maintainer has made. Agents build on these; changing one needs the maintainer. This file records what was decided and why.

| # | Decision | Why |
|---|---|---|
| D1 | **GitHub org `wayglass`.** Repos: `wayglass` (CLI + Worker + in-product audit view), `site` (wayglass.dev, static Astro), `docs` (docs.wayglass.dev, Astro Starlight), Cloudflare skills in `tpscloud/flared` (`cloudflare-artifacts`), plus `wayglass-skill`. All public at release. | One clone and one run-instructions block for judges; sites and skills ship independently. |
| D2 | **Credentials.** The maintainer's scoped API token (this account only) in a shell for spikes and namespace setup. Deploys via **Workers Builds** on push to main. The deployed Worker holds no Cloudflare API token for Artifacts: Artifacts is reached only through the `env.ARTIFACTS` binding. The one exception is D12. | Cloudflare end to end; the product's own Zero Trust story applies to how it is built. |
| D3 | **Server-side policy = the Go evaluator compiled to Wasm**, run inside the Worker. The Worker re-derives tags from what it reads in Artifacts (`readCommit`/`readTree`/`readBlob`) and never trusts client-reported tags or actions. Client-side evaluation is a preview only. | One implementation, no drift; a lying client cannot self-approve. |
| D4 | **Push detection.** The client reports changed files on `wayglass push`; the Worker verifies against `readCommit`. One Queues event subscription per **trunk** repo catches lands, created by the Worker on project create (D12). No per-fork subscriptions. | `repo.pushed` is per-repo only; per-fork subscriptions would need an API token in the Worker. |
| D5 | **LLM is Cloudflare-first and pluggable.** The AI reviewer uses the `AI` binding, with gateway `AI_GATEWAY_ID` and enabled providers from `AI_PROVIDERS` (Workers AI by default; Anthropic and OpenAI through Gateway BYOK or Unified Billing). Policy can choose `reviewer: ai\|agent\|any`, a model and confidence floor. The reviewer chooses another model family, approves through the ordinary decide path, or escalates to a person. Human-routed changes can receive advisory summaries without a verdict. The diff cap is 60 KB; `AI_DAILY_REVIEWS` defaults to 200 calls per project per UTC day, reserved atomically in the Project DO before each call, including fallback calls. Over budget, review escalates and advice is skipped, with an audit record. `GET /v1/cost` reports AI usage and estimates. Hosted production stays `manual`; self-hosting defaults to `off`. See [POLICY.md](POLICY.md). | Bound cost and preserve the human gate across providers. |
| D6 | Conventions changed: Go floor 1.25.8; `@cloudflare/vitest-plugin`; git tokens via `GIT_CONFIG_*` env. (AGENTS.md updated.) | Charm v2 requirement; upstream deprecation; keep tokens out of `ps`. |
| D7 | Changes use reviewed branches and small conventional commits that explain why. | Keep each change reviewable. |
| D8 | **One build.** Priorities: design quality and cost. | Decided Oct 11. |
| D9 | **Coding agents and the AI reviewer share the review queue.** A reviewer agent picks up `agent_review` work with `wayglass review --for agent --json`. Policy can request that agent explicitly or invoke the AI reviewer; `any` permits either. The AI provider must differ from the author’s model family. Advice on changes routed to a person never substitutes for approval. Calls, outcomes, token usage and the remaining daily budget are recorded per project. | Use existing agent subscriptions where appropriate and account for paid model calls. |
| D10 | Golden-path step 7 second machine: a **Linux VM** (uses the linux build from CI). | Shows cross-platform. |
| D11 | Vendored skills are reviewed by the maintainer before release; personal/unrelated skills are not vendored. | `no-decorative-counts` slipped in and was removed. |
| D12 | **The Worker subscribes each new trunk to `pushed`.** `POST /v1/projects` calls the event-subscriptions REST API with secret `CF_EVENTS_API_TOKEN`, an API token limited to **Account > Queues > Edit** (plus secret `CF_ACCOUNT_ID`). Failure never blocks project creation; it is audited as `trunk.events_unsubscribed` and `wayglass init` warns. (Oct 12.) | `pushed` exists only per repo, there is no binding for subscriptions, and agents creating projects have no Cloudflare credentials. Queues Edit cannot touch Artifacts, Workers or DNS. |
| D13 | **Jurisdiction is per project.** `POST /v1/projects {jurisdiction}` (from `wayglass init --jurisdiction us|eu`) picks the namespace: `ARTIFACTS` (`wayglass`, us) or `ARTIFACTS_EU` (`wayglass-eu`, eu). Every repo call for the project goes to that binding, and the Worker tags changes with the project's jurisdiction; the client cannot claim another. Unknown jurisdictions are refused. (Oct 12.) **Oct 14: the project's Worker state lives there too.** A project in a jurisdiction Durable Objects can pin to (`eu`, `fedramp`) gets its Project Durable Object from `env.PROJECT.jurisdiction(j)`, so its intents, decisions, audit log, sessions, approvals, land records, token ids and cost meters never leave that jurisdiction; projects in the Worker's main jurisdiction keep their unpinned object. A name -> jurisdiction directory in the Registry object (written at create, before any state exists) tells every route, the queue consumer and the public views which object to use; nothing falls back. An EU project from before Oct 14 is refused (`residency_pending`, 503) until the admin runs `wayglass project migrate-residency <name>`, which copies every table row for row, verifies counts and a digest on both sides, wipes the old object and leaves a tombstone that refuses every call (`project_moved`), and appends `project.residency_migrated` to the audit log. See "Where a project's data lives" below. | Golden-path step 4 (PII quarantined) needs real EU data residency, not a flag; Artifacts cannot move repos between namespaces, so the choice is made once at creation. A repo in the EU with its audit log in a US Durable Object is residency in name only. |
| D14 | **Per-agent keys, bound to one name** (built Oct 13). The admin mints `wgk_` keys (`wayglass keys create --agent <name>`, `POST /v1/keys`); the Worker stores only their SHA-256 in one `Registry` Durable Object (migration `v2`). A request made with one is attributed to its bound name on every route, ignoring `X-Wayglass-Agent`, `body.agent` and `body.by`, so it cannot approve its own change under another name. Key management is admin-only; the admin key still works and still names itself. `GET /v1/whoami` reports the identity. Oct 13: keys have a `kind` (`agent` by default, `human` with `--human`); only the admin key or a human key may decide a `pending_human` or `quarantined` decision (`needs_human` otherwise). Not done: short-lived session tokens exchanged for a key, keys scoped to projects, audit of key minting in a project log. | One shared admin key let any caller write any name into the audit log, including an approver's. A hash lookup per request is one DO call; keys are 32 random bytes, so there is nothing to time. |

## Where a project's data lives (D13, Oct 14)

For a project made with `--jurisdiction eu` (`worker/src/residency.ts`):

| Data | Where | Why |
|---|---|---|
| Repos: trunk, every fork, git notes, repo tokens | Artifacts namespace `wayglass-eu` (jurisdiction `eu`) | Picked at create; Artifacts cannot move a repo. |
| Project state: intents, decisions, approvals, lands, token ids, sessions (snapshots and notes), audit log, cost meters, the public flag | Project Durable Object created with `env.PROJECT.jurisdiction("eu")` | Pinned at create; the directory routes every request there. |
| Push events for the trunk (`cf.artifacts.repo.pushed`) | Recorded in the pinned object | The queue consumer looks the project up like any route; a project whose state is not where it belongs is retried, never written elsewhere. |
| Public caches (`caches.default` of `/v1/public/*`) | The colo that answered, for 15 s | Only what the public view already shows, and only for projects the admin opted in. |

What stays global, and why:

| Data | Where | Why it is global |
|---|---|---|
| Name -> jurisdiction directory (`project_directory`) | Registry Durable Object (unpinned) | Names and jurisdictions only, no project data; it is what finds the pinned object. |
| Agent and human keys (`keys`, `key_events`) | Registry Durable Object | Identities of callers, not project data; one key works across projects. |
| Worker compute, Queue transit, Access sign-in | Cloudflare's edge | A request is handled where it arrives; the state it reads and writes stays pinned. Workers Logs lines (`wayglass.project.*` events) carry project names and counts, no change content. |
| The Artifacts events subscription for the trunk | Cloudflare account resource | Subscription id and repo name only. |

Projects in the Worker's main jurisdiction (`us` on api.wayglass.dev) keep the unpinned object they always had; the rule only pins jurisdictions Durable Objects can pin to.

## Spike results (Oct 12, real Artifacts, namespace `wayglass`)
`scripts/spike.sh` against `https://api.wayglass.dev` passed all five steps (run `spike-1791788867`, 56 s):
1. **Trunk create + token: OK.** Remote `https://<account>.artifacts.cloudflare.net/git/wayglass/<repo>.git`. The Worker revokes the creation token and mints a 900 s write token.
2. **Fork per intent: OK.** Fork `<project>--<intent>` with its own 900 s write token.
3. **Clone / commit / push with `http.extraHeader` via `GIT_CONFIG_*`: OK.** No token in `.git/config`. **Zero Trust holds:** the fork token is refused by the trunk.
4. **`refs/notes/wayglass` push + fetch back: OK** (to and from the fork). Notes surviving a *fork* is still unverified.
5. **`cf.artifacts.repo.pushed` reaches the queue consumer: OK**, within one 30 s re-push. `before` is the previous tip on an existing ref. Wrangler accepts the short form `--events pushed`.

What the real API disagreed with (all fixed):
- **Token format.** Real tokens are `art_v2_<letter>_<40 hex>?expires=<unix>`; the docs still show `art_v1_<40 hex>`. Redaction (`gitx`) and the example `secrets` pattern now match any version.
- **Stub lifetime.** A repo stub from `env.ARTIFACTS.get()` held with `using` is disposed when the function returns, so `return guard(...)` without `await` failed with "RPC stub used after being disposed". Covered by `worker/test/binding-host.test.ts`.
- **Secret propagation.** `wrangler secret put` takes a few seconds to reach requests; `wayglass login` right after it can see `not_configured` or `unauthorized`.

### Follow-up POCs (Oct 13, `scripts/demo/golden-path.sh`, run `demo-1791938487`)
Plain git with Worker-minted fork tokens via `GIT_CONFIG_*`; no Cloudflare API token.

| Question | Result |
|---|---|
| Do git notes survive an Artifacts fork? | **Yes.** A fork made after `land` pushed `refs/notes/wayglass` to the trunk lists that ref, and the note on the landed commit reads back intact (`intent_id`, `action`, `decided_by`), although the Worker forks with `defaultBranchOnly: true`. |
| Is `before` set on a new ref in `repo.pushed`? | **Yes, 40 zeros.** Seen for `refs/heads/main` on the `init` push and for `refs/notes/wayglass` on the first land (also in the `wayglass` project's log, seq 3 and 19). Notes pushes carry `commits: []`. |
| Does `commits[]` hold every commit? | **No, at most 20.** The first `wayglass` trunk push listed 20 commits with `total_commits: 21`. Use `before..after`, not `commits[]`, when it matters. |
| Shallow clone (`--depth 1`)? | **Works** on a fork. |
| Blobless clone (`--filter=blob:none`)? | **Works and is honoured**, despite the docs: historical blobs stay on the server (`GIT_NO_LAZY_FETCH=1` shows them missing) and checkout lazy-fetches current ones with the same token. |
| Can an agent join an existing project with no trunk access? | **Yes.** The EU agent started from an empty `git init`, declared an intent, fetched the trunk's history from its own fork with the declare token, and pushed and proposed as usual. |

## Open findings from live docs (verify in the spike)
- `repo.pushed` exists only on the per-repo `artifacts.repo` event source.
- `createToken` returns `{id, plaintext, scope, expiresAt}`; TTL 60 s to 1 year, default 86,400 s; scope defaults to `write`. Always pass an explicit short TTL.
- `fork` copies only the default branch by default. Unknown: whether `refs/notes/*` survive a fork.
- Artifacts has no local simulator; `wrangler dev` with the binding hits the real namespace (costs operations).
- Unverified: do binding reads count as billed ops. (Blobless clone, `before` on new refs and notes surviving a fork: verified Oct 13, see above.)

