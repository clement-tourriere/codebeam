package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsSecureURL(t *testing.T) {
	cases := map[string]bool{
		"https://tenant.okta.com": true,
		"http://localhost:8080":   true,
		"http://127.0.0.1":        true,
		"http://okta.com":         false, // non-loopback http is refused
		"ftp://okta.com":          false,
		"":                        false,
	}
	for raw, want := range cases {
		if got := isSecureURL(raw); got != want {
			t.Errorf("isSecureURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestDiscoverRejectsNonHTTPSIssuer(t *testing.T) {
	c := New(Config{Issuer: "http://public-idp.example", ClientID: "x", ClientSecret: "y"})
	if _, err := c.discover(context.Background()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected https rejection, got %v", err)
	}
}

func TestDiscoverRejectsIssuerMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Metadata claims a different issuer than the one we configured.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 "https://impersonated.example",
			"authorization_endpoint": "https://impersonated.example/auth",
			"token_endpoint":         "https://impersonated.example/token",
		})
	}))
	defer srv.Close()

	c := New(Config{Issuer: srv.URL, ClientID: "x", ClientSecret: "y"}) // srv.URL is http loopback (allowed)
	if _, err := c.discover(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected issuer-mismatch rejection, got %v", err)
	}
}
