# Task 07: search the catalog

**Expected route:** `routine-app` → `agent_review` (exit 0, `pending_agent`).
A reviewer agent approves; then `wayglass-land-when-approved` lands it.

**Overlap on purpose:** you declare all of `internal/catalog/**`, which
includes the file the tests agent (task 05) is writing. `declare` or `push`
reports the overlap with a suggested re-plan. Note it in your report and
carry on; keep your own tests in `search_test.go` so the two changes do not
collide. Task 08 will stack on your change, so push promptly.

- In `internal/catalog/catalog.go`, add
  `func (c *Catalog) Search(q string) []Product`: case-insensitive match of
  every whitespace-separated word of `q` against name or category; an empty
  query returns everything; results keep ID order.
- In `internal/server/server.go`, make `GET /api/products?q=...` use it.
- Tests: `internal/catalog/search_test.go` (table-driven) and one case in
  `internal/server/server_test.go`.

```bash
wayglass intent declare --json \
  --goal "catalog: search products by name and category" \
  --reason "Shoppers scroll the whole list to find one product; search narrows it" \
  --path "internal/catalog/**" --path "internal/server/**"
```
