package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/secretbox"
	"github.com/ctourriere/codebeam/internal/store"
)

func TestTemplatesParse(t *testing.T) {
	if _, err := New(config.Config{TemplateGlob: filepath.Join("..", "..", "templates", "*.html")}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSearchResultsReloadRendersFullShell(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(config.Config{TemplateGlob: filepath.Join("..", "..", "templates", "*.html"), SessionSecret: "test-secret", GitLabBaseURL: "https://gitlab.com"}, st, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/search/results?q=", nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()
	srv.route(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<link rel="stylesheet" href="/static/app.css">`) {
		t.Fatalf("/search/results reload should render the full shell with CSS, got %q", body)
	}
}

func TestSearchPageRendersStructuralResults(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newTestAPIServer(t, ctx)
	if err := srv.indexer.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	source := "package main\n\nfunc run() error {\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n"
	if err := os.WriteFile(filepath.Join(repo.LocalPath, "err.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	// The API-server helper skips template parsing; attach the real templates
	// so this exercises page_search + partial_search_results end to end.
	full, err := New(config.Config{TemplateGlob: filepath.Join("..", "..", "templates", "*.html"), SessionSecret: "test-secret", GitLabBaseURL: "https://gitlab.com"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.templates = full.templates
	srv.chromaCSS = full.chromaCSS

	req := httptest.NewRequest(http.MethodGet, "/search?mode=structural&lang=go&q="+url.QueryEscape("if $ERR != nil { $$$ }"), nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()
	srv.route(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		">structural<",         // engine badge
		"err.go",               // matched file
		"$ERR",                 // metavariable name
		"<mark",                // highlighted match segment
		`value="structural"`,   // mode toggle checked state present
		"Choose language…",     // structural language select
		"Refine these results", // facet sidebar
		"Top-level path",       // a facet group computed from structural matches
		`name="mode"`,          // facet links must keep mode=structural
		"mode=structural",      // …in their URLs
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered page missing %q", want)
		}
	}
	// The working-tree facet stays in structural mode: dirtiness is computed
	// for worktree scans just like lexical search. (The language facet's absence
	// is asserted in the structural engine tests — the form's own language
	// selector makes a body-text check ambiguous here.)
	if !strings.Contains(body, "Working tree") {
		t.Fatalf("rendered page should contain the working-tree facet in structural mode")
	}
}

func TestSafeJoinAllowsRepoPath(t *testing.T) {
	root := t.TempDir()
	got, err := safeJoin(root, "internal/app.go")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "internal", "app.go")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	if _, err := safeJoin(root, "../outside.txt"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestLocalRepoFullNameUsesDisplayName(t *testing.T) {
	got := localRepoFullName("Demo Repository")
	if got != "local/Demo Repository" {
		t.Fatalf("got %q", got)
	}
}

func TestRepoIDsFromRequestDeduplicatesCheckedRepos(t *testing.T) {
	req := httptest.NewRequest("POST", "/repos/bulk", strings.NewReader("repo_id=12&repo_id=12&repo_id=27"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	got, err := repoIDsFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 12 || got[1] != 27 {
		t.Fatalf("got %#v", got)
	}
}

func TestRepoIDsFromRequestRejectsInvalidID(t *testing.T) {
	req := httptest.NewRequest("POST", "/repos/bulk", strings.NewReader("repo_id=abc"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if _, err := repoIDsFromRequest(req); err == nil {
		t.Fatal("expected invalid id error")
	}
}

func TestCodeURLIncludesBranch(t *testing.T) {
	got := codeURL(7, "internal/app.go", 12, []string{"dev"})
	if got != "/code/7/internal/app.go?branch=dev&line=12" {
		t.Fatalf("got %q", got)
	}
}

func TestRepoHasIndexedBranchUsesResolvedBranches(t *testing.T) {
	repo := store.Repo{DefaultBranch: "main", IndexedBranches: "*", IndexedBranchNames: "main,dev"}
	if !repoHasIndexedBranch(repo, "dev") {
		t.Fatal("expected resolved indexed branch to be accepted")
	}
	if repoHasIndexedBranch(repo, "missing") {
		t.Fatal("unexpected unindexed branch accepted")
	}
}

func TestReferencesURLAnchorsWordBoundaries(t *testing.T) {
	got := string(referencesURL("/search", "local/Demo", "NewServer"))
	if !strings.HasPrefix(got, "/search?") || !strings.Contains(got, "repo=local%2FDemo") {
		t.Fatalf("missing base or repo scope: %q", got)
	}
	// q must be the URL-encoded word-boundary regex \bNewServer\b, encoded once.
	if !strings.Contains(got, "q=%5CbNewServer%5Cb") {
		t.Fatalf("query is not a single-encoded word-boundary search: %q", got)
	}
}

func TestRepoSearchURLPersistsSearchParams(t *testing.T) {
	got := repoSearchURL(42, SearchParams{Query: "panic", Branch: "main", Path: `internal/.*\.go`, TopPath: "internal", Ext: ".go", Lang: "go", Source: "gitlab.example.com", Provider: "local", Dirty: "dirty", SymbolKind: "function", Freshness: "day", Sort: "path", Normalized: true, Symbols: true})
	if !strings.HasPrefix(got, "/repo/42?") {
		t.Fatalf("missing repo path: %q", got)
	}
	for _, want := range []string{"q=panic", "branch=main", "path=internal%2F.%2A%5C.go", "top=internal", "ext=.go", "lang=go", "source=gitlab.example.com", "provider=local", "dirty=dirty", "symbol_kind=function", "freshness=day", "sort=path", "norm=1", "sym=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("URL %q missing %q", got, want)
		}
	}
}

func TestSearchURLPersistsMultipleRepoFilters(t *testing.T) {
	got := searchURL("/search", SearchParams{Query: "panic", Repos: []string{"github.com/acme/api", "gitlab.com/acme/web"}})
	if strings.Count(got, "repo=") != 2 {
		t.Fatalf("URL %q should contain two repo params", got)
	}
	for _, want := range []string{"repo=github.com%2Facme%2Fapi", "repo=gitlab.com%2Facme%2Fweb"} {
		if !strings.Contains(got, want) {
			t.Fatalf("URL %q missing %q", got, want)
		}
	}
}

func TestSearchParamsFromQueryNormalizesMultipleRepoFilters(t *testing.T) {
	params := searchParamsFromQuery(url.Values{"repo": []string{" github.com/acme/api ", "", "gitlab.com/acme/web", "github.com/acme/api"}})
	if params.Repo != "github.com/acme/api" {
		t.Fatalf("first repo = %q", params.Repo)
	}
	if got, want := strings.Join(params.Repos, ","), "github.com/acme/api,gitlab.com/acme/web"; got != want {
		t.Fatalf("repos = %q, want %q", got, want)
	}
}

func TestRepoReferencesURLAnchorsWordBoundaries(t *testing.T) {
	got := string(repoReferencesURL(42, "NewServer"))
	if !strings.HasPrefix(got, "/repo/42?") {
		t.Fatalf("missing repo path: %q", got)
	}
	if !strings.Contains(got, "q=%5CbNewServer%5Cb") {
		t.Fatalf("query is not a single-encoded word-boundary search: %q", got)
	}
}

func TestBulkNoticeIncludesSkippedCount(t *testing.T) {
	got := bulkNotice("Installed", "checked", 2, 1)
	want := "Installed 2 repositories. Skipped 1."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBulkNoticeDescribesMatchingScope(t *testing.T) {
	got := bulkNotice("Removed", "matching", 3, 0)
	want := "Removed 3 repositories matching the current filters."
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestChipMatches(t *testing.T) {
	view := func(status string, stale, selected bool) RepoView {
		return RepoView{Status: status, Stale: stale, Repo: store.Repo{Selected: selected}}
	}
	cases := []struct {
		name string
		view RepoView
		chip string
		want bool
	}{
		{"all always matches", view("not-indexed", false, false), "all", true},
		{"off matches unselected", view("not-indexed", false, false), "off", true},
		{"off excludes selected", view("indexed", false, true), "off", false},
		{"indexed fresh", view("indexed", false, true), "indexed", true},
		{"indexed excludes stale", view("indexed", true, true), "indexed", false},
		{"stale includes soft-stale", view("indexed", true, true), "stale", true},
		{"stale includes needs-index", view("needs-index", false, true), "stale", true},
		{"stale includes selected-not-indexed", view("not-indexed", false, true), "stale", true},
		{"indexing queued", view("queued", false, true), "indexing", true},
		{"indexing running", view("running", false, true), "indexing", true},
		{"failed", view("failed", false, true), "failed", true},
	}
	for _, tc := range cases {
		if got := chipMatches(tc.view, tc.chip); got != tc.want {
			t.Errorf("%s: chipMatches(%q)=%v, want %v", tc.name, tc.chip, got, tc.want)
		}
	}
}

func TestBuildRepoViewMarksStale(t *testing.T) {
	const interval = 30 * time.Minute
	now := int64(1_700_000_000)

	fresh := buildRepoView(store.Repo{Selected: true, HostProvider: "github", IndexedAt: now - 60}, store.IndexJob{}, interval, now)
	if fresh.Stale || fresh.Status != "indexed" {
		t.Fatalf("fresh repo: got Stale=%v Status=%q, want false/indexed", fresh.Stale, fresh.Status)
	}

	stale := buildRepoView(store.Repo{Selected: true, HostProvider: "github", IndexedAt: now - int64(2*interval/time.Second)}, store.IndexJob{}, interval, now)
	if !stale.Stale {
		t.Fatalf("old remote repo should be stale, got Stale=%v Status=%q", stale.Stale, stale.Status)
	}
	if stale.StatusLabel != "Stale" {
		t.Fatalf("stale repo label = %q, want Stale", stale.StatusLabel)
	}

	local := buildRepoView(store.Repo{Selected: true, HostProvider: "local", IndexedAt: now - 100000, LocalPath: "/p"}, store.IndexJob{}, interval, now)
	if local.Stale {
		t.Fatal("local repo should never be soft-stale")
	}
}

func TestRepoFilterFromRequestDefaultsAndIgnoresTab(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/repos/list?tab=available&q=foo&source=github", nil)
	filter := repoFilterFromRequest(r)
	if filter.Status != "all" {
		t.Fatalf("default status = %q, want all", filter.Status)
	}
	if filter.Query != "foo" {
		t.Fatalf("query = %q, want foo", filter.Query)
	}
	if filter.Source != "provider:github" {
		t.Fatalf("source = %q, want provider:github", filter.Source)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/repos/list?status=stale", nil)
	if got := repoFilterFromRequest(r2).Status; got != "stale" {
		t.Fatalf("explicit status = %q, want stale", got)
	}
}

func TestSourceMatchesSpecificProvider(t *testing.T) {
	if !sourceMatches("gitlab:https://gitlab.gitguardian.ovh", "provider:gitlab:https://gitlab.gitguardian.ovh") {
		t.Fatal("expected exact self-managed GitLab provider to match")
	}
	if sourceMatches("gitlab:https://other.example", "provider:gitlab:https://gitlab.gitguardian.ovh") {
		t.Fatal("expected different self-managed GitLab provider not to match")
	}
	if !sourceMatches("gitlab:https://other.example", "family:gitlab") {
		t.Fatal("expected GitLab family source to match every GitLab provider")
	}
}

func TestBuildSourceSummariesIncludesConnectedAndTrackedSources(t *testing.T) {
	repos := []store.Repo{
		{HostProvider: "github", FullName: "github/acme/api", Selected: true, IndexedAt: 10},
		{HostProvider: "github", FullName: "github/acme/web", LastIndexError: "failed"},
		{HostProvider: "local", FullName: "local/app", Selected: true},
	}
	identities := []store.Identity{{Provider: "github", Username: "octo"}}

	sources := buildSourceSummaries(repos, identities)
	if len(sources) != 2 {
		t.Fatalf("got %#v", sources)
	}
	if sources[0].Provider != "github" || !sources[0].Connected || !sources[0].CanSync || sources[0].RepoCount != 2 || sources[0].IndexedCount != 1 || sources[0].FailedCount != 1 {
		t.Fatalf("unexpected GitHub source summary: %#v", sources[0])
	}
	if sources[1].Provider != "local" || sources[1].CanSync || !sources[1].IsLocal || sources[1].RepoCount != 1 {
		t.Fatalf("unexpected local source summary: %#v", sources[1])
	}
}
