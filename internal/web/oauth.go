// OAuth 2.1 authorization server + protected-resource metadata, implementing
// the MCP authorization spec (2025-06-18): RFC 9728 protected resource
// metadata, RFC 8414 authorization server metadata, RFC 7591 dynamic client
// registration, PKCE-only authorization code flow, and RFC 8707 resource
// indicators. Hand-rolled on the stdlib like the rest of Codebeam's auth — the
// surface MCP clients need is small and well-specified.
package web

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/ctourriere/codebeam/internal/store"
)

const (
	// protectedResourceMetadataPath is the RFC 9728 path-aware metadata URL for
	// the /mcp resource; WWW-Authenticate challenges point here.
	protectedResourceMetadataPath = "/.well-known/oauth-protected-resource/mcp"
	oauthScope                    = "codebeam"
)

// mcpResource is the canonical RFC 8707 resource identifier of this instance's
// MCP endpoint — the audience access tokens are bound to.
func (s *Server) mcpResource() string {
	return s.cfg.BaseURL + "/mcp"
}

// oauthCORS makes the discovery/registration/token endpoints callable from
// browser-based MCP clients; they are public endpoints secured by PKCE and
// client credentials, not cookies.
func oauthCORS(w http.ResponseWriter, r *http.Request) (handled bool) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// handleProtectedResourceMetadata serves RFC 9728 metadata telling MCP clients
// which authorization server protects the /mcp resource.
func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	apiJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.mcpResource(),
		"authorization_servers":    []string{s.cfg.BaseURL},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{oauthScope},
	})
}

// handleAuthServerMetadata serves RFC 8414 authorization server metadata.
func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	base := s.cfg.BaseURL
	apiJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{oauthScope},
	})
}

// handleOAuthRegister implements RFC 7591 dynamic client registration, which
// lets an MCP client (Claude Code, Cursor, ...) register itself before sending
// the user through the consent flow — no manual client setup.
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "request body is not valid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, raw := range req.RedirectURIs {
		if !validRedirectURI(raw) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect uris must be https, a custom app scheme, or http on localhost")
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "MCP client"
	}
	confidential := req.TokenEndpointAuthMethod == "client_secret_post" || req.TokenEndpointAuthMethod == "client_secret_basic"
	client, secret, err := s.store.CreateOAuthClient(r.Context(), name, req.RedirectURIs, confidential)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	authMethod := "none"
	if confidential {
		authMethod = req.TokenEndpointAuthMethod
	}
	resp := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        client.CreatedAt,
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": authMethod,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      oauthScope,
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	apiJSON(w, http.StatusCreated, resp)
}

// validRedirectURI accepts https URLs, custom native-app schemes, and plain
// http only on loopback hosts — the OAuth 2.1 rules for public clients.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	default:
		// Custom scheme (e.g. cursor://callback) used by native apps.
		return !strings.ContainsAny(u.Scheme, " ")
	}
}

// OAuthConsentData feeds the consent template.
type OAuthConsentData struct {
	User       *store.User
	ClientName string
	Resource   string
	Params     map[string]string
}

// handleOAuthAuthorize runs the authorization endpoint: GET validates the
// request and renders the consent page; POST records the user's decision and
// redirects back to the client with a single-use code.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	get := func(key string) string { return strings.TrimSpace(r.Form.Get(key)) }

	// Client and redirect URI must be valid before anything is redirected —
	// never bounce a user to an unverified location (RFC 6749 §4.1.2.1).
	client, err := s.store.GetOAuthClient(r.Context(), get("client_id"))
	if err != nil {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	redirectURI := get("redirect_uri")
	if !containsString(client.RedirectURIs, redirectURI) {
		http.Error(w, "redirect_uri is not registered for this client", http.StatusBadRequest)
		return
	}
	redirectBack := func(params url.Values) {
		if state := get("state"); state != "" {
			params.Set("state", state)
		}
		sep := "?"
		if strings.Contains(redirectURI, "?") {
			sep = "&"
		}
		http.Redirect(w, r, redirectURI+sep+params.Encode(), http.StatusSeeOther)
	}
	fail := func(code, description string) {
		redirectBack(url.Values{"error": {code}, "error_description": {description}})
	}

	if get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	// PKCE is mandatory for every client (OAuth 2.1 / MCP requirement).
	challenge := get("code_challenge")
	if challenge == "" || get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	// RFC 8707: the token is bound to this instance's MCP resource. A request
	// for any other audience is refused rather than silently rebound.
	resource := get("resource")
	if resource == "" {
		resource = s.mcpResource()
	}
	if !s.acceptableResource(resource) {
		fail("invalid_target", "unknown resource "+resource)
		return
	}

	user, ok := s.currentUser(r)
	if !ok {
		// Remember where to come back to after the user signs in.
		s.setSignedCookie(w, loginNextCookie, r.URL.RequestURI(), loginNextTTL)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodGet {
		// The consent page must never render inside a frame (clickjacking).
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		s.render(w, http.StatusOK, "page_oauth_consent", OAuthConsentData{
			User:       user,
			ClientName: client.Name,
			Resource:   resource,
			Params: map[string]string{
				"response_type":         get("response_type"),
				"client_id":             get("client_id"),
				"redirect_uri":          redirectURI,
				"state":                 get("state"),
				"code_challenge":        challenge,
				"code_challenge_method": "S256",
				"scope":                 get("scope"),
				"resource":              resource,
			},
		})
		return
	}

	// POST: the consent decision. The session cookie is SameSite=Lax, so a
	// cross-site POST cannot carry it — the decision is same-origin by
	// construction.
	if get("action") != "approve" {
		fail("access_denied", "the user denied the request")
		return
	}
	code, err := s.store.CreateOAuthCode(r.Context(), store.OAuthCode{
		ClientID:      client.ClientID,
		UserID:        user.ID,
		RedirectURI:   redirectURI,
		CodeChallenge: challenge,
		Scope:         oauthScope,
		Resource:      resource,
	})
	if err != nil {
		fail("server_error", err.Error())
		return
	}
	redirectBack(url.Values{"code": {code}})
}

// acceptableResource reports whether a requested RFC 8707 resource identifies
// this instance (the /mcp endpoint or the instance base URL).
func (s *Server) acceptableResource(resource string) bool {
	trimmed := strings.TrimRight(resource, "/")
	return trimmed == s.mcpResource() || trimmed == s.cfg.BaseURL
}

// handleOAuthToken exchanges authorization codes and refresh tokens for access
// tokens.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if oauthCORS(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "request body is not valid form data")
		return
	}
	get := func(key string) string { return strings.TrimSpace(r.PostForm.Get(key)) }

	clientID, clientSecret := get("client_id"), get("client_secret")
	if basicID, basicSecret, ok := r.BasicAuth(); ok {
		// client_secret_basic: credentials are URL-encoded per RFC 6749 §2.3.1.
		if id, err := url.QueryUnescape(basicID); err == nil && id != "" {
			clientID = id
		}
		if secret, err := url.QueryUnescape(basicSecret); err == nil {
			clientSecret = secret
		}
	}
	client, err := s.store.GetOAuthClient(r.Context(), clientID)
	if err != nil {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}
	if !client.CheckClientSecret(clientSecret) {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	switch get("grant_type") {
	case "authorization_code":
		code, err := s.store.ConsumeOAuthCode(r.Context(), get("code"))
		if err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
			return
		}
		if code.ClientID != client.ClientID || code.RedirectURI != get("redirect_uri") {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code was issued to a different client or redirect uri")
			return
		}
		if !verifyPKCE(code.CodeChallenge, get("code_verifier")) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		if resource := get("resource"); resource != "" && strings.TrimRight(resource, "/") != strings.TrimRight(code.Resource, "/") {
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the authorization request")
			return
		}
		access, refresh, expiresIn, err := s.store.IssueOAuthTokens(r.Context(), client.ClientID, code.UserID, code.Scope, code.Resource)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		s.writeTokenResponse(w, access, refresh, expiresIn, code.Scope)
	case "refresh_token":
		access, refresh, expiresIn, err := s.store.RefreshOAuthTokens(r.Context(), client.ClientID, get("refresh_token"))
		if errors.Is(err, sql.ErrNoRows) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid, expired, or revoked")
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
			return
		}
		s.writeTokenResponse(w, access, refresh, expiresIn, oauthScope)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) writeTokenResponse(w http.ResponseWriter, access, refresh string, expiresIn int64, scope string) {
	w.Header().Set("Cache-Control", "no-store")
	apiJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// verifyPKCE checks code_verifier against the stored S256 challenge.
func verifyPKCE(challenge, verifier string) bool {
	if verifier == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	apiJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
