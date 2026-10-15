# Task 08: filter by category, stacked on search

**Expected route:** `routine-app` → `agent_review` (exit 0, `pending_agent`),
**stacked** on task 07: you build on the search agent's unlanded change, and
yours can land only after it does. `wayglass-land-when-approved` waits for
both.

Your change edits the same handler as task 07's search. Instead of racing
it and merging later, **stack** on it:

1. Find the search intent:
   `wayglass intent list --json | jq -r '.data.intents[] | select(.goal | startswith("catalog: search")) | "\(.id) \(.head_commit)"'`.
   If there is none yet, or its `head_commit` is `null` (nothing pushed),
   wait 30 seconds and look again, for up to 10 minutes.
2. Declare with `--on <that id>`. Wayglass forks from its head and
   fast-forwards your checkout to it (`.data.fast_forwarded`), so you start
   from the code with search in it.

```bash
wayglass intent declare --json --on <search-intent-id> \
  --goal "catalog: filter products by category" \
  --reason "Shoppers browse one department at a time; the list mixes kitchen, outdoor and bags" \
  --path "internal/catalog/catalog.go" --path "internal/server/server.go" \
  --path "internal/catalog/category_test.go" --path "web/**"
```

Then:

- `internal/catalog/catalog.go`: `func (c *Catalog) Categories() []string`
  (sorted, unique) and `func (c *Catalog) InCategory(cat string) []Product`.
- `internal/server/server.go`: `GET /api/products?category=kitchen`, which
  combines with `q` when both are given; `GET /api/categories`.
- `web/app.js` and `web/index.html`: a row of category buttons above the
  grid that reloads the list with `?category=`.
- Tests in a new `internal/catalog/category_test.go`.
