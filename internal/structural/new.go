package structural

import (
	"path/filepath"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
)

// NewEngine builds the structural search engine from config, sharing the
// same construction shape across the web server, MCP server, and CLI. The
// wazero compilation cache lives under the data dir so the embedded module
// is compiled once per machine, not once per boot.
func NewEngine(cfg config.Config, ix *indexer.Indexer) *Engine {
	cacheDir := ""
	if cfg.DataDir != "" {
		cacheDir = filepath.Join(cfg.DataDir, "wazero-cache")
	}
	return &Engine{
		Matcher: &Matcher{
			CacheDir: cacheDir,
		},
		Indexer:    ix,
		MaxFiles:   cfg.StructuralMaxFiles,
		MaxMatches: cfg.StructuralMaxMatches,
		Timeout:    cfg.StructuralTimeout,
	}
}
