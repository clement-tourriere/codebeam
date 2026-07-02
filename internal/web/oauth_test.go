package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/store"
)

const testBaseURL = "http://codebeam.test"

// newOAuthTestServer is newTestAPIServer plus the pieces the OAuth flow needs:
// a base URL for metadata and real templates for the consent page.
func newOAuthTestServer(t *testing.T, ctx context.Context) (*Server, *store.User, *store.Repo) {
	t.Helper()
	srv, user, repo := newTestAPIServer(t, ctx)
	srv.cfg.BaseURL = testBaseURL
	full, err := New(config.Config{TemplateGlob: filepath.Join("..", "..", "templates", "*.html"), SessionSecret: "test-secret", GitLabBaseURL: "https://gitlab.com"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.templates = full.templates
	return srv, user, repo
}

func do(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.route(rr, req)
	return rr
}

func registerTestClient(t *testing.T, srv *Server, redirectURI string) string {
	t.Helper()
	body := `{"redirect_uris":["` + redirectURI + `"],"client_name":"Claude Code","token_endpoint_auth_method":"none"}`
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := do(srv, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ClientID == "" || resp.ClientSecret != "" {
		t.Fatalf("public client registration returned %+v", resp)
	}
	return resp.ClientID
}

func pkcePair() (verifier, challenge string) {
	verifier = "test-verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeParams(clientID, redirectURI, challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {"xyz-state"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {testBaseURL + "/mcp"},
	}
}

func TestOAuthDiscoveryMetadata(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := newOAuthTestServer(t, ctx)

	rr := do(srv, httptest.NewRequest(http.MethodGet, protectedResourceMetadataPath, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("prm status=%d", rr.Code)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&prm); err != nil {
		t.Fatal(err)
	}
	if prm.Resource != testBaseURL+"/mcp" || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != testBaseURL {
		t.Fatalf("unexpected protected resource metadata: %+v", prm)
	}

	rr = do(srv, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("as metadata status=%d", rr.Code)
	}
	var as struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RegistrationEndpoint  string   `json:"registration_endpoint"`
		CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&as); err != nil {
		t.Fatal(err)
	}
	if as.Issuer != testBaseURL || as.TokenEndpoint != testBaseURL+"/oauth/token" ||
		as.RegistrationEndpoint != testBaseURL+"/oauth/register" ||
		len(as.CodeChallengeMethods) != 1 || as.CodeChallengeMethods[0] != "S256" {
		t.Fatalf("unexpected AS metadata: %+v", as)
	}
}

func TestOAuthFullAuthorizationCodeFlow(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newOAuthTestServer(t, ctx)
	if err := srv.indexer.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	const redirectURI = "http://127.0.0.1:33418/callback"
	clientID := registerTestClient(t, srv, redirectURI)
	verifier, challenge := pkcePair()
	params := authorizeParams(clientID, redirectURI, challenge)

	// Consent page renders for the signed-in user.
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := do(srv, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Claude Code") {
		t.Fatalf("consent page status=%d body=%s", rr.Code, rr.Body.String())
	}

	// Approving redirects back with a code and the original state.
	form := url.Values{"action": {"approve"}}
	for key := range params {
		form.Set(key, params.Get(key))
	}
	req = httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addSessionCookie(t, srv, req, user.ID)
	rr = do(srv, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("approve status=%d body=%s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), redirectURI) {
		t.Fatalf("unexpected redirect %q", rr.Header().Get("Location"))
	}
	code := loc.Query().Get("code")
	if code == "" || loc.Query().Get("state") != "xyz-state" {
		t.Fatalf("redirect missing code/state: %q", loc.String())
	}

	// Exchange the code with the PKCE verifier.
	exchange := func(codeValue, verifierValue string) *httptest.ResponseRecorder {
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {codeValue},
			"redirect_uri":  {redirectURI},
			"client_id":     {clientID},
			"code_verifier": {verifierValue},
			"resource":      {testBaseURL + "/mcp"},
		}
		req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return do(srv, req)
	}
	rr = exchange(code, verifier)
	if rr.Code != http.StatusOK {
		t.Fatalf("token status=%d body=%s", rr.Code, rr.Body.String())
	}
	var tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&tokens); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tokens.AccessToken, "cbo_") || !strings.HasPrefix(tokens.RefreshToken, "cbr_") || tokens.TokenType != "Bearer" || tokens.ExpiresIn <= 0 {
		t.Fatalf("unexpected token response: %+v", tokens)
	}

	// The access token works on the JSON API…
	req = httptest.NewRequest(http.MethodGet, "/api/search?q=UniqueNeedle", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	rr = do(srv, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "main.go") {
		t.Fatalf("api with oauth token status=%d body=%s", rr.Code, rr.Body.String())
	}

	// …and on the MCP endpoint.
	rr = mcpPost(t, srv, tokens.AccessToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("mcp initialize status=%d body=%s", rr.Code, rr.Body.String())
	}

	// The code is single-use.
	if rr := exchange(code, verifier); rr.Code != http.StatusBadRequest {
		t.Fatalf("code replay status=%d, want 400", rr.Code)
	}

	// Refresh rotates the pair and kills the old access token.
	form = url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens.RefreshToken}, "client_id": {clientID}}
	req = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = do(srv, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", rr.Code, rr.Body.String())
	}
	var rotated struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/search?q=UniqueNeedle", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	if rr := do(srv, req); rr.Code != http.StatusUnauthorized {
		t.Fatalf("old access token after rotation status=%d, want 401", rr.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/search?q=UniqueNeedle", nil)
	req.Header.Set("Authorization", "Bearer "+rotated.AccessToken)
	if rr := do(srv, req); rr.Code != http.StatusOK {
		t.Fatalf("rotated access token status=%d", rr.Code)
	}
}

func TestOAuthTokenRejectsWrongVerifier(t *testing.T) {
	ctx := context.Background()
	srv, user, _ := newOAuthTestServer(t, ctx)
	const redirectURI = "http://localhost:9999/cb"
	clientID := registerTestClient(t, srv, redirectURI)
	_, challenge := pkcePair()
	form := url.Values{"action": {"approve"}}
	for key, values := range authorizeParams(clientID, redirectURI, challenge) {
		form.Set(key, values[0])
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addSessionCookie(t, srv, req, user.ID)
	rr := do(srv, req)
	loc, _ := url.Parse(rr.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect %q", rr.Header().Get("Location"))
	}

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {"completely-wrong-verifier-value-aaaaaaaaaaa"},
	}
	req = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = do(srv, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_grant") {
		t.Fatalf("wrong verifier status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOAuthAuthorizeValidation(t *testing.T) {
	ctx := context.Background()
	srv, user, _ := newOAuthTestServer(t, ctx)
	const redirectURI = "http://localhost:9999/cb"
	clientID := registerTestClient(t, srv, redirectURI)
	_, challenge := pkcePair()

	// Unregistered redirect URI: hard 400, never a redirect.
	params := authorizeParams(clientID, "http://evil.example/cb", challenge)
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil)
	addSessionCookie(t, srv, req, user.ID)
	if rr := do(srv, req); rr.Code != http.StatusBadRequest {
		t.Fatalf("unregistered redirect status=%d, want 400", rr.Code)
	}

	// Missing PKCE: error is delivered to the registered redirect URI.
	params = authorizeParams(clientID, redirectURI, challenge)
	params.Del("code_challenge")
	req = httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := do(srv, req)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "error=invalid_request") {
		t.Fatalf("missing pkce: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}

	// Foreign resource: refused with invalid_target.
	params = authorizeParams(clientID, redirectURI, challenge)
	params.Set("resource", "https://other-server.example/mcp")
	req = httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil)
	addSessionCookie(t, srv, req, user.ID)
	rr = do(srv, req)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "error=invalid_target") {
		t.Fatalf("foreign resource: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}

	// Anonymous users are sent to login and return after signing in.
	params = authorizeParams(clientID, redirectURI, challenge)
	req = httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+params.Encode(), nil)
	rr = do(srv, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous authorize: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
	foundNext := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == loginNextCookie && c.Value != "" {
			foundNext = true
		}
	}
	if !foundNext {
		t.Fatal("anonymous authorize should set the login-next cookie")
	}
}

func mcpPost(t *testing.T, srv *Server, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return do(srv, req)
}

func TestMCPEndpointRequiresAuth(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := newOAuthTestServer(t, ctx)

	rr := mcpPost(t, srv, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.Contains(got, protectedResourceMetadataPath) {
		t.Fatalf("WWW-Authenticate should point at resource metadata, got %q", got)
	}
}

func TestMCPEndpointScopesReposToUser(t *testing.T) {
	ctx := context.Background()
	srv, owner, repo := newOAuthTestServer(t, ctx)
	if err := srv.indexer.Reindex(ctx, repo.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	ownerPAT, _, err := srv.store.CreateAPIToken(ctx, owner.ID, "owner", 0)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := srv.store.UpsertUserIdentity(ctx, "github", "999", "outsider", "out@x.test", "Outsider", "", "")
	if err != nil {
		t.Fatal(err)
	}
	outsiderPAT, _, err := srv.store.CreateAPIToken(ctx, outsider.ID, "outsider", 0)
	if err != nil {
		t.Fatal(err)
	}

	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_repos"}}`
	rr := mcpPost(t, srv, ownerPAT, call)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), repo.FullName) {
		t.Fatalf("owner list_repos status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = mcpPost(t, srv, outsiderPAT, call)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), repo.FullName) {
		t.Fatalf("outsider must not see the repo: %s", rr.Body.String())
	}

	// Notifications are accepted with 202 and no body.
	rr = mcpPost(t, srv, ownerPAT, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rr.Code != http.StatusAccepted || rr.Body.Len() != 0 {
		t.Fatalf("notification status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestCSRFGuardRejectsForeignOrigin(t *testing.T) {
	ctx := context.Background()
	srv, admin, _ := newOAuthTestServer(t, ctx)

	// A cross-origin POST carrying the session cookie is rejected.
	req := httptest.NewRequest(http.MethodPost, "/settings/users/role", strings.NewReader("user_id=1&role=member"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	addSessionCookie(t, srv, req, admin.ID)
	if rr := do(srv, req); rr.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin POST status=%d, want 403", rr.Code)
	}

	// A same-origin POST (Origin matches BaseURL) is allowed through the guard.
	req = httptest.NewRequest(http.MethodPost, "/settings/tokens", strings.NewReader("name=ci"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", testBaseURL)
	addSessionCookie(t, srv, req, admin.ID)
	if rr := do(srv, req); rr.Code != http.StatusOK {
		t.Fatalf("same-origin POST status=%d, want 200", rr.Code)
	}

	// The bearer/PKCE machine endpoints stay callable cross-origin (they don't
	// rely on the cookie): a foreign Origin must not 403 the token endpoint.
	req = httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader("grant_type=authorization_code"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://claude.ai")
	if rr := do(srv, req); rr.Code == http.StatusForbidden {
		t.Fatal("token endpoint must not be blocked by the CSRF origin guard")
	}
}

func TestDevLoginRejectsGET(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := newOAuthTestServer(t, ctx)
	srv.cfg.DevLogin = true

	// A drive-by GET must not create a session (passwordless login is POST-only).
	rr := do(srv, httptest.NewRequest(http.MethodGet, "/dev-login", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /dev-login status=%d, want 405", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("GET /dev-login must not set a session cookie")
		}
	}
}

func TestRBACAdminGating(t *testing.T) {
	ctx := context.Background()
	srv, admin, _ := newOAuthTestServer(t, ctx)
	if !admin.IsAdmin() {
		t.Fatalf("first user should be admin, got %q", admin.Role)
	}
	member, err := srv.store.UpsertUserIdentity(ctx, "github", "2", "bob", "bob@x.test", "Bob", "", "")
	if err != nil {
		t.Fatal(err)
	}

	post := func(userID int64, path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		addSessionCookie(t, srv, req, userID)
		return do(srv, req)
	}

	settingsForm := url.Values{"auto_index_remote": {"1"}, "remote_refresh_interval": {"30m"}}
	if rr := post(member.ID, "/settings", settingsForm); rr.Code != http.StatusForbidden {
		t.Fatalf("member POST /settings status=%d, want 403", rr.Code)
	}
	if rr := post(admin.ID, "/settings", settingsForm); rr.Code != http.StatusSeeOther {
		t.Fatalf("admin POST /settings status=%d, want 303", rr.Code)
	}
	if rr := post(member.ID, "/repos/local", url.Values{"path": {"/tmp"}}); rr.Code != http.StatusForbidden {
		t.Fatalf("member POST /repos/local status=%d, want 403", rr.Code)
	}
	if rr := post(member.ID, "/settings/users/role", url.Values{"user_id": {"1"}, "role": {"member"}}); rr.Code != http.StatusForbidden {
		t.Fatalf("member role change status=%d, want 403", rr.Code)
	}
	rr := post(admin.ID, "/settings/users/role", url.Values{"user_id": {strconv.FormatInt(member.ID, 10)}, "role": {"admin"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("admin role change status=%d body=%s", rr.Code, rr.Body.String())
	}
	promoted, err := srv.store.GetUser(ctx, member.ID)
	if err != nil || !promoted.IsAdmin() {
		t.Fatalf("member should be promoted: %v %+v", err, promoted)
	}

	// Every user manages their own tokens; the created secret is shown once.
	rr = post(member.ID, "/settings/tokens", url.Values{"name": {"ci"}})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "cbp_") {
		t.Fatalf("token create status=%d body missing secret", rr.Code)
	}
}
