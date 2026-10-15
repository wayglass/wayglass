package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"acme.example/shop/internal/auth"
	"acme.example/shop/internal/catalog"
)

func newServer(t *testing.T) http.Handler {
	t.Helper()
	users := auth.NewStore([]byte("test-key-test-key-test-key-test-key"))
	if err := users.Add("ada@example.com", "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return New(catalog.Default(), users, t.TempDir())
}

func TestRoutes(t *testing.T) {
	h := newServer(t)
	tests := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/healthz", "", 200},
		{"GET", "/api/products", "", 200},
		{"GET", "/api/products/mug-001", "", 200},
		{"GET", "/api/products/nope", "", 404},
		{"POST", "/api/login", `{"email":"ada@example.com","password":"correct horse battery"}`, 200},
		{"POST", "/api/login", `{"email":"ada@example.com","password":"wrong password!"}`, 401},
		{"POST", "/api/login", `not json`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestProductsJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	newServer(t).ServeHTTP(rec, httptest.NewRequest("GET", "/api/products", nil))
	var out struct {
		Products []catalog.Product `json:"products"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil || len(out.Products) != 5 {
		t.Fatalf("products = %v, err %v", out.Products, err)
	}
}
