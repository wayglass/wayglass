# Architecture

One Go binary serves the API and the storefront page. It keeps the catalog
in memory and has no database yet. Sessions are stateless: a token is the
email and an expiry, signed with HMAC-SHA256 using `ACME_SESSION_KEY`.

Billing computes invoices with VAT by the customer's country. Customer
records hold only what an invoice needs: name, email and country.

In production the binary runs as a Cloudflare Container (`infra/`).
