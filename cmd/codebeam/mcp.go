package main

import (
	"context"
	"flag"
	"io"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	mcpserver "github.com/ctourriere/codebeam/internal/mcp"
	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/structural"
)

// runMCPCommand serves the Model Context Protocol over stdio so editor agents can
// use the local Codebeam index as a retrieval toolbox. Protocol traffic flows on
// stdin/stdout; logs go to stderr so they never corrupt the JSON-RPC stream.
func runMCPCommand(ctx context.Context, cfg config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return err
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close() // nolint:errcheck

	ix := indexer.New(cfg, st)
	srv := &mcpserver.Server{
		Store:      st,
		Search:     codesearch.Engine{IndexDir: cfg.IndexDir},
		Structural: structural.NewEngine(cfg, ix),
		Indexer:    ix,
	}
	return srv.Serve(ctx, stdin, stdout)
}
