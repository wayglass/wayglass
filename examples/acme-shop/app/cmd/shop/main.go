// Command shop serves the Acme storefront and its JSON API.
package main

import (
	"cmp"
	"log"
	"net/http"
	"os"
	"time"

	"acme.example/shop/internal/auth"
	"acme.example/shop/internal/catalog"
	"acme.example/shop/internal/server"
)

func main() {
	key := os.Getenv("ACME_SESSION_KEY")
	if len(key) < 32 {
		log.Fatal("ACME_SESSION_KEY must be at least 32 characters")
	}
	users := auth.NewStore([]byte(key))
	if email, pw := os.Getenv("ACME_DEMO_EMAIL"), os.Getenv("ACME_DEMO_PASSWORD"); email != "" && pw != "" {
		if err := users.Add(email, pw); err != nil {
			log.Fatal(err)
		}
	}
	srv := &http.Server{
		Addr:              ":" + cmp.Or(os.Getenv("PORT"), "8080"),
		Handler:           server.New(catalog.Default(), users, "web"),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("acme shop on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
