# Contributor instructions

Wayglass applies policy to changes proposed by coding agents. Read
[README.md](README.md) for the run instructions,
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for design decisions, and
[docs/POLICY.md](docs/POLICY.md) before changing classification or approval.

> We believe people should stay the authors of their software, even when agents write most of it. So every change carries its why, and every decision goes to whoever should make it: an agent when it's routine, a person when it matters.

## Build and check

Use Go 1.25.8 or later, git 2.40 or later, and Node.js 22.12 or later.
Install Worker dependencies with `cd worker && npm ci`. The desktop is a
separate Go module; its platform dependencies are in
[desktop/README.md](desktop/README.md).

```sh
go build -o bin/wayglass ./cmd/wayglass
go test ./...
go vet ./...
gofmt -l .
golangci-lint run ./...
(cd worker && npx tsc --noEmit && npx vitest run)
(cd desktop && go test ./...)
make wasm-check
```

`gofmt -l .` should print nothing. The Wasm check uses the Go toolchain pinned
in the Makefile; rebuild it when the policy evaluator changes. Run relevant
browser and script checks for changes to those surfaces. Never skip or disable
tests to make CI pass. Describe any live behavior that remains unverified.

## Conventions

- Go commands use `spf13/cobra`; terminal interfaces use Charm v2
  (`charm.land/*/v2`). Keep git subprocesses in `internal/gitx`.
- Every command supports `--json`. Preserve the versioned envelope:
  `{"v":1,"ok":true,"data":{...}}` or
  `{"v":1,"ok":false,"error":{"code":"...","message":"..."}}`.
  Exit codes are 0 for success, 1 for an error, 2 for usage, 3 for a policy
  block, and 4 when a person must act. JSON and non-TTY output must not prompt,
  spin, or use colour. A successful response can still describe pending review.
- Policy and classification need table-driven tests. The Worker uses the Go
  evaluator compiled to Wasm; client previews do not authorize a change.
- The TypeScript Worker handles authentication, tokens, intent state, policy
  decisions, events and audit records. Git merges run in the client.
  Use the pinned `npx wrangler` from `worker/`, never a global installation.
- The static application is in `web/`. The landing and documentation sites
  have separate repositories. Keep deployment-specific settings out of source.
- Why-notes use `refs/notes/wayglass`. Preserve intent, goal, reason, tags,
  matched rule, action, deciding identity and decision time in the JSON note.
- Keep the MIT licence and use permissively licensed dependencies. Discuss
  large dependencies, new runtimes, public API changes and policy semantics
  with maintainers before implementing them.

## Security boundaries

Trust is evaluated per change. Do not add a policy bypass, share a long-lived
write token across agents, or put a Cloudflare API token in the client.
The Worker derives authoritative classification from repository contents and
trunk policy. Decisions routed to people require a human credential; authors
must not approve their own changes. Bind a verdict to the decision reviewed.

Keep credentials out of source, fixtures, notes, URLs, command arguments and
logs. Pass git authorization through `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`
and `GIT_CONFIG_VALUE_n` environment variables. Use short-lived repository
write tokens. Do not deploy, create paid resources, change credentials or
rewrite shared history without explicit maintainer authorization.

## Proposing changes

Use a separate branch and describe the problem, intended paths and reason
before editing. Keep commits focused and use conventional commit subjects
such as `fix:` or `docs:`. Explain why the change is needed. In the pull
request, include the resulting behavior, validation and remaining limitations.
Use disposable `demo-test-*` projects for authorized live testing; preserve
shared demo data and pending decisions. The offline evaluation path in
[docs/EVALUATING.md](docs/EVALUATING.md) needs no Cloudflare account.
