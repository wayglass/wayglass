package billing

import (
	"fmt"
	"strings"
)

// vatRates are standard VAT rates in basis points (1900 = 19%) for the
// countries the shop ships to. Countries not listed are charged no VAT.
var vatRates = map[string]int64{
	"DE": 1900,
	"FR": 2000,
	"NL": 2100,
	"IE": 2300,
	"AT": 2000,
}

// Line is one item on an invoice.
type Line struct {
	ProductID  string `json:"product_id"`
	Quantity   int64  `json:"quantity"`
	PriceCents int64  `json:"price_cents"` // unit price, excluding VAT
}

// Invoice is what the customer pays.
type Invoice struct {
	Number     string   `json:"number"`
	Customer   Customer `json:"customer"`
	Currency   string   `json:"currency"`
	Lines      []Line   `json:"lines"`
	NetCents   int64    `json:"net_cents"`
	VATCents   int64    `json:"vat_cents"`
	TotalCents int64    `json:"total_cents"`
}

// VATRate returns the VAT rate in basis points for a country.
func VATRate(country string) int64 { return vatRates[strings.ToUpper(country)] }

// NewInvoice totals the lines and applies the customer's VAT rate, rounding
// VAT half up to the cent.
func NewInvoice(number string, c Customer, currency string, lines []Line) (Invoice, error) {
	if err := c.Validate(); err != nil {
		return Invoice{}, err
	}
	inv := Invoice{Number: number, Customer: c, Currency: currency, Lines: lines}
	for i, l := range lines {
		if l.Quantity <= 0 || l.PriceCents < 0 {
			return Invoice{}, fmt.Errorf("line %d: quantity must be positive and price not negative", i+1)
		}
		inv.NetCents += l.Quantity * l.PriceCents
	}
	inv.VATCents = (inv.NetCents*VATRate(c.Country) + 5000) / 10000
	inv.TotalCents = inv.NetCents + inv.VATCents
	return inv, nil
}
