package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/secretbox"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/ctourriere/codebeam/internal/structural"
)

func TestAPISearchReturnsJSONMatches(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newTestAPIServer(t, ctx)

	if err := srv.indexer.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=UniqueNeedle", nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()

	srv.route(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type %q is not json", ct)
	}
	var body apiSearchResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Stats.MatchCount == 0 || len(body.Files) == 0 {
		t.Fatalf("expected search matches, got %#v", body)
	}
	if len(body.Facets) == 0 {
		t.Fatalf("expected search facets, got %#v", body)
	}
	if body.Files[0].Repository != repo.FullName || body.Files[0].Path != "main.go" {
		t.Fatalf("unexpected first file: %#v", body.Files[0])
	}
}

func TestAPIReadReturnsRequestedRange(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newTestAPIServer(t, ctx)

	req := httptest.NewRequest(http.MethodGet, "/api/read?repo="+repo.FullName+"&path=main.go&start=2&end=3", nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()

	srv.route(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body apiReadResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.StartLine != 2 || body.EndLine != 3 || len(body.Lines) != 2 {
		t.Fatalf("unexpected line range: %#v", body)
	}
	if body.Lines[0].Text != "// UniqueNeedle" || body.Lines[1].Text != "func main() {}" {
		t.Fatalf("unexpected lines: %#v", body.Lines)
	}
}

func TestAPIReadRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newTestAPIServer(t, ctx)

	req := httptest.NewRequest(http.MethodGet, "/api/read?repo="+repo.FullName+"&path=../secret.txt", nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()

	srv.route(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPIReadRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newTestAPIServer(t, ctx)

	// A symlink inside the repo pointing outside it must not be readable, even
	// though the lexical path stays inside the repository root.
	secret := filepath.Join(filepath.Dir(repo.LocalPath), "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo.LocalPath, "evil.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Dir(repo.LocalPath), filepath.Join(repo.LocalPath, "evildir")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"evil.txt", "evildir/secret.txt"} {
		req := httptest.NewRequest(http.MethodGet, "/api/read?repo="+repo.FullName+"&path="+path, nil)
		addSessionCookie(t, srv, req, user.ID)
		rr := httptest.NewRecorder()
		srv.route(rr, req)

		if rr.Code == http.StatusOK || strings.Contains(rr.Body.String(), "top secret") {
			t.Fatalf("path %q: symlink escape not blocked: status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestAPIRequiresAuth(t *testing.T) {
	ctx := context.Background()
	srv, _, _ := newTestAPIServer(t, ctx)
	rr := httptest.NewRecorder()
	srv.route(rr, httptest.NewRequest(http.MethodGet, "/api/search?q=UniqueNeedle", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAPILineRangeCapsLargeDefaultReads(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/read?start=10", nil)
	start, end, err := apiLineRange(req, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if start != 10 || end != 509 {
		t.Fatalf("got start=%d end=%d", start, end)
	}
}

func newTestAPIServer(t *testing.T, ctx context.Context) (*Server, *store.User, *store.Repo) {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n// UniqueNeedle\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(root, "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
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
	cfg := config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos"), DataDir: root}
	ix := indexer.New(cfg, st)
	return &Server{
		store:      st,
		indexer:    ix,
		search:     codesearch.Engine{IndexDir: indexDir},
		structural: structural.NewEngine(cfg, ix),
		secret:     []byte("test-secret"),
	}, user, repo
}

func addSessionCookie(t *testing.T, srv *Server, req *http.Request, userID int64) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.setSession(rr, userID)
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie set")
	}
	req.AddCookie(cookies[0])
}

func TestAPIStructuralSearchReturnsMatches(t *testing.T) {
	ctx := context.Background()
	srv, user, repo := newTestAPIServer(t, ctx)

	// The repo must be indexed to be visible to search at all…
	if err := srv.indexer.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	// …but structural search scans the live worktree, so a file added after
	// indexing is still found.
	source := "package main\n\nfunc run() error {\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn nil\n}\n"
	if err := os.WriteFile(filepath.Join(repo.LocalPath, "err.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet,
		"/api/search?mode=structural&lang=go&q="+url.QueryEscape("if $ERR != nil { $$$ }"), nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()

	srv.route(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body apiSearchResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Engine != "structural" {
		t.Fatalf("engine=%q, want structural", body.Engine)
	}
	if body.EngineQuery != "if $ERR != nil { $$$ }" {
		t.Fatalf("engine_query=%q", body.EngineQuery)
	}
	if body.Stats.MatchCount != 1 || len(body.Files) != 1 {
		t.Fatalf("expected exactly one match, got %#v", body)
	}
	file := body.Files[0]
	if file.Repository != repo.FullName || file.Path != "err.go" {
		t.Fatalf("unexpected file: %#v", file)
	}
	if len(file.Lines) != 1 || file.Lines[0].Number != 4 {
		t.Fatalf("expected match on line 4, got %#v", file.Lines)
	}
	if got := file.Lines[0].MetaVars["$ERR"]; got != "err" {
		t.Fatalf("meta_vars[$ERR]=%q, want err", got)
	}
	if len(body.Facets) == 0 {
		t.Fatalf("expected structural facets, got %#v", body)
	}
	for _, group := range body.Facets {
		if group.Field == "language" {
			t.Fatalf("facet group %q should be omitted in structural mode", group.Field)
		}
	}
}

func TestAPIStructuralSearchRequiresLang(t *testing.T) {
	ctx := context.Background()
	srv, user, _ := newTestAPIServer(t, ctx)

	req := httptest.NewRequest(http.MethodGet,
		"/api/search?mode=structural&q="+url.QueryEscape("if $ERR != nil { $$$ }"), nil)
	addSessionCookie(t, srv, req, user.ID)
	rr := httptest.NewRecorder()

	srv.route(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body apiErrorResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error, "language") {
		t.Fatalf("error=%q, want language requirement", body.Error)
	}
}
