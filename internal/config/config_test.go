package config

import "testing"

func TestLoadIndexingDefaults(t *testing.T) {
	cfg := Load()
	if cfg.MaxIndexedBranches != DefaultMaxIndexedBranches {
		t.Errorf("MaxIndexedBranches default = %d, want %d", cfg.MaxIndexedBranches, DefaultMaxIndexedBranches)
	}
	if !cfg.AutoExcludeInaccessible {
		t.Error("AutoExcludeInaccessible should default to true")
	}
}

func TestEnvIntNonNegAllowsZero(t *testing.T) {
	t.Setenv("CODEBEAM_MAX_INDEXED_BRANCHES", "0")
	if got := Load().MaxIndexedBranches; got != 0 {
		t.Errorf("0 should be kept as 0 (the indexer resolves it to the Zoekt max), got %d", got)
	}
	t.Setenv("CODEBEAM_MAX_INDEXED_BRANCHES", "-5")
	if got := Load().MaxIndexedBranches; got != DefaultMaxIndexedBranches {
		t.Errorf("negative should fall back to the default %d, got %d", DefaultMaxIndexedBranches, got)
	}
	t.Setenv("CODEBEAM_MAX_INDEXED_BRANCHES", "40")
	if got := Load().MaxIndexedBranches; got != 40 {
		t.Errorf("valid value should be used, got %d", got)
	}
}

func TestIsLoopbackURL(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8080":     true,
		"http://127.0.0.1:8080":     true,
		"http://[::1]:8080":         true,
		"https://127.0.0.1":         true,
		"https://codebeam.acme.com": false,
		"http://10.0.0.5:8080":      false,
		"http://example.com":        false,
		"not a url":                 false,
		"":                          false,
	}
	for raw, want := range cases {
		if got := IsLoopbackURL(raw); got != want {
			t.Errorf("IsLoopbackURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
