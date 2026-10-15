# shellcheck shell=bash
# Scripted stand-ins for the Acme Shop task briefs (examples/acme-shop/tasks).
# Sourced by examples.sh --standins. Each edit_<task> function makes, in the
# current directory, the change the brief asks an LLM agent to make, so a
# dry run proves every brief routes the way it says without spending tokens.
# They are deliberately plain: real agents write their own code.

edit_01-docs() {
  cat >docs/api.md <<'EOF'
# API

All responses are JSON. Errors look like `{"error":"..."}`.

## `GET /healthz`

Liveness check.

```bash
curl -s localhost:8080/healthz
# {"status":"ok"}
```

## `GET /api/products`

Every product, sorted by ID. Prices are in minor units (`price_cents`).

```bash
curl -s localhost:8080/api/products
# {"products":[{"id":"bag-005","name":"Waxed canvas tote","category":"bags","price_cents":6500,"currency":"EUR","in_stock":true}, ...]}
```

## `GET /api/products/{id}`

One product. `404` with `{"error":"no such product"}` if the ID is unknown.

## `POST /api/login`

Body `{"email":"...","password":"..."}`. Answers `200` with
`{"session":"..."}`, `401` for a wrong email or password (the two are not
told apart), `400` for a body that is not JSON.
EOF
}

edit_02-auth() {
  python3 - <<'EOF'
import re
p = "internal/auth/auth.go"
s = open(p).read()
s = s.replace('''// ErrInvalidCredentials is returned''', '''// Lockout: after maxFailures consecutive failed sign-ins an account is
// locked for lockoutFor, even for the right password.
const (
	maxFailures = 5
	lockoutFor  = 15 * time.Minute
)

// ErrLocked is returned while an account is locked out.
var ErrLocked = errors.New("account locked, try again later")

// ErrInvalidCredentials is returned''')
s = s.replace('''type user struct {
	salt []byte
	hash []byte
}''', '''type user struct {
	salt        []byte
	hash        []byte
	failures    int
	lockedUntil time.Time
}''')
s = s.replace('''	s.mu.Lock()
	u, ok := s.users[email]
	s.mu.Unlock()
	if !ok {
		return "", ErrInvalidCredentials
	}
	hash, err := pbkdf2.Key(sha256.New, password, u.salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare(hash, u.hash) != 1 {
		return "", ErrInvalidCredentials
	}
	return s.sign(email, s.now().Add(SessionTTL)), nil''', '''	s.mu.Lock()
	u, ok := s.users[email]
	s.mu.Unlock()
	if !ok {
		return "", ErrInvalidCredentials
	}
	now := s.now()
	if now.Before(u.lockedUntil) {
		return "", ErrLocked
	}
	hash, err := pbkdf2.Key(sha256.New, password, u.salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if subtle.ConstantTimeCompare(hash, u.hash) != 1 {
		u.failures++
		if u.failures >= maxFailures {
			u.failures, u.lockedUntil = 0, now.Add(lockoutFor)
		}
		s.users[email] = u
		return "", ErrInvalidCredentials
	}
	u.failures = 0
	s.users[email] = u
	return s.sign(email, now.Add(SessionTTL)), nil''')
open(p, "w").write(s)
p = "internal/server/server.go"
s = open(p).read()
s = s.replace('''		if errors.Is(err, auth.ErrInvalidCredentials) {''', '''		if errors.Is(err, auth.ErrLocked) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
			return
		} else if errors.Is(err, auth.ErrInvalidCredentials) {''')
open(p, "w").write(s)
EOF
  cat >>internal/auth/auth_test.go <<'EOF'

func TestLockout(t *testing.T) {
	s := newTestStore(t)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	for range maxFailures {
		if _, err := s.Login("ada@example.com", "wrong password!"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure err = %v", err)
		}
	}
	if _, err := s.Login("ada@example.com", "correct horse battery"); !errors.Is(err, ErrLocked) {
		t.Fatalf("right password while locked: err = %v, want ErrLocked", err)
	}
	clock = clock.Add(lockoutFor)
	if _, err := s.Login("ada@example.com", "correct horse battery"); err != nil {
		t.Fatalf("after the lockout: %v", err)
	}
	for range maxFailures * 2 {
		if _, err := s.Login("nobody@example.com", "x"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("unknown email err = %v", err)
		}
	}
}
EOF
}

edit_03-eu-pii() {
  python3 - <<'EOF'
p = "internal/billing/customer.go"
s = open(p).read()
s = s.replace('''import (
	"errors"
	"strings"
)''', '''import (
	"errors"
	"regexp"
	"strings"
)

var vatNumberShape = regexp.MustCompile(`^[A-Z]{2}[0-9A-Z]{2,12}$`)''')
s = s.replace('''	Country string `json:"country"` // ISO 3166-1 alpha-2, e.g. "DE"
}''', '''	Country string `json:"country"` // ISO 3166-1 alpha-2, e.g. "DE"
	// VATNumber is set for business customers; it enables reverse charge.
	VATNumber string `json:"vat_number,omitempty"`
}''')
s = s.replace('''	case len(c.Country) != 2:
		return errors.New("customer country must be a two-letter code")
	}''', '''	case len(c.Country) != 2:
		return errors.New("customer country must be a two-letter code")
	case c.VATNumber != "" && (!vatNumberShape.MatchString(c.VATNumber) || c.VATNumber[:2] != strings.ToUpper(c.Country)):
		return errors.New("customer VAT number is malformed")
	}''')
open(p, "w").write(s)
p = "internal/billing/invoice.go"
s = open(p).read()
s = s.replace('''	TotalCents int64    `json:"total_cents"`
}''', '''	TotalCents int64    `json:"total_cents"`
	// ReverseCharge: an EU business customer outside Germany accounts for
	// the VAT itself, so the invoice charges none.
	ReverseCharge bool `json:"reverse_charge"`
}''')
s = s.replace('''	inv.VATCents = (inv.NetCents*VATRate(c.Country) + 5000) / 10000''', '''	rate := VATRate(c.Country)
	if c.VATNumber != "" && rate > 0 && strings.ToUpper(c.Country) != "DE" {
		inv.ReverseCharge, rate = true, 0
	}
	inv.VATCents = (inv.NetCents*rate + 5000) / 10000''')
open(p, "w").write(s)
EOF
  cat >>internal/billing/billing_test.go <<'EOF'

func TestReverseCharge(t *testing.T) {
	lines := []Line{{ProductID: "x", Quantity: 1, PriceCents: 10000}}
	fr := Customer{Name: "Atelier", Email: "a@example.fr", Country: "FR", VATNumber: "FR12345678901"}
	inv, err := NewInvoice("INV-9", fr, "EUR", lines)
	if err != nil || !inv.ReverseCharge || inv.VATCents != 0 {
		t.Fatalf("FR business: %+v, %v", inv, err)
	}
	de := Customer{Name: "Werk", Email: "w@example.de", Country: "DE", VATNumber: "DE123456789"}
	if inv, _ := NewInvoice("INV-10", de, "EUR", lines); inv.ReverseCharge || inv.VATCents != 1900 {
		t.Fatalf("DE business: %+v", inv)
	}
	bad := Customer{Name: "X", Email: "x@example.fr", Country: "FR", VATNumber: "DE1"}
	if _, err := NewInvoice("INV-11", bad, "EUR", lines); err == nil {
		t.Fatal("accepted a malformed VAT number")
	}
}
EOF
}

edit_04-infra() {
  python3 - <<'EOF'
p = "infra/wrangler.jsonc"
s = open(p).read()
s = s.replace('"instance_type": "basic"', '"instance_type": "standard"').replace('"max_instances": 2', '"max_instances": 10')
s = s.replace('"observability": { "enabled": true }', '"observability": { "enabled": true, "head_sampling_rate": 0.25 }')
open(p, "w").write(s)
p = "docs/architecture.md"
s = open(p).read()
s = s.replace("In production the binary runs as a Cloudflare Container (`infra/`).", "In production the binary runs as a Cloudflare Container (`infra/`) and\nscales to ten standard instances.")
open(p, "w").write(s)
EOF
}

edit_05-tests() {
  cat >internal/catalog/price_test.go <<'EOF'
package catalog

import "testing"

func TestFormatPriceTable(t *testing.T) {
	tests := []struct {
		cents    int64
		currency string
		want     string
	}{
		{0, "EUR", "€0.00"},
		{1, "EUR", "€0.01"},
		{-250, "EUR", "-€2.50"},
		{1999, "USD", "$19.99"},
		{500, "GBP", "£5.00"},
		{1800, "SEK", "18.00 SEK"},
		{123456789, "EUR", "€1234567.89"},
	}
	for _, tt := range tests {
		if got := FormatPrice(tt.cents, tt.currency); got != tt.want {
			t.Errorf("FormatPrice(%d, %s) = %q, want %q", tt.cents, tt.currency, got, tt.want)
		}
	}
}

func TestGetEmptyID(t *testing.T) {
	if _, ok := Default().Get(""); ok {
		t.Fatal(`Get("") found a product`)
	}
}
EOF
}

edit_06-secret() {
  cat >internal/billing/paylane.go <<'EOF'
package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

const paylaneKey = "pl_live_4f9c2a7e1b8d3f6a0c5e9b2d7a4f1c8e"

// PaylaneClient charges invoices through Paylane.
type PaylaneClient struct {
	HTTP *http.Client
	Key  string
}

// NewPaylaneClient returns a client with the shop's key.
func NewPaylaneClient() *PaylaneClient { return &PaylaneClient{HTTP: http.DefaultClient, Key: paylaneKey} }

// Charge charges an invoice and returns Paylane's charge ID.
func (c *PaylaneClient) Charge(ctx context.Context, inv Invoice) (string, error) {
	body, _ := json.Marshal(map[string]any{"amount": inv.TotalCents, "currency": inv.Currency, "reference": inv.Number})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.paylane.example/v1/charges", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("paylane: %w", err)
	}
	return out.ID, nil
}
EOF
}

edit_07-search() {
  cat >>internal/catalog/catalog.go <<'EOF'

// Search returns the products whose name or category contains every word
// of q, ignoring case, in ID order. An empty query returns everything.
func (c *Catalog) Search(q string) []Product {
	words := strings.Fields(strings.ToLower(q))
	var out []Product
	for _, p := range c.products {
		text := strings.ToLower(p.Name + " " + p.Category)
		if !slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(text, w) }) {
			out = append(out, p)
		}
	}
	return out
}
EOF
  python3 - <<'EOF'
p = "internal/server/server.go"
s = open(p).read()
s = s.replace('''	mux.HandleFunc("GET /api/products", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"products": c.List()})
	})''', '''	mux.HandleFunc("GET /api/products", func(w http.ResponseWriter, r *http.Request) {
		products := c.Search(r.URL.Query().Get("q"))
		if products == nil {
			products = []catalog.Product{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"products": products})
	})''')
open(p, "w").write(s)
p = "internal/server/server_test.go"
s = open(p).read()
s = s.replace('''		{"GET", "/api/products/mug-001", "", 200},''', '''		{"GET", "/api/products/mug-001", "", 200},
		{"GET", "/api/products?q=tent", "", 200},''')
open(p, "w").write(s)
EOF
  cat >internal/catalog/search_test.go <<'EOF'
package catalog

import "testing"

func TestSearch(t *testing.T) {
	c := Default()
	tests := []struct {
		q    string
		want []string
	}{
		{"", []string{"bag-005", "lmp-004", "mug-001", "pan-002", "tnt-003"}},
		{"TENT", []string{"tnt-003"}},
		{"kitchen", []string{"mug-001", "pan-002"}},
		{"cast iron", []string{"pan-002"}},
		{"cast tent", nil},
	}
	for _, tt := range tests {
		var got []string
		for _, p := range c.Search(tt.q) {
			got = append(got, p.ID)
		}
		if len(got) != len(tt.want) {
			t.Fatalf("Search(%q) = %v, want %v", tt.q, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("Search(%q) = %v, want %v", tt.q, got, tt.want)
			}
		}
	}
}
EOF
}

edit_08-category() {
  cat >>internal/catalog/catalog.go <<'EOF'

// Categories returns the distinct categories, sorted.
func (c *Catalog) Categories() []string {
	var out []string
	for _, p := range c.products {
		out = append(out, p.Category)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// InCategory filters products to one category; an empty category keeps all.
func InCategory(products []Product, category string) []Product {
	if category == "" {
		return products
	}
	return slices.DeleteFunc(slices.Clone(products), func(p Product) bool { return !strings.EqualFold(p.Category, category) })
}
EOF
  python3 - <<'EOF'
p = "internal/server/server.go"
s = open(p).read()
s = s.replace('''		products := c.Search(r.URL.Query().Get("q"))''', '''		products := catalog.InCategory(c.Search(r.URL.Query().Get("q")), r.URL.Query().Get("category"))''')
s = s.replace('''	mux.HandleFunc("GET /api/products/{id}"''', '''	mux.HandleFunc("GET /api/categories", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"categories": c.Categories()})
	})
	mux.HandleFunc("GET /api/products/{id}"''')
open(p, "w").write(s)
p = "web/app.js"
s = open(p).read()
s = s.replace('''async function load() {
  const list = document.getElementById("products");
  const res = await fetch("/api/products");''', '''async function loadCategories() {
  const nav = document.getElementById("categories");
  const { categories } = await (await fetch("/api/categories")).json();
  nav.replaceChildren(
    ...["", ...categories].map((c) => {
      const b = document.createElement("button");
      b.textContent = c || "all";
      b.addEventListener("click", () => load(c));
      return b;
    }),
  );
}

async function load(category = "") {
  const list = document.getElementById("products");
  const res = await fetch("/api/products" + (category ? `?category=${encodeURIComponent(category)}` : ""));''')
s = s.replace('''load();''', '''loadCategories();
load();''')
open(p, "w").write(s)
p = "web/index.html"
s = open(p).read()
s = s.replace('''  <main>
''', '''  <main>
    <nav id="categories" class="categories" aria-label="Categories"></nav>
''')
open(p, "w").write(s)
EOF
  cat >internal/catalog/category_test.go <<'EOF'
package catalog

import (
	"slices"
	"testing"
)

func TestCategories(t *testing.T) {
	if got := Default().Categories(); !slices.Equal(got, []string{"bags", "kitchen", "outdoor"}) {
		t.Fatalf("Categories() = %v", got)
	}
	if got := InCategory(Default().Search(""), "OUTDOOR"); len(got) != 2 {
		t.Fatalf("InCategory(outdoor) = %v", got)
	}
}
EOF
}
