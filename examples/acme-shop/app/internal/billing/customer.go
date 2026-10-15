// Package billing turns orders into invoices with the right VAT.
package billing

import (
	"errors"
	"strings"
)

// Customer is who an invoice is addressed to. Keep personal data here to
// the minimum invoicing needs.
type Customer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Country string `json:"country"` // ISO 3166-1 alpha-2, e.g. "DE"
}

// Validate checks the fields an invoice cannot do without.
func (c Customer) Validate() error {
	switch {
	case strings.TrimSpace(c.Name) == "":
		return errors.New("customer name is required")
	case !strings.Contains(c.Email, "@"):
		return errors.New("customer email is invalid")
	case len(c.Country) != 2:
		return errors.New("customer country must be a two-letter code")
	}
	return nil
}
