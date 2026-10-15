# Getting started

You need Go 1.25 or later.

```bash
go test ./...
ACME_SESSION_KEY=$(openssl rand -hex 32) go run ./cmd/shop
```

Open http://localhost:8080 for the storefront. To sign in locally, also set
`ACME_DEMO_EMAIL` and `ACME_DEMO_PASSWORD` (at least 12 characters).
