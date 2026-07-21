package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Cloudflare Access support. Company deployments often sit behind a Cloudflare
// Access application (SSO via Okta or similar): the gateway intercepts every
// unauthenticated request and redirects it to <team>.cloudflareaccess.com, so
// cb's OAuth discovery, registration, and /mcp calls never reach Codebeam at
// all. cb detects that landing host and attaches the Access credential to
// every request for the server — a service token from the environment
// (headless), or the JWT the cloudflared CLI obtains through the browser.

// cfAccessDomain is the Cloudflare Access edge domain; a var so tests can
// substitute an httptest host.
var cfAccessDomain = "cloudflareaccess.com"

// Environment variables carrying a Cloudflare Access service token for
// headless use (CI, agents) — the standard names Cloudflare's own tooling
// documents for the CF-Access-Client-Id/-Secret header pair.
const (
	envCFClientID     = "CF_ACCESS_CLIENT_ID"
	envCFClientSecret = "CF_ACCESS_CLIENT_SECRET"
)

func isCFAccessHost(host string) bool {
	host = strings.ToLower(host)
	return host == cfAccessDomain || strings.HasSuffix(host, "."+cfAccessDomain)
}

// blockedByCFAccess reports whether a response never reached the server
// because the Access gateway redirected the request to its login page. The
// HTTP client has already followed the redirect, so the tell is the final
// request's host.
func blockedByCFAccess(resp *http.Response) bool {
	return resp != nil && resp.Request != nil && resp.Request.URL != nil &&
		isCFAccessHost(resp.Request.URL.Host)
}

func cfServiceTokenSet() bool {
	return os.Getenv(envCFClientID) != "" && os.Getenv(envCFClientSecret) != ""
}

// cfAccessCredentials obtains the headers that satisfy the Access gateway:
// the service token pair when the environment provides one, otherwise a JWT
// from cloudflared's token cache. When interactive, a missing session runs
// `cloudflared access login`, which sends the browser through the identity
// provider. This is the production value of app.cfCredentials.
func cfAccessCredentials(ctx context.Context, server string, interactive bool, out io.Writer) (http.Header, error) {
	if cfServiceTokenSet() {
		return http.Header{
			"Cf-Access-Client-Id":     {os.Getenv(envCFClientID)},
			"Cf-Access-Client-Secret": {os.Getenv(envCFClientSecret)},
		}, nil
	}
	bin, err := exec.LookPath("cloudflared")
	if err != nil {
		return nil, errors.New("this server is behind Cloudflare Access and cb needs the cloudflared CLI to pass it — " +
			"install cloudflared (e.g. `brew install cloudflared`) and retry, or set " +
			envCFClientID + "/" + envCFClientSecret + " to a service token")
	}
	if tok := jwtFromCommand(exec.CommandContext(ctx, bin, "access", "token", "-app="+server)); tok != "" {
		return http.Header{"Cf-Access-Token": {tok}}, nil
	}
	if !interactive {
		return nil, fmt.Errorf("the Cloudflare Access session for %s is missing or expired — run `cb login %s`", server, server)
	}
	fmt.Fprintln(out, "Signing in to Cloudflare Access first (your identity provider will open in the browser) ...")
	login := exec.CommandContext(ctx, bin, "access", "login", server)
	login.Stderr = out // cloudflared prints the fallback URL and progress there
	if tok := jwtFromCommand(login); tok != "" {
		return http.Header{"Cf-Access-Token": {tok}}, nil
	}
	return nil, errors.New("cloudflared could not obtain a Cloudflare Access token for " + server)
}

// jwtRE matches a JWT in cloudflared's output — the token is the only thing
// shaped like three dot-joined base64url segments, whatever prose surrounds it.
var jwtRE = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)

func jwtFromCommand(cmd *exec.Cmd) string {
	raw, err := cmd.Output()
	if err != nil {
		return ""
	}
	return jwtRE.FindString(string(raw))
}

// headerTransport decorates requests to one host with fixed headers, leaving
// everything else (including the gateway's own redirects) untouched.
type headerTransport struct {
	base    http.RoundTripper
	host    string
	headers http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.host {
		for key, values := range t.headers {
			req.Header.Set(key, values[0])
		}
	}
	return t.base.RoundTrip(req)
}

// withExtraHeaders returns a copy of base whose requests to the server's host
// carry the given headers.
func withExtraHeaders(base *http.Client, server string, headers http.Header) *http.Client {
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	host := server
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		host = u.Host
	}
	return &http.Client{Timeout: base.Timeout, Transport: &headerTransport{base: rt, host: host, headers: headers}}
}

// accessAwareClient probes the server once and, when Cloudflare Access is in
// front of it, returns an http.Client that attaches Access credentials to
// every request for that host. Probe failures return the plain client — the
// actual operation will surface a better error.
func (a *app) accessAwareClient(ctx context.Context, server string, interactive bool, out io.Writer) (hc *http.Client, protected bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return a.hc, false, nil
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return a.hc, false, nil
	}
	resp.Body.Close() // nolint:errcheck
	if !blockedByCFAccess(resp) {
		return a.hc, false, nil
	}
	fmt.Fprintf(out, "%s is protected by Cloudflare Access.\n", server)
	headers, err := a.cfCredentials(ctx, server, interactive, out)
	if err != nil {
		return nil, true, err
	}
	return withExtraHeaders(a.hc, server, headers), true, nil
}
