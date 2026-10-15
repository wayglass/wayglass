# Task 03: VAT numbers on EU invoices (EU repo)

**Run this against the EU project** (`eu-demo-acme-*`, jurisdiction `eu`).

**Expected route:** `eu-personal-data` → `quarantine` (exit 4, `quarantined`).
A VAT number of a sole trader is personal data, and this repo lives in the
EU namespace, so the change is held for a data protection review. Stop after
`propose` and report; do not wait for it.

EU business customers need reverse-charge invoices: when a customer has a
VAT number and is in an EU country other than Germany (where Acme is
registered), the invoice charges 0% VAT and says so.

- Add an optional `VATNumber string` field (`json:"vat_number,omitempty"`)
  to `Customer` in `internal/billing/customer.go`. Validate its shape:
  two-letter country prefix matching `Country`, then 2 to 12 letters or
  digits.
- In `internal/billing/invoice.go`, add `ReverseCharge bool` to `Invoice`
  and apply the rule above.
- Tests in `internal/billing/billing_test.go`: a French business customer
  with a VAT number pays 0% VAT and gets `ReverseCharge`; a German one still
  pays 19%; a malformed VAT number is refused.

```bash
wayglass intent declare --json \
  --goal "feat(billing): reverse-charge VAT for EU business customers" \
  --reason "EU B2B customers are charged VAT they should not pay; reverse charge needs their VAT number" \
  --path "internal/billing/**"
```
