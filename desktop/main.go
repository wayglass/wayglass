// Command wayglass-desktop is the Wayglass desktop window: the swarm, the
// changes routed to a person, and the audit trail, with Approve and Reject.
// It reads the login 'wayglass login' stored and holds no secret of its own.
package main

import (
	"embed"
	"log"
	"os"

	"github.com/wailsapp/wails/v2"

	"github.com/wayglass/wayglass/desktop/app"
	"github.com/wayglass/wayglass/internal/api"
)

//go:embed all:frontend/dist
var assets embed.FS

// version is the release tag, set at link time with
// -ldflags "-X main.version=v0.2.0" (scripts/desktop-release.sh). A build
// without one is "dev" and never checks for updates.
var version = "dev"

func main() {
	creds, err := api.LoadCredentials()
	prefs, perr := app.PrefsPath()
	if perr != nil {
		log.Printf("wayglass desktop: project list kept in memory: %v", perr)
	}
	cwd, _ := os.Getwd()
	a := app.New(app.Options{Creds: creds, CredsErr: err, PrefsPath: prefs, Cwd: cwd, Version: version})
	if err := wails.Run(app.NewOptions(a, assets)); err != nil {
		log.Fatal(err)
	}
}
