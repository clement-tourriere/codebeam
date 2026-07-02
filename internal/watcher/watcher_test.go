package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/secretbox"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/fsnotify/fsnotify"
)

func TestShouldIgnorePathOnlyChecksChangedEntryName(t *testing.T) {
	if !shouldIgnorePath("/tmp/repo/node_modules") {
		t.Fatal("expected skipped directory name to be ignored")
	}
	if shouldIgnorePath("/tmp/build/repo/main.go") {
		t.Fatal("parent directories outside the watched repo should not suppress normal files")
	}
}

func TestIsIndexRelevant(t *testing.T) {
	if !isIndexRelevant(fsnotify.Event{Name: "file.go", Op: fsnotify.Write}) {
		t.Fatal("write events should trigger reindex")
	}
	if isIndexRelevant(fsnotify.Event{Name: "file.go", Op: fsnotify.Chmod}) {
		t.Fatal("chmod-only events should not trigger reindex")
	}
}

func TestLocalRepoWatcherQueuesInitialRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    repoDir,
		Name:          "repo",
		FullName:      "local/repo",
		CloneURL:      repoDir,
		DefaultBranch: "HEAD",
		LocalPath:     repoDir,
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	ix := indexer.New(config.Config{
		IndexDir: filepath.Join(root, "index"),
		RepoDir:  filepath.Join(root, "repos"),
	}, st)
	w := NewLocalRepoWatcher(st, ix)
	w.debounce = 20 * time.Millisecond
	w.refresh = time.Hour
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fresh, err := st.GetRepo(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.IndexedAt > 0 && fresh.LastIndexError == "" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	fresh, _ := st.GetRepo(ctx, repo.ID)
	t.Fatalf("repository was not indexed by watcher: indexed_at=%d error=%q", fresh.IndexedAt, fresh.LastIndexError)
}
