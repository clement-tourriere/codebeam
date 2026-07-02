package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ctourriere/codebeam/internal/secretbox"
	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	cipher *secretbox.Cipher
}

type User struct {
	ID        int64
	Email     string
	Name      string
	AvatarURL string
	Role      string
	CreatedAt int64
}

type Identity struct {
	ID             int64
	UserID         int64
	Provider       string
	ProviderUserID string
	Username       string
	UpdatedAt      int64
}

type Repo struct {
	ID                 int64
	HostProvider       string
	HostRepoID         string
	Name               string
	FullName           string
	CloneURL           string
	WebURL             string
	DefaultBranch      string
	IndexedBranches    string
	IndexedBranchNames string
	LocalPath          string
	Private            bool
	Selected           bool
	IndexedAt          int64
	// IndexedCommit is the commit hash the primary indexed branch was at when
	// the repository was last indexed, for file:line@commit provenance. Empty
	// for non-git sources or before the first successful index.
	IndexedCommit string
	// LastCommitAt is IndexedCommit's committer timestamp (unix seconds),
	// captured at index time. Zero for non-git sources or when unknown.
	LastCommitAt int64
	// ContentStats is a JSON-encoded RepoContentStats snapshot of the primary
	// branch's indexed content, captured at index time. Empty until the first
	// successful index after content stats were introduced.
	ContentStats   string
	LastIndexError string
	UpdatedAt      int64
}

// RepoContentStats summarizes the indexed content of a repository's primary
// branch, captured while building the Zoekt index: file/line/byte totals plus
// a per-language breakdown using the same detection (go-enry) Zoekt applies to
// search results. It covers exactly what is searchable — vendor/build/VCS
// directories and oversized files are excluded like everywhere else.
type RepoContentStats struct {
	Files     int                     `json:"files"`
	Lines     int64                   `json:"lines"`
	Bytes     int64                   `json:"bytes"`
	Languages map[string]LanguageStat `json:"languages,omitempty"`
}

// LanguageStat is one language's share of a repository's indexed content.
// Files with no detectable language aggregate under the "" key.
type LanguageStat struct {
	Files int   `json:"files"`
	Lines int64 `json:"lines"`
	Bytes int64 `json:"bytes"`
}

// DecodeContentStats parses the stats snapshot; ok is false when the repo has
// not been successfully indexed since content stats were introduced.
func (r Repo) DecodeContentStats() (stats RepoContentStats, ok bool) {
	if strings.TrimSpace(r.ContentStats) == "" {
		return RepoContentStats{}, false
	}
	if err := json.Unmarshal([]byte(r.ContentStats), &stats); err != nil {
		return RepoContentStats{}, false
	}
	return stats, true
}

// IndexResult carries what a successful index run learned about a repository,
// persisted for provenance (file:line@commit) and repository statistics.
type IndexResult struct {
	// IndexedBranches is the comma-joined list of branch names indexed.
	IndexedBranches string
	// Commit is the primary branch's commit hash; empty for non-git sources.
	Commit string
	// LastCommitAt is Commit's committer timestamp in unix seconds; 0 unknown.
	LastCommitAt int64
	// ContentStats is a JSON-encoded RepoContentStats; empty when unknown.
	ContentStats string
}

type IndexJob struct {
	ID         int64
	RepoID     int64
	RepoName   string
	Status     string
	Message    string
	StartedAt  int64
	FinishedAt int64
}

// SourceDeleteResult describes the database rows affected when a user removes a
// source. DeletedRepos had their repo rows removed; SharedRepos were only
// detached from the user because another user can still access them.
type SourceDeleteResult struct {
	Provider        string
	IdentityDeleted bool
	Repos           []Repo
	DeletedRepos    []Repo
	SharedRepos     []Repo
}

func Open(ctx context.Context, path string, cipher *secretbox.Cipher) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db, cipher: cipher}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	// Encrypt any tokens still stored as plaintext from before encryption-at-rest.
	if err := s.encryptExistingTokens(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	email TEXT NOT NULL,
	name TEXT NOT NULL,
	avatar_url TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS identities (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	provider TEXT NOT NULL,
	provider_user_id TEXT NOT NULL,
	username TEXT NOT NULL,
	access_token TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL DEFAULT (unixepoch()),
	updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE(provider, provider_user_id)
);

CREATE TABLE IF NOT EXISTS repos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	host_provider TEXT NOT NULL,
	host_repo_id TEXT NOT NULL,
	name TEXT NOT NULL,
	full_name TEXT NOT NULL,
	clone_url TEXT NOT NULL,
	web_url TEXT NOT NULL,
	default_branch TEXT NOT NULL DEFAULT 'HEAD',
	indexed_branches TEXT NOT NULL DEFAULT '',
	indexed_branch_names TEXT NOT NULL DEFAULT '',
	local_path TEXT NOT NULL DEFAULT '',
	private INTEGER NOT NULL DEFAULT 0,
	selected INTEGER NOT NULL DEFAULT 0,
	indexed_at INTEGER NOT NULL DEFAULT 0,
	last_index_error TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
	UNIQUE(host_provider, host_repo_id)
);

CREATE TABLE IF NOT EXISTS repo_permissions (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL DEFAULT (unixepoch()),
	PRIMARY KEY(user_id, repo_id)
);

CREATE TABLE IF NOT EXISTS index_jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	status TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL DEFAULT (unixepoch()),
	finished_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at INTEGER NOT NULL DEFAULT (unixepoch())
);

-- Per-user selection intent. repos.selected is the OR of these rows: a repo
-- stays selected (and indexed) as long as at least one permitted user wants
-- it, so one user removing it never deletes a shard another user relies on.
CREATE TABLE IF NOT EXISTS repo_selections (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL DEFAULT (unixepoch()),
	PRIMARY KEY(user_id, repo_id)
);
`)
	if err != nil {
		return err
	}
	// One-time backfill: seed selection intent for repos already selected before
	// this table existed, crediting every user who can access them, so existing
	// indexes survive the upgrade. Guarded on an empty table so a user's later
	// removal is never resurrected on restart.
	var selections int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo_selections`).Scan(&selections); err != nil {
		return err
	}
	if selections == 0 {
		if _, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO repo_selections (user_id, repo_id)
SELECT p.user_id, p.repo_id FROM repo_permissions p
JOIN repos r ON r.id = p.repo_id
WHERE r.selected = 1
`); err != nil {
			return err
		}
	}
	if err := s.ensureColumn(ctx, "repos", "indexed_branches", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "repos", "indexed_branch_names", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "repos", "indexed_commit", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "repos", "last_commit_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "repos", "content_stats", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE repos
SET full_name = 'local/' || name,
	indexed_at = 0,
	last_index_error = 'Reindex required after local repository label cleanup.',
	updated_at = unixepoch()
WHERE host_provider = 'local'
	AND full_name LIKE 'local/%/%';
`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE index_jobs
SET status = 'failed',
	message = 'Interrupted by server restart.',
	finished_at = unixepoch()
WHERE status IN ('queued', 'running')
	AND finished_at = 0;
`)
	if err != nil {
		return err
	}
	return s.migrateAuth(ctx)
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

func (s *Store) CreateDevUser(ctx context.Context) (*User, error) {
	user, err := s.UpsertUserIdentity(ctx, "dev", "local", "dev", "dev@codebeam.local", "Development User", "", "")
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (s *Store) UpsertUserIdentity(ctx context.Context, provider, providerUserID, username, email, name, avatarURL, accessToken string) (*User, error) {
	if provider == "" || providerUserID == "" {
		return nil, errors.New("provider and provider user id are required")
	}
	if username == "" {
		username = providerUserID
	}
	if email == "" {
		email = fmt.Sprintf("%s@%s.local", providerUserID, provider)
	}
	if name == "" {
		name = username
	}

	// Encrypt the code-host token before it touches SQL. Empty stays empty so the
	// "WHERE access_token != ''" sentinels keep meaning "has a token".
	storedToken, err := s.cipher.Encrypt(accessToken)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck

	var userID int64
	err = tx.QueryRowContext(ctx, `
SELECT user_id FROM identities WHERE provider = ? AND provider_user_id = ?
`, provider, providerUserID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		// The first user to sign in owns the instance; everyone after that is a
		// member until an admin promotes them.
		role := RoleMember
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&existing); err != nil {
			return nil, err
		}
		if existing == 0 {
			role = RoleAdmin
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO users (email, name, avatar_url, role) VALUES (?, ?, ?, ?)
`, email, name, avatarURL, role)
		if err != nil {
			return nil, err
		}
		userID, err = res.LastInsertId()
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO identities (user_id, provider, provider_user_id, username, access_token)
VALUES (?, ?, ?, ?, ?)
`, userID, provider, providerUserID, username, storedToken)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		_, err = tx.ExecContext(ctx, `
UPDATE users SET email = ?, name = ?, avatar_url = ? WHERE id = ?
`, email, name, avatarURL, userID)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `
UPDATE identities
SET username = ?, access_token = ?, updated_at = unixepoch()
WHERE provider = ? AND provider_user_id = ?
`, username, storedToken, provider, providerUserID)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetUser(ctx, userID)
}

func (s *Store) UpsertIdentityForUser(ctx context.Context, userID int64, provider, providerUserID, username, accessToken string) error {
	if userID == 0 {
		return errors.New("user id is required")
	}
	if provider == "" || providerUserID == "" {
		return errors.New("provider and provider user id are required")
	}
	if username == "" {
		username = providerUserID
	}
	storedToken, err := s.cipher.Encrypt(accessToken)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO identities (user_id, provider, provider_user_id, username, access_token)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(provider, provider_user_id) DO UPDATE SET
	user_id = excluded.user_id,
	username = excluded.username,
	access_token = excluded.access_token,
	updated_at = unixepoch()
`, userID, provider, providerUserID, username, storedToken)
	return err
}

func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.db.QueryRowContext(ctx, `
SELECT id, email, name, avatar_url, role, created_at FROM users WHERE id = ?
`, id).Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Role, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) ListIdentities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, provider, provider_user_id, username, updated_at
FROM identities
WHERE user_id = ?
ORDER BY provider
`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Identity
	for rows.Next() {
		var ident Identity
		if err := rows.Scan(&ident.ID, &ident.UserID, &ident.Provider, &ident.ProviderUserID, &ident.Username, &ident.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ident)
	}
	return out, rows.Err()
}

func (s *Store) GetAccessToken(ctx context.Context, userID int64, provider string) (string, error) {
	var token string
	err := s.db.QueryRowContext(ctx, `
SELECT access_token FROM identities WHERE user_id = ? AND provider = ?
`, userID, provider).Scan(&token)
	if err != nil {
		return "", err // preserve sql.ErrNoRows for callers' errors.Is checks
	}
	// Decrypt maps ""→"" and legacy plaintext→itself; a marked value under a wrong
	// key returns an error rather than a bogus token. The error carries no secret.
	return s.cipher.Decrypt(token)
}

// encryptExistingTokens is a one-time, idempotent migration that encrypts any
// identity access tokens still stored as plaintext (rows written before
// encryption-at-rest existed). It runs on every Open but only rewrites values that
// lack the secretbox marker, so re-runs — and two processes opening the same DB —
// can never double-encrypt. It deliberately leaves updated_at untouched: this is
// not a semantic change and must not disturb ordering that keys off it (e.g.
// ListRemoteReindexCandidates).
func (s *Store) encryptExistingTokens(ctx context.Context) error {
	// Read every non-empty token into memory and close the cursor BEFORE issuing
	// any UPDATE: the store caps the pool at a single connection
	// (SetMaxOpenConns(1)), so writing while a SELECT cursor is still open would
	// deadlock on busy_timeout. The set is tiny — one row per code-host identity.
	type row struct {
		id    int64
		token string
	}
	cur, err := s.db.QueryContext(ctx, `SELECT id, access_token FROM identities WHERE access_token != ''`)
	if err != nil {
		return err
	}
	var pending []row
	for cur.Next() {
		var r row
		if err := cur.Scan(&r.id, &r.token); err != nil {
			cur.Close()
			return err
		}
		if !secretbox.IsEncrypted(r.token) {
			pending = append(pending, r)
		}
	}
	if err := cur.Err(); err != nil {
		cur.Close()
		return err
	}
	cur.Close()
	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	for _, r := range pending {
		enc, err := s.cipher.Encrypt(r.token)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE identities SET access_token = ? WHERE id = ?`, enc, r.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountUnreadableTokens returns how many non-empty, encrypted identity tokens the
// current cipher fails to decrypt — i.e. tokens sealed under a different key. It
// is used only for a startup warning (e.g. after a key file was regenerated on a
// stateless container); it never returns or logs the token values.
func (s *Store) CountUnreadableTokens(ctx context.Context) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT access_token FROM identities WHERE access_token != ''`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var unreadable int
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return 0, err
		}
		if !secretbox.IsEncrypted(token) {
			continue // legacy plaintext decrypts via passthrough, not a failure
		}
		if _, err := s.cipher.Decrypt(token); err != nil {
			unreadable++
		}
	}
	return unreadable, rows.Err()
}

func (s *Store) DeleteSourceForUser(ctx context.Context, userID int64, provider string) (SourceDeleteResult, error) {
	provider = strings.TrimSpace(provider)
	result := SourceDeleteResult{Provider: provider}
	if userID == 0 {
		return result, errors.New("user id is required")
	}
	if provider == "" {
		return result, errors.New("source provider is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback() // nolint:errcheck

	rows, err := tx.QueryContext(ctx, `
SELECT r.id, r.host_provider, r.host_repo_id, r.name, r.full_name, r.clone_url, r.web_url, r.default_branch, r.indexed_branches, r.indexed_branch_names,
	r.local_path, r.private, r.selected, r.indexed_at, r.last_index_error, r.updated_at, r.indexed_commit, r.last_commit_at, r.content_stats
FROM repos r
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ? AND r.host_provider = ?
ORDER BY r.full_name ASC
`, userID, provider)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			rows.Close()
			return result, err
		}
		result.Repos = append(result.Repos, repo)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE user_id = ? AND provider = ?`, userID, provider)
	if err != nil {
		return result, err
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		result.IdentityDeleted = true
	}

	for _, repo := range result.Repos {
		if _, err := tx.ExecContext(ctx, `DELETE FROM repo_permissions WHERE user_id = ? AND repo_id = ?`, userID, repo.ID); err != nil {
			return result, err
		}
		// Losing access also drops this user's selection intent; keep the
		// invariant that repos.selected reflects the remaining selectors.
		if _, err := tx.ExecContext(ctx, `DELETE FROM repo_selections WHERE user_id = ? AND repo_id = ?`, userID, repo.ID); err != nil {
			return result, err
		}
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo_permissions WHERE repo_id = ?`, repo.ID).Scan(&remaining); err != nil {
			return result, err
		}
		if remaining == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, repo.ID); err != nil {
				return result, err
			}
			result.DeletedRepos = append(result.DeletedRepos, repo)
		} else {
			var selectors int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo_selections WHERE repo_id = ?`, repo.ID).Scan(&selectors); err != nil {
				return result, err
			}
			if selectors == 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE repos SET selected = 0, updated_at = unixepoch() WHERE id = ?`, repo.ID); err != nil {
					return result, err
				}
			}
			result.SharedRepos = append(result.SharedRepos, repo)
		}
	}

	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) UpsertRepo(ctx context.Context, repo Repo, userID int64) (*Repo, error) {
	if repo.HostProvider == "" || repo.HostRepoID == "" || repo.FullName == "" {
		return nil, errors.New("repo provider, provider id, and full name are required")
	}
	if repo.Name == "" {
		repo.Name = repo.FullName
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "HEAD"
	}
	repo.IndexedBranches = NormalizeBranchPolicy(repo.IndexedBranches)
	repo.IndexedBranchNames = NormalizeBranchList(repo.IndexedBranchNames)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // nolint:errcheck

	var repoID int64
	err = tx.QueryRowContext(ctx, `
INSERT INTO repos (
	host_provider, host_repo_id, name, full_name, clone_url, web_url, default_branch, indexed_branches, indexed_branch_names,
	local_path, private, selected, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch())
ON CONFLICT(host_provider, host_repo_id) DO UPDATE SET
	name = excluded.name,
	full_name = excluded.full_name,
	clone_url = excluded.clone_url,
	web_url = excluded.web_url,
	default_branch = excluded.default_branch,
	indexed_branches = CASE WHEN excluded.indexed_branches != '' THEN excluded.indexed_branches ELSE repos.indexed_branches END,
	local_path = excluded.local_path,
	private = excluded.private,
	updated_at = unixepoch()
RETURNING id
`, repo.HostProvider, repo.HostRepoID, repo.Name, repo.FullName, repo.CloneURL, repo.WebURL, repo.DefaultBranch, repo.IndexedBranches, repo.IndexedBranchNames, repo.LocalPath, boolToInt(repo.Private), boolToInt(repo.Selected)).Scan(&repoID)
	if err != nil {
		return nil, err
	}
	if userID != 0 {
		if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO repo_permissions (user_id, repo_id) VALUES (?, ?)
`, userID, repoID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetRepo(ctx, repoID)
}

func (s *Store) GetRepo(ctx context.Context, repoID int64) (*Repo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, host_provider, host_repo_id, name, full_name, clone_url, web_url, default_branch, indexed_branches, indexed_branch_names,
	local_path, private, selected, indexed_at, last_index_error, updated_at, indexed_commit, last_commit_at, content_stats
FROM repos
WHERE id = ?
`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	repo, err := scanRepo(rows)
	if err != nil {
		return nil, err
	}
	return &repo, rows.Err()
}

func (s *Store) GetRepoForUser(ctx context.Context, repoID, userID int64) (*Repo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.host_provider, r.host_repo_id, r.name, r.full_name, r.clone_url, r.web_url, r.default_branch, r.indexed_branches, r.indexed_branch_names,
	r.local_path, r.private, r.selected, r.indexed_at, r.last_index_error, r.updated_at, r.indexed_commit, r.last_commit_at, r.content_stats
FROM repos r
JOIN repo_permissions p ON p.repo_id = r.id
WHERE r.id = ? AND p.user_id = ?
`, repoID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	repo, err := scanRepo(rows)
	if err != nil {
		return nil, err
	}
	return &repo, rows.Err()
}

func (s *Store) UserCanAccessRepo(ctx context.Context, userID, repoID int64) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
SELECT 1 FROM repo_permissions WHERE user_id = ? AND repo_id = ?
`, userID, repoID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) ListReposForUser(ctx context.Context, userID int64) ([]Repo, error) {
	return s.listRepos(ctx, `
SELECT r.id, r.host_provider, r.host_repo_id, r.name, r.full_name, r.clone_url, r.web_url, r.default_branch, r.indexed_branches, r.indexed_branch_names,
	r.local_path, r.private, r.selected, r.indexed_at, r.last_index_error, r.updated_at, r.indexed_commit, r.last_commit_at, r.content_stats
FROM repos r
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ?
ORDER BY r.selected DESC, r.full_name ASC
`, userID)
}

func (s *Store) ListSelectedReposForUser(ctx context.Context, userID int64) ([]Repo, error) {
	return s.listRepos(ctx, `
SELECT r.id, r.host_provider, r.host_repo_id, r.name, r.full_name, r.clone_url, r.web_url, r.default_branch, r.indexed_branches, r.indexed_branch_names,
	r.local_path, r.private, r.selected, r.indexed_at, r.last_index_error, r.updated_at, r.indexed_commit, r.last_commit_at, r.content_stats
FROM repos r
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ? AND r.selected = 1
ORDER BY r.full_name ASC
`, userID)
}

// ListSelectedLocalRepos returns every selected local repository, independent of
// user. Background local-refresh services use this to keep on-disk repositories
// fresh without needing code-host credentials.
func (s *Store) ListSelectedLocalRepos(ctx context.Context) ([]Repo, error) {
	return s.listRepos(ctx, `
SELECT id, host_provider, host_repo_id, name, full_name, clone_url, web_url, default_branch, indexed_branches, indexed_branch_names,
	local_path, private, selected, indexed_at, last_index_error, updated_at, indexed_commit, last_commit_at, content_stats
FROM repos
WHERE host_provider = 'local' AND selected = 1 AND local_path != ''
ORDER BY full_name ASC
`)
}

func (s *Store) ListIndexedReposForUser(ctx context.Context, userID int64) ([]Repo, error) {
	return s.listRepos(ctx, `
SELECT r.id, r.host_provider, r.host_repo_id, r.name, r.full_name, r.clone_url, r.web_url, r.default_branch, r.indexed_branches, r.indexed_branch_names,
	r.local_path, r.private, r.selected, r.indexed_at, r.last_index_error, r.updated_at, r.indexed_commit, r.last_commit_at, r.content_stats
FROM repos r
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ? AND r.selected = 1 AND r.indexed_at > 0 AND r.last_index_error = ''
ORDER BY r.full_name ASC
`, userID)
}

// ReindexCandidate pairs a repository due for a background reindex with a user
// whose token can fetch it.
type ReindexCandidate struct {
	RepoID int64
	UserID int64
}

// ListRemoteReindexCandidates returns selected non-local repositories whose most
// recent index attempt (queued, succeeded, or failed) started before
// attemptedBefore, each paired with a user that can fetch it. Private repos need
// a user with a non-empty token for the provider; public repos can be refreshed
// with any user who has installed the repo, even when GitHub/GitLab OAuth was
// never connected. The attempt-based staleness check covers never-indexed repos
// and naturally backs off recently-tried ones.
func (s *Store) ListRemoteReindexCandidates(ctx context.Context, attemptedBefore int64) ([]ReindexCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id,
	CASE
		WHEN r.private = 0 THEN COALESCE(
			(SELECT i.user_id
			 FROM identities i
			 JOIN repo_permissions p ON p.user_id = i.user_id AND p.repo_id = r.id
			 WHERE i.provider = r.host_provider AND i.access_token != ''
			 ORDER BY i.updated_at DESC
			 LIMIT 1),
			(SELECT p.user_id
			 FROM repo_permissions p
			 WHERE p.repo_id = r.id
			 ORDER BY p.created_at ASC
			 LIMIT 1)
		)
		ELSE (SELECT i.user_id
			 FROM identities i
			 JOIN repo_permissions p ON p.user_id = i.user_id AND p.repo_id = r.id
			 WHERE i.provider = r.host_provider AND i.access_token != ''
			 ORDER BY i.updated_at DESC
			 LIMIT 1)
	END AS user_id
FROM repos r
WHERE r.host_provider != 'local'
	AND r.host_provider != 'dev'
	AND r.selected = 1
	AND NOT EXISTS (
		SELECT 1 FROM index_jobs j
		WHERE j.repo_id = r.id AND j.started_at >= ?
	)
ORDER BY r.indexed_at ASC, r.id ASC
`, attemptedBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ReindexCandidate
	for rows.Next() {
		var repoID int64
		var userID sql.NullInt64
		if err := rows.Scan(&repoID, &userID); err != nil {
			return nil, err
		}
		if !userID.Valid {
			continue
		}
		out = append(out, ReindexCandidate{RepoID: repoID, UserID: userID.Int64})
	}
	return out, rows.Err()
}

// ListIndexedRepos returns all selected, successfully indexed repositories. It is
// used by local CLI commands that run against the user's on-disk Codebeam data
// without a web session.
func (s *Store) ListIndexedRepos(ctx context.Context) ([]Repo, error) {
	return s.listRepos(ctx, `
SELECT id, host_provider, host_repo_id, name, full_name, clone_url, web_url, default_branch, indexed_branches, indexed_branch_names,
	local_path, private, selected, indexed_at, last_index_error, updated_at, indexed_commit, last_commit_at, content_stats
FROM repos
WHERE selected = 1 AND indexed_at > 0 AND last_index_error = ''
ORDER BY full_name ASC
`)
}

func (s *Store) listRepos(ctx context.Context, query string, args ...any) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		repo, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func scanRepo(scanner interface {
	Scan(dest ...any) error
}) (Repo, error) {
	var repo Repo
	var private, selected int
	err := scanner.Scan(
		&repo.ID,
		&repo.HostProvider,
		&repo.HostRepoID,
		&repo.Name,
		&repo.FullName,
		&repo.CloneURL,
		&repo.WebURL,
		&repo.DefaultBranch,
		&repo.IndexedBranches,
		&repo.IndexedBranchNames,
		&repo.LocalPath,
		&private,
		&selected,
		&repo.IndexedAt,
		&repo.LastIndexError,
		&repo.UpdatedAt,
		&repo.IndexedCommit,
		&repo.LastCommitAt,
		&repo.ContentStats,
	)
	repo.Private = private != 0
	repo.Selected = selected != 0
	return repo, err
}

// SelectRepoForUser records that the user wants the repository indexed and
// marks the shared repo row selected. Idempotent per user.
func (s *Store) SelectRepoForUser(ctx context.Context, userID, repoID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck
	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO repo_selections (user_id, repo_id) VALUES (?, ?)
`, userID, repoID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE repos SET selected = 1, updated_at = unixepoch() WHERE id = ?
`, repoID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeselectRepoForUser drops the user's selection intent and reports whether any
// other user still wants the repository. When none do it clears the shared
// selected flag and returns false, so the caller can safely remove the index;
// otherwise the shard must be kept for the remaining selectors.
func (s *Store) DeselectRepoForUser(ctx context.Context, userID, repoID int64) (stillSelected bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // nolint:errcheck
	if _, err := tx.ExecContext(ctx, `
DELETE FROM repo_selections WHERE user_id = ? AND repo_id = ?
`, userID, repoID); err != nil {
		return false, err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM repo_selections WHERE repo_id = ?
`, repoID).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE repos SET selected = 0, updated_at = unixepoch() WHERE id = ?
`, repoID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return remaining > 0, nil
}

// ExcludeRepoForAccessFailure drops one user's selection intent after an index
// run failed because that user's token cannot access the repository, and records
// why on the repo row. When no other user still wants the repository it is fully
// deselected (so the background refresher stops retrying it) and its index state
// is cleared; when others remain it stays selected for them and simply carries
// the recorded error, so their next refresh can retry under a token that works.
// It returns whether the repository is still selected by someone.
func (s *Store) ExcludeRepoForAccessFailure(ctx context.Context, userID, repoID int64, reason string) (stillSelected bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() // nolint:errcheck
	if _, err := tx.ExecContext(ctx, `
DELETE FROM repo_selections WHERE user_id = ? AND repo_id = ?
`, userID, repoID); err != nil {
		return false, err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM repo_selections WHERE repo_id = ?
`, repoID).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE repos SET selected = 0, indexed_at = 0, indexed_branch_names = '', indexed_commit = '',
	last_commit_at = 0, content_stats = '', last_index_error = ?, updated_at = unixepoch() WHERE id = ?
`, reason, repoID); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
UPDATE repos SET last_index_error = ?, updated_at = unixepoch() WHERE id = ?
`, reason, repoID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return remaining > 0, nil
}

func (s *Store) SetRepoIndexedBranches(ctx context.Context, repoID int64, branches string) error {
	normalized := NormalizeBranchPolicy(branches)
	_, err := s.db.ExecContext(ctx, `
UPDATE repos
SET indexed_branches = ?,
	indexed_branch_names = '',
	indexed_at = 0,
	last_index_error = 'Reindex required after branch selection changed.',
	updated_at = unixepoch()
WHERE id = ? AND indexed_branches != ?
`, normalized, repoID, normalized)
	return err
}

func (s *Store) MarkRepoIndexStarted(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE repos SET last_index_error = '', updated_at = unixepoch() WHERE id = ?
`, repoID)
	return err
}

func (s *Store) MarkRepoIndexFinished(ctx context.Context, repoID int64, errMessage string) error {
	if errMessage == "" {
		return s.MarkRepoIndexSucceeded(ctx, repoID, IndexResult{})
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE repos SET last_index_error = ?, updated_at = unixepoch() WHERE id = ?
`, errMessage, repoID)
	return err
}

// MarkRepoIndexSucceeded records a successful index along with what the run
// learned: the branches indexed, the primary branch's commit hash and commit
// timestamp (for provenance and freshness), and the content stats snapshot.
func (s *Store) MarkRepoIndexSucceeded(ctx context.Context, repoID int64, result IndexResult) error {
	normalized := NormalizeBranchList(result.IndexedBranches)
	_, err := s.db.ExecContext(ctx, `
UPDATE repos SET indexed_at = unixepoch(), indexed_branch_names = ?, indexed_commit = ?, last_commit_at = ?, content_stats = ?, last_index_error = '', updated_at = unixepoch() WHERE id = ?
`, normalized, result.Commit, result.LastCommitAt, result.ContentStats, repoID)
	return err
}

func (s *Store) MarkRepoUnindexed(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE repos SET indexed_at = 0, indexed_branch_names = '', indexed_commit = '', last_commit_at = 0, content_stats = '', last_index_error = '', updated_at = unixepoch() WHERE id = ?
`, repoID)
	return err
}

func (s *Store) CreateIndexJob(ctx context.Context, repoID int64, message string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO index_jobs (repo_id, status, message) VALUES (?, 'queued', ?)
`, repoID, message)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateIndexJob(ctx context.Context, jobID int64, status, message string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE index_jobs SET status = ?, message = ? WHERE id = ?
`, status, message, jobID)
	return err
}

func (s *Store) FinishIndexJob(ctx context.Context, jobID int64, status, message string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE index_jobs SET status = ?, message = ?, finished_at = unixepoch() WHERE id = ?
`, status, message, jobID)
	return err
}

func (s *Store) CancelLatestIndexJobForRepo(ctx context.Context, repoID int64, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck

	res, err := tx.ExecContext(ctx, `
UPDATE index_jobs
SET status = 'cancelled', message = ?, finished_at = unixepoch()
WHERE id = (
	SELECT id FROM index_jobs
	WHERE repo_id = ? AND status IN ('queued', 'running') AND finished_at = 0
	ORDER BY id DESC
	LIMIT 1
)
`, message, repoID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE repos SET last_index_error = ?, updated_at = unixepoch() WHERE id = ?
`, message, repoID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LatestIndexJobsForUser(ctx context.Context, userID int64) (map[int64]IndexJob, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT j.id, j.repo_id, r.full_name, j.status, j.message, j.started_at, j.finished_at
FROM index_jobs j
JOIN (
	SELECT repo_id, max(id) AS id
	FROM index_jobs
	GROUP BY repo_id
) latest ON latest.id = j.id
JOIN repos r ON r.id = j.repo_id
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ?
`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := map[int64]IndexJob{}
	for rows.Next() {
		var job IndexJob
		if err := rows.Scan(&job.ID, &job.RepoID, &job.RepoName, &job.Status, &job.Message, &job.StartedAt, &job.FinishedAt); err != nil {
			return nil, err
		}
		jobs[job.RepoID] = job
	}
	return jobs, rows.Err()
}

func (s *Store) ListIndexJobsForUser(ctx context.Context, userID int64, limit int) ([]IndexJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT j.id, j.repo_id, r.full_name, j.status, j.message, j.started_at, j.finished_at
FROM index_jobs j
JOIN repos r ON r.id = j.repo_id
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ?
ORDER BY j.id DESC
LIMIT ?
`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []IndexJob
	for rows.Next() {
		var job IndexJob
		if err := rows.Scan(&job.ID, &job.RepoID, &job.RepoName, &job.Status, &job.Message, &job.StartedAt, &job.FinishedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListIndexJobsForRepo returns the most recent index jobs for a single repo the
// user can access, newest first. Used by the per-repo management drawer.
func (s *Store) ListIndexJobsForRepo(ctx context.Context, userID, repoID int64, limit int) ([]IndexJob, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT j.id, j.repo_id, r.full_name, j.status, j.message, j.started_at, j.finished_at
FROM index_jobs j
JOIN repos r ON r.id = j.repo_id
JOIN repo_permissions p ON p.repo_id = r.id
WHERE p.user_id = ? AND r.id = ?
ORDER BY j.id DESC
LIMIT ?
`, userID, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []IndexJob
	for rows.Next() {
		var job IndexJob
		if err := rows.Scan(&job.ID, &job.RepoID, &job.RepoName, &job.Status, &job.Message, &job.StartedAt, &job.FinishedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

const (
	settingAutoIndexRemote         = "auto_index_remote"
	settingRemoteRefreshInterval   = "remote_refresh_interval"
	settingMaxIndexedBranches      = "max_indexed_branches"
	settingAutoExcludeInaccessible = "auto_exclude_inaccessible"
)

// GetSetting returns a persisted setting value and whether it was set.
func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting persists a setting value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings (key, value, updated_at) VALUES (?, ?, unixepoch())
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = unixepoch()
`, key, value)
	return err
}

// AutoIndexSettings returns the remote auto-index configuration, falling back to
// the supplied defaults (from env/config) when not set in the database.
func (s *Store) AutoIndexSettings(ctx context.Context, defaultEnabled bool, defaultInterval time.Duration) (bool, time.Duration, error) {
	enabled, interval := defaultEnabled, defaultInterval
	if value, ok, err := s.GetSetting(ctx, settingAutoIndexRemote); err != nil {
		return enabled, interval, err
	} else if ok {
		enabled = value == "true"
	}
	if value, ok, err := s.GetSetting(ctx, settingRemoteRefreshInterval); err != nil {
		return enabled, interval, err
	} else if ok {
		if d, perr := time.ParseDuration(value); perr == nil && d > 0 {
			interval = d
		}
	}
	return enabled, interval, nil
}

// MaxIndexedBranches returns the configured cap on how many branches a single
// repository indexes, falling back to the supplied default (from env/config)
// when unset. A value of 0 means unlimited.
func (s *Store) MaxIndexedBranches(ctx context.Context, def int) (int, error) {
	value, ok, err := s.GetSetting(ctx, settingMaxIndexedBranches)
	if err != nil {
		return def, err
	}
	if !ok {
		return def, nil
	}
	n, perr := strconv.Atoi(strings.TrimSpace(value))
	if perr != nil || n < 0 {
		return def, nil
	}
	return n, nil
}

// SetMaxIndexedBranches persists the branch cap. A value of 0 means unlimited.
func (s *Store) SetMaxIndexedBranches(ctx context.Context, n int) error {
	if n < 0 {
		n = 0
	}
	return s.SetSetting(ctx, settingMaxIndexedBranches, strconv.Itoa(n))
}

// AutoExcludeInaccessible returns whether repositories that fail to index with
// an access/permission error are automatically deselected, falling back to the
// supplied default when unset.
func (s *Store) AutoExcludeInaccessible(ctx context.Context, def bool) (bool, error) {
	value, ok, err := s.GetSetting(ctx, settingAutoExcludeInaccessible)
	if err != nil {
		return def, err
	}
	if !ok {
		return def, nil
	}
	return value == "true", nil
}

// SetAutoExcludeInaccessible persists the auto-exclude-on-access-error toggle.
func (s *Store) SetAutoExcludeInaccessible(ctx context.Context, enabled bool) error {
	return s.SetSetting(ctx, settingAutoExcludeInaccessible, strconv.FormatBool(enabled))
}

// SetAutoIndexSettings persists the remote auto-index configuration.
func (s *Store) SetAutoIndexSettings(ctx context.Context, enabled bool, interval time.Duration) error {
	if err := s.SetSetting(ctx, settingAutoIndexRemote, strconv.FormatBool(enabled)); err != nil {
		return err
	}
	return s.SetSetting(ctx, settingRemoteRefreshInterval, interval.String())
}

func UnixNow() int64 {
	return time.Now().Unix()
}

func (r Repo) BranchesToIndex() []string {
	if branches := BranchList(r.IndexedBranches); len(branches) > 0 {
		return branches
	}
	if branch := strings.TrimSpace(r.DefaultBranch); branch != "" {
		return []string{branch}
	}
	return []string{"HEAD"}
}

func (r Repo) IndexedBranchList() []string {
	return BranchList(r.IndexedBranchNames)
}

func (r Repo) BranchLabel() string {
	policy := BranchPolicyList(r.IndexedBranches)
	if len(policy) == 0 || !BranchPolicyRequiresDiscovery(r.IndexedBranches) {
		return strings.Join(r.BranchesToIndex(), ", ")
	}
	actual := r.IndexedBranchList()
	label := BranchPolicyLabel(r.IndexedBranches)
	if len(actual) == 0 {
		return label
	}
	return fmt.Sprintf("%s (%d indexed)", label, len(actual))
}

func NormalizeBranchList(value string) string {
	return strings.Join(BranchList(value), ",")
}

func NormalizeBranchPolicy(value string) string {
	return strings.Join(BranchPolicyList(value), ",")
}

func BranchPolicyLabel(value string) string {
	tokens := BranchPolicyList(value)
	if len(tokens) == 1 && tokens[0] == "*" {
		return "all branches"
	}
	return strings.Join(tokens, ", ")
}

func BranchPolicyRequiresDiscovery(value string) bool {
	for _, token := range BranchPolicyList(value) {
		pattern := strings.TrimPrefix(token, "!")
		if strings.HasPrefix(token, "!") || pattern == "*" || isBranchGlob(pattern) {
			return true
		}
	}
	return false
}

func ResolveBranchPolicy(policy, defaultBranch string, available []string) (branches []string, missing []string) {
	tokens := BranchPolicyList(policy)
	if len(tokens) == 0 {
		return defaultBranchList(defaultBranch), nil
	}
	if !BranchPolicyRequiresDiscovery(policy) {
		return BranchList(policy), nil
	}

	available = orderedAvailableBranches(available, defaultBranch)
	if len(available) == 0 {
		return nil, nil
	}

	var includes, excludes []string
	includeAll := false
	for _, token := range tokens {
		exclude := strings.HasPrefix(token, "!")
		pattern := strings.TrimPrefix(token, "!")
		if pattern == "*" {
			if exclude {
				excludes = append(excludes, pattern)
			} else {
				includeAll = true
			}
			continue
		}
		if exclude {
			excludes = append(excludes, pattern)
		} else {
			includes = append(includes, pattern)
		}
	}
	if len(includes) == 0 && !includeAll {
		includeAll = true
	}

	availableSet := make(map[string]struct{}, len(available))
	for _, branch := range available {
		availableSet[branch] = struct{}{}
	}
	for _, pattern := range includes {
		if isBranchGlob(pattern) {
			continue
		}
		if _, ok := availableSet[pattern]; !ok {
			missing = append(missing, pattern)
		}
	}

	for _, branch := range available {
		included := includeAll
		for _, pattern := range includes {
			if branchPatternMatches(pattern, branch) {
				included = true
				break
			}
		}
		if !included {
			continue
		}
		excluded := false
		for _, pattern := range excludes {
			if branchPatternMatches(pattern, branch) {
				excluded = true
				break
			}
		}
		if !excluded {
			branches = append(branches, branch)
		}
	}
	return branches, missing
}

func BranchList(value string) []string {
	var branches []string
	for _, token := range BranchPolicyList(value) {
		if strings.HasPrefix(token, "!") || token == "*" || isBranchGlob(token) {
			continue
		}
		branches = append(branches, token)
	}
	return branches
}

func BranchPolicyList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	seen := map[string]struct{}{}
	branches := make([]string, 0, len(parts))
	for _, part := range parts {
		branch := normalizeBranchPolicyToken(part)
		if branch == "" {
			continue
		}
		if _, ok := seen[branch]; ok {
			continue
		}
		seen[branch] = struct{}{}
		branches = append(branches, branch)
	}
	return branches
}

func normalizeBranchPolicyToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	exclude := strings.HasPrefix(value, "!")
	if exclude {
		value = strings.TrimSpace(strings.TrimPrefix(value, "!"))
	}
	value = normalizeBranchName(value)
	if strings.EqualFold(value, "all") {
		value = "*"
	}
	if value == "" {
		return ""
	}
	if exclude {
		return "!" + value
	}
	return value
}

func normalizeBranchName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "refs/heads/")
	value = strings.TrimPrefix(value, "origin/")
	return strings.Trim(value, "/")
}

func defaultBranchList(defaultBranch string) []string {
	if branch := strings.TrimSpace(defaultBranch); branch != "" {
		return []string{normalizeBranchName(branch)}
	}
	return []string{"HEAD"}
}

func orderedAvailableBranches(available []string, defaultBranch string) []string {
	seen := map[string]struct{}{}
	branches := make([]string, 0, len(available))
	for _, branch := range available {
		branch = normalizeBranchName(branch)
		if branch == "" {
			continue
		}
		if _, ok := seen[branch]; ok {
			continue
		}
		seen[branch] = struct{}{}
		branches = append(branches, branch)
	}
	sort.Strings(branches)
	defaultBranch = normalizeBranchName(defaultBranch)
	if defaultBranch == "" {
		return branches
	}
	for idx, branch := range branches {
		if branch == defaultBranch {
			copy(branches[1:idx+1], branches[:idx])
			branches[0] = branch
			break
		}
	}
	return branches
}

func branchPatternMatches(pattern, branch string) bool {
	if pattern == "*" {
		return true
	}
	if !isBranchGlob(pattern) {
		return pattern == branch
	}
	var expr strings.Builder
	expr.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			expr.WriteString(".*")
		case '?':
			expr.WriteString(".")
		default:
			expr.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	expr.WriteString("$")
	matched, err := regexp.MatchString(expr.String(), branch)
	return err == nil && matched
}

func isBranchGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?")
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
