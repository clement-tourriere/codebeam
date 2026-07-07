// Package cli implements cb, the Codebeam command-line client: a thin,
// dependency-free HTTP client for a Codebeam server's /mcp endpoint. It
// authenticates exactly like an MCP client — the built-in OAuth 2.1 flow
// (PKCE + dynamic client registration) through a browser, or a personal
// access token for headless use — so the server needs no CLI-specific
// surface at all.
package cli

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/ctourriere/codebeam/internal/config"
)

// Environment variables understood by cb. CODEBEAM_TOKEN makes any invocation
// work with zero setup (agents, CI); CODEBEAM_URL picks the server without a
// flag; CODEBEAM_CLI_CONFIG relocates the credentials file (containers, tests).
const (
	envToken      = "CODEBEAM_TOKEN"
	envServer     = "CODEBEAM_URL"
	envConfigPath = "CODEBEAM_CLI_CONFIG"
)

const defaultServer = "http://localhost:8080"

// credentials is what cb stores per server after a login.
type credentials struct {
	// Kind is "oauth" (browser flow, refreshable) or "token" (PAT).
	Kind  string `json:"kind"`
	Token string `json:"token,omitempty"`

	ClientID     string `json:"client_id,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is the access token expiry (unix seconds); 0 means unknown.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// TokenEndpoint is remembered from login-time discovery so a refresh
	// never needs another discovery round-trip.
	TokenEndpoint string `json:"token_endpoint,omitempty"`
}

func (c *credentials) bearer() string {
	if c.Kind == "token" {
		return c.Token
	}
	return c.AccessToken
}

// configFile is the on-disk shape of the credentials file, keyed by server URL
// so one cb can talk to several Codebeam instances.
type configFile struct {
	DefaultServer string                  `json:"default_server,omitempty"`
	Servers       map[string]*credentials `json:"servers,omitempty"`
}

// configPath locates the credentials file: CODEBEAM_CLI_CONFIG when set,
// otherwise cli.json next to the server's own files under the user config dir.
func configPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv(envConfigPath)); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", errors.New("cannot determine a config directory; set " + envConfigPath)
	}
	return filepath.Join(dir, "codebeam", "cli.json"), nil
}

// loadConfig reads the credentials file; a missing file is an empty config,
// not an error.
func loadConfig() (*configFile, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &configFile{Servers: map[string]*credentials{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var cf configFile
	if err := json.Unmarshal(raw, &cf); err != nil {
		return nil, errors.New("credentials file " + path + " is corrupt: " + err.Error())
	}
	if cf.Servers == nil {
		cf.Servers = map[string]*credentials{}
	}
	return &cf, nil
}

// saveConfig writes the credentials file with owner-only permissions — it
// holds bearer tokens.
func saveConfig(cf *configFile) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// normalizeServerURL turns user input ("codebeam.acme.dev", "localhost:8080/")
// into a canonical base URL. A bare host gets https:// unless it is loopback,
// where plain http is what a local Codebeam actually serves.
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", errors.New("server URL is empty")
	}
	if !strings.Contains(raw, "://") {
		scheme := "https://"
		if config.IsLoopbackURL("http://" + raw) {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("invalid server URL: " + raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// resolveServer picks the server URL by precedence — --server flag, then
// CODEBEAM_URL, then the last login's default, then localhost — and reports
// where the choice came from for status/error messages.
func resolveServer(flagValue string, cf *configFile) (server, source string, err error) {
	switch {
	case strings.TrimSpace(flagValue) != "":
		server, err = normalizeServerURL(flagValue)
		return server, "--server", err
	case strings.TrimSpace(os.Getenv(envServer)) != "":
		server, err = normalizeServerURL(os.Getenv(envServer))
		return server, envServer, err
	case cf != nil && cf.DefaultServer != "":
		return cf.DefaultServer, "last login", nil
	default:
		return defaultServer, "default", nil
	}
}

// resolveCredentials picks the credentials for a server: CODEBEAM_TOKEN wins
// (zero-config for agents and CI), then whatever a login stored. Returns nil
// when there is nothing — callers turn that into a "run cb login" hint.
func resolveCredentials(cf *configFile, server string) *credentials {
	if token := strings.TrimSpace(os.Getenv(envToken)); token != "" {
		return &credentials{Kind: "token", Token: token}
	}
	if cf == nil {
		return nil
	}
	return cf.Servers[server]
}
