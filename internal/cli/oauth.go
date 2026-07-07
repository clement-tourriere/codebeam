package cli

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// loginTimeout bounds how long cb waits for the user to finish the browser
// consent flow before giving up.
const loginTimeout = 5 * time.Minute

// authServerMeta is the slice of RFC 8414 authorization server metadata cb
// needs to run the flow.
type authServerMeta struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// discoverAuthServer fetches the server's OAuth metadata. When discovery is
// unavailable (older server, proxy stripping the path) it falls back to
// Codebeam's conventional endpoint paths so login still works.
func discoverAuthServer(ctx context.Context, hc *http.Client, server string) authServerMeta {
	fallback := authServerMeta{
		AuthorizationEndpoint: server + "/oauth/authorize",
		TokenEndpoint:         server + "/oauth/token",
		RegistrationEndpoint:  server + "/oauth/register",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return fallback
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close() // nolint:errcheck
	var meta authServerMeta
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta) != nil {
		return fallback
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" || meta.RegistrationEndpoint == "" {
		return fallback
	}
	return meta
}

// tokenResponse is an RFC 6749 token endpoint success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

// loginBrowser runs the OAuth 2.1 authorization code flow the way an MCP
// client does: bind a loopback callback, register a public client for it
// (RFC 7591), send the user's browser through authorize with PKCE, then
// exchange the code. openURL launches the browser and is injectable for tests.
func loginBrowser(ctx context.Context, hc *http.Client, server string, openURL func(string) error, out io.Writer) (*credentials, error) {
	meta := discoverAuthServer(ctx, hc, server)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cannot bind a loopback callback port: %w", err)
	}
	defer listener.Close() // nolint:errcheck
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	clientID, err := registerClient(ctx, hc, meta.RegistrationEndpoint, redirectURI)
	if err != nil {
		return nil, err
	}

	verifier := randomURLSafe(32)
	challenge := sha256.Sum256([]byte(verifier))
	state := randomURLSafe(16)
	authorizeURL := meta.AuthorizationEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"scope":                 {"codebeam"},
		"resource":              {server + "/mcp"},
	}.Encode()

	type callback struct {
		code string
		err  error
	}
	results := make(chan callback, 1)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		switch {
		case q.Get("state") != state:
			writeCallbackPage(w, "Login failed: state mismatch — try again.")
			results <- callback{err: errors.New("authorization response state mismatch")}
		case q.Get("error") != "":
			msg := q.Get("error")
			if d := q.Get("error_description"); d != "" {
				msg += ": " + d
			}
			writeCallbackPage(w, "Login failed: "+msg)
			results <- callback{err: errors.New("authorization refused: " + msg)}
		case q.Get("code") == "":
			writeCallbackPage(w, "Login failed: no authorization code — try again.")
			results <- callback{err: errors.New("authorization response carried no code")}
		default:
			writeCallbackPage(w, "Logged in to Codebeam — you can close this tab and return to the terminal.")
			results <- callback{code: q.Get("code")}
		}
	})}
	go srv.Serve(listener) // nolint:errcheck
	defer srv.Close()      // nolint:errcheck

	fmt.Fprintf(out, "Opening your browser to sign in to %s ...\n", server)
	fmt.Fprintf(out, "If nothing opens, visit:\n\n  %s\n\n", authorizeURL)
	if openURL != nil {
		_ = openURL(authorizeURL) // the printed URL is the fallback
	}

	var code string
	select {
	case res := <-results:
		if res.err != nil {
			return nil, res.err
		}
		code = res.code
	case <-time.After(loginTimeout):
		return nil, errors.New("timed out waiting for the browser login")
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tok, err := postTokenForm(ctx, hc, meta.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {server + "/mcp"},
	})
	if err != nil {
		return nil, err
	}
	return &credentials{
		Kind:          "oauth",
		ClientID:      clientID,
		AccessToken:   tok.AccessToken,
		RefreshToken:  tok.RefreshToken,
		ExpiresAt:     expiryFromNow(tok.ExpiresIn),
		TokenEndpoint: meta.TokenEndpoint,
	}, nil
}

// registerClient performs RFC 7591 dynamic registration of a public client
// bound to this login's loopback redirect URI.
func registerClient(ctx context.Context, hc *http.Client, endpoint, redirectURI string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "cb (Codebeam CLI)",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close() // nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("client registration failed: %s", oauthErrorMessage(resp.StatusCode, raw))
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil || reg.ClientID == "" {
		return "", errors.New("client registration returned no client_id")
	}
	return reg.ClientID, nil
}

// refreshCredentials rotates an OAuth access/refresh pair in place using the
// token endpoint remembered at login time.
func refreshCredentials(ctx context.Context, hc *http.Client, creds *credentials) error {
	if creds.Kind != "oauth" || creds.RefreshToken == "" || creds.TokenEndpoint == "" {
		return errors.New("credentials are not refreshable")
	}
	tok, err := postTokenForm(ctx, hc, creds.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {creds.RefreshToken},
		"client_id":     {creds.ClientID},
	})
	if err != nil {
		return err
	}
	creds.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		creds.RefreshToken = tok.RefreshToken
	}
	creds.ExpiresAt = expiryFromNow(tok.ExpiresIn)
	return nil
}

func postTokenForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", endpoint, err)
	}
	defer resp.Body.Close() // nolint:errcheck
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed: %s", oauthErrorMessage(resp.StatusCode, raw))
	}
	var tok tokenResponse
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	return &tok, nil
}

// oauthErrorMessage renders an RFC 6749 error body, falling back to the HTTP
// status when the body is not the expected JSON.
func oauthErrorMessage(status int, raw []byte) string {
	var body struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != "" {
		if body.Description != "" {
			return body.Error + ": " + body.Description
		}
		return body.Error
	}
	return http.StatusText(status)
}

func expiryFromNow(expiresIn int64) int64 {
	if expiresIn <= 0 {
		return 0
	}
	return time.Now().Unix() + expiresIn
}

func randomURLSafe(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // crypto/rand failing means no secure login is possible
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// writeCallbackPage renders the browser-facing outcome and flushes it to the
// socket before the handler signals the main goroutine — which tears the
// callback server down and must not race the response bytes.
func writeCallbackPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Codebeam CLI</title>
<body style="font-family:system-ui,sans-serif;display:grid;place-items:center;min-height:80vh">
<p style="font-size:1.1rem;max-width:32rem;text-align:center">%s</p></body>`, message)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// openBrowser launches the platform's URL opener; the caller has already
// printed the URL as a fallback.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
