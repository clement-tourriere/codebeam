package codehost

import "testing"

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
