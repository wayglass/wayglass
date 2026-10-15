# Roadmap

What is planned and not built. Everything here is a plan, not a promise: a
plan moves into the README's [Status](../README.md#status) table only once
it runs against the deployed Worker. The "Not built" list in the README is
the short form of this file.

## Planned

- **GitHub App sync.** Install the Wayglass GitHub App on an account or org
  and choose repositories in GitHub's own install screen; each one is
  mirrored as a project trunk and kept current from push webhooks, with
  short-lived installation tokens. Public repositories first; we are looking
  at the privacy implications of syncing private repositories before
  offering that on the hosted Worker. Today a repository is brought under
  Wayglass one at a time with `wayglass init --from <url>` and pushed back
  with `wayglass publish`.
- **Short-lived session keys and project-scoped keys.** Per-agent keys are
  bound to a name; the admin key's caller still names itself. A key that
  expires with the session and one that reaches a single project are next.
- **Branch protection on the trunk.** Artifacts has none, so the Worker
  detects a trunk push that skipped `land` and flags it in the audit log,
  but cannot prevent it. If Artifacts gains protected refs, `land` is the
  only writer.
- **Automatic re-planning.** Overlaps come with a suggestion (`land_first`,
  `stack` or `split`); an agent or person acts on it. Applying the
  suggestion on request (`wayglass overlaps --apply`) is the next step;
  nothing re-plans without being asked.
- **Linux arm64 desktop build.** One more matrix entry in
  `scripts/desktop-release.sh linux`, once a Linux arm64 runner is available.

## Not planned

- A long-lived write token for any agent, a "skip policy" flag, or a
  Cloudflare API token inside the CLI. The Zero Trust rules in
  [AGENTS.md](../AGENTS.md) section 7 are not on any roadmap.
