package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

func TestServeHandshakeListsTools(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses (notification suppressed), got %d: %v", len(responses), responses)
	}

	initResult := resultOf(t, responses[0])
	if initResult["protocolVersion"] != "2025-06-18" {
		t.Fatalf("expected echoed protocol version, got %v", initResult["protocolVersion"])
	}
	serverInfo, _ := initResult["serverInfo"].(map[string]any)
	if serverInfo["name"] != serverName {
		t.Fatalf("unexpected serverInfo: %v", initResult["serverInfo"])
	}

	tools := resultOf(t, responses[1])["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"search_code", "structural_search", "read_file", "list_repos", "repo_stats"} {
		if !names[want] {
			t.Fatalf("tools/list missing %q: %v", want, names)
		}
	}
}

func TestStructuralSearchToolReturnsMatchesWithMetaVars(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"structural_search","arguments":{"pattern":"func $NAME() { $$$ }","lang":"go"}}}`,
	)
	text := toolText(t, responses[0])
	for _, want := range []string{"local/repo:main.go", "> ", "$NAME = main"} {
		if !strings.Contains(text, want) {
			t.Fatalf("structural result missing %q:\n%s", want, text)
		}
	}
}

func TestStructuralSearchToolRequiresLang(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"structural_search","arguments":{"pattern":"console.log($$$)"}}}`,
	)
	result := resultOf(t, responses[0])
	if result["isError"] != true {
		t.Fatalf("expected isError for missing lang, got %v", result)
	}
}

func TestSearchToolSchemaExposesFacetsAndExclusions(t *testing.T) {
	var searchTool map[string]any
	for _, tool := range toolDefinitions() {
		if tool["name"] == "search_code" {
			searchTool = tool
			break
		}
	}
	if searchTool == nil {
		t.Fatal("search_code tool definition missing")
	}
	schema := searchTool["inputSchema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	for _, want := range []string{"repos", "exclude_repos", "exclude_langs", "exclude_top_paths", "facets"} {
		if _, ok := properties[want]; !ok {
			t.Errorf("search_code schema missing %q", want)
		}
	}
}

func TestSearchCodeToolReturnsCitations(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_code","arguments":{"query":"UniqueNeedle"}}}`,
	)
	text := toolText(t, responses[0])
	for _, want := range []string{"UniqueNeedle", "local/repo:main.go", "> "} {
		if !strings.Contains(text, want) {
			t.Fatalf("search result missing %q:\n%s", want, text)
		}
	}
}

func TestSearchCodeToolCanReturnFacets(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_code","arguments":{"query":"UniqueNeedle","facets":true}}}`,
	)
	text := toolText(t, responses[0])
	for _, want := range []string{"## Facets", "Repository:", "Language:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("faceted search result missing %q:\n%s", want, text)
		}
	}
}

func TestSearchCodeToolExcludesRepositories(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_code","arguments":{"query":"UniqueNeedle","exclude_repos":["local/repo"]}}}`,
	)
	text := toolText(t, responses[0])
	if strings.Contains(text, "local/repo:main.go") || !strings.Contains(text, "No indexed repositories") {
		t.Fatalf("repository exclusion was not applied:\n%s", text)
	}
}

func TestReadFileToolReturnsBoundedRange(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"repo":"local/repo","path":"main.go","start":2,"end":3}}}`,
	)
	text := toolText(t, responses[0])
	if !strings.Contains(text, "local/repo:main.go lines 2-3") {
		t.Fatalf("read header missing:\n%s", text)
	}
	if !strings.Contains(text, "// UniqueNeedle") || !strings.Contains(text, "func main() {}") {
		t.Fatalf("read body missing expected lines:\n%s", text)
	}
	if strings.Contains(text, "package main") {
		t.Fatalf("read returned lines outside the requested range:\n%s", text)
	}
}

func TestReadFileToolRejectsTraversal(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"repo":"local/repo","path":"../secret.txt"}}}`,
	)
	result := resultOf(t, responses[0])
	if result["isError"] != true {
		t.Fatalf("expected isError result for traversal, got %v", result)
	}
}

func TestReadFileToolRejectsSymlinkEscape(t *testing.T) {
	ctx := context.Background()
	srv, repo := newTestServer(t, ctx)

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

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"repo":"local/repo","path":"evil.txt"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_file","arguments":{"repo":"local/repo","path":"evildir/secret.txt"}}}`,
	)
	for i, resp := range responses {
		result := resultOf(t, resp)
		if result["isError"] != true {
			t.Fatalf("request %d: expected isError result for symlink escape, got %v", i+1, result)
		}
	}
}

func TestSymbolSearchTool(t *testing.T) {
	ctags := universalCtagsPath()
	if ctags == "" {
		t.Skip("Universal Ctags not available; symbol indexing is unavailable")
	}
	ctx := context.Background()
	srv, _ := newTestServerCTags(t, ctx, ctags)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"symbol_search","arguments":{"symbol":"main"}}}`,
	)
	text := toolText(t, responses[0])
	if !strings.Contains(text, "local/repo:main.go") || !strings.Contains(text, "func main()") {
		t.Fatalf("symbol_search did not return the definition:\n%s", text)
	}
}

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

func TestFindReferencesTool(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_references","arguments":{"symbol":"UniqueNeedle"}}}`,
	)
	text := toolText(t, responses[0])
	if !strings.Contains(text, "References to `UniqueNeedle`") || !strings.Contains(text, "local/repo:main.go") {
		t.Fatalf("find_references did not return the usage:\n%s", text)
	}
}

func TestFindReferencesUsesWordBoundary(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	// "Needle" is a strict substring of "UniqueNeedle"; a word-boundary search
	// must not match it.
	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_references","arguments":{"symbol":"Needle"}}}`,
	)
	if text := toolText(t, responses[0]); !strings.Contains(text, "No matches.") {
		t.Fatalf("expected no word-boundary match for partial identifier:\n%s", text)
	}
}

func TestFileTreeTool(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"file_tree","arguments":{"repo":"local/repo"}}}`,
	)
	text := toolText(t, responses[0])
	if !strings.Contains(text, "Files in local/repo") || !strings.Contains(text, "main.go") {
		t.Fatalf("file_tree did not list the repository:\n%s", text)
	}
}

func TestListReposTool(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_repos","arguments":{}}}`,
	)
	if text := toolText(t, responses[0]); !strings.Contains(text, "local/repo") {
		t.Fatalf("list_repos missing repo:\n%s", text)
	}
}

func TestRepoStatsTool(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"repo_stats","arguments":{}}}`,
	)
	text := toolText(t, responses[0])
	// The test repo holds one Go file with three lines; the fixture is not a
	// git repository, so no "last commit" line should appear.
	for _, want := range []string{
		"# Repository stats (1 repository)",
		"Totals: 1 file · 3 lines · 44 B.",
		"## local/repo (local)",
		"- files: 1 · lines: 3 · size: 44 B",
		"- languages: Go 100.0%",
		"- indexed: just now",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("repo_stats missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "last commit") {
		t.Fatalf("non-git repo should have no last-commit line:\n%s", text)
	}
}

func TestUnknownMethodReturnsError(t *testing.T) {
	ctx := context.Background()
	srv, _ := newTestServer(t, ctx)

	responses := runSession(t, srv, ctx,
		`{"jsonrpc":"2.0","id":9,"method":"does/not/exist"}`,
	)
	var resp response
	decode(t, responses[0], &resp)
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("expected method-not-found error, got %+v", resp)
	}
}

// runSession feeds newline-delimited requests through Serve and returns each
// response line as raw JSON.
func runSession(t *testing.T, srv *Server, ctx context.Context, lines ...string) []json.RawMessage {
	t.Helper()
	var out strings.Builder
	if err := srv.Serve(ctx, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var responses []json.RawMessage
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		responses = append(responses, json.RawMessage(raw))
	}
	return responses
}

func resultOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	return resp.Result
}

func toolText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	result := resultOf(t, raw)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tool result missing content: %v", result)
	}
	return content[0].(map[string]any)["text"].(string)
}

func decode(t *testing.T, raw json.RawMessage, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func newTestServer(t *testing.T, ctx context.Context) (*Server, *store.Repo) {
	return newTestServerCTags(t, ctx, "")
}

func newTestServerCTags(t *testing.T, ctx context.Context, ctags string) (*Server, *store.Repo) {
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

	cfg := config.Config{IndexDir: filepath.Join(root, "index"), RepoDir: filepath.Join(root, "repos"), CTagsPath: ctags}
	ix := indexer.New(cfg, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	return &Server{
		Store:      st,
		Search:     codesearch.Engine{IndexDir: cfg.IndexDir},
		Structural: structural.NewEngine(cfg, ix),
		Indexer:    ix,
	}, repo
}
