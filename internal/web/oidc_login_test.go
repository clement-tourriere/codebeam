package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/codehost"
	"github.com/ctourriere/codebeam/internal/oidc"
	"github.com/ctourriere/codebeam/internal/store"
)

// fakeIdP is a minimal OpenID provider: discovery + token endpoint. The token
// endpoint validates PKCE against the challenge captured from the authorize
// redirect and mints an ID token for `email`.
type fakeIdP struct {
	server        *httptest.Server
	clientID      string
	email         string
	emailVerified bool
	nonce         string // captured by the test from the authorize URL
	challenge     string // captured by the test from the authorize URL
	badNonce      bool
	t             *testing.T
}

func newFakeIdP(t *testing.T, clientID, email string) *fakeIdP {
	idp := &fakeIdP{clientID: clientID, email: email, emailVerified: true, t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 idp.server.URL,
			"authorization_endpoint": idp.server.URL + "/authorize",
			"token_endpoint":         idp.server.URL + "/token",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "fake-code" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		if oidc.S256Challenge(r.PostForm.Get("code_verifier")) != idp.challenge {
			http.Error(w, "pkce mismatch", http.StatusBadRequest)
			return
		}
		if _, _, ok := r.BasicAuth(); !ok {
			http.Error(w, "missing client auth", http.StatusUnauthorized)
			return
		}
		nonce := idp.nonce
		if idp.badNonce {
			nonce = "tampered-nonce"
		}
		claims := map[string]any{
			"iss":            idp.server.URL,
			"aud":            idp.clientID,
			"exp":            time.Now().Add(time.Hour).Unix(),
			"nonce":          nonce,
			"sub":            "sso-user-1",
			"email":          idp.email,
			"email_verified": idp.emailVerified,
			"name":           "Sso User",
		}
		payload, _ := json.Marshal(claims)
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
		idToken := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fakesignature"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-1",
			"token_type":   "Bearer",
			"id_token":     idToken,
		})
	})
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

// newOIDCLoginServer wires a test web server against the fake IdP.
func newOIDCLoginServer(t *testing.T, ctx context.Context, idp *fakeIdP, allowedDomains []string) *Server {
	t.Helper()
	srv, _, _ := newOAuthTestServer(t, ctx)
	srv.cfg.OIDCIssuer = idp.server.URL
	srv.cfg.OIDCClientID = idp.clientID
	srv.cfg.OIDCClientSecret = "s3cret"
	srv.cfg.OIDCName = "Okta"
	srv.cfg.OIDCAllowedDomains = allowedDomains
	srv.oidc = oidc.New(oidc.Config{Issuer: idp.server.URL, ClientID: idp.clientID, ClientSecret: "s3cret"})
	// The API-server fixture skips codehost clients; the login page needs them.
	srv.hosts = map[string]*codehost.Client{
		string(codehost.GitHub): codehost.New(codehost.OAuthConfig{Provider: codehost.GitHub}),
		string(codehost.GitLab): codehost.New(codehost.OAuthConfig{Provider: codehost.GitLab}),
	}
	return srv
}

// startSSO drives /auth/oidc and returns the callback request primed with the
// auth cookie and the state from the provider redirect.
func startSSO(t *testing.T, srv *Server, idp *fakeIdP) *http.Request {
	t.Helper()
	rr := do(srv, httptest.NewRequest(http.MethodGet, "/auth/oidc", nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("sso start status=%d body=%s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), idp.server.URL+"/authorize") {
		t.Fatalf("unexpected authorize redirect %q", rr.Header().Get("Location"))
	}
	q := loc.Query()
	for _, param := range []string{"client_id", "redirect_uri", "state", "nonce", "code_challenge", "scope"} {
		if q.Get(param) == "" {
			t.Fatalf("authorize URL missing %s: %q", param, loc.String())
		}
	}
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != idp.clientID {
		t.Fatalf("unexpected authorize params: %v", q)
	}
	if q.Get("redirect_uri") != testBaseURL+"/auth/oidc/callback" {
		t.Fatalf("redirect_uri=%q", q.Get("redirect_uri"))
	}
	idp.nonce = q.Get("nonce")
	idp.challenge = q.Get("code_challenge")

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=fake-code&state="+url.QueryEscape(q.Get("state")), nil)
	for _, c := range rr.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func TestOIDCLoginFullFlow(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t, "cb-client", "alice@acme.com")
	srv := newOIDCLoginServer(t, ctx, idp, nil)

	rr := do(srv, startSSO(t, srv, idp))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/repos" {
		t.Fatalf("callback status=%d location=%q body=%s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("callback did not set a session cookie")
	}

	// The session works, and the provisioned user carries the token claims.
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(session)
	if rr := do(srv, req); rr.Code != http.StatusOK {
		t.Fatalf("settings with SSO session status=%d", rr.Code)
	}
	users, err := srv.store.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ssoUser *store.User
	for i := range users {
		if users[i].Email == "alice@acme.com" {
			ssoUser = &users[i]
		}
	}
	if ssoUser == nil {
		t.Fatalf("SSO user not provisioned: %+v", users)
	}
	// The dev user from the fixture already owns the instance; SSO users join
	// as members.
	if ssoUser.IsAdmin() {
		t.Fatalf("SSO user should be a member, got %q", ssoUser.Role)
	}

	// Login page offers the configured SSO button.
	rr = do(srv, httptest.NewRequest(http.MethodGet, "/login", nil))
	if !strings.Contains(rr.Body.String(), "Continue with Okta") {
		t.Fatal("login page missing the SSO button")
	}
}

func TestOIDCLoginRejectsDisallowedDomain(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t, "cb-client", "mallory@evil.example")
	srv := newOIDCLoginServer(t, ctx, idp, []string{"acme.com"})

	rr := do(srv, startSSO(t, srv, idp))
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "/login?error=") {
		t.Fatalf("disallowed domain: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("no session may be created for a disallowed domain")
		}
	}
}

func TestOIDCLoginRejectsUnverifiedEmailWhenDomainGated(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t, "cb-client", "alice@acme.com")
	idp.emailVerified = false // provider does not confirm the address
	srv := newOIDCLoginServer(t, ctx, idp, []string{"acme.com"})

	rr := do(srv, startSSO(t, srv, idp))
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "/login?error=") {
		t.Fatalf("unverified email under domain gate: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("no session may be created for an unverified email under a domain gate")
		}
	}
}

func TestOIDCLoginAllowsVerifiedEmailInDomain(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t, "cb-client", "alice@acme.com") // emailVerified defaults true
	srv := newOIDCLoginServer(t, ctx, idp, []string{"acme.com"})

	rr := do(srv, startSSO(t, srv, idp))
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/repos" {
		t.Fatalf("verified email in allowed domain should sign in: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestOIDCLoginRejectsStateMismatch(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t, "cb-client", "alice@acme.com")
	srv := newOIDCLoginServer(t, ctx, idp, nil)

	req := startSSO(t, srv, idp)
	tampered := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=fake-code&state=wrong", nil)
	for _, c := range req.Cookies() {
		tampered.AddCookie(c)
	}
	rr := do(srv, tampered)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Fatalf("state mismatch: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestOIDCLoginRejectsNonceMismatch(t *testing.T) {
	ctx := context.Background()
	idp := newFakeIdP(t, "cb-client", "alice@acme.com")
	idp.badNonce = true
	srv := newOIDCLoginServer(t, ctx, idp, nil)

	rr := do(srv, startSSO(t, srv, idp))
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "error=") {
		t.Fatalf("nonce mismatch: status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("no session may be created on nonce mismatch")
		}
	}
}
