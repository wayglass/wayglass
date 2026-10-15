# How you work here (read first)

You are a coding agent on **Acme Shop**, a small Go web shop. Other agents
are working on the same repo at the same time. Every change goes through
**Wayglass**, which forks the repo for your change, classifies what you
changed, and lets policy decide whether it lands, goes to a reviewer agent,
or waits for a person. You do not decide that, and you never work around it.

Your checkout is the current directory. `wayglass` is on your PATH and
`WAYGLASS_AGENT` is already set to your name; do not change it. Add `--json`
to every `wayglass` command and read the JSON: `{"v":1,"ok":true,"data":{...}}`
or `{"v":1,"ok":false,"error":{"code":"...","message":"..."}}`.

## The steps

1. **Declare** your intent with the exact `--goal`, `--reason` and `--path`
   values from your task. Keep `.data.intent.id`; you need it in the commit.
   If `.data.overlaps` is not empty, note each overlap and its
   `replan.summary`: another agent plans to touch the same files. Carry on
   unless your task says otherwise. If `.data.warnings` asks you to merge
   something, do it before you edit.
2. **Change** only files inside your declared paths. Keep it small and
   focused: this is a demo, so under about 150 changed lines.
3. **Test**: `go vet ./... && go test ./...` must pass (skip for
   docs-only or config-only changes).
4. **Commit** once. Subject in conventional-commit form; body says *why*;
   last line `Wayglass-Intent: <your intent id>`:

   ```
   feat(catalog): search products by name

   Shoppers scroll the whole list to find one item; search narrows it.

   Wayglass-Intent: i0123456789
   ```

5. **Push**: `wayglass push --json`. Note any `overlaps` and `warnings`.
6. **Propose**: `wayglass propose --json`, then act on the exit code:

   | Exit | `.data.decision.status` | What it means | What you do |
   |---|---|---|---|
   | 0 | `approved` | Policy lets it land. | `wayglass land --json` |
   | 0 | `pending_agent` | A reviewer agent must approve first. | `wayglass-land-when-approved` |
   | 4 | `pending_human` | A person (or two) must approve. | `wayglass-land-when-approved` |
   | 4 | `quarantined` | Held for a data protection review. | Stop. Report. |
   | 3 | `blocked` | Policy refuses this change. | Stop. Report. Do not retry. |

   `wayglass-land-when-approved` waits about 2 minutes for the approval and
   lands the change as soon as it comes. Exit 75 means still waiting: run it
   again, for up to 20 minutes in all. Exit 3 means rejected and exit 4
   quarantined: stop and report what it prints.

## Never

- Approve, reject or review your own change, or edit `.wayglass/`.
- Use `git push`, print a token, or pass `--show-token`. Wayglass handles
  the remote.
- Split, rename or reword a change to get a different decision.

## Your final report

End with a short report: intent id, the tags and the matched rule from
`propose` (`.data.preview.tags`, `.data.decision.matched_rule`), the
decision status, any `shadow` line, any overlap and its suggested re-plan,
and whether the change landed (`.data.commit` from `land`).
