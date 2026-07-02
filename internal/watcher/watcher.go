package watcher

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/fsnotify/fsnotify"
)

const (
	defaultDebounce = 900 * time.Millisecond
	defaultRefresh  = 30 * time.Second
)

// LocalRepoWatcher keeps selected local repositories fresh by watching their
// working trees and queuing a debounced reindex when files change. It is a first
// step toward the live/fresh vision: today the refresh is a full Zoekt rebuild,
// but the service boundary can later feed per-file delta indexing.
type LocalRepoWatcher struct {
	store   *store.Store
	indexer *indexer.Indexer

	debounce time.Duration
	refresh  time.Duration
	logger   *slog.Logger

	mu         sync.Mutex
	repos      map[int64]store.Repo
	watched    map[string]int64
	repoPaths  map[int64]map[string]struct{}
	timers     map[int64]*time.Timer
	watcher    *fsnotify.Watcher
	started    bool
	startError error
}

func NewLocalRepoWatcher(st *store.Store, ix *indexer.Indexer) *LocalRepoWatcher {
	return &LocalRepoWatcher{
		store:     st,
		indexer:   ix,
		debounce:  defaultDebounce,
		refresh:   defaultRefresh,
		logger:    slog.Default(),
		repos:     map[int64]store.Repo{},
		watched:   map[string]int64{},
		repoPaths: map[int64]map[string]struct{}{},
		timers:    map[int64]*time.Timer{},
	}
}

// Start launches the watcher loop. It returns after the initial repository scan
// and watch registration succeeds; the background loop stops when ctx is done.
func (w *LocalRepoWatcher) Start(ctx context.Context) error {
	w.mu.Lock()
	if w.started {
		defer w.mu.Unlock()
		return w.startError
	}
	w.started = true
	w.mu.Unlock()

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.mu.Lock()
		w.startError = err
		w.mu.Unlock()
		return err
	}
	w.watcher = fsw
	if err := w.refreshRepos(ctx, true); err != nil {
		_ = fsw.Close()
		w.mu.Lock()
		w.startError = err
		w.mu.Unlock()
		return err
	}
	go w.run(ctx)
	return nil
}

func (w *LocalRepoWatcher) run(ctx context.Context) {
	defer w.stopAll()
	ticker := time.NewTicker(w.refresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.refreshRepos(context.Background(), false); err != nil {
				w.logger.Warn("refresh local repo watchers", "error", err)
			}
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.logger.Warn("local repo watcher", "error", err)
		}
	}
}

func (w *LocalRepoWatcher) refreshRepos(ctx context.Context, initial bool) error {
	repos, err := w.store.ListSelectedLocalRepos(ctx)
	if err != nil {
		return err
	}
	seen := map[int64]struct{}{}
	for _, repo := range repos {
		seen[repo.ID] = struct{}{}
		w.mu.Lock()
		_, known := w.repos[repo.ID]
		w.repos[repo.ID] = repo
		w.mu.Unlock()
		if err := w.watchRepo(repo); err != nil {
			w.logger.Warn("watch local repository", "repo", repo.FullName, "path", repo.LocalPath, "error", err)
			continue
		}
		if !known || initial {
			w.schedule(repo.ID, "initial local freshness refresh")
		}
	}

	w.mu.Lock()
	var removed []int64
	for repoID := range w.repos {
		if _, ok := seen[repoID]; !ok {
			removed = append(removed, repoID)
		}
	}
	w.mu.Unlock()
	for _, repoID := range removed {
		w.unwatchRepo(repoID)
	}
	return nil
}

func (w *LocalRepoWatcher) watchRepo(repo store.Repo) error {
	root, err := filepath.Abs(repo.LocalPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("local repository path is not a directory")
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root && shouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		return w.addWatch(repo.ID, path)
	})
}

func (w *LocalRepoWatcher) addWatch(repoID int64, dir string) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	w.mu.Lock()
	if _, ok := w.watched[dir]; ok {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()

	if err := w.watcher.Add(dir); err != nil {
		return err
	}

	w.mu.Lock()
	w.watched[dir] = repoID
	if w.repoPaths[repoID] == nil {
		w.repoPaths[repoID] = map[string]struct{}{}
	}
	w.repoPaths[repoID][dir] = struct{}{}
	w.mu.Unlock()
	return nil
}

func (w *LocalRepoWatcher) unwatchRepo(repoID int64) {
	w.mu.Lock()
	paths := w.repoPaths[repoID]
	delete(w.repoPaths, repoID)
	delete(w.repos, repoID)
	if timer := w.timers[repoID]; timer != nil {
		timer.Stop()
		delete(w.timers, repoID)
	}
	w.mu.Unlock()

	for path := range paths {
		_ = w.watcher.Remove(path)
		w.mu.Lock()
		delete(w.watched, path)
		w.mu.Unlock()
	}
}

func (w *LocalRepoWatcher) handleEvent(event fsnotify.Event) {
	if !isIndexRelevant(event) || shouldIgnorePath(event.Name) {
		return
	}
	repoID := w.repoIDForPath(event.Name)
	if repoID == 0 {
		return
	}
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() && !shouldSkipDir(filepath.Base(event.Name)) {
			w.mu.Lock()
			repo := w.repos[repoID]
			w.mu.Unlock()
			if err := w.watchRepo(repo); err != nil {
				w.logger.Warn("watch new local directory", "path", event.Name, "error", err)
			}
		}
	}
	w.schedule(repoID, event.Op.String()+" "+event.Name)
}

func (w *LocalRepoWatcher) repoIDForPath(name string) int64 {
	abs, err := filepath.Abs(name)
	if err != nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if repoID := w.watched[abs]; repoID != 0 {
		return repoID
	}
	for dir := filepath.Dir(abs); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if repoID := w.watched[dir]; repoID != 0 {
			return repoID
		}
		next := filepath.Dir(dir)
		if next == dir {
			break
		}
	}
	return 0
}

func (w *LocalRepoWatcher) schedule(repoID int64, reason string) {
	w.mu.Lock()
	if _, ok := w.repos[repoID]; !ok {
		w.mu.Unlock()
		return
	}
	if timer := w.timers[repoID]; timer != nil {
		timer.Reset(w.debounce)
		w.mu.Unlock()
		return
	}
	w.timers[repoID] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, repoID)
		repo := w.repos[repoID]
		w.mu.Unlock()

		if repo.ID == 0 {
			return
		}
		if _, err := w.indexer.EnqueueRepoReindex(context.Background(), repoID); err != nil {
			if !strings.Contains(err.Error(), "already indexing") {
				w.logger.Warn("queue local repo reindex", "repo", repo.FullName, "reason", reason, "error", err)
			}
			return
		}
		w.logger.Info("queued local repo reindex", "repo", repo.FullName, "reason", reason)
	})
	w.mu.Unlock()
}

func (w *LocalRepoWatcher) stopAll() {
	w.mu.Lock()
	for _, timer := range w.timers {
		timer.Stop()
	}
	w.timers = map[int64]*time.Timer{}
	w.mu.Unlock()
	if w.watcher != nil {
		_ = w.watcher.Close()
	}
}

func isIndexRelevant(event fsnotify.Event) bool {
	return event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)
}

func shouldIgnorePath(path string) bool {
	return shouldSkipDir(filepath.Base(path))
}

func shouldSkipDir(name string) bool {
	return indexer.ShouldSkipDir(name)
}
