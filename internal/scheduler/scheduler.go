// Package scheduler keeps remote repositories (GitHub, GitLab, self-managed
// GitLab) fresh without manual reindexing or inbound webhooks. A poll-based
// refresher is the right fit when Codebeam runs locally against an on-prem host:
// the host never has to reach back to the laptop, so it works behind NAT.
package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/store"
)

const (
	pollInterval = time.Minute
	initialDelay = 15 * time.Second
)

// settingsStore and reindexer are the dependencies the refresher needs, kept as
// interfaces so the loop can be tested without a real store or git fetches. The
// auto-index toggle and interval are read on every poll so changes made in the
// settings UI take effect without a restart.
type settingsStore interface {
	AutoIndexSettings(ctx context.Context, defaultEnabled bool, defaultInterval time.Duration) (bool, time.Duration, error)
	ListRemoteReindexCandidates(ctx context.Context, attemptedBefore int64) ([]store.ReindexCandidate, error)
}

type reindexer interface {
	EnqueueReindex(ctx context.Context, repoID, userID int64) (int64, error)
}

// RemoteRefresher periodically enqueues reindex jobs for selected remote
// repositories whose last index attempt is older than the configured interval.
type RemoteRefresher struct {
	store           settingsStore
	indexer         reindexer
	defaultEnabled  bool
	defaultInterval time.Duration
	logger          *slog.Logger
}

func NewRemoteRefresher(st *store.Store, ix *indexer.Indexer, defaultEnabled bool, defaultInterval time.Duration) *RemoteRefresher {
	if defaultInterval <= 0 {
		defaultInterval = 30 * time.Minute
	}
	return &RemoteRefresher{store: st, indexer: ix, defaultEnabled: defaultEnabled, defaultInterval: defaultInterval, logger: slog.Default()}
}

// Start runs the refresh loop in the background until ctx is cancelled.
func (r *RemoteRefresher) Start(ctx context.Context) {
	go r.run(ctx)
}

func (r *RemoteRefresher) log() *slog.Logger {
	if r.logger == nil {
		return slog.Default()
	}
	return r.logger
}

func (r *RemoteRefresher) run(ctx context.Context) {
	// First pass shortly after startup, then every poll interval. Each pass reads
	// the current settings, so the staleness test only touches genuinely-due repos
	// (a restart does not reindex everything) and the toggle/interval can change
	// live from the settings UI.
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.refreshDue(ctx)
			timer.Reset(pollInterval)
		}
	}
}

func (r *RemoteRefresher) refreshDue(ctx context.Context) {
	enabled, interval, err := r.store.AutoIndexSettings(ctx, r.defaultEnabled, r.defaultInterval)
	if err != nil {
		r.log().Warn("read auto-index settings", "error", err)
		return
	}
	if !enabled {
		return
	}
	attemptedBefore := time.Now().Add(-interval).Unix()
	candidates, err := r.store.ListRemoteReindexCandidates(ctx, attemptedBefore)
	if err != nil {
		r.log().Warn("list remote reindex candidates", "error", err)
		return
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		if _, err := r.indexer.EnqueueReindex(ctx, candidate.RepoID, candidate.UserID); err != nil {
			// A repo already indexing (e.g. a manual reindex in flight) is expected.
			if !strings.Contains(err.Error(), "already indexing") {
				r.log().Warn("queue remote repo reindex", "repo_id", candidate.RepoID, "error", err)
			}
			continue
		}
		r.log().Info("queued remote repo reindex", "repo_id", candidate.RepoID, "user_id", candidate.UserID)
	}
}
