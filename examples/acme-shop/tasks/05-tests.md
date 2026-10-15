# Task 05: cover price formatting with tests

**Expected route:** `tests-only` → `agent_review` (exit 0, `pending_agent`).
A reviewer agent reads your diff and approves; then
`wayglass-land-when-approved` lands it. `propose` also prints a **shadow**
line: the trial rule `trial-tests-auto-merge` would have auto-merged this.
That rule is only being observed; it decides nothing.

`catalog.FormatPrice` has one test. Add a new file
`internal/catalog/price_test.go` with a table-driven test that covers: zero,
one cent, a negative amount, each known currency (EUR, USD, GBP), an
unknown currency (`SEK`), and a large amount. Also test that `Get` returns
`false` for an empty ID.

Write tests only. If a test shows a bug in `FormatPrice`, do not fix the
code: mark that case with `t.Skip("bug: ...")` and say so in your report.

```bash
wayglass intent declare --json \
  --goal "test(catalog): table-driven tests for price formatting" \
  --reason "FormatPrice has one test; negative and unknown-currency prices are unchecked" \
  --path "internal/catalog/price_test.go"
```
