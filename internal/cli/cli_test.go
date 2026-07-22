package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://localhost:8080/", want: "http://localhost:8080"},
		{in: "localhost:8080", want: "http://localhost:8080"},
		{in: "127.0.0.1:9999", want: "http://127.0.0.1:9999"},
		{in: "codebeam.acme.dev", want: "https://codebeam.acme.dev"},
		{in: "https://codebeam.acme.dev/", want: "https://codebeam.acme.dev"},
		{in: "  ", wantErr: true},
		{in: "ftp://x", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeServerURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeServerURL(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeServerURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestParseReadTarget(t *testing.T) {
	cases := []struct {
		in      []string
		want    readTarget
		wantErr bool
	}{
		{in: []string{"local/repo:cmd/main.go"}, want: readTarget{repo: "local/repo", path: "cmd/main.go"}},
		{in: []string{"local/repo:cmd/main.go:12-40"}, want: readTarget{repo: "local/repo", path: "cmd/main.go", start: 12, end: 40}},
		{in: []string{"local/repo:cmd/main.go:12"}, want: readTarget{repo: "local/repo", path: "cmd/main.go", start: 12}},
		// Copied straight from a search heading: strip the @commit provenance.
		{in: []string{"local/repo:cmd/main.go@1a2b3c4d"}, want: readTarget{repo: "local/repo", path: "cmd/main.go"}},
		{in: []string{"local/repo:cmd/main.go@1a2b3c4d:5-9"}, want: readTarget{repo: "local/repo", path: "cmd/main.go", start: 5, end: 9}},
		// A colon inside the path stays part of the path.
		{in: []string{"local/repo:weird:name.txt:3-4"}, want: readTarget{repo: "local/repo", path: "weird:name.txt", start: 3, end: 4}},
		{in: []string{"local/repo", "cmd/main.go"}, want: readTarget{repo: "local/repo", path: "cmd/main.go"}},
		{in: []string{"local/repo", "cmd/main.go", "7-8"}, want: readTarget{repo: "local/repo", path: "cmd/main.go", start: 7, end: 8}},
		{in: []string{"local/repo"}, wantErr: true},
		{in: []string{}, wantErr: true},
		{in: []string{"local/repo", "cmd/main.go", "8-7"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseReadTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseReadTarget(%q): want error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseReadTarget(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}

func TestParseArgsInterspersed(t *testing.T) {
	flags := flag.NewFlagSet("search", flag.ContinueOnError)
	repo := flags.String("repo", "", "")
	pos, err := parseArgs(flags, []string{"foo", "--repo", "local/x", "bar", "--", "--baz"})
	if err != nil {
		t.Fatal(err)
	}
	if *repo != "local/x" {
		t.Errorf("repo = %q, want local/x", *repo)
	}
	if want := []string{"foo", "bar", "--baz"}; strings.Join(pos, " ") != strings.Join(want, " ") {
		t.Errorf("positional = %q, want %q", pos, want)
	}
}

func TestToolArgsKeepsFacetListsAndBoolean(t *testing.T) {
	got := toolArgs(map[string]any{
		"exclude_repos": []string{" local/a ", "", "local/b"},
		"facets":        true,
		"unset":         false,
	})
	values, ok := got["exclude_repos"].([]string)
	if !ok || strings.Join(values, ",") != "local/a,local/b" {
		t.Fatalf("exclude_repos = %#v", got["exclude_repos"])
	}
	if got["facets"] != true {
		t.Fatalf("facets = %#v", got["facets"])
	}
	if _, ok := got["unset"]; ok {
		t.Fatalf("false option should be omitted: %#v", got)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv(envConfigPath, filepath.Join(t.TempDir(), "cli.json"))
	cf, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cf.DefaultServer != "" || len(cf.Servers) != 0 {
		t.Fatalf("fresh config not empty: %+v", cf)
	}
	cf.DefaultServer = "http://localhost:8080"
	cf.Servers["http://localhost:8080"] = &credentials{Kind: "token", Token: "cbp_x"}
	if err := saveConfig(cf); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DefaultServer != cf.DefaultServer || loaded.Servers["http://localhost:8080"].Token != "cbp_x" {
		t.Fatalf("round trip lost data: %+v", loaded)
	}
}

// fakeCodebeam is an httptest stand-in for the server's OAuth + /mcp surface.
type fakeCodebeam struct {
	t             *testing.T
	mu            chan struct{} // 1-slot semaphore keeps handler state race-free
	base          string
	code          string
	verifier      string // expected PKCE verifier hash — not enforced, presence-checked
	access        string
	refresh       string
	refreshed     int
	toolCalled    string
	toolArguments map[string]any
}

func newFakeCodebeam(t *testing.T) (*fakeCodebeam, *httptest.Server) {
	f := &fakeCodebeam{t: t, mu: make(chan struct{}, 1), access: "cba_1", refresh: "cbr_1"}
	srv := httptest.NewServer(f)
	f.base = srv.URL
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCodebeam) lock()   { f.mu <- struct{}{} }
func (f *fakeCodebeam) unlock() { <-f.mu }

func (f *fakeCodebeam) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lock()
	defer f.unlock()
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		json.NewEncoder(w).Encode(map[string]string{ // nolint:errcheck
			"authorization_endpoint": f.base + "/oauth/authorize",
			"token_endpoint":         f.base + "/oauth/token",
			"registration_endpoint":  f.base + "/oauth/register",
		})
	case "/oauth/register":
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		json.NewDecoder(r.Body).Decode(&req) // nolint:errcheck
		if len(req.RedirectURIs) != 1 || !strings.HasPrefix(req.RedirectURIs[0], "http://127.0.0.1:") {
			http.Error(w, "bad redirect", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"client_id": "cbc_test"}) // nolint:errcheck
	case "/oauth/authorize":
		q := r.URL.Query()
		if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		f.code = "cbg_test"
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+url.Values{
			"code":  {f.code},
			"state": {q.Get("state")},
		}.Encode(), http.StatusSeeOther)
	case "/oauth/token":
		r.ParseForm() // nolint:errcheck
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			if r.PostForm.Get("code") != f.code || r.PostForm.Get("code_verifier") == "" {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
		case "refresh_token":
			if r.PostForm.Get("refresh_token") != f.refresh {
				http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
				return
			}
			f.refreshed++
			f.access = fmt.Sprintf("cba_%d", f.refreshed+1)
			f.refresh = fmt.Sprintf("cbr_%d", f.refreshed+1)
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{ // nolint:errcheck
			"access_token": f.access, "refresh_token": f.refresh, "expires_in": 3600,
		})
	case "/mcp":
		if r.Header.Get("Authorization") != "Bearer "+f.access {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req) // nolint:errcheck
		if len(req.ID) == 0 {
			// Notifications get 202 with no body, like the real transport.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		f.toolCalled = req.Params.Name
		f.toolArguments = req.Params.Arguments
		json.NewEncoder(w).Encode(map[string]any{ // nolint:errcheck
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"content": []map[string]string{{"type": "text", "text": "# Indexed repositories (2)\n\n- local/a\n- local/b\n"}},
			},
		})
	default:
		http.NotFound(w, r)
	}
}

// browserFollowingRedirects simulates the user approving consent: it GETs the
// authorize URL and follows the redirect back to cb's loopback callback.
func browserFollowingRedirects(t *testing.T) func(string) error {
	return func(authorizeURL string) error {
		go func() {
			resp, err := http.Get(authorizeURL)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			resp.Body.Close() // nolint:errcheck
		}()
		return nil
	}
}

func testApp(t *testing.T) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Setenv(envConfigPath, filepath.Join(t.TempDir(), "cli.json"))
	t.Setenv(envToken, "")
	t.Setenv(envServer, "")
	var stdout, stderr bytes.Buffer
	return &app{
		ctx:     context.Background(),
		stdout:  &stdout,
		stderr:  &stderr,
		hc:      &http.Client{Timeout: 10 * time.Second},
		openURL: browserFollowingRedirects(t),
	}, &stdout, &stderr
}

func stringArgs(value any) string {
	raw, _ := value.([]any)
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok {
			values = append(values, text)
		}
	}
	return strings.Join(values, ",")
}

func TestLoginThenSearchEndToEnd(t *testing.T) {
	fake, srv := newFakeCodebeam(t)
	a, stdout, stderr := testApp(t)

	if code := a.run([]string{"login", srv.URL}); code != 0 {
		t.Fatalf("login exit %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Logged in to "+srv.URL) {
		t.Fatalf("login output: %s", stdout.String())
	}
	if fake.toolCalled != "list_repos" {
		t.Fatalf("login verification called %q, want list_repos", fake.toolCalled)
	}

	// The login became the default server, so a bare search finds it.
	stdout.Reset()
	if code := a.run([]string{"search", "foo", "--lang", "go", "--repo", "local/a", "--repo", "local/b", "--exclude-repo", "local/noise", "--facets"}); code != 0 {
		t.Fatalf("search exit %d, stderr: %s", code, stderr.String())
	}
	if fake.toolCalled != "search_code" {
		t.Fatalf("search called %q, want search_code", fake.toolCalled)
	}
	if fake.toolArguments["facets"] != true {
		t.Fatalf("facets argument = %#v", fake.toolArguments["facets"])
	}
	if got := stringArgs(fake.toolArguments["repos"]); got != "local/a,local/b" {
		t.Fatalf("repos argument = %q", got)
	}
	if got := stringArgs(fake.toolArguments["exclude_repos"]); got != "local/noise" {
		t.Fatalf("exclude_repos argument = %q", got)
	}
	if !strings.Contains(stdout.String(), "Indexed repositories") {
		t.Fatalf("search output: %s", stdout.String())
	}
}

func TestExpiredTokenIsRefreshedAndPersisted(t *testing.T) {
	fake, srv := newFakeCodebeam(t)
	a, stdout, stderr := testApp(t)

	// Seed stored OAuth credentials whose access token is stale and wrong.
	cf, _ := loadConfig()
	cf.DefaultServer = srv.URL
	cf.Servers[srv.URL] = &credentials{
		Kind: "oauth", ClientID: "cbc_test",
		AccessToken: "cba_stale", RefreshToken: "cbr_1",
		ExpiresAt:     time.Now().Add(-time.Hour).Unix(),
		TokenEndpoint: srv.URL + "/oauth/token",
	}
	if err := saveConfig(cf); err != nil {
		t.Fatal(err)
	}

	if code := a.run([]string{"repos"}); code != 0 {
		t.Fatalf("repos exit %d, stderr: %s", code, stderr.String())
	}
	if fake.refreshed != 1 {
		t.Fatalf("refreshed %d times, want 1", fake.refreshed)
	}
	if !strings.Contains(stdout.String(), "Indexed repositories") {
		t.Fatalf("output: %s", stdout.String())
	}
	saved, _ := loadConfig()
	if got := saved.Servers[srv.URL]; got.AccessToken != fake.access || got.RefreshToken != fake.refresh {
		t.Fatalf("rotated tokens not persisted: %+v", got)
	}
}

func TestEnvTokenNeedsNoLogin(t *testing.T) {
	fake, srv := newFakeCodebeam(t)
	a, stdout, stderr := testApp(t)
	fake.access = "cbp_agent_token" // the fake accepts whatever equals f.access
	t.Setenv(envToken, "cbp_agent_token")
	t.Setenv(envServer, srv.URL)

	if code := a.run([]string{"stats"}); code != 0 {
		t.Fatalf("stats exit %d, stderr: %s", code, stderr.String())
	}
	if fake.toolCalled != "repo_stats" {
		t.Fatalf("called %q, want repo_stats", fake.toolCalled)
	}
	if stdout.Len() == 0 {
		t.Fatal("no output")
	}
}

func TestNotLoggedInHint(t *testing.T) {
	a, _, stderr := testApp(t)
	if code := a.run([]string{"search", "foo"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cb login") || !strings.Contains(stderr.String(), envToken) {
		t.Fatalf("stderr missing login hint: %s", stderr.String())
	}
}

func TestToolErrorSurfacesAsExitOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ // nolint:errcheck
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"isError": true,
				"content": []map[string]string{{"type": "text", "text": "query is required"}},
			},
		})
	}))
	defer srv.Close()
	a, _, stderr := testApp(t)
	t.Setenv(envToken, "cbp_x")
	t.Setenv(envServer, srv.URL)
	if code := a.run([]string{"repos"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "query is required") {
		t.Fatalf("stderr: %s", stderr.String())
	}
}

// --- MCP stdio proxy ---

func TestMCPProxyForwardsMessages(t *testing.T) {
	fake, srv := newFakeCodebeam(t)
	a, stdout, stderr := testApp(t)
	t.Setenv(envToken, "cba_1") // the fake accepts whatever equals f.access
	t.Setenv(envServer, srv.URL)
	a.stdin = strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_repos","arguments":{}}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")

	if code := a.run([]string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit %d, stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 response line (the notification answers nothing), got %d: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"id":1`) || !strings.Contains(lines[0], "Indexed repositories") {
		t.Fatalf("response: %s", lines[0])
	}
	if fake.toolCalled != "list_repos" {
		t.Fatalf("called %q, want list_repos", fake.toolCalled)
	}
}

func TestMCPProxyAnswersErrorsInBand(t *testing.T) {
	_, srv := newFakeCodebeam(t)
	a, stdout, stderr := testApp(t)
	t.Setenv(envToken, "cbp_wrong")
	t.Setenv(envServer, srv.URL)
	a.stdin = strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n")

	if code := a.run([]string{"mcp"}); code != 0 {
		t.Fatalf("mcp exit %d, stderr: %s", code, stderr.String())
	}
	var resp struct {
		ID    int `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &resp); err != nil {
		t.Fatalf("output is not a JSON-RPC message: %q", stdout.String())
	}
	if resp.ID != 7 || !strings.Contains(resp.Error.Message, "cb login") {
		t.Fatalf("error response: %+v", resp)
	}
}

// --- Cloudflare Access ---

func TestIsCFAccessHost(t *testing.T) {
	for host, want := range map[string]bool{
		"acme.cloudflareaccess.com": true,
		"cloudflareaccess.com":      true,
		"Acme.CloudflareAccess.com": true,
		"codebeam.paas.acme.dev":    false,
		"evilcloudflareaccess.com":  false,
	} {
		if got := isCFAccessHost(host); got != want {
			t.Errorf("isCFAccessHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// protectedCodebeam fronts a fakeCodebeam the way a Cloudflare Access gateway
// does: requests without the expected Access token are redirected to the
// gateway's login page instead of reaching the app. cfAccessDomain is pointed
// at the gateway's host for the duration of the test.
func protectedCodebeam(t *testing.T, token string) (*fakeCodebeam, *httptest.Server) {
	t.Helper()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>SSO login required</html>") // nolint:errcheck
	}))
	t.Cleanup(gateway.Close)

	f := &fakeCodebeam{t: t, mu: make(chan struct{}, 1), access: "cba_1", refresh: "cbr_1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cf-Access-Token") != token {
			http.Redirect(w, r, gateway.URL+"/login", http.StatusFound)
			return
		}
		f.ServeHTTP(w, r)
	}))
	f.base = srv.URL
	t.Cleanup(srv.Close)

	old := cfAccessDomain
	cfAccessDomain = strings.TrimPrefix(gateway.URL, "http://")
	t.Cleanup(func() { cfAccessDomain = old })
	return f, srv
}

// cfBrowser is browserFollowingRedirects with the Access token a real
// browser's gateway cookie would carry.
func cfBrowser(t *testing.T, token string) func(string) error {
	return func(authorizeURL string) error {
		go func() {
			req, err := http.NewRequest(http.MethodGet, authorizeURL, nil)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			req.Header.Set("Cf-Access-Token", token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("browser: %v", err)
				return
			}
			resp.Body.Close() // nolint:errcheck
		}()
		return nil
	}
}

func TestCloudflareAccessLoginAndSearch(t *testing.T) {
	fake, srv := protectedCodebeam(t, "cfjwt_test")
	a, stdout, stderr := testApp(t)
	a.openURL = cfBrowser(t, "cfjwt_test")
	var interactives []bool
	a.cfCredentials = func(_ context.Context, server string, interactive bool, _ io.Writer) (http.Header, error) {
		if server != srv.URL {
			t.Errorf("cfCredentials for %q, want %q", server, srv.URL)
		}
		interactives = append(interactives, interactive)
		return http.Header{"Cf-Access-Token": {"cfjwt_test"}}, nil
	}

	if code := a.run([]string{"login", srv.URL}); code != 0 {
		t.Fatalf("login exit %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "protected by Cloudflare Access") {
		t.Fatalf("login output missing the Access note: %s", stdout.String())
	}
	if len(interactives) != 1 || !interactives[0] {
		t.Fatalf("credential acquisitions during login: %v, want one interactive", interactives)
	}
	saved, _ := loadConfig()
	if !saved.Servers[srv.URL].CFAccess {
		t.Fatalf("stored credentials not marked cf_access: %+v", saved.Servers[srv.URL])
	}

	// A later invocation re-acquires the Access header non-interactively.
	stdout.Reset()
	if code := a.run([]string{"search", "foo"}); code != 0 {
		t.Fatalf("search exit %d, stderr: %s", code, stderr.String())
	}
	if fake.toolCalled != "search_code" {
		t.Fatalf("called %q, want search_code", fake.toolCalled)
	}
	if last := interactives[len(interactives)-1]; last {
		t.Fatal("tool call acquired Access credentials interactively")
	}
}

func TestCloudflareAccessBlockedToolCallExplains(t *testing.T) {
	_, srv := protectedCodebeam(t, "cfjwt_test")
	a, _, stderr := testApp(t)
	t.Setenv(envToken, "cbp_x")
	t.Setenv(envServer, srv.URL)

	// Nothing marks the server as protected and no Access credential exists,
	// so the gateway swallows the call — the error must say so.
	if code := a.run([]string{"repos"}); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Cloudflare Access") || !strings.Contains(stderr.String(), "cb login") {
		t.Fatalf("stderr missing the Cloudflare Access hint: %s", stderr.String())
	}
}
