package structural

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/store"
)

func TestPerfProbeCodebeamRepo(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ix := indexer.New(config.Config{IndexDir: t.TempDir(), RepoDir: t.TempDir()}, nil)
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	eng := &Engine{Matcher: m, Indexer: ix, Timeout: 60 * time.Second}
	repo := store.Repo{ID: 1, HostProvider: "local", Name: "codebeam", FullName: "local/codebeam", LocalPath: root}

	start := time.Now()
	res, err := eng.Search(context.Background(), Request{
		Pattern: "if $ERR != nil { $$$ }",
		Lang:    "go",
		Allowed: []store.Repo{repo},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cold: %d matches in %d files, %v", res.MatchCount, res.FileCount, time.Since(start))

	start = time.Now()
	res, err = eng.Search(context.Background(), Request{
		Pattern: "if $ERR != nil { $$$ }",
		Lang:    "go",
		Allowed: []store.Repo{repo},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("warm: %d matches in %d files, %v", res.MatchCount, res.FileCount, time.Since(start))
}
