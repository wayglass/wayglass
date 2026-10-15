package billing

import "testing"

func TestNewInvoice(t *testing.T) {
	c := Customer{ID: "c1", Name: "Ada", Email: "ada@example.com", Country: "DE"}
	tests := []struct {
		name    string
		country string
		lines   []Line
		net     int64
		vat     int64
	}{
		{"german VAT", "DE", []Line{{ProductID: "mug-001", Quantity: 2, PriceCents: 1800}}, 3600, 684},
		{"rounds half up", "IE", []Line{{ProductID: "x", Quantity: 1, PriceCents: 150}}, 150, 35},
		{"no VAT outside the list", "US", []Line{{ProductID: "x", Quantity: 3, PriceCents: 1000}}, 3000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.Country = tt.country
			inv, err := NewInvoice("INV-1", c, "EUR", tt.lines)
			if err != nil {
				t.Fatal(err)
			}
			if inv.NetCents != tt.net || inv.VATCents != tt.vat || inv.TotalCents != tt.net+tt.vat {
				t.Fatalf("net %d vat %d total %d, want net %d vat %d", inv.NetCents, inv.VATCents, inv.TotalCents, tt.net, tt.vat)
			}
		})
	}
}

func TestNewInvoiceRejectsBadInput(t *testing.T) {
	good := Customer{Name: "Ada", Email: "ada@example.com", Country: "FR"}
	if _, err := NewInvoice("INV-2", Customer{Name: "Ada", Email: "nope", Country: "FR"}, "EUR", nil); err == nil {
		t.Error("accepted an invalid email")
	}
	if _, err := NewInvoice("INV-3", good, "EUR", []Line{{Quantity: 0, PriceCents: 100}}); err == nil {
		t.Error("accepted a zero quantity")
	}
}
