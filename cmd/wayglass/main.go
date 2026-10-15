// Command wayglass is the CLI, TUI and agent interface for Wayglass.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/wayglass/wayglass/internal/cli"
)

// Set at link time: -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.Execute(ctx, cli.BuildInfo{Version: version, Commit: commit}, os.Args[1:], os.Stdout, os.Stderr)
}
