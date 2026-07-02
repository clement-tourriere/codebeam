package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/ctourriere/codebeam/internal/store"
)

// permissionStore lists the identities whose repository access can be re-synced.
type permissionStore interface {
	ListSyncableIdentities(ctx context.Context) ([]store.Identity, error)
}

// permissionResolver reconciles one user's permissions for one provider against
// the code host: it grants access the user newly gained and revokes access the
// user lost, without importing repositories. Implemented by the web server,
// which owns the code-host clients.
type permissionResolver interface {
	ReconcileRepoPermissions(ctx context.Context, userID int64, provider string) error
}

// PermissionSyncer periodically mirrors code-host repository access into
// Codebeam's per-user permissions, so a developer who loses (or gains) access
// to a private repo on GitHub/GitLab loses (or gains) it in search — even
// though they would never manually re-sync in order to lose access.
type PermissionSyncer struct {
	store    permissionStore
	resolver permissionResolver
	interval time.Duration
	logger   *slog.Logger
}

func NewPermissionSyncer(st permissionStore, resolver permissionResolver, interval time.Duration) *PermissionSyncer {
	if interval <= 0 {
		interval = time.Hour
	}
	return &PermissionSyncer{store: st, resolver: resolver, interval: interval, logger: slog.Default()}
}

// Start runs the sync loop in the background until ctx is cancelled.
func (p *PermissionSyncer) Start(ctx context.Context) {
	go p.run(ctx)
}

func (p *PermissionSyncer) log() *slog.Logger {
	if p.logger == nil {
		return slog.Default()
	}
	return p.logger
}

func (p *PermissionSyncer) run(ctx context.Context) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			p.syncAll(ctx)
			timer.Reset(p.interval)
		}
	}
}

func (p *PermissionSyncer) syncAll(ctx context.Context) {
	identities, err := p.store.ListSyncableIdentities(ctx)
	if err != nil {
		p.log().Warn("list syncable identities", "error", err)
		return
	}
	for _, identity := range identities {
		if ctx.Err() != nil {
			return
		}
		if err := p.resolver.ReconcileRepoPermissions(ctx, identity.UserID, identity.Provider); err != nil {
			// A single user's token being expired or the host being unreachable
			// must not stop the others.
			p.log().Warn("reconcile repo permissions", "user_id", identity.UserID, "provider", identity.Provider, "error", err)
		}
	}
}
