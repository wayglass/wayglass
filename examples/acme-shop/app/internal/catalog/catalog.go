// Package catalog holds the products the shop sells and formats their prices.
package catalog

import (
	"fmt"
	"slices"
	"strings"
)

// Product is one item for sale. Prices are in minor units (cents) so money
// never goes through floating point.
type Product struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"`
	PriceCents int64  `json:"price_cents"`
	Currency   string `json:"currency"`
	InStock    bool   `json:"in_stock"`
}

// Catalog is an in-memory, read-only product list.
type Catalog struct {
	products []Product
}

// New returns a catalog of the given products, sorted by ID.
func New(products []Product) *Catalog {
	p := slices.Clone(products)
	slices.SortFunc(p, func(a, b Product) int { return strings.Compare(a.ID, b.ID) })
	return &Catalog{products: p}
}

// Default is the demo catalog.
func Default() *Catalog {
	return New([]Product{
		{ID: "mug-001", Name: "Enamel camp mug", Category: "kitchen", PriceCents: 1800, Currency: "EUR", InStock: true},
		{ID: "pan-002", Name: "Cast iron skillet, 26 cm", Category: "kitchen", PriceCents: 4900, Currency: "EUR", InStock: true},
		{ID: "tnt-003", Name: "Two-person tent", Category: "outdoor", PriceCents: 18900, Currency: "EUR", InStock: false},
		{ID: "lmp-004", Name: "Rechargeable head lamp", Category: "outdoor", PriceCents: 3450, Currency: "EUR", InStock: true},
		{ID: "bag-005", Name: "Waxed canvas tote", Category: "bags", PriceCents: 6500, Currency: "EUR", InStock: true},
	})
}

// List returns every product, sorted by ID.
func (c *Catalog) List() []Product { return slices.Clone(c.products) }

// Get returns the product with the given ID.
func (c *Catalog) Get(id string) (Product, bool) {
	i := slices.IndexFunc(c.products, func(p Product) bool { return p.ID == id })
	if i < 0 {
		return Product{}, false
	}
	return c.products[i], true
}

var symbols = map[string]string{"EUR": "€", "USD": "$", "GBP": "£"}

// FormatPrice renders minor units as a price, e.g. 1800 EUR -> "€18.00".
// Unknown currencies use the ISO code: 1800 SEK -> "18.00 SEK".
func FormatPrice(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	amount := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if s, ok := symbols[currency]; ok {
		return sign + s + amount
	}
	return sign + amount + " " + currency
}
