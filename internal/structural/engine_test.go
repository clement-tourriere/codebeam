package structural

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/indexer"
	codesearch "github.com/ctourriere/codebeam/internal/search"
	"github.com/ctourriere/codebeam/internal/store"
)

func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	ix := indexer.New(config.Config{
		IndexDir: filepath.Join(root, "index"),
		RepoDir:  filepath.Join(root, "repos"),
	}, nil)
	m := &Matcher{}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return &Engine{Matcher: m, Indexer: ix}, root
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func localRepo(dir string) store.Repo {
	return store.Repo{
		ID:           1,
		HostProvider: "local",
		Name:         "myrepo",
		FullName:     "local/myrepo",
		LocalPath:    dir,
	}
}

func TestEngineSearchWorktree(t *testing.T) {
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "main.go", "package main\n\nfunc main() {\n\tif err != nil {\n\t\tpanic(err)\n\t}\n}\n")
	writeFile(t, repoDir, "sub/util.go", "package sub\n\nfunc f() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")
	writeFile(t, repoDir, "ignore.txt", "if err != nil { }\n")
	writeFile(t, repoDir, "node_modules/dep.go", "package dep\n\nfunc g() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")

	repo := localRepo(repoDir)
	res, err := eng.Search(context.Background(), Request{
		Pattern: "if $ERR != nil { $$$ }",
		Lang:    "go",
		Allowed: []store.Repo{repo},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.FileCount != 2 || res.MatchCount != 2 {
		t.Fatalf("want 2 files / 2 matches, got %d/%d (%+v)", res.FileCount, res.MatchCount, res.Files)
	}
	first := res.Files[0]
	if first.Path != "main.go" || first.Repository != "local/myrepo" {
		t.Errorf("unexpected first file %+v", first)
	}
	if len(first.Lines) != 1 || first.Lines[0].Number != 4 {
		t.Errorf("want match on line 4 (one-based), got %+v", first.Lines)
	}
	if first.Language != "Go" {
		t.Errorf("Language = %q, want Go", first.Language)
	}
	lm := first.Lines[0]
	if got := lm.MetaVars["$ERR"]; got != "err" {
		t.Errorf("MetaVars[$ERR] = %q, want err", got)
	}
	var matched string
	for _, seg := range lm.Segments {
		if seg.Match {
			matched = seg.Text
		}
	}
	if !strings.HasPrefix(matched, "if err != nil {") {
		t.Errorf("matched segment = %q", matched)
	}
}

func TestEngineSearchPathFilter(t *testing.T) {
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "a/x.go", "package a\n\nfunc f() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")
	writeFile(t, repoDir, "b/x.go", "package b\n\nfunc f() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")

	repo := localRepo(repoDir)
	res, err := eng.Search(context.Background(), Request{
		Pattern:    "if $ERR != nil { $$$ }",
		Lang:       "go",
		PathFilter: "^a/",
		Allowed:    []store.Repo{repo},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.FileCount != 1 || res.Files[0].Path != "a/x.go" {
		t.Fatalf("want only a/x.go, got %+v", res.Files)
	}
}

func TestEngineSearchMatchCap(t *testing.T) {
	eng, root := newTestEngine(t)
	eng.MaxMatches = 3
	repoDir := filepath.Join(root, "src")
	var b strings.Builder
	b.WriteString("function f() {\n")
	for range 10 {
		b.WriteString("  console.log(1)\n")
	}
	b.WriteString("}\n")
	writeFile(t, repoDir, "app.js", b.String())

	repo := localRepo(repoDir)
	res, err := eng.Search(context.Background(), Request{
		Pattern: "console.log($$$)",
		Lang:    "js",
		Allowed: []store.Repo{repo},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.MatchCount != 3 || !res.Truncated {
		t.Fatalf("want 3 matches truncated, got %d truncated=%v", res.MatchCount, res.Truncated)
	}
}

func TestEngineSearchFacetsAndFacetFilters(t *testing.T) {
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "a/x.go", "package a\n\nfunc f() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")
	writeFile(t, repoDir, "b/y.go", "package b\n\nfunc f() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")
	repo := localRepo(repoDir)
	base := Request{
		Pattern: "if $ERR != nil { $$$ }",
		Lang:    "go",
		Allowed: []store.Repo{repo},
	}

	res, err := eng.Search(context.Background(), base)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var topGroup *struct{ a, b int }
	for _, group := range res.Facets {
		if group.Field == "language" {
			t.Errorf("facet group %q should be omitted for structural results", group.Field)
		}
		if group.Field == "top_path" {
			counts := struct{ a, b int }{}
			for _, v := range group.Values {
				switch v.Value {
				case "a":
					counts.a = v.Count
				case "b":
					counts.b = v.Count
				}
			}
			topGroup = &counts
		}
	}
	if topGroup == nil || topGroup.a != 1 || topGroup.b != 1 {
		t.Fatalf("want top_path facet a=1 b=1, got %+v (facets %+v)", topGroup, res.Facets)
	}
	if res.FacetedFileCount != 2 {
		t.Errorf("FacetedFileCount = %d, want 2", res.FacetedFileCount)
	}

	// Facet-driven filters narrow the scan.
	filtered := base
	filtered.TopPathFilter = "a"
	res, err = eng.Search(context.Background(), filtered)
	if err != nil {
		t.Fatalf("Search with top filter: %v", err)
	}
	if res.FileCount != 1 || res.Files[0].Path != "a/x.go" {
		t.Fatalf("top_path filter: want only a/x.go, got %+v", res.Files)
	}

	filtered = base
	filtered.Exclude = codesearch.FacetFilters{TopPaths: []string{"a"}}
	res, err = eng.Search(context.Background(), filtered)
	if err != nil {
		t.Fatalf("Search with excluded top filter: %v", err)
	}
	if res.FileCount != 1 || res.Files[0].Path != "b/y.go" {
		t.Fatalf("excluded top_path a: want only b/y.go, got %+v", res.Files)
	}
	var excludedA bool
	for _, group := range res.Facets {
		if group.Field == "top_path" {
			for _, value := range group.Values {
				excludedA = excludedA || (value.Value == "a" && value.Excluded)
			}
		}
	}
	if !excludedA {
		t.Fatalf("excluded top_path was not retained in facets: %+v", res.Facets)
	}

	filtered = base
	filtered.ExtFilter = ".ts"
	res, err = eng.Search(context.Background(), filtered)
	if err != nil {
		t.Fatalf("Search with ext filter: %v", err)
	}
	if res.FileCount != 0 {
		t.Fatalf("ext filter .ts should match nothing, got %+v", res.Files)
	}
}

func TestEngineSearchRepoFilterOutsideAllowed(t *testing.T) {
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "x.go", "package x\n")
	repo := localRepo(repoDir)
	_, err := eng.Search(context.Background(), Request{
		Pattern:    "if $E != nil { $$$ }",
		Lang:       "go",
		RepoFilter: "local/other",
		Allowed:    []store.Repo{repo},
	})
	if err == nil {
		t.Fatal("want error for repo filter outside allowed set")
	}
}

func TestEngineSearchRequiresLang(t *testing.T) {
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "x.go", "package x\n")
	_, err := eng.Search(context.Background(), Request{
		Pattern: "if $E != nil { $$$ }",
		Allowed: []store.Repo{localRepo(repoDir)},
	})
	if err == nil || !strings.Contains(err.Error(), "language") {
		t.Fatalf("want language-required error, got %v", err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestEngineSearchWorktreeDirtyFlagAndFilter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "committed.go", "package a\n\nfunc f() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")
	git(t, repoDir, "init", "-q", "-b", "main")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "init")
	// An uncommitted file must be badged dirty, a committed one clean.
	writeFile(t, repoDir, "wip.go", "package a\n\nfunc g() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")

	repo := localRepo(repoDir)
	res, err := eng.Search(context.Background(), Request{
		Pattern: "if $ERR != nil { $$$ }",
		Lang:    "go",
		Allowed: []store.Repo{repo},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	dirtyByPath := map[string]bool{}
	for _, f := range res.Files {
		dirtyByPath[f.Path] = f.Dirty
	}
	if len(dirtyByPath) != 2 || dirtyByPath["committed.go"] || !dirtyByPath["wip.go"] {
		t.Fatalf("dirty flags wrong: %+v", dirtyByPath)
	}

	res, err = eng.Search(context.Background(), Request{
		Pattern:     "if $ERR != nil { $$$ }",
		Lang:        "go",
		DirtyFilter: "dirty",
		Allowed:     []store.Repo{repo},
	})
	if err != nil {
		t.Fatalf("Search with dirty filter: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "wip.go" || !res.Files[0].Dirty {
		t.Fatalf("dirty filter should return only wip.go, got %+v", res.Files)
	}
}

func TestEngineSearchBranchBlobs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	eng, root := newTestEngine(t)
	repoDir := filepath.Join(root, "src")
	writeFile(t, repoDir, "main.go", "package main\n\nfunc main() {}\n")
	git(t, repoDir, "init", "-q", "-b", "main")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "init")
	git(t, repoDir, "checkout", "-q", "-b", "feature")
	writeFile(t, repoDir, "feature.go", "package main\n\nfunc feat() {\n\tif err != nil {\n\t\treturn\n\t}\n}\n")
	git(t, repoDir, "add", ".")
	git(t, repoDir, "commit", "-q", "-m", "feature work")
	git(t, repoDir, "checkout", "-q", "main")

	// feature.go is not in the working tree anymore…
	if _, err := os.Stat(filepath.Join(repoDir, "feature.go")); !os.IsNotExist(err) {
		t.Fatal("expected feature.go to be absent from worktree")
	}

	repo := localRepo(repoDir)
	// …but a branch-filtered structural search still finds it via git blobs.
	res, err := eng.Search(context.Background(), Request{
		Pattern:      "if $ERR != nil { $$$ }",
		Lang:         "go",
		BranchFilter: "feature",
		Allowed:      []store.Repo{repo},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.FileCount != 1 || res.Files[0].Path != "feature.go" {
		t.Fatalf("want feature.go from branch blobs, got %+v", res.Files)
	}
	if len(res.Files[0].Branches) != 1 || res.Files[0].Branches[0] != "feature" {
		t.Errorf("Branches = %v, want [feature]", res.Files[0].Branches)
	}

	// An unknown branch is a clear error, not a silent empty result.
	if _, err := eng.Search(context.Background(), Request{
		Pattern:      "if $ERR != nil { $$$ }",
		Lang:         "go",
		BranchFilter: "does-not-exist",
		Allowed:      []store.Repo{repo},
	}); err == nil {
		t.Fatal("want error for unknown branch")
	}
}
