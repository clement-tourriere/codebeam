package config

import (
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultSessionSecret signs cookies when CODEBEAM_SESSION_SECRET is unset.
// Fine on a laptop; the server warns loudly when it is used, because anyone
// who knows it can forge sessions on a reachable instance.
const DefaultSessionSecret = "dev-secret-change-me"

// ZoektMaxBranches is the hard ceiling on branches indexed per repository:
// Zoekt encodes each document's branch membership in a 64-bit mask, so one
// shard physically cannot hold more than 64 branches (its builder fails with
// "too many branches"). The configured limit is clamped to this at index time.
const ZoektMaxBranches = 64

// DefaultMaxIndexedBranches is the out-of-the-box branch cap. Deliberately well
// under the Zoekt ceiling: indexing every branch of a busy repo is expensive and
// rarely what people want, and admins can raise it (up to 64) in the settings UI
// or via CODEBEAM_MAX_INDEXED_BRANCHES.
const DefaultMaxIndexedBranches = 20

type Config struct {
	Addr                 string
	BaseURL              string
	DataDir              string
	DBPath               string
	RepoDir              string
	IndexDir             string
	StaticDir            string
	TemplateGlob         string
	SessionSecret        string
	EncryptionKey        string
	EncryptionKeyFile    string
	DevLogin             bool
	WatchLocalRepos      bool
	CTagsPath            string
	IndexConcurrency     int
	IndexFileConcurrency int

	// MaxIndexedBranches caps how many branches a single repository indexes when
	// its branch policy discovers many (e.g. the "all branches" glob). Without a
	// cap, a repo with thousands of branches clones and indexes for a very long
	// time and then fails on Zoekt's 64-branch shard limit anyway. When the
	// discovered set is larger, the most recently updated branches are kept (the
	// default branch always is). 0 and values above ZoektMaxBranches mean "the
	// maximum Zoekt supports" (64).
	MaxIndexedBranches int

	AutoIndexRemote       bool
	RemoteRefreshInterval time.Duration

	// AutoExcludeInaccessible deselects a remote repository after an index run
	// fails with an access/permission error (e.g. a GitLab project the token
	// cannot read), so the background refresher stops retrying it forever. The
	// repository can be re-selected manually once access is restored.
	AutoExcludeInaccessible bool

	// Background code-host permission sync: periodically mirror each user's
	// GitHub/GitLab repository access so grants and revocations propagate
	// without a manual re-sync.
	SyncPermissions        bool
	PermissionSyncInterval time.Duration

	// Structural (AST) search caps. The engine runs ast-grep in-process via
	// an embedded WASM module; these bound one search's work.
	StructuralTimeout    time.Duration
	StructuralMaxFiles   int
	StructuralMaxMatches int

	GitHubClientID     string
	GitHubClientSecret string

	GitLabBaseURL      string
	GitLabClientID     string
	GitLabClientSecret string

	// Generic OIDC SSO login (Okta, Entra ID, Google Workspace, Keycloak, …).
	OIDCIssuer         string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCName           string
	OIDCScopes         string
	OIDCAllowedDomains []string
}

func Load() Config {
	dataDir := env("CODEBEAM_DATA_DIR", ".codebeam")
	baseURL := strings.TrimRight(env("CODEBEAM_BASE_URL", "http://localhost:8080"), "/")
	return Config{
		Addr:          env("CODEBEAM_ADDR", ":8080"),
		BaseURL:       baseURL,
		DataDir:       dataDir,
		DBPath:        env("CODEBEAM_DB_PATH", filepath.Join(dataDir, "codebeam.db")),
		RepoDir:       env("CODEBEAM_REPO_DIR", filepath.Join(dataDir, "repos")),
		IndexDir:      env("CODEBEAM_INDEX_DIR", filepath.Join(dataDir, "index")),
		StaticDir:     env("CODEBEAM_STATIC_DIR", "static"),
		TemplateGlob:  env("CODEBEAM_TEMPLATE_GLOB", "templates/*.html"),
		SessionSecret: env("CODEBEAM_SESSION_SECRET", DefaultSessionSecret),
		// Encryption of code-host access tokens at rest. When neither var is set, a
		// key file is auto-generated outside the data dir on first run (see
		// secretbox.ResolveKey), so encryption is always on with no manual setup.
		EncryptionKey:     env("CODEBEAM_ENCRYPTION_KEY", ""),
		EncryptionKeyFile: env("CODEBEAM_ENCRYPTION_KEY_FILE", defaultKeyFilePath()),
		// Dev login is passwordless, so it defaults on only for a loopback
		// BaseURL (the solo/laptop case) and off for a real host — a reachable
		// server never ships an unauthenticated admin door by accident. An
		// explicit CODEBEAM_DEV_LOGIN still overrides either way.
		DevLogin:        envBool("CODEBEAM_DEV_LOGIN", IsLoopbackURL(baseURL)),
		WatchLocalRepos: envBool("CODEBEAM_WATCH_LOCAL_REPOS", true),
		// Path to a Universal Ctags binary for symbol indexing. Falls back to
		// Zoekt's own CTAGS_COMMAND env var; empty means "auto-detect on PATH".
		CTagsPath: env("CODEBEAM_CTAGS_PATH", os.Getenv("CTAGS_COMMAND")),
		// Repository jobs can run concurrently, while document reads within one
		// repository use a separate pool. Both values are bounded by the machine's
		// available parallelism unless explicitly overridden.
		IndexConcurrency:     envInt("CODEBEAM_INDEX_CONCURRENCY", defaultIndexConcurrency()),
		IndexFileConcurrency: envInt("CODEBEAM_INDEX_FILE_CONCURRENCY", defaultIndexFileConcurrency()),
		MaxIndexedBranches:   envIntNonNeg("CODEBEAM_MAX_INDEXED_BRANCHES", DefaultMaxIndexedBranches),

		// Background scheduler that re-pulls and reindexes selected remote repos
		// (GitHub/GitLab/self-managed) on an interval, so their search stays fresh
		// without manual reindexing or inbound webhooks.
		AutoIndexRemote:         envBool("CODEBEAM_AUTO_INDEX_REMOTE", true),
		RemoteRefreshInterval:   envDuration("CODEBEAM_REMOTE_REFRESH_INTERVAL", 30*time.Minute),
		AutoExcludeInaccessible: envBool("CODEBEAM_AUTO_EXCLUDE_INACCESSIBLE", true),

		SyncPermissions:        envBool("CODEBEAM_SYNC_PERMISSIONS", true),
		PermissionSyncInterval: envDuration("CODEBEAM_PERMISSION_SYNC_INTERVAL", time.Hour),

		StructuralTimeout:    envDuration("CODEBEAM_STRUCTURAL_TIMEOUT", 15*time.Second),
		StructuralMaxFiles:   envInt("CODEBEAM_STRUCTURAL_MAX_FILES", 5000),
		StructuralMaxMatches: envInt("CODEBEAM_STRUCTURAL_MAX_MATCHES", 1000),

		GitHubClientID:     env("CODEBEAM_GITHUB_CLIENT_ID", ""),
		GitHubClientSecret: env("CODEBEAM_GITHUB_CLIENT_SECRET", ""),

		GitLabBaseURL:      strings.TrimRight(env("CODEBEAM_GITLAB_BASE_URL", "https://gitlab.com"), "/"),
		GitLabClientID:     env("CODEBEAM_GITLAB_CLIENT_ID", ""),
		GitLabClientSecret: env("CODEBEAM_GITLAB_CLIENT_SECRET", ""),

		OIDCIssuer:         strings.TrimRight(env("CODEBEAM_OIDC_ISSUER", ""), "/"),
		OIDCClientID:       env("CODEBEAM_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:   env("CODEBEAM_OIDC_CLIENT_SECRET", ""),
		OIDCName:           env("CODEBEAM_OIDC_NAME", "SSO"),
		OIDCScopes:         env("CODEBEAM_OIDC_SCOPES", "openid profile email"),
		OIDCAllowedDomains: envList("CODEBEAM_OIDC_ALLOWED_DOMAINS"),
	}
}

// defaultKeyFilePath is where the auto-generated token-encryption key lives when
// CODEBEAM_ENCRYPTION_KEY and CODEBEAM_ENCRYPTION_KEY_FILE are both unset. It sits
// under the user config dir — deliberately OUTSIDE CODEBEAM_DATA_DIR, so a stolen
// data backup does not also carry the key. Returns "" when no config dir can be
// determined (e.g. no HOME), in which case an explicit key must be configured.
func defaultKeyFilePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "codebeam", "encryption.key")
}

// IsLoopbackURL reports whether a URL's host is a loopback address (localhost,
// 127.0.0.0/8, or ::1) — i.e. the instance is only reachable from the machine
// it runs on. Used to pick safe defaults for the passwordless dev login.
func IsLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// envList parses a comma-separated env var into trimmed, lowercased values.
func envList(key string) []string {
	var out []string
	for _, item := range strings.Split(os.Getenv(key), ",") {
		if item = strings.ToLower(strings.TrimSpace(item)); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// envIntNonNeg is like envInt but accepts 0, so callers can use 0 as a sentinel
// (e.g. "unlimited"). Negative or unparsable values fall back to the default.
func envIntNonNeg(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func defaultIndexConcurrency() int {
	return max(1, min(runtime.GOMAXPROCS(0)/2, 4))
}

func defaultIndexFileConcurrency() int {
	return max(1, min(runtime.GOMAXPROCS(0), 8))
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
