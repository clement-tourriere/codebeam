// Package oidc implements a minimal OpenID Connect relying party for SSO
// login: provider discovery, the authorization-code flow with PKCE, state and
// nonce, and ID-token claim validation. One generic implementation covers
// Okta, Microsoft Entra ID, Google Workspace, Keycloak, and any other
// spec-compliant provider. Hand-rolled on the stdlib like the rest of
// Codebeam's auth.
package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config identifies Codebeam at the OpenID provider.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scopes is the space-separated scope list; "openid profile email" when empty.
	Scopes string
}

// Identity is what a completed login yields.
type Identity struct {
	Subject string
	Email   string
	// EmailVerified reflects the provider's email_verified claim. It is only
	// true when the provider explicitly asserts the email is verified; callers
	// that gate on the email domain must require it.
	EmailVerified bool
	Name          string
	Picture       string
}

// Client is a relying party for one provider. Discovery is fetched lazily and
// cached for the process lifetime.
type Client struct {
	cfg  Config
	http *http.Client

	mu   sync.Mutex
	disc *discovery
}

type discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second}}
}

// Configured reports whether SSO login can be offered.
func (c *Client) Configured() bool {
	return c != nil && c.cfg.Issuer != "" && c.cfg.ClientID != "" && c.cfg.ClientSecret != ""
}

func (c *Client) scopes() string {
	if strings.TrimSpace(c.cfg.Scopes) != "" {
		return c.cfg.Scopes
	}
	return "openid profile email"
}

// discover fetches and caches {issuer}/.well-known/openid-configuration.
func (c *Client) discover(ctx context.Context) (*discovery, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disc != nil {
		return c.disc, nil
	}
	// The whole flow (including the unsigned id_token trust in
	// validateIDToken) rests on TLS to the provider, so a non-HTTPS issuer is
	// refused outright — loopback excepted for local development.
	if !isSecureURL(c.cfg.Issuer) {
		return nil, fmt.Errorf("OIDC issuer must be an https URL (got %q)", c.cfg.Issuer)
	}
	well := strings.TrimRight(c.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, well, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	defer resp.Body.Close() // nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC discovery: %s returned %d", well, resp.StatusCode)
	}
	var disc discovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&disc); err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	if disc.AuthorizationEndpoint == "" || disc.TokenEndpoint == "" {
		return nil, errors.New("OIDC discovery: provider metadata is missing endpoints")
	}
	// The metadata's self-declared issuer must match what we configured
	// (OIDC Discovery §4.3), so a rogue document can't point us at endpoints
	// for a different issuer than the one we trust.
	if strings.TrimRight(disc.Issuer, "/") != strings.TrimRight(c.cfg.Issuer, "/") {
		return nil, fmt.Errorf("OIDC discovery: issuer %q does not match configured %q", disc.Issuer, c.cfg.Issuer)
	}
	c.disc = &disc
	return c.disc, nil
}

// isSecureURL reports whether raw is an https URL, or an http loopback URL
// (allowed so local development against a dev IdP works).
func isSecureURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	return false
}

// AuthCodeURL builds the provider authorization URL. verifier is the PKCE
// code verifier; its S256 challenge is embedded in the URL.
func (c *Client) AuthCodeURL(ctx context.Context, state, nonce, verifier, redirectURI string) (string, error) {
	disc, err := c.discover(ctx)
	if err != nil {
		return "", err
	}
	params := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {c.scopes()},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {S256Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(disc.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return disc.AuthorizationEndpoint + sep + params.Encode(), nil
}

// Exchange trades the authorization code for tokens and returns the validated
// identity. nonce must match the value sent in AuthCodeURL.
func (c *Client) Exchange(ctx context.Context, code, verifier, nonce, redirectURI string) (*Identity, error) {
	disc, err := c.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, disc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic with URL-encoded credentials (RFC 6749 §2.3.1) — the
	// default auth method every major provider accepts.
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC token exchange: %w", err)
	}
	defer resp.Body.Close() // nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC token exchange failed (%d): %s", resp.StatusCode, truncate(string(body), 200))
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("OIDC token exchange: %w", err)
	}
	if tokens.IDToken == "" {
		return nil, errors.New("OIDC token exchange: response is missing id_token")
	}
	claims, err := c.validateIDToken(tokens.IDToken, disc.Issuer, nonce)
	if err != nil {
		return nil, err
	}
	identity := &Identity{Subject: claims.Sub, Email: claims.Email, EmailVerified: claims.EmailVerified, Name: claims.Name, Picture: claims.Picture}
	if identity.Email == "" && disc.UserinfoEndpoint != "" && tokens.AccessToken != "" {
		c.fillFromUserinfo(ctx, identity, disc.UserinfoEndpoint, tokens.AccessToken)
	}
	if identity.Subject == "" {
		return nil, errors.New("OIDC: id_token has no subject")
	}
	return identity, nil
}

type idTokenClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"`
	Exp           int64           `json:"exp"`
	Nonce         string          `json:"nonce"`
	Sub           string          `json:"sub"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
	Name          string          `json:"name"`
	Picture       string          `json:"picture"`
}

// validateIDToken checks the issuer, audience, expiry, and nonce claims. The
// token signature is deliberately not verified: the token was just received
// directly from the token endpoint over TLS with client authentication, and
// OIDC Core §3.1.3.7 permits TLS server validation in place of a signature
// check on that path.
func (c *Client) validateIDToken(raw, issuer, nonce string) (*idTokenClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("OIDC: id_token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("OIDC: id_token payload: %w", err)
	}
	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("OIDC: id_token claims: %w", err)
	}
	if strings.TrimRight(claims.Iss, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("OIDC: id_token issuer %q does not match %q", claims.Iss, issuer)
	}
	if !audienceContains(claims.Aud, c.cfg.ClientID) {
		return nil, errors.New("OIDC: id_token audience does not include this client")
	}
	if claims.Exp <= time.Now().Unix() {
		return nil, errors.New("OIDC: id_token is expired")
	}
	if claims.Nonce != nonce {
		return nil, errors.New("OIDC: id_token nonce mismatch")
	}
	return &claims, nil
}

// fillFromUserinfo best-effort completes missing profile fields; failures are
// ignored because the login already has a validated subject.
func (c *Client) fillFromUserinfo(ctx context.Context, identity *Identity, endpoint, accessToken string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close() // nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return
	}
	var info struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return
	}
	if identity.Email == "" {
		identity.Email = info.Email
		identity.EmailVerified = info.EmailVerified
	}
	if identity.Name == "" {
		identity.Name = info.Name
	}
	if identity.Picture == "" {
		identity.Picture = info.Picture
	}
}

func audienceContains(raw json.RawMessage, clientID string) bool {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == clientID
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, aud := range many {
			if aud == clientID {
				return true
			}
		}
	}
	return false
}

// S256Challenge derives the PKCE code challenge from a verifier.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
