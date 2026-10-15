package catalog

import "testing"

func TestGet(t *testing.T) {
	c := Default()
	if p, ok := c.Get("mug-001"); !ok || p.Name != "Enamel camp mug" {
		t.Fatalf("Get(mug-001) = %+v, %v", p, ok)
	}
	if _, ok := c.Get("nope"); ok {
		t.Fatal("Get(nope) found a product")
	}
}

func TestListIsSortedAndCopied(t *testing.T) {
	c := New([]Product{{ID: "b"}, {ID: "a"}})
	got := c.List()
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("List() = %v, want sorted by ID", got)
	}
	got[0].ID = "changed"
	if c.List()[0].ID != "a" {
		t.Fatal("List() returned the catalog's own slice")
	}
}

func TestFormatPrice(t *testing.T) {
	if got := FormatPrice(1800, "EUR"); got != "€18.00" {
		t.Fatalf("FormatPrice(1800, EUR) = %q", got)
	}
}
