// Command cb is the Codebeam CLI: code search from the terminal for humans
// and agents, backed by a Codebeam server's /mcp endpoint.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ctourriere/codebeam/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
