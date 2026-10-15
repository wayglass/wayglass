# Policy

`.wayglass/policy.yaml` maps classified changes to actions. Rules are checked
top to bottom, and the first **enforcing** rule that matches decides. This page
covers how rules match, what the file may not contain, trying a rule before it
decides anything, and which policy a decision is made under. The full example
is [`examples/policy.yaml`](../examples/policy.yaml).

## `area` and `only_area`

```yaml
rules:
  - { when: { area: auth }, action: human_approve, reason: "Auth is a security boundary." }
  - { when: { only_area: docs, risk: low }, action: auto_merge, reason: "Every file is docs." }
  - { when: { only_area: tests }, action: agent_review, reason: "Every file is a test." }
```

- `area: [a, b]` matches when **any** changed file is in a or b. Use it for
  strict rules (block, quarantine, a person decides): one auth file is enough.
- `only_area: [a, b]` matches when **every** changed file is in a or b (a file
  may also be in other areas). Use it for permissive rules: `area: docs` would
  auto-merge a change that edits a README *and* the billing code;
  `only_area: docs` does not. A change with no files never matches.

Tags carry `area_sets`, the distinct sets of areas the changed files fall in
(`[]` for a file in no area), so the decision record shows why `only_area`
did or did not match.

## How paths match

Area globs and `data_class.paths` globs are matched against each changed
file's repo-relative path with [doublestar](https://github.com/bmatcuk/doublestar)
syntax: `*` and `?` stay inside one path segment, `**` spans any number of
directories including none (`**/auth/**` covers `auth/x.go`, `a/auth/x.go`
and the file `auth` itself), and `{a,b}` and `[ab]` work.

- Dotfiles are ordinary files: `docs/**` covers `docs/.notes.md` and
  `.github/**` covers its workflows.
- A leading `./` is dropped and a backslash is read as `/`, so a file
  committed as `web\login.ts` is still in `web/**` and `.wayglass\policy.yaml`
  still meets `builtin:protect-policy`.
- Matching is **case-sensitive**: `web/**` does not cover `Web/`, and
  `**/*.md` does not cover `README.MD`. Write globs for the paths the repo
  has, and add a variant when a case differs on purpose.
- A rename is a delete plus an add. The added file's every line is scanned,
  so a secret moved into a new file is found again; the deleted side carries
  no text and is never scanned.
- A binary file (a NUL byte in its first 8,000 bytes, as git decides) carries
  no lines: `content` detectors never see it and it adds nothing to
  `lines_changed`. Only a `paths` detector can tag it, and unless its name is
  in `binary.allow` it goes to a person (see below). List the file types
  that hold secrets under `data_class.secrets.paths`: the example lists
  `**/*.pem`, `**/*.p12`, `**/*.pfx`, `**/*.key`, `**/id_rsa`,
  `**/id_ed25519`, `**/.env` and the usual `.env.<stage>` names. A glob
  cannot exclude a name, so `.env.example` and `.env.sample` are left out by
  listing the stages (`{local,development,dev,production,prod,staging,stage,test,ci}`)
  instead of `.env.*`; a real key pasted into a template is still caught by
  the `content` patterns.
- `content` regexes (RE2, one line at a time) see only added lines. A value
  split across lines or encoded is not matched; the detectors catch the
  common slip, not an agent determined to hide something. The example's
  `secrets` patterns cover the words `api_key`/`api-key`, Artifacts repo
  tokens, PEM private keys (`-----BEGIN ... PRIVATE KEY-----`, not
  certificates or public keys), AWS access key ids (`AKIA`/`ASIA` plus 16
  characters, so the word in prose does not match), GitHub tokens
  (`ghp_`/`gho_`/`ghu_`/`ghs_`/`ghr_`, `github_pat_`), Anthropic (`sk-ant-`),
  OpenAI (`sk-`, `sk-proj-`; eight key characters must follow, so `sk-ant-`
  and `sk-SK` are not OpenAI keys), Slack (`xox[baprs]-`), Stripe live keys
  (`sk_live_`, `rk_live_`; test keys pass) and Google API keys (`AIza`).
  Cloudflare API tokens have no fixed prefix and are not listed. Evidence
  records the pattern, the file and the line number, never the matched text.
  Every pattern has a positive and a false-positive case in
  `testdata/policy-vectors.json`, run by Go and by the Worker.

A change too large to read in full is marked truncated and goes to a person
at minimum (`builtin:diff-truncated`); a stricter decision such as `block`
stands.

## `binary`: files the detectors cannot read

```yaml
classify:
  risk:
    high: { lines_changed_over: 400, binary_bytes_over: 250000 }
binary:
  allow: ["**/*.png", "**/*.jpg", "**/*.jpeg", "**/*.gif", "**/*.webp", "**/*.ico", "**/*.woff", "**/*.woff2"]
```

The content detectors read none of a binary file, so a secret, a key store
or a credential dump behind a NUL byte would otherwise be tagged by its
path alone and could reach `only_area: docs, risk: low` as a routine docs
change. The evaluator closes that:

- Every binary file in a change is listed in the tags as `unscanned` (with
  an `unscanned: binary` evidence line naming the path and its size), and
  `binary_bytes` sums their sizes.
- **A binary file whose path matches no `binary.allow` glob sends the change
  to a person at minimum** (`builtin:unscanned-binary`). Like the other
  built-in guards it only raises: `multi_approve`, `quarantine` and `block`
  stand, and a shadow rule judged under the same floor does not "differ".
  The reason names the files (`tools/run.bin is a binary file outside
  binary.allow; its content cannot be scanned, so a person decides.`), and
  the record keeps the rule it overrode.
- `binary.allow` is matched like area globs (case-sensitive, `**` spans
  directories). When the key is absent, the list above applies: the image
  and font types a web or docs repo ships. `allow: []` sends every binary
  file to a person. A stricter list pins assets to where they live
  (`web/assets/**/*.png`). Trust rests on the name: a file called
  `logo.png` that holds plain text is text and is scanned in full, but one
  that holds a NUL byte and anything after it passes as an image. Keep the
  list to what the repo really ships. PDFs are not on it: a PDF can carry
  scripts and embedded files, so one goes to a person unless the project lists it.
- Archives, executables, `.pem`, `.p12`, `.der`, `.keystore`, files with no
  extension, and submodule pointers are never in the default list, so they
  stop at a person. A `paths` detector can still tag them (`secrets` for
  `**/*.p12`) and a rule can block them outright.
- Only the new side counts. A deleted binary brings nothing to scan and is
  not unscanned; a file that turns from binary to text is scanned in full
  as a fresh add; a text file renamed to a binary one is a delete plus an
  unscanned add.
- `binary_bytes_over` under `classify.risk` is a threshold on
  `binary_bytes`, so a large allowlisted asset is not low risk. The Worker
  reads blobs up to 512 KiB; a larger file marks the diff truncated and goes
  to a person anyway, so set the threshold below that. The CLI preview sizes
  binary files with one `git cat-file` call so `propose --dry-run` and
  `policy test` agree with the Worker.

Decisions recorded before this guard existed carry no `unscanned` field;
nothing already decided changes. Only new proposals are affected.

## `reviewer`: a reviewer from another model family

```yaml
rules:
  - name: tests-cross-review
    when: { only_area: tests }
    action: agent_review
    reviewer: { different_family: true }
    reason: "Tests only; a model from another family checks them."
```

`reviewer: { different_family: true }` means whoever approves the change must
belong to a different model family than its author, so a model never signs off
on its own family's blind spots. Families are `claude`, `openai`, `gemini`, `meta`,
`other` and `human` (a person); `anthropic`, `codex`, `gpt`, `google` and
`person` are accepted as aliases (and `llama` for `meta`).

- The author declares a family with `wayglass intent declare --family claude`
  or `WAYGLASS_AGENT_FAMILY=claude`; it is recorded on the intent. An approver
  sends theirs the same way (`wayglass approve --family openai`), and it is
  recorded on the approval and in the audit log.
- The Worker enforces it when someone approves: `same_family` if the families
  match, `family_required` if the author or the approver declared none. Two
  `other`s are refused, because nothing shows they differ. Rejecting is always
  allowed.
- It applies to `agent_review`, `human_approve`, `multi_approve` and
  `quarantine` (rules and `default`). On `auto_merge` or `block` nobody
  approves, so the file is refused. A built-in guard that raises the action
  (for example `builtin:protect-policy`) replaces the rule's routing, its
  reviewer constraint included.

**Families are self-reported.** Nothing proves an agent is the model it says
it is, and per-agent keys do not carry a family yet. Like the author check
(the author cannot approve their own change), this stops honest mistakes and
lazy self-review, not an agent that lies about itself.

## `multi_approve`: who counts as an approver

`multi_approve` waits for `approvers` **distinct identities**, and an
identity is a key, not a name:

- A bound key (an agent key, or a human key minted with
  `wayglass keys create --human` or by Cloudflare Access sign-in) decides as
  the name it is bound to; it cannot report another.
- The admin key is one identity, recorded as `admin`, whatever name it
  reports (`--agent`, `WAYGLASS_AGENT` or `by`). The reported name is kept
  on the approval and in the audit event as `claimed_by`, so `why` reads
  `admin (john)`, but it never counts as a second approver. No key can be
  bound to the name `admin`.

So two people are two keys: mint a human key per person, or have each sign
in through Access. An approval that leaves the decision pending says so in
the reply (`waiting_for`: "needs another approver: 1 of 2 distinct approvers
so far"), and the CLI prints it after "Recorded". The author check holds the
admin key to the name it claims as well, so an admin approving its own
intent under the same name is still refused.

## `reviewer: ai | agent | any` and `ai: { ... }`

An `agent_review` rule can also say *who* reviews, and tune the Worker's AI
reviewer; a rule routed to a person can ask the AI for advice:

```yaml
rules:
  - name: tests-ai
    when: { only_area: tests }
    action: agent_review
    reviewer: ai                       # any (default), ai or agent
    ai: { min_confidence: 0.8, model: "@cf/meta/llama-4-scout-17b-16e-instruct" }
    reason: "Tests only; the AI reviewer decides, confidently."
  - name: docs-agent
    when: { only_area: docs }
    action: agent_review
    reviewer: { kind: agent, different_family: true }   # the long form takes both
    reason: "Docs get a reviewer agent from another family."
  - name: web
    when: { area: web }
    action: human_approve
    ai: { advise: true }
    reason: "A person decides, with AI advice on the side."
```

`reviewer` is a bare word (`ai`, `agent`, `any`) or a mapping with `kind` and
`different_family`. The kind only applies to `agent_review`; on an action a
person decides it is refused.

- `ai`: the Worker's AI reviewer runs on `propose`, whenever the Worker has it
  on at all (`AI_REVIEW=manual` or `auto`). `AI_REVIEW=off` is the kill
  switch and still wins: the change then waits for a reviewer agent.
- `agent`: a reviewer agent decides; `wayglass ai-review` refuses with
  `needs_agent`, and `AI_REVIEW=auto` skips the change.
- `any` (the default): a reviewer agent, or the AI reviewer when asked
  (`wayglass ai-review`) or when the Worker runs it on every propose.

`ai` holds the AI settings for the rule. Everything in it is optional, but an
empty block is refused:

| Key | On | Meaning |
|---|---|---|
| `min_confidence` | `agent_review` | The self-reported confidence (0 to 1) below which the AI's approval is not trusted and the change goes to a person. Default 0.7. |
| `model` | any reviewed action | Pins the model: a Workers AI id (`@cf/<vendor>/<model>`) or an AI Gateway id (`anthropic/claude-sonnet-5.5`, `openai/gpt-5.4-mini`). Used only when its family differs from the author's and its provider is enabled on the Worker (`AI_PROVIDERS`); otherwise the Worker picks as usual and records why. |
| `advise` | `human_approve`, `multi_approve`, `quarantine` | On propose, the AI writes advice for the person deciding (see "AI advice" below). It never decides. |

The Worker reads these from the decision it recorded, so a built-in guard
that raises the action (for example `builtin:protect-policy`) drops them with
the rest of the rule's routing, like `reviewer`.

## `agent_review` and the AI reviewer

`agent_review` hands a change to a second agent: the decision waits as
`pending_agent` until a reviewer that did not write it approves or rejects
it. A coding agent finds that work with `wayglass review --for agent --json`,
reads the change with `wayglass diff <intent-id>` and decides with
`wayglass approve|reject`. Every reviewer rule applies to it: the author never
decides their own change, a placeholder or rubber-stamp reason is refused, and
`reviewer: { different_family: true }` needs a reviewer from another model
family.

A Worker with the AI reviewer on can also decide `agent_review` changes with a
model (`wayglass ai-review <intent-id>`, or on its own after every
`propose` when the Worker runs with `AI_REVIEW=auto`). The model is treated as
one more agent, with less trust, not more:

- It decides as `<provider>:<model>`, with the model vendor’s family
  (`meta`, `claude` or `openai`) and identity source `ai`. Its approval goes through the same path as any reviewer's, so the
  author check and the cross-family rule hold (an author who declared no family
  under a `different_family` rule gets `family_required`, as for any
  reviewer), and `wayglass why`, the audit log and the note on the landed
  commit name it.
- It can only **approve** or **escalate**. Escalating moves the decision to a
  person (`human_approve`, `pending_human`) with the model's reason and
  concerns in the audit log (`decision.escalated`) and in the decision's
  `detail.escalation`; from there only a person decides, and the AI cannot be
  asked again. It never rejects, and it never touches a change policy routed
  to a person (`needs_human`, like an agent key).
- Everything it reads from the change (goal, reason, paths, diff) is handed
  over as delimited untrusted data. It must answer in a fixed JSON schema with
  a specific reason and a confidence. Anything else escalates: text in the
  change addressed to the reviewer (checked before the model is called, and
  reported by the model), an answer off the schema, a model error, a
  confidence under the rule’s floor (0.7 by default), a rubber-stamp reason, a diff over 60 KB or one the
  Worker could not read in full. Nothing on that path can approve.
- With `AI_REVIEW=off` (the default) or no Workers AI binding, `ai-review`
  answers `ai_unavailable` and the change keeps waiting for a reviewer agent.

Workers AI bills the account per token; see
[docs/SELF-HOSTING.md](SELF-HOSTING.md#costs).

## AI advice and daily budget

`wayglass ai-advice <intent-id>` records a summary, risks and questions for
changes waiting for a person. It does not approve, reject or change the
policy action. A rule with `ai: { advise: true }` requests this on propose;
`AI_REVIEW=auto` also requests advice for human-routed changes. The audit
entry is `decision.ai_advice`; clients read `detail.ai_advice` and label it
advisory. An escalation preserves the original rule reason in
`detail.escalation.rule_reason` and explains why a person must decide.

`AI_DAILY_REVIEWS` limits model calls per project per UTC day (default 200).
Reviews and advice share the limit; a fallback consumes another call.
The Project Durable Object reserves each call before contacting the model,
so simultaneous requests cannot exceed the limit. Reservations remain used
if a request is interrupted. At the limit, reviews escalate to a person;
advice returns `ai_budget` and records `decision.ai_skipped`.

`GET /v1/cost` includes `ai`: calls, approvals, escalations, advice, tokens,
estimated dollars and today’s calls and estimate. `budget` reports the daily
limit and remaining calls. Estimates use the listed model prices and
reported tokens; unpriced calls are counted separately and are not free.
Provider selection, gateway configuration and self-host defaults are in
[SELF-HOSTING.md](SELF-HOSTING.md#ai-providers).

## `mode`

```yaml
rules:
  - name: trial-pii-review
    when: { data_class: pii }
    action: quarantine
    reason: "Trial: all personal data gets a DPO look."
    mode: shadow          # enforce (the default) or shadow
```

- `mode: enforce` (the default when `mode` is absent): the rule decides when it is
  the first enforcing rule that matches.
- `mode: shadow`: the rule never decides. Deleting every shadow rule leaves every
  decision unchanged; a test checks this.
- `shadow: true` is the older spelling and still works. `mode: enforce` together
  with `shadow: true` is rejected as a contradiction.

Give shadow rules a `name`. Without one, a rule is labelled `rules[i]`, and
inserting a rule renumbers every rule after it.

## What a decision records

Each decision, whether it is the CLI preview, the Worker's decision record
(`detail`) or the `decision.proposed` audit entry, carries:

| Field | Meaning |
|---|---|
| `shadow_matches` | Every shadow rule that matched, in file order: `{rule, index, action, approvers?, reason, preempts}`. Always a list, empty when none matched. |
| `preempts` | The shadow rule comes before the rule that decided, so it would have decided had it been enforced. A shadow rule after the deciding rule is recorded but would never be reached. |
| `would_differ` | Enforcing the **first** preempting shadow rule would have changed the action (or, for `multi_approve`, the approver count). Built-in guards apply to the hypothetical too: a lax shadow rule on a `.wayglass/` change does not "differ", because `builtin:protect-policy` would still send it to a person. |

`wayglass propose` prints one line when a shadow rule matched, for example:

```
  shadow: trial-pii-review would have quarantined this
```

## Which policy decides, and when it changes

The Worker reads `.wayglass/policy.yaml` from the **trunk's current head** when
a change is proposed, never from the intent's fork, so an agent cannot relax
the rules its own change is judged by. The decision records the policy it was
made under: `policy_commit` (the trunk commit it was read from) and
`policy_sha256` (`sha256:<hex>` of the file's exact bytes, or `absent`).

A decision holds only under that policy. When `wayglass land` starts, the
Worker reads the trunk's policy again; if its bytes differ in any way (a rule,
a comment, or the file removed), land stops with **`policy_changed`** (409,
CLI exit 1) before any trunk token is issued. Recover in the intent's clone:

```bash
wayglass sync       # merges the trunk (and its policy) into the intent; never rebases
wayglass push
wayglass propose    # decided under the current rules
```

The merged policy edit is the trunk's, not the intent's, so it is not part of
the change being decided (see "What a decision covers" below); the intent's
own change is decided under the new rules. If `sync` stops with `sync_conflict`, it lists
the files and leaves the merge in progress: resolve them, `git add`, `git
commit --no-edit`, then push and propose. The same steps recover from
`land_conflict` and `repropose_required`. The refused land is
cancelled and audited as `land.cancelled` with the reason, like a land refused
because the fork moved; refusals before a land is reserved are not audited.

Fails closed: if the trunk's policy cannot be read, land refuses (an
`upstream_error`, no token). Decisions made before Oct 14 carry no digest;
land compares the policy at the intent's trunk base instead, and refuses with
`policy_changed` when that cannot be read either.

## What a decision covers

Classification, the risk size and the decision's path set cover only the
intent's **own change**: what landing it would add to the trunk. The Worker
diffs the fork head from its merge base with the trunk's current head, so
trunk files that `wayglass sync` (or a `land_conflict` resolution) merged into
the intent do not count, and a one-file docs change stays a one-file change
however much the trunk moved. Anything the merge itself changes does count:
a resolution that edits a trunk file, a file added in the merge commit, or a
merged trunk change the intent later reverts. An intent that edits
`.wayglass/` still meets `builtin:protect-policy`.

Fails closed: the merge base is accepted only when the Worker proves it (a
commit in the trunk's store must be reached by a bounded walk from the trunk
head). If it cannot, or the history is too long to walk, the decision falls
back to the diff from the intent's fork base, which classifies more. Stacked
intents keep the diff from their parent's head. The decision's `detail.diff_base`
records which base was used (`trunk_head`, `merge_base`, `merge_bases` or
`fork_base` with the reason). Land then checks the same set: a merge or a
fast-forward that would change a path outside the decision stops with
`repropose_required` before anything is pushed, and the Worker flags it on
`landed`. Inside the set, the Worker also checks what a merge holds: each
decided file must be the approved head's version, unless the trunk edited
that file too since the intent's trunk base (both sides changed it, or a
conflict was resolved). A hand-made "merge" that puts other content in an
approved path is logged as `land.mismatch` and not recorded as landed.

## Validation: the file fails closed

`wayglass propose` refuses to run with a policy file it cannot validate, and
the Worker treats the same file on the trunk as absent: every change is
routed to a person by `builtin:invalid-policy`, with the parse error in the
decision's `policy_error`. A mistake never widens what lands. The parser
refuses:

- a `version` other than `1`, or one written as a string;
- any key it does not know, at any level (`whne:`, `areas:`, `path:` under
  `when`), and a key given twice;
- more than one YAML document in the file;
- an unknown or differently cased `action` or `mode`, an empty `when`, a
  missing or blank `reason`, `approvers` on anything but `multi_approve` or
  below 2, a `reviewer` on `auto_merge` or `block`;
- a rule naming an area, `only_area` or `data_class` that `classify` does not
  define, an unknown risk level, a bad regex or glob, a duplicate rule name.

Rule names and areas are compared exactly. The first enforcing rule that
matches wins, whatever the actions of later rules, so order strict rules
first. The example policy puts `block` for secrets before everything else.

## Trying a rule on history: `wayglass policy test`

```bash
wayglass policy test                                   # newest 20 commits on HEAD
wayglass policy test --since v0.1.0                    # every commit after a revision
wayglass policy test --file ../drafts/policy.yaml -n 50 --json
```

Replays commits (first parent only) through the local policy file, or the one
given with `--file`, using the same classifier and evaluator as `propose`. It
lists each commit's decision and shadow line, then a table of every rule:
how often it matched, how often it decided (for a shadow rule, how often it
would have decided), and how often enforcing it would have changed the
action. Commits with a why note also show the decision recorded when they
landed, if today's file decides differently. Nothing is recorded and the
Worker is not contacted, so it runs offline and costs nothing.

Workflow: add the rule with `mode: shadow`, run `wayglass policy test`, land
it (a `.wayglass/` change always goes to a person), watch the shadow lines on
real proposals, then remove `mode: shadow`.
