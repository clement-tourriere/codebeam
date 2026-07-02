package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/secretbox"
	"github.com/ctourriere/codebeam/internal/store"
)

// openStore resolves the token-encryption key and opens the store with it. Every
// entry point (server, ctx, mcp) goes through here, so the auto-generated key
// file is created once and shared: whichever process starts first mints it and
// the rest read it, and the store's one-time plaintext migration always runs
// under that one key.
func openStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	if cfg.EncryptionKeyFile != "" && insideDir(cfg.EncryptionKeyFile, cfg.DataDir) {
		slog.Warn("encryption key file is inside the data directory — a stolen data backup would contain both the ciphertext and the key that unlocks it; point CODEBEAM_ENCRYPTION_KEY_FILE at a path outside CODEBEAM_DATA_DIR",
			"key_file", cfg.EncryptionKeyFile, "data_dir", cfg.DataDir)
	}

	key, generated, err := secretbox.ResolveKey(cfg.EncryptionKey, cfg.EncryptionKeyFile)
	if err != nil {
		return nil, err
	}
	if generated {
		slog.Info("generated a new token-encryption key", "path", cfg.EncryptionKeyFile)
	}

	cipher, err := secretbox.NewCipher(key)
	if err != nil {
		return nil, err
	}

	st, err := store.Open(ctx, cfg.DBPath, cipher)
	if err != nil {
		return nil, err
	}

	// A freshly-generated key can't decrypt tokens sealed under a previous one
	// (e.g. a container that lost its key file on restart). Surface that loudly
	// instead of letting indexing fail one repo at a time.
	if generated {
		if n, err := st.CountUnreadableTokens(ctx); err == nil && n > 0 {
			slog.Warn("generated a new encryption key but found tokens encrypted under a different key — they are now unreadable; set CODEBEAM_ENCRYPTION_KEY or CODEBEAM_ENCRYPTION_KEY_FILE to the original key, or affected users must reconnect their code host",
				"count", n)
		}
	}
	return st, nil
}

// insideDir reports whether path resolves to a location within dir. Best-effort:
// on any path-resolution error it returns false rather than a spurious warning.
func insideDir(path, dir string) bool {
	if dir == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
