# Task 01: document the API

**Expected route:** `docs-only` → `auto_merge` (exit 0, `approved`). You land it yourself.

The API reference in `docs/api.md` is a bare table. Support keeps
answering the same questions about it. Rewrite it into a useful reference:
for each route in `internal/server/server.go` give the method and path, what
it returns, one `curl` example with a short sample response, and the error
shape (`{"error":"..."}` with the status codes the handler really uses).
Read the handler code; document only what exists.

```bash
wayglass intent declare --json \
  --goal "docs: a usable API reference" \
  --reason "Support keeps answering the same API questions; docs/api.md is only a table" \
  --path "docs/api.md"
```

Change only `docs/api.md`. No tests to run.
