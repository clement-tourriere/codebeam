package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/scheduler"
	"github.com/ctourriere/codebeam/internal/watcher"
	"github.com/ctourriere/codebeam/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("create data dir", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.IndexDir, 0o755); err != nil {
		slog.Error("create index dir", "error", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.RepoDir, 0o755); err != nil {
		slog.Error("create repo dir", "error", err)
		os.Exit(1)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ctx":
			if err := runCtxCommand(ctx, cfg, os.Args[2:], os.Stdout, os.Stderr); err != nil {
				slog.Error("ctx failed", "error", err)
				os.Exit(1)
			}
			return
		case "mcp":
			if err := runMCPCommand(ctx, cfg, os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
				slog.Error("mcp failed", "error", err)
				os.Exit(1)
			}
			return
		}
	}

	// Loud warnings for defaults that are fine on a laptop but dangerous the
	// moment the instance is reachable by anyone else — so only warn when the
	// base URL is not loopback.
	if !config.IsLoopbackURL(cfg.BaseURL) {
		if cfg.SessionSecret == config.DefaultSessionSecret {
			slog.Warn("using the default session secret on a non-loopback base URL — anyone who knows it can forge sessions; set CODEBEAM_SESSION_SECRET")
		}
		if cfg.DevLogin {
			slog.Warn("passwordless dev login is enabled on a non-loopback base URL — anyone who can reach this instance can sign in; set CODEBEAM_DEV_LOGIN=false")
		}
	}

	st, err := openStore(ctx, cfg)
	if err != nil {
		slog.Error("open store", "error", err)
		os.Exit(1)
	}
	defer st.Close() // nolint:errcheck

	ix := indexer.New(cfg, st)
	if missing, err := ix.ReconcileMissingIndexes(ctx); err != nil {
		slog.Warn("index integrity check failed", "error", err)
	} else if missing > 0 {
		slog.Warn("marked repositories with missing index shards as needing reindex", "count", missing)
	}
	if cfg.WatchLocalRepos {
		localWatcher := watcher.NewLocalRepoWatcher(st, ix)
		if err := localWatcher.Start(ctx); err != nil {
			slog.Warn("local repository watcher disabled", "error", err)
		} else {
			slog.Info("local repository watcher started")
		}
	}
	// Always run the scheduler; it reads the live auto-index setting each poll so
	// it can be toggled from the settings UI without a restart. The env values are
	// the defaults when nothing is stored yet.
	scheduler.NewRemoteRefresher(st, ix, cfg.AutoIndexRemote, cfg.RemoteRefreshInterval).Start(ctx)
	slog.Info("remote auto-index scheduler started", "default_enabled", cfg.AutoIndexRemote, "default_interval", cfg.RemoteRefreshInterval)
	server, err := web.New(cfg, st, ix)
	if err != nil {
		slog.Error("create server", "error", err)
		os.Exit(1)
	}
	// Periodically mirror code-host repository access into per-user permissions
	// so grants and revocations propagate without a manual re-sync. The server
	// owns the code-host clients, so it is the permission resolver.
	if cfg.SyncPermissions {
		scheduler.NewPermissionSyncer(st, server, cfg.PermissionSyncInterval).Start(ctx)
		slog.Info("code-host permission sync started", "interval", cfg.PermissionSyncInterval)
	}

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("codebeam listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
}
