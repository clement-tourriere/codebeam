package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	"github.com/ctourriere/codebeam/internal/secretbox"
	"github.com/ctourriere/codebeam/internal/store"
)

func TestSearchDoesNotExposeTransientIndexPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "index-does-not-exist")
	_, err := (Engine{IndexDir: missing}).Search(context.Background(), Request{
		Query: "needle", Allowed: []store.Repo{{FullName: "local/repo"}},
	})
	if !errors.Is(err, errIndexChanging) {
		t.Fatalf("error = %v, want temporary index update error", err)
	}
	if strings.Contains(err.Error(), missing) || strings.Contains(err.Error(), ".zoekt") || strings.Contains(err.Error(), "lstat") {
		t.Fatalf("internal index path leaked to user: %q", err)
	}
}

func TestBuildZoektQueryScopesToAllowedRepos(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query: "TODO",
		Allowed: []store.Repo{
			{FullName: "github.com/acme/api"},
			{FullName: "gitlab.com/acme/web"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"(TODO)",
		"repo:^github\\.com/acme/api$",
		"repo:^gitlab\\.com/acme/web$",
		" or ",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q does not contain %q", query, want)
		}
	}
}

func TestBuildZoektQueryRejectsUnauthorizedRepoFilter(t *testing.T) {
	_, err := BuildZoektQuery(Request{
		Query:      "TODO",
		RepoFilter: "github.com/acme/private",
		Allowed:    []store.Repo{{FullName: "github.com/acme/public"}},
	})
	if err == nil {
		t.Fatal("expected unauthorized repo filter error")
	}
}

func TestBuildZoektQueryFiltersMultipleRepos(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query:       "TODO",
		RepoFilters: []string{"github.com/acme/api", "gitlab.com/acme/web"},
		Allowed: []store.Repo{
			{FullName: "github.com/acme/api"},
			{FullName: "gitlab.com/acme/web"},
			{FullName: "local/other"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo:^github\\.com/acme/api$", "repo:^gitlab\\.com/acme/web$", " or "} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q does not contain %q", query, want)
		}
	}
	if strings.Contains(query, "local/other") {
		t.Fatalf("query should not include unselected repo: %q", query)
	}
}

func TestBuildZoektQueryAddsFilters(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query:         "panic",
		BranchFilter:  "main",
		PathFilter:    "internal/.*\\.go",
		TopPathFilter: "internal",
		ExtFilter:     "go",
		LangFilter:    "go",
		Allowed:       []store.Repo{{FullName: "github.com/acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"branch:main", "file:internal/.*\\.go", "file:^internal/", "file:\\.go$", "lang:go"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q does not contain %q", query, want)
		}
	}
}

func TestBuildZoektQueryFiltersReposByProviderAndFreshness(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query:           "TODO",
		ProviderFilter:  "github",
		FreshnessFilter: freshnessHour,
		Allowed: []store.Repo{
			{FullName: "github.com/acme/api", HostProvider: "github", IndexedAt: time.Now().Unix()},
			{FullName: "local/repo", HostProvider: "local", IndexedAt: time.Now().Unix()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "repo:^github\\.com/acme/api$") || strings.Contains(query, "local/repo") {
		t.Fatalf("query did not restrict repos by provider/freshness: %q", query)
	}
}

func TestBuildZoektQueryFiltersReposBySource(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query:        "TODO",
		SourceFilter: "gitlab.acme.dev",
		Allowed: []store.Repo{
			{FullName: "gitlab.acme.dev/platform/sandbox", HostProvider: "gitlab:https://gitlab.acme.dev"},
			{FullName: "github.com/acme/api", HostProvider: "github"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "repo:^gitlab\\.acme\\.dev/platform/sandbox$") || strings.Contains(query, "github.com/acme/api") {
		t.Fatalf("query did not restrict repos by source: %q", query)
	}
}

func TestBuildFacetsIncludesRequestedBuckets(t *testing.T) {
	now := time.Now()
	files := []FileMatch{
		{
			Repository: "local/repo",
			RepoName:   "repo",
			Provider:   "local",
			Branches:   []string{"main"},
			Path:       "internal/search/search.go",
			Language:   "Go",
			IndexedAt:  now.Add(-2 * time.Hour).Unix(),
			Dirty:      true,
			Lines:      []LineMatch{{Number: 1, SymbolKinds: []string{"function"}}},
		},
		{
			Repository: "github.com/acme/web",
			Provider:   "github",
			Branches:   []string{"main"},
			Path:       "README",
			IndexedAt:  now.Add(-10 * 24 * time.Hour).Unix(),
			Lines:      []LineMatch{{Number: 1}},
		},
	}
	facets := buildFacets(files, Request{SourceFilter: "github.com", DirtyFilter: "dirty", ExtFilter: "go"}, now)
	wantFields := []string{"source", "repo", "branch", "language", "top_path", "extension", "provider", "dirty", "symbol_kind", "freshness"}
	for _, field := range wantFields {
		if !facetHasField(facets, field) {
			t.Fatalf("facets missing %q: %#v", field, facets)
		}
	}
	if !facetValueActive(facets, "source", "github.com") || !facetValueActive(facets, "dirty", dirtyValue) || !facetValueActive(facets, "extension", ".go") {
		t.Fatalf("selected facets were not marked active: %#v", facets)
	}
	if !facetHasLabel(facets, "repo", "acme/web") {
		t.Fatalf("repository facet should show repo path without source host: %#v", facets)
	}
}

func facetHasField(facets []FacetGroup, field string) bool {
	for _, facet := range facets {
		if facet.Field == field {
			return true
		}
	}
	return false
}

func facetValueActive(facets []FacetGroup, field, value string) bool {
	for _, facet := range facets {
		if facet.Field != field {
			continue
		}
		for _, facetValue := range facet.Values {
			if facetValue.Value == value {
				return facetValue.Active
			}
		}
	}
	return false
}

func facetHasLabel(facets []FacetGroup, field, label string) bool {
	for _, facet := range facets {
		if facet.Field != field {
			continue
		}
		for _, facetValue := range facet.Values {
			if facetValue.Label == label {
				return true
			}
		}
	}
	return false
}

func TestSortCollectedFiles(t *testing.T) {
	files := []collectedFileMatch{
		{match: FileMatch{Repository: "gitlab.example.com/team/zeta", RepoName: "zeta", Path: "b.go", Score: 10, IndexedAt: 20, Lines: []LineMatch{{Number: 1}}}},
		{match: FileMatch{Repository: "gitlab.example.com/team/alpha", RepoName: "alpha", Path: "c.go", Score: 30, IndexedAt: 10, Lines: []LineMatch{{Number: 1}, {Number: 2}}}},
		{match: FileMatch{Repository: "github.com/acme/api", RepoName: "api", Path: "a.go", Score: 20, IndexedAt: 30, Lines: []LineMatch{{Number: 1}}}},
	}

	sortCollectedFiles(files, sortPath)
	if got := []string{files[0].match.Path, files[1].match.Path, files[2].match.Path}; strings.Join(got, ",") != "a.go,b.go,c.go" {
		t.Fatalf("path sort order = %#v", got)
	}

	sortCollectedFiles(files, sortMatchCount)
	if files[0].match.Repository != "gitlab.example.com/team/alpha" {
		t.Fatalf("match-count sort should put the two-match file first: %#v", files)
	}
}

func TestBuildZoektQueryNormalizedMode(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query:      "TODO",
		Normalized: true,
		Allowed:    []store.Repo{{FullName: "github.com/acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "case:no") {
		t.Fatalf("normalized query should force case:no: %q", query)
	}

	plain, err := BuildZoektQuery(Request{
		Query:   "TODO",
		Allowed: []store.Repo{{FullName: "github.com/acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "case:no") {
		t.Fatalf("plain query should keep Zoekt's default case semantics: %q", plain)
	}
}

func TestBuildZoektQuerySymbolMode(t *testing.T) {
	query, err := BuildZoektQuery(Request{
		Query:      "Handler",
		Symbols:    true,
		LangFilter: "go",
		Allowed:    []store.Repo{{FullName: "github.com/acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "sym:(Handler)") {
		t.Fatalf("symbol query missing sym: atom: %q", query)
	}
	if strings.Contains(query, "(sym:(Handler))") {
		t.Fatalf("symbol atom must not be wrapped in an extra group: %q", query)
	}
	if !strings.Contains(query, "lang:go") {
		t.Fatalf("symbol query dropped filters: %q", query)
	}
}

func TestSearchHonorsIndexedBranchFilter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n// BranchNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    repoDir,
		Name:          "repo",
		FullName:      "local/repo",
		CloneURL:      repoDir,
		DefaultBranch: "main",
		LocalPath:     repoDir,
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (Engine{IndexDir: indexDir}).Search(ctx, Request{Query: "BranchNeedle", BranchFilter: "main", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if result.MatchCount == 0 || len(result.Files) != 1 || !containsString(result.Files[0].Branches, "main") {
		t.Fatalf("expected branch-scoped search to find main branch result, got %#v", result)
	}
}

func TestSearchHonorsRemoteMultiBranchIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "init", "-b", "main")
	runGit(t, src, "config", "user.email", "test@example.com")
	runGit(t, src, "config", "user.name", "Test User")
	runGit(t, src, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(src, "shared.txt"), []byte("SharedNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "branch.txt"), []byte("MainNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dedup.txt"), []byte("DedupNeedle\nsame context\nsame context\nmain-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "main")
	runGit(t, src, "checkout", "-b", "dev")
	if err := os.WriteFile(filepath.Join(src, "branch.txt"), []byte("DevNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "dedup.txt"), []byte("DedupNeedle\nsame context\nsame context\ndev-only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "commit", "-am", "dev")
	runGit(t, src, "checkout", "main")

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:    "github",
		HostRepoID:      "public-remote",
		Name:            "remote",
		FullName:        "github.com/acme/remote",
		CloneURL:        src,
		DefaultBranch:   "main",
		IndexedBranches: "*",
		Selected:        true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.NormalizeBranchList(fresh.IndexedBranchNames); got != "main,dev" {
		t.Fatalf("all-branches index should record actual branches, got %q", got)
	}
	engine := Engine{IndexDir: indexDir}
	dev, err := engine.Search(ctx, Request{Query: "DevNeedle", BranchFilter: "dev", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if dev.MatchCount == 0 || len(dev.Files) != 1 || !containsString(dev.Files[0].Branches, "dev") {
		t.Fatalf("expected dev branch match, got %#v", dev)
	}
	main, err := engine.Search(ctx, Request{Query: "DevNeedle", BranchFilter: "main", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if main.MatchCount != 0 {
		t.Fatalf("main branch should not match dev-only content: %#v", main)
	}
	shared, err := engine.Search(ctx, Request{Query: "SharedNeedle", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if shared.MatchCount == 0 || len(shared.Files) != 1 || !containsString(shared.Files[0].Branches, "main") || !containsString(shared.Files[0].Branches, "dev") {
		t.Fatalf("unchanged file should be shared by both branches, got %#v", shared)
	}
	dedup, err := engine.Search(ctx, Request{Query: "DedupNeedle", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if dedup.FileCount != 1 || len(dedup.Files) != 1 || !containsString(dedup.Files[0].Branches, "main") || !containsString(dedup.Files[0].Branches, "dev") {
		t.Fatalf("equivalent branch hits should be deduplicated, got %#v", dedup)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, strings.TrimSpace(string(out)))
	}
}

func TestSearchNormalizesCaseAndDiacritics(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "owners:\n  - clement@acme.dev\n  - clément@example.com\n"
	if err := os.WriteFile(filepath.Join(repoDir, "teams.yml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    repoDir,
		Name:          "repo",
		FullName:      "local/repo",
		CloneURL:      repoDir,
		DefaultBranch: "HEAD",
		LocalPath:     repoDir,
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	engine := Engine{IndexDir: indexDir}
	plainUpper, err := engine.Search(ctx, Request{Query: "Clem", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if plainUpper.MatchCount != 0 {
		t.Fatalf("expected plain search to keep Zoekt's case:auto semantics, got %#v", plainUpper)
	}

	upper, err := engine.Search(ctx, Request{Query: "Clem", Normalized: true, Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if upper.MatchCount == 0 {
		t.Fatalf("expected normalized search to match uppercase query, got %#v", upper)
	}

	accented, err := engine.Search(ctx, Request{Query: "clém", Normalized: true, Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if accented.MatchCount < 2 {
		t.Fatalf("expected accent-normalized query to match accented and ASCII text, got %#v", accented)
	}

	ascii, err := engine.Search(ctx, Request{Query: "clem", Normalized: true, Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if ascii.MatchCount < 2 {
		t.Fatalf("expected normalized ASCII query to match accented and ASCII text, got %#v", ascii)
	}

	exactCase, err := engine.Search(ctx, Request{Query: "case:yes Clem", Normalized: true, Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if exactCase.MatchCount != 0 {
		t.Fatalf("expected scoped case:yes to preserve exact-case search, got %#v", exactCase)
	}
}

func TestSearchFindsSymbols(t *testing.T) {
	ctags := universalCtagsPath()
	if ctags == "" {
		t.Skip("Universal Ctags not available; symbol indexing is unavailable")
	}
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package main\n\nfunc UniqueSymbolName() string { return \"x\" }\n\nfunc other() { _ = UniqueSymbolName() }\n"
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    repoDir,
		Name:          "repo",
		FullName:      "local/repo",
		CloneURL:      repoDir,
		DefaultBranch: "HEAD",
		LocalPath:     repoDir,
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos"), CTagsPath: ctags}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Symbol search returns only the definition line, not the call site.
	result, err := (Engine{IndexDir: indexDir}).Search(ctx, Request{Query: "UniqueSymbolName", Symbols: true, Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if result.MatchCount != 1 {
		t.Fatalf("expected exactly the definition match, got %d: %#v", result.MatchCount, result.Files)
	}
	if len(result.Files) != 1 || result.Files[0].Lines[0].Number != 3 {
		t.Fatalf("expected match on the definition line 3, got %#v", result.Files)
	}
}

// universalCtagsPath returns a reachable Universal Ctags binary, or "" when none
// is available. It mirrors the indexer's resolution so the symbol test exercises
// the same path users do.
func universalCtagsPath() string {
	for _, c := range []string{os.Getenv("CODEBEAM_CTAGS_PATH"), os.Getenv("CTAGS_COMMAND")} {
		if c != "" {
			return c
		}
	}
	if p, err := exec.LookPath("universal-ctags"); err == nil {
		return p
	}
	return ""
}

func TestSearchFacetsCoverBeyondDisplayLimit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const n = displayFileLimit + 50 // more files than we render
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("file%03d.go", i)
		if err := os.WriteFile(filepath.Join(repoDir, name), []byte("package main\n// FacetNeedle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    repoDir,
		Name:          "repo",
		FullName:      "local/repo",
		CloneURL:      repoDir,
		DefaultBranch: "HEAD",
		LocalPath:     repoDir,
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}

	result, err := (Engine{IndexDir: indexDir}).Search(ctx, Request{Query: "FacetNeedle", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != n {
		t.Fatalf("expected file count %d, got %d", n, result.FileCount)
	}
	if len(result.Files) != displayFileLimit {
		t.Fatalf("expected display to be capped at %d, got %d", displayFileLimit, len(result.Files))
	}
	// A capped file list must be flagged so agents/API consumers know the
	// results are partial.
	if !result.Truncated {
		t.Fatal("expected Truncated to be set when the display cap cuts the file list")
	}
	// The repository facet must count every matched file, not just the rendered page.
	var repoCount int
	for _, group := range result.Facets {
		if group.Field == "repo" {
			for _, v := range group.Values {
				repoCount += v.Count
			}
		}
	}
	if repoCount != n {
		t.Fatalf("repo facet should count all %d files, got %d", n, repoCount)
	}
}

func TestParseGitStatusPorcelainZ(t *testing.T) {
	dirty := parseGitStatusPorcelainZ([]byte(" M main.go\x00?? new file.go\x00R  renamed.go\x00old.go\x00"))
	for _, want := range []string{"main.go", "new file.go", "renamed.go", "old.go"} {
		if _, ok := dirty[want]; !ok {
			t.Fatalf("dirty paths missing %q: %#v", want, dirty)
		}
	}
}

func TestParseGitStatusPorcelainZEmpty(t *testing.T) {
	if dirty := parseGitStatusPorcelainZ(nil); dirty != nil {
		t.Fatalf("expected nil dirty paths, got %#v", dirty)
	}
}

func TestSearchMarksLocalDirtyFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "-C", repoDir, "init").Run(); err != nil {
		t.Skipf("git init unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n// DirtyNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    repoDir,
		Name:          "repo",
		FullName:      "local/repo",
		CloneURL:      repoDir,
		DefaultBranch: "HEAD",
		LocalPath:     repoDir,
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (Engine{IndexDir: indexDir}).Search(ctx, Request{Query: "DirtyNeedle", Allowed: []store.Repo{*fresh}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 {
		t.Fatalf("expected one file, got %#v", result.Files)
	}
	if !result.Files[0].Dirty {
		t.Fatalf("expected dirty file badge, got %#v", result.Files[0])
	}
}

func TestSearchIncludesIndexedCommit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable: %v\n%s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n// CommitNeedle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "init", "--no-gpg-sign")
	headOut, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headOut))

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck
	user, _ := st.CreateDevUser(ctx)
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider: "local", HostRepoID: repoDir, Name: "repo", FullName: "local/repo",
		CloneURL: repoDir, DefaultBranch: "HEAD", LocalPath: repoDir, Selected: true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := indexer.New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IndexedCommit != head {
		t.Fatalf("repo IndexedCommit = %q, want %q", fresh.IndexedCommit, head)
	}
	// The shard is the source of truth for a match's revision. During atomic
	// publication the database can briefly describe the preceding or following
	// generation, so a DB-derived commit would make result links unstable.
	allowed := *fresh
	allowed.IndexedCommit = strings.Repeat("f", 40)
	result, err := (Engine{IndexDir: indexDir}).Search(ctx, Request{Query: "CommitNeedle", Allowed: []store.Repo{allowed}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Commit != head || !result.Files[0].CommitFromShard {
		t.Fatalf("expected exact shard commit %q on the match, got %#v", head, result.Files)
	}
}
