package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// User roles. The first user of an instance becomes the admin; everyone who
// signs in afterwards is a member until an admin promotes them.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// Token prefixes make leaked credentials identifiable (and grep-able) without
// revealing anything: cbp = personal access token, cbo = OAuth access token,
// cbr = OAuth refresh token. Only the SHA-256 of a token is stored.
const (
	patPrefix          = "cbp_"
	oauthAccessPrefix  = "cbo_"
	oauthRefreshPrefix = "cbr_"
)

// Credential lifetimes. PATs may live forever (expiresAt = 0); OAuth
// credentials always expire so a lost token dies on its own.
const (
	OAuthCodeTTL         = 10 * time.Minute
	OAuthAccessTokenTTL  = time.Hour
	OAuthRefreshTokenTTL = 30 * 24 * time.Hour
)

// APIToken is a personal access token as shown in the settings UI. The secret
// itself is only available at creation time.
type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	Prefix     string // first characters of the token, for recognition
	CreatedAt  int64
	LastUsedAt int64
	ExpiresAt  int64 // 0 = never
	RevokedAt  int64 // 0 = active
}

// OAuthClient is a dynamically registered OAuth client (RFC 7591), typically
// an MCP client like Claude Code registering itself before the consent flow.
type OAuthClient struct {
	ClientID     string
	SecretHash   string // empty for public clients (token_endpoint_auth_method "none")
	Name         string
	RedirectURIs []string
	CreatedAt    int64
}

// OAuthCode is a single-use authorization code awaiting exchange at the token
// endpoint. PKCE is mandatory, so CodeChallenge is always set.
type OAuthCode struct {
	ClientID      string
	UserID        int64
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Resource      string
	ExpiresAt     int64
}

// OAuthToken is one access/refresh token pair issued to a client for a user.
// Refresh rotates both secrets in place, keeping one row per grant.
type OAuthToken struct {
	ID               int64
	ClientID         string
	UserID           int64
	Scope            string
	Resource         string
	AccessExpiresAt  int64
	RefreshExpiresAt int64
	CreatedAt        int64
	LastUsedAt       int64
	RevokedAt        int64
}

// TokenUsage carries where a bearer credential was just used from, recorded on
// each authenticated request so grants can be told apart when revoking.
type TokenUsage struct {
	IP        string
	UserAgent string
}

func (s *Store) migrateAuth(ctx context.Context) error {
	if err := s.ensureColumn(ctx, "users", "role", "TEXT NOT NULL DEFAULT 'member'"); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS api_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	prefix TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch()),
	last_used_at INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS oauth_clients (
	client_id TEXT PRIMARY KEY,
	secret_hash TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	redirect_uris TEXT NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS oauth_codes (
	code_hash TEXT PRIMARY KEY,
	client_id TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	redirect_uri TEXT NOT NULL,
	code_challenge TEXT NOT NULL,
	scope TEXT NOT NULL DEFAULT '',
	resource TEXT NOT NULL DEFAULT '',
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS oauth_tokens (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	client_id TEXT NOT NULL REFERENCES oauth_clients(client_id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	access_hash TEXT NOT NULL UNIQUE,
	refresh_hash TEXT NOT NULL UNIQUE,
	scope TEXT NOT NULL DEFAULT '',
	resource TEXT NOT NULL DEFAULT '',
	access_expires_at INTEGER NOT NULL,
	refresh_expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL DEFAULT (unixepoch()),
	last_used_at INTEGER NOT NULL DEFAULT 0,
	last_used_ip TEXT NOT NULL DEFAULT '',
	last_used_ua TEXT NOT NULL DEFAULT '',
	use_count INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0
);
`)
	if err != nil {
		return err
	}
	// Usage-provenance columns added after the table shipped.
	for _, col := range [][2]string{
		{"last_used_ip", "TEXT NOT NULL DEFAULT ''"},
		{"last_used_ua", "TEXT NOT NULL DEFAULT ''"},
		{"use_count", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if err := s.ensureColumn(ctx, "oauth_tokens", col[0], col[1]); err != nil {
			return err
		}
	}
	// Existing single-user installs predate roles: promote the earliest user so
	// the instance always has an admin who can manage the others.
	_, err = s.db.ExecContext(ctx, `
UPDATE users SET role = 'admin'
WHERE id = (SELECT MIN(id) FROM users)
	AND NOT EXISTS (SELECT 1 FROM users WHERE role = 'admin');
`)
	return err
}

// IsAdmin reports whether the user holds the admin role.
func (u *User) IsAdmin() bool {
	return u != nil && u.Role == RoleAdmin
}

// ListUsers returns every user, admins first, then by creation order.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, email, name, avatar_url, role, created_at FROM users
ORDER BY role = 'admin' DESC, id ASC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetUserRole changes a user's role. Demoting the last admin is refused so an
// instance can never lock itself out of administration.
func (s *Store) SetUserRole(ctx context.Context, userID int64, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return fmt.Errorf("unknown role %q", role)
	}
	if role != RoleAdmin {
		var admins int
		err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM users WHERE role = 'admin' AND id != ?
`, userID).Scan(&admins)
		if err != nil {
			return err
		}
		if admins == 0 {
			return errors.New("cannot demote the last admin")
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// --- Personal access tokens ---

// CreateAPIToken mints a new PAT for the user and returns the secret, which is
// never recoverable afterwards. ttl <= 0 means the token never expires.
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name string, ttl time.Duration) (string, *APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("token name is required")
	}
	secret := patPrefix + randomSecret()
	var expiresAt int64
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl).Unix()
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO api_tokens (user_id, name, token_hash, prefix, expires_at)
VALUES (?, ?, ?, ?, ?)
`, userID, name, hashToken(secret), displayPrefix(secret), expiresAt)
	if err != nil {
		return "", nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", nil, err
	}
	token := &APIToken{ID: id, UserID: userID, Name: name, Prefix: displayPrefix(secret), CreatedAt: time.Now().Unix(), ExpiresAt: expiresAt}
	return secret, token, nil
}

// ListAPITokens returns the user's tokens, newest first, including revoked ones
// so the UI can show a history.
func (s *Store) ListAPITokens(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, name, prefix, created_at, last_used_at, expires_at, revoked_at
FROM api_tokens WHERE user_id = ? ORDER BY id DESC
`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []APIToken
	for rows.Next() {
		var t APIToken
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// RevokeAPIToken revokes one of the user's tokens. Scoping by user prevents
// revoking someone else's token by id.
func (s *Store) RevokeAPIToken(ctx context.Context, userID, tokenID int64) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE api_tokens SET revoked_at = unixepoch()
WHERE id = ? AND user_id = ? AND revoked_at = 0
`, tokenID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UserForBearerToken resolves an Authorization: Bearer credential (PAT or OAuth
// access token) to its user, enforcing expiry and revocation, and records the
// request's provenance (usage) on the credential. It returns sql.ErrNoRows for
// anything invalid so callers can uniformly answer 401.
func (s *Store) UserForBearerToken(ctx context.Context, token string, usage TokenUsage) (*User, error) {
	now := time.Now().Unix()
	hash := hashToken(token)
	switch {
	case strings.HasPrefix(token, patPrefix):
		var id, userID int64
		err := s.db.QueryRowContext(ctx, `
SELECT id, user_id FROM api_tokens
WHERE token_hash = ? AND revoked_at = 0 AND (expires_at = 0 OR expires_at > ?)
`, hash, now).Scan(&id, &userID)
		if err != nil {
			return nil, err
		}
		_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now, id)
		return s.GetUser(ctx, userID)
	case strings.HasPrefix(token, oauthAccessPrefix):
		var id, userID int64
		err := s.db.QueryRowContext(ctx, `
SELECT id, user_id FROM oauth_tokens
WHERE access_hash = ? AND revoked_at = 0 AND access_expires_at > ?
`, hash, now).Scan(&id, &userID)
		if err != nil {
			return nil, err
		}
		_, _ = s.db.ExecContext(ctx, `
UPDATE oauth_tokens
SET last_used_at = ?, last_used_ip = ?, last_used_ua = ?, use_count = use_count + 1
WHERE id = ?
`, now, clip(usage.IP, 64), clip(usage.UserAgent, 200), id)
		return s.GetUser(ctx, userID)
	default:
		return nil, sql.ErrNoRows
	}
}

// clip bounds attacker-controlled request metadata before it is stored,
// cutting on a rune boundary.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// --- OAuth 2.1 authorization server storage ---

// CreateOAuthClient registers a client (RFC 7591). confidential controls
// whether a client secret is minted; MCP clients are typically public and
// authenticate with PKCE alone.
func (s *Store) CreateOAuthClient(ctx context.Context, name string, redirectURIs []string, confidential bool) (*OAuthClient, string, error) {
	if len(redirectURIs) == 0 {
		return nil, "", errors.New("at least one redirect uri is required")
	}
	uris, err := json.Marshal(redirectURIs)
	if err != nil {
		return nil, "", err
	}
	client := &OAuthClient{
		ClientID:     "cbc_" + randomSecret(),
		Name:         strings.TrimSpace(name),
		RedirectURIs: redirectURIs,
		CreatedAt:    time.Now().Unix(),
	}
	var secret string
	if confidential {
		secret = "cbs_" + randomSecret()
		client.SecretHash = hashToken(secret)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO oauth_clients (client_id, secret_hash, name, redirect_uris)
VALUES (?, ?, ?, ?)
`, client.ClientID, client.SecretHash, client.Name, string(uris))
	if err != nil {
		return nil, "", err
	}
	return client, secret, nil
}

// GetOAuthClient looks a registered client up by id.
func (s *Store) GetOAuthClient(ctx context.Context, clientID string) (*OAuthClient, error) {
	var c OAuthClient
	var uris string
	err := s.db.QueryRowContext(ctx, `
SELECT client_id, secret_hash, name, redirect_uris, created_at
FROM oauth_clients WHERE client_id = ?
`, clientID).Scan(&c.ClientID, &c.SecretHash, &c.Name, &uris, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return nil, err
	}
	return &c, nil
}

// CheckClientSecret verifies a confidential client's secret in constant time.
func (c *OAuthClient) CheckClientSecret(secret string) bool {
	if c.SecretHash == "" {
		return secret == ""
	}
	return subtle.ConstantTimeCompare([]byte(c.SecretHash), []byte(hashToken(secret))) == 1
}

// CreateOAuthCode stores a new single-use authorization code and returns its
// secret value for the redirect back to the client.
func (s *Store) CreateOAuthCode(ctx context.Context, code OAuthCode) (string, error) {
	if code.CodeChallenge == "" {
		return "", errors.New("code challenge is required")
	}
	secret := "cbg_" + randomSecret()
	code.ExpiresAt = time.Now().Add(OAuthCodeTTL).Unix()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO oauth_codes (code_hash, client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, hashToken(secret), code.ClientID, code.UserID, code.RedirectURI, code.CodeChallenge, code.Scope, code.Resource, code.ExpiresAt)
	if err != nil {
		return "", err
	}
	return secret, nil
}

// ConsumeOAuthCode atomically deletes and returns the code, so a code can be
// exchanged exactly once (OAuth 2.1 requirement).
func (s *Store) ConsumeOAuthCode(ctx context.Context, code string) (*OAuthCode, error) {
	row := s.db.QueryRowContext(ctx, `
DELETE FROM oauth_codes WHERE code_hash = ?
RETURNING client_id, user_id, redirect_uri, code_challenge, scope, resource, expires_at
`, hashToken(code))
	var c OAuthCode
	if err := row.Scan(&c.ClientID, &c.UserID, &c.RedirectURI, &c.CodeChallenge, &c.Scope, &c.Resource, &c.ExpiresAt); err != nil {
		return nil, err
	}
	if time.Now().Unix() > c.ExpiresAt {
		return nil, sql.ErrNoRows
	}
	return &c, nil
}

// IssueOAuthTokens mints a fresh access/refresh pair for a granted code.
func (s *Store) IssueOAuthTokens(ctx context.Context, clientID string, userID int64, scope, resource string) (accessToken, refreshToken string, expiresIn int64, err error) {
	access := oauthAccessPrefix + randomSecret()
	refresh := oauthRefreshPrefix + randomSecret()
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO oauth_tokens (client_id, user_id, access_hash, refresh_hash, scope, resource, access_expires_at, refresh_expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
`, clientID, userID, hashToken(access), hashToken(refresh), scope, resource,
		now.Add(OAuthAccessTokenTTL).Unix(), now.Add(OAuthRefreshTokenTTL).Unix())
	if err != nil {
		return "", "", 0, err
	}
	return access, refresh, int64(OAuthAccessTokenTTL.Seconds()), nil
}

// RefreshOAuthTokens rotates a refresh token: the old pair stops working and a
// new pair is returned. Returns sql.ErrNoRows for unknown/expired/revoked
// tokens or a client mismatch.
func (s *Store) RefreshOAuthTokens(ctx context.Context, clientID, refreshToken string) (accessToken, newRefreshToken string, expiresIn int64, err error) {
	if !strings.HasPrefix(refreshToken, oauthRefreshPrefix) {
		return "", "", 0, sql.ErrNoRows
	}
	now := time.Now()
	access := oauthAccessPrefix + randomSecret()
	refresh := oauthRefreshPrefix + randomSecret()
	res, err := s.db.ExecContext(ctx, `
UPDATE oauth_tokens
SET access_hash = ?, refresh_hash = ?, access_expires_at = ?, refresh_expires_at = ?, last_used_at = ?
WHERE refresh_hash = ? AND client_id = ? AND revoked_at = 0 AND refresh_expires_at > ?
`, hashToken(access), hashToken(refresh),
		now.Add(OAuthAccessTokenTTL).Unix(), now.Add(OAuthRefreshTokenTTL).Unix(), now.Unix(),
		hashToken(refreshToken), clientID, now.Unix())
	if err != nil {
		return "", "", 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", "", 0, sql.ErrNoRows
	}
	return access, refresh, int64(OAuthAccessTokenTTL.Seconds()), nil
}

// ListOAuthGrants returns the user's active OAuth grants (client identity plus
// usage provenance) so the settings UI can list and revoke connected agents.
func (s *Store) ListOAuthGrants(ctx context.Context, userID int64) ([]OAuthGrantInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.id, c.name, c.client_id, t.created_at, t.last_used_at, t.last_used_ip, t.last_used_ua,
	t.use_count, t.refresh_expires_at
FROM oauth_tokens t JOIN oauth_clients c ON c.client_id = t.client_id
WHERE t.user_id = ? AND t.revoked_at = 0 AND t.refresh_expires_at > unixepoch()
ORDER BY t.id DESC
`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []OAuthGrantInfo
	for rows.Next() {
		var g OAuthGrantInfo
		if err := rows.Scan(&g.ID, &g.ClientName, &g.ClientID, &g.CreatedAt, &g.LastUsedAt,
			&g.LastUsedIP, &g.LastUsedUA, &g.UseCount, &g.RefreshExpiresAt); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// OAuthGrantInfo is a row in the settings UI's "connected agents" list.
type OAuthGrantInfo struct {
	ID               int64
	ClientName       string
	ClientID         string
	CreatedAt        int64
	LastUsedAt       int64
	LastUsedIP       string
	LastUsedUA       string
	UseCount         int64
	RefreshExpiresAt int64
}

// ClientIDSuffix returns the identifying tail of the client id for display.
// Dynamic registration mints a fresh client per install, so two grants named
// "Claude Code" are only distinguishable by client id (and usage provenance).
func (g OAuthGrantInfo) ClientIDSuffix() string {
	id := strings.TrimPrefix(g.ClientID, "cbc_")
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// RevokeOAuthGrant revokes one of the user's OAuth grants.
func (s *Store) RevokeOAuthGrant(ctx context.Context, userID, grantID int64) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE oauth_tokens SET revoked_at = unixepoch()
WHERE id = ? AND user_id = ? AND revoked_at = 0
`, grantID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListSyncableIdentities returns the code-host identities that carry an access
// token, i.e. the (user, provider) pairs whose repository access can be
// re-synced against the host. Dev and OIDC identities have no token and are
// skipped.
func (s *Store) ListSyncableIdentities(ctx context.Context) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, user_id, provider, provider_user_id, username, updated_at
FROM identities WHERE access_token != '' ORDER BY user_id, provider
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.ID, &i.UserID, &i.Provider, &i.ProviderUserID, &i.Username, &i.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// GrantExistingRepoPermissions grants the user access to repositories that
// already exist in Codebeam, belong to the given provider, and are in the
// accessible set — propagating access a user newly gained on the host without
// importing repos the user never chose to add. Returns the number of new
// grants. A no-op when the accessible set is empty.
func (s *Store) GrantExistingRepoPermissions(ctx context.Context, userID int64, provider string, accessibleHostRepoIDs []string) (int, error) {
	if provider == "" || len(accessibleHostRepoIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(accessibleHostRepoIDs))
	args := make([]any, 0, len(accessibleHostRepoIDs)+2)
	args = append(args, userID, provider)
	for i, id := range accessibleHostRepoIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	res, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO repo_permissions (user_id, repo_id)
SELECT ?, r.id FROM repos r
WHERE r.host_provider = ?
	AND r.host_repo_id IN (`+strings.Join(placeholders, ",")+`)
`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RevokeStalePermissions mirrors a code host's answer for one user: it removes
// the user's access to PRIVATE repositories of the given provider whose host
// repo id is not in accessibleHostRepoIDs (the set the host currently says the
// user can reach). It intentionally never touches public repos — those are
// world-readable and may have been added deliberately by URL — nor other
// providers, local repos, or other users. Returns the number of permissions
// revoked. Callers must only invoke this after a COMPLETE host listing, or a
// truncated list would wrongly revoke access.
func (s *Store) RevokeStalePermissions(ctx context.Context, userID int64, provider string, accessibleHostRepoIDs []string) (int, error) {
	if provider == "" {
		return 0, errors.New("provider is required")
	}
	// Build a parameterized NOT IN (...) list; an empty accessible set revokes
	// every private permission for this provider (the user lost all access).
	placeholders := make([]string, len(accessibleHostRepoIDs))
	args := make([]any, 0, len(accessibleHostRepoIDs)+2)
	args = append(args, userID, provider)
	for i, id := range accessibleHostRepoIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	notIn := ""
	if len(placeholders) > 0 {
		notIn = " AND r.host_repo_id NOT IN (" + strings.Join(placeholders, ",") + ")"
	}
	res, err := s.db.ExecContext(ctx, `
DELETE FROM repo_permissions
WHERE user_id = ?
	AND repo_id IN (
		SELECT r.id FROM repos r
		WHERE r.host_provider = ?
			AND r.private = 1`+notIn+`
	)
`, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomSecret() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(buf[:])
}

// displayPrefix returns the identifying head of a token for list views.
func displayPrefix(secret string) string {
	if len(secret) > 12 {
		return secret[:12]
	}
	return secret
}
