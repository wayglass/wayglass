# Vendored skills

The open-source skills this project uses are committed here. `.claude/skills` is a
relative symlink to this directory, so Claude Code and Codex load the same
copies. Global installs do not count.

Skills are installed with the `skills` CLI, which records each source and a
content hash in `skills-lock.json` at the repo root:

```bash
npx skills add <source> -s <skill> -a claude-code codex -y   # add one
npx skills update -p -y                                      # update all
npx skills experimental_install                              # restore from the lock
```

## Cloudflare (official)

Source: `cloudflare/skills`. Licence: Apache-2.0.

| Skill | Why it is here |
|---|---|
| `cloudflare` | Product map plus references for Queues, static assets, observability, KV, D1 and more |
| `durable-objects` | Intent registry and audit log live in Durable Objects |
| `workers-best-practices` | The Worker in `worker/` |
| `wrangler` | Local dev, bindings and deploy config |

Cloudflare has no official skill for Artifacts, so `cloudflare-artifacts` is
written separately for this repo.

## Cloudflare Artifacts

| Skill | Source | Licence |
|---|---|---|
| `cloudflare-artifacts` | `tpscloud/flared` | MIT |

Checked against the live Artifacts docs, Wrangler 4.149.0 and
`@cloudflare/workers-types` 5.20261012.1 on 2026-10-12.

## Go and terminal UI

| Skill | Source | Licence |
|---|---|---|
| `cobra` | `tpscloud/gophers` | MIT |
| `wails2` | `tpscloud/gophers` (the desktop app in `desktop/` is Wails v2) | MIT |
| `use-modern-go` | `coversheets/go-modern-guidelines-skill` (fork of `JetBrains/go-modern-guidelines`) | Apache-2.0 |
| `charmed-ecosystem`, `charmed-bubbletea`, `charmed-bubbles`, `charmed-lipgloss`, `charmed-glamour`, `charmed-huh`, `charmed-log`, `charmed-vhs` | `tpscloud/charmedimsure-skill` | MIT |

`use-modern-go/scripts/run-tool.sh` installs
`github.com/JetBrains/go-modern-guidelines@v0.1.1` with `go install` on first
use and caches it under `~/.cache/go-modern-guidelines`.

## Writing

| Skill | Source | Licence |
|---|---|---|
| `avoid-ai-writing` | `coversheets/avoid-ai-writing-skill` (fork of `conorbronsdon/avoid-ai-writing`) | MIT, Copyright 2026 Conor Bronsdon |
| `humanizer` | `blader/humanizer` | MIT, Copyright 2025 Siqi Chen |

## Licence files

The Apache-2.0 sources keep their `LICENSE` at the repo root, so the CLI does
not copy it. `cloudflare`, `durable-objects`, `workers-best-practices`,
`wrangler` and `use-modern-go` each carry a copy added by hand. Check they are
still there after `npx skills update`.

## Web UI design tools

The web UI was designed with a proprietary daisyUI dashboard skill and the
daisyUI Blueprint MCP service. Neither is included or needed to build or run it.
