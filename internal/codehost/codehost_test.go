package codehost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeBaseURLAddsSchemeAndTrims(t *testing.T) {
	got, err := NormalizeBaseURL("gitlab.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://gitlab.example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestParseGitHubRepoSpec(t *testing.T) {
	cases := map[string][2]string{
		"sourcegraph/zoekt":                              {"sourcegraph", "zoekt"},
		"github.com/sourcegraph/zoekt":                   {"sourcegraph", "zoekt"},
		"https://github.com/sourcegraph/zoekt.git":       {"sourcegraph", "zoekt"},
		"https://github.com/sourcegraph/zoekt/tree/main": {"sourcegraph", "zoekt"},
		"git@github.com:sourcegraph/zoekt.git":           {"sourcegraph", "zoekt"},
	}
	for input, want := range cases {
		owner, repo, err := ParseGitHubRepoSpec(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if owner != want[0] || repo != want[1] {
			t.Fatalf("%s: got %s/%s, want %s/%s", input, owner, repo, want[0], want[1])
		}
	}
}

func TestParseGitHubRepoSpecRejectsOtherHosts(t *testing.T) {
	if _, _, err := ParseGitHubRepoSpec("https://gitlab.com/sourcegraph/zoekt"); err == nil {
		t.Fatal("expected non-github host to be rejected")
	}
}

func TestGitLabOAuthExchangeAndRefreshPreserveRotatingCredential(t *testing.T) {
	createdAt := time.Now().Unix()
	var grants []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		grant := r.Form.Get("grant_type")
		grants = append(grants, grant)
		if r.Form.Get("client_id") != "client" || r.Form.Get("client_secret") != "secret" || r.Form.Get("redirect_uri") != "https://codebeam.test/auth/gitlab/callback" {
			t.Fatalf("unexpected OAuth form: %#v", r.Form)
		}
		switch grant {
		case "authorization_code":
			if r.Form.Get("code") != "code-1" {
				t.Fatalf("code = %q", r.Form.Get("code"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-1", "refresh_token": "refresh-1", "expires_in": 7200, "created_at": createdAt,
			})
		case "refresh_token":
			if r.Form.Get("refresh_token") != "refresh-1" {
				t.Fatalf("refresh token = %q", r.Form.Get("refresh_token"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access-2", "refresh_token": "refresh-2", "expires_in": 7200, "created_at": createdAt + 60,
			})
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	client := New(OAuthConfig{Provider: GitLab, BaseURL: srv.URL, ClientID: "client", ClientSecret: "secret"})
	redirectURI := "https://codebeam.test/auth/gitlab/callback"
	first, err := client.ExchangeCodeTokens(context.Background(), "code-1", "state", redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken != "access-1" || first.RefreshToken != "refresh-1" || first.ExpiresAt != createdAt+7200 {
		t.Fatalf("authorization credential = %#v", first)
	}
	second, err := client.RefreshOAuthToken(context.Background(), first.RefreshToken, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessToken != "access-2" || second.RefreshToken != "refresh-2" || second.ExpiresAt != createdAt+60+7200 {
		t.Fatalf("refreshed credential = %#v", second)
	}
	if len(grants) != 2 || grants[0] != "authorization_code" || grants[1] != "refresh_token" {
		t.Fatalf("grant sequence = %#v", grants)
	}
}

func TestGitLabProviderKeyRoundTrip(t *testing.T) {
	key, err := GitLabProviderKey("https://gitlab.example.com/gitlab/")
	if err != nil {
		t.Fatal(err)
	}
	if !IsGitLabProvider(key) {
		t.Fatalf("%q should be a GitLab provider", key)
	}
	baseURL := GitLabBaseURLFromProvider(key, "")
	if baseURL != "https://gitlab.example.com/gitlab" {
		t.Fatalf("got %q", baseURL)
	}
	if HostFromBaseURL(baseURL) != "gitlab.example.com/gitlab" {
		t.Fatalf("unexpected host label")
	}
}
