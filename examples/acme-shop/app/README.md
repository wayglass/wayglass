# Acme Shop

A small storefront: a product catalog, sign-in, and EU-aware invoicing,
served by one Go binary. It is the sample app the Wayglass demo agents work
on, so it is deliberately small, but every part of it is real code with
tests.

```bash
go test ./...
ACME_SESSION_KEY=$(openssl rand -hex 32) go run ./cmd/shop   # http://localhost:8080
```

| Path | What lives there |
|---|---|
| `cmd/shop` | The server binary. |
| `internal/catalog` | Products, prices, price formatting. |
| `internal/auth` | Password sign-in and signed session tokens. |
| `internal/billing` | Customers, invoices, VAT by country. |
| `internal/server` | HTTP routes. |
| `web/` | The storefront page. |
| `infra/` | Deployment: container image and Cloudflare config. |
| `docs/` | Guides for developers and support. |

Changes go through Wayglass: see `.wayglass/policy.yaml` for who decides what.
