package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/secretbox"
	"github.com/ctourriere/codebeam/internal/store"
)

func TestParseCtagsJSON(t *testing.T) {
	out := []byte(`{"_type":"tag","name":"NewServer","line":9,"kind":"func","scope":"main"}
{"_type":"tag","name":"Server","line":5,"kind":"struct"}
not json
{"_type":"ptag","name":"ignored","line":1}
{"_type":"tag","name":"","line":2}
{"_type":"tag","name":"MaxRetries","line":11,"kind":"const"}
`)
	symbols := parseCtagsJSON(out)
	if len(symbols) != 3 {
		t.Fatalf("expected 3 valid symbols, got %d: %#v", len(symbols), symbols)
	}
	// Sorted by line: Server(5), NewServer(9), MaxRetries(11).
	if symbols[0].Name != "Server" || symbols[1].Name != "NewServer" || symbols[2].Name != "MaxRetries" {
		t.Fatalf("symbols not in line order: %#v", symbols)
	}
	if symbols[1].Scope != "main" || symbols[1].Kind != "func" {
		t.Fatalf("unexpected fields on NewServer: %#v", symbols[1])
	}
}

func TestRemoveRepoShardsDoesNotMatchRepoIDPrefixes(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"repo_35_v16.00000.zoekt", "repo_350_v16.00000.zoekt", "repo_351_v16.00000.zoekt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("shard"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, repoID := range []int64{35, 350, 351} {
		hasShard, err := hasRepoShard(dir, repoID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasShard {
			t.Fatalf("repo %d should have a shard", repoID)
		}
	}

	if err := removeRepoShards(dir, 35); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "repo_35_v16.00000.zoekt")); !os.IsNotExist(err) {
		t.Fatalf("repo 35 shard should be removed, stat err=%v", err)
	}
	for _, name := range []string{"repo_350_v16.00000.zoekt", "repo_351_v16.00000.zoekt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s should not be removed: %v", name, err)
		}
	}
}

func TestFailedReindexKeepsPublishedShard(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n// StableNeedle\n"), 0o644); err != nil {
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
		HostProvider: "local", HostRepoID: source, Name: "source", FullName: "local/source",
		DefaultBranch: "HEAD", LocalPath: source, Selected: true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	ix := New(config.Config{IndexDir: indexDir, RepoDir: filepath.Join(root, "repos"), IndexFileConcurrency: 1}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	shards, err := repoShardMatches(indexDir, repo.ID)
	if err != nil || len(shards) != 1 {
		t.Fatalf("published shards = %#v, err = %v", shards, err)
	}
	before, err := os.ReadFile(shards[0])
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = ix.buildIndex(cancelled, *repo, source, []branchRevision{{Name: "HEAD", Version: "WORKTREE"}}, 0, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("failed build error = %v, want context.Canceled", err)
	}
	after, err := os.ReadFile(shards[0])
	if err != nil {
		t.Fatalf("previous shard disappeared after failed build: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed build replaced the previously published shard")
	}
	entries, err := os.ReadDir(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "."+shardPrefix(repo.ID)+"-build-") {
			t.Fatalf("staging directory was not cleaned up: %s", entry.Name())
		}
	}
}

func TestRemoveRepositoryDeletesIndexAndManagedClone(t *testing.T) {
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
	repo, err := st.UpsertRepo(ctx, store.Repo{HostProvider: "github", HostRepoID: "r1", Name: "r1", FullName: "github/r1", CloneURL: "https://x/r1", DefaultBranch: "main", Selected: true}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRepoIndexFinished(ctx, repo.ID, ""); err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(root, "index")
	repoDir := filepath.Join(root, "repos")
	repoID := strconv.FormatInt(repo.ID, 10)
	shardName := "repo_" + repoID + "_v16.00000.zoekt"
	if err := os.MkdirAll(filepath.Join(repoDir, repoID, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, shardName), []byte("shard"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := New(config.Config{IndexDir: indexDir, RepoDir: repoDir}, st)
	if err := ix.RemoveRepository(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(indexDir, shardName)); !os.IsNotExist(err) {
		t.Fatalf("index shard should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, repoID)); !os.IsNotExist(err) {
		t.Fatalf("managed clone should be removed, stat err=%v", err)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IndexedAt != 0 {
		t.Fatalf("repo should be marked unindexed, got indexed_at=%d", fresh.IndexedAt)
	}
}

func TestReconcileMissingIndexesMarksMissingShard(t *testing.T) {
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
	repo, err := st.UpsertRepo(ctx, store.Repo{
		HostProvider:  "local",
		HostRepoID:    "missing",
		Name:          "missing",
		FullName:      "local/missing",
		DefaultBranch: "main",
		Selected:      true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRepoIndexFinished(ctx, repo.ID, ""); err != nil {
		t.Fatal(err)
	}

	ix := New(config.Config{IndexDir: filepath.Join(root, "index")}, st)
	missing, err := ix.ReconcileMissingIndexes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if missing != 1 {
		t.Fatalf("missing = %d, want 1", missing)
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastIndexError != "Index shard missing; reindex required." {
		t.Fatalf("last index error = %q", fresh.LastIndexError)
	}
	indexed, err := st.ListIndexedRepos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexed) != 0 {
		t.Fatalf("repo with missing shard should not remain searchable: %#v", indexed)
	}
}

func TestFileSymbolsDisabledWithoutCtags(t *testing.T) {
	ix := New(config.Config{}, nil)
	ix.ctagsPath = "" // force-disabled regardless of host PATH
	if got := ix.FileSymbols(context.Background(), "/tmp/whatever.go"); got != nil {
		t.Fatalf("expected nil symbols when ctags disabled, got %#v", got)
	}
	if ix.SymbolsEnabled() {
		t.Fatal("SymbolsEnabled should be false with empty ctags path")
	}
}

func TestFileSymbols(t *testing.T) {
	ctags := universalCtagsPath()
	if ctags == "" {
		t.Skip("Universal Ctags not available")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	src := "package main\n\ntype Server struct{}\n\nfunc NewServer() *Server { return &Server{} }\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := New(config.Config{CTagsPath: ctags}, &store.Store{})
	if !ix.SymbolsEnabled() {
		t.Fatal("expected symbols enabled with configured ctags")
	}
	symbols := ix.FileSymbols(context.Background(), file)
	names := map[string]int{}
	for _, s := range symbols {
		names[s.Name] = s.Line
	}
	if names["Server"] != 3 {
		t.Fatalf("expected Server defined on line 3, got %#v", symbols)
	}
	if names["NewServer"] != 5 {
		t.Fatalf("expected NewServer defined on line 5, got %#v", symbols)
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

// gitCmd runs git in dir with a deterministic identity and (optionally) a
// fixed commit date, failing the test on error. Signing and hooks are disabled
// so the developer's global git config cannot break the test.
func gitCmd(t *testing.T, dir, commitDate string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=test", "-c", "user.email=test@example.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
		"-c", "core.hooksPath=/dev/null",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if commitDate != "" {
		cmd.Env = append(cmd.Env, "GIT_AUTHOR_DATE="+commitDate, "GIT_COMMITTER_DATE="+commitDate)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReindexLocalGitRepoCapturesCommitDateAndContentStats(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, src, "", "init")
	writeTestFile(t, src, "main.go", "package main\nfunc main() {}\n")
	writeTestFile(t, src, "README.md", "# readme\n")
	gitCmd(t, src, "", "add", ".")
	const commitEpoch = 1750000000
	gitCmd(t, src, "1750000000 +0000", "commit", "-m", "initial")
	head := gitCmd(t, src, "", "rev-parse", "HEAD")

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
		HostProvider: "local", HostRepoID: src, Name: "src", FullName: "local/src",
		CloneURL: src, LocalPath: src, DefaultBranch: "HEAD", Selected: true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	ix := New(config.Config{IndexDir: filepath.Join(root, "index"), RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}

	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IndexedCommit != head {
		t.Fatalf("indexed commit = %q, want %q", fresh.IndexedCommit, head)
	}
	if fresh.LastCommitAt != commitEpoch {
		t.Fatalf("last commit at = %d, want %d", fresh.LastCommitAt, commitEpoch)
	}
	stats, ok := fresh.DecodeContentStats()
	if !ok {
		t.Fatalf("content stats missing, raw = %q", fresh.ContentStats)
	}
	if stats.Files != 2 {
		t.Fatalf("stats files = %d, want 2: %#v", stats.Files, stats)
	}
	if stats.Lines != 3 {
		t.Fatalf("stats lines = %d, want 3: %#v", stats.Lines, stats)
	}
	if lang := stats.Languages["Go"]; lang.Files != 1 || lang.Lines != 2 {
		t.Fatalf("Go stats = %#v", stats.Languages)
	}
	if lang := stats.Languages["Markdown"]; lang.Files != 1 {
		t.Fatalf("Markdown stats = %#v", stats.Languages)
	}
}

func TestReindexGitBranchesScopesStatsToPrimaryBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, origin, "", "init")
	writeTestFile(t, origin, "main.go", "package main\nfunc main() {}\n")
	writeTestFile(t, origin, "README.md", "# readme\n")
	gitCmd(t, origin, "", "add", ".")
	const mainEpoch = 1750000000
	gitCmd(t, origin, "1750000000 +0000", "commit", "-m", "initial")
	gitCmd(t, origin, "", "branch", "-M", "main")
	gitCmd(t, origin, "", "checkout", "-b", "feature")
	writeTestFile(t, origin, "extra.py", "print('feature only')\n")
	gitCmd(t, origin, "", "add", ".")
	gitCmd(t, origin, "1750100000 +0000", "commit", "-m", "feature work")
	gitCmd(t, origin, "", "checkout", "main")

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
		HostProvider: "github", HostRepoID: "r1", Name: "r1", FullName: "github/r1",
		CloneURL: origin, DefaultBranch: "main", IndexedBranches: "main,feature", Selected: true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	ix := New(config.Config{IndexDir: filepath.Join(root, "index"), RepoDir: filepath.Join(root, "repos")}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}

	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IndexedBranchNames != "main,feature" {
		t.Fatalf("indexed branches = %q", fresh.IndexedBranchNames)
	}
	// Freshness must describe the primary branch (main), not the feature tip.
	if fresh.LastCommitAt != mainEpoch {
		t.Fatalf("last commit at = %d, want %d", fresh.LastCommitAt, mainEpoch)
	}
	stats, ok := fresh.DecodeContentStats()
	if !ok {
		t.Fatalf("content stats missing, raw = %q", fresh.ContentStats)
	}
	// Two files on main; the feature-only Python file must not be counted.
	if stats.Files != 2 {
		t.Fatalf("stats files = %d, want 2: %#v", stats.Files, stats)
	}
	if _, ok := stats.Languages["Python"]; ok {
		t.Fatalf("feature-only Python file leaked into primary-branch stats: %#v", stats.Languages)
	}
	if lang := stats.Languages["Go"]; lang.Files != 1 {
		t.Fatalf("Go stats = %#v", stats.Languages)
	}
}

func TestWithDefaultBranchFirst(t *testing.T) {
	cases := []struct {
		name     string
		branches []string
		def      string
		want     []string
	}{
		{"moves default to front", []string{"a", "main", "b"}, "main", []string{"main", "a", "b"}},
		{"already first", []string{"main", "a", "b"}, "main", []string{"main", "a", "b"}},
		{"default absent leaves order", []string{"a", "b"}, "main", []string{"a", "b"}},
		{"empty default leaves order", []string{"a", "b"}, "", []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withDefaultBranchFirst(tc.branches, tc.def)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("withDefaultBranchFirst(%v, %q) = %v, want %v", tc.branches, tc.def, got, tc.want)
			}
		})
	}
}

// TestMaxIndexedBranchesClampsToZoektLimit pins the regression that shipped a
// default of 100: Zoekt shards hold at most 64 branches (a uint64 mask), so any
// configured value above that — and 0, "no explicit limit" — must resolve to 64
// or the build fails with Zoekt's bare "too many branches".
func TestMaxIndexedBranchesClampsToZoektLimit(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ configured, want int }{
		{0, config.ZoektMaxBranches},
		{100, config.ZoektMaxBranches},
		{config.ZoektMaxBranches, config.ZoektMaxBranches},
		{10, 10},
	} {
		ix := New(config.Config{MaxIndexedBranches: tc.configured}, nil)
		if got := ix.maxIndexedBranches(ctx); got != tc.want {
			t.Errorf("maxIndexedBranches(configured=%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

// An explicit list of named branches over the limit must fail loudly rather
// than silently drop branches the user asked for by name.
func TestLimitBranchesRejectsOversizedExplicitList(t *testing.T) {
	ix := New(config.Config{MaxIndexedBranches: 2}, nil)
	repo := store.Repo{FullName: "github/r1", IndexedBranches: "a,b,c", DefaultBranch: "a"}
	_, err := ix.limitBranches(context.Background(), "", "", repo, []string{"a", "b", "c"}, nil)
	if err == nil || !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("expected a clear over-limit error for explicit branch lists, got %v", err)
	}
}

func TestIsAccessError(t *testing.T) {
	access := []string{
		"git fetch failed: exit status 128: fatal: Authentication failed for 'https://gitlab.com/x/y.git/'",
		"git clone failed: remote: HTTP Basic: Access denied\nfatal: Authentication failed",
		"git clone failed: fatal: unable to access '...': The requested URL returned error: 403",
		"git ls-remote failed: remote: The project you were looking for could not be found or you don't have permission to view it.",
		"fatal: could not read Username for 'https://gitlab.com': terminal prompts disabled",
		"git fetch failed: The requested URL returned error: 401 Unauthorized",
	}
	for _, msg := range access {
		if !isAccessError(errors.New(msg)) {
			t.Errorf("expected access error for %q", msg)
		}
	}
	notAccess := []string{
		"git fetch failed: fatal: unable to connect to github.com: Connection timed out",
		"branch selection references missing branches: feature-x",
		"context deadline exceeded",
	}
	for _, msg := range notAccess {
		if isAccessError(errors.New(msg)) {
			t.Errorf("did not expect access error for %q", msg)
		}
	}
	if isAccessError(context.Canceled) || isAccessError(context.DeadlineExceeded) {
		t.Error("cancellation and timeout must not count as access errors")
	}
}

// TestLimitBranchesKeepsMostRecent drives a real reindex of a repo whose "all
// branches" policy resolves to more branches than the cap, and asserts the cap
// keeps the default branch plus the most recently updated branches — not the
// alphabetically-first ones a naive cap would keep.
func TestLimitBranchesKeepsMostRecent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, origin, "", "init")
	writeTestFile(t, origin, "main.go", "package main\nfunc main() {}\n")
	gitCmd(t, origin, "", "add", ".")
	gitCmd(t, origin, "1750000000 +0000", "commit", "-m", "initial")
	gitCmd(t, origin, "", "branch", "-M", "main")
	// Branch tips, oldest to newest. Alphabetical order (aaa-old, mmm-mid,
	// zzz-new) is the reverse of recency, so a recency cap and a naive cap
	// disagree — letting the assertion prove recency ordering.
	for _, b := range []struct {
		name, date, file string
	}{
		{"aaa-old", "1750100000 +0000", "a.txt"},
		{"mmm-mid", "1750300000 +0000", "m.txt"},
		{"zzz-new", "1750500000 +0000", "z.txt"},
	} {
		gitCmd(t, origin, "", "checkout", "main")
		gitCmd(t, origin, "", "checkout", "-b", b.name)
		writeTestFile(t, origin, b.file, b.name+"\n")
		gitCmd(t, origin, "", "add", ".")
		gitCmd(t, origin, b.date, "commit", "-m", b.name)
	}
	gitCmd(t, origin, "", "checkout", "main")
	// Enable partial clone so the metadata-only recency fetch engages over the
	// file:// transport used below.
	gitCmd(t, origin, "", "config", "uploadpack.allowFilter", "true")

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
		HostProvider: "github", HostRepoID: "r1", Name: "r1", FullName: "github/r1",
		CloneURL: "file://" + origin, DefaultBranch: "main", IndexedBranches: "*", Selected: true,
	}, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	ix := New(config.Config{
		IndexDir:           filepath.Join(root, "index"),
		RepoDir:            filepath.Join(root, "repos"),
		MaxIndexedBranches: 2,
	}, st)
	if err := ix.Reindex(ctx, repo.ID, user.ID); err != nil {
		t.Fatal(err)
	}

	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, b := range fresh.IndexedBranchList() {
		got[b] = true
	}
	if len(got) != 2 {
		t.Fatalf("indexed %d branches, want 2: %q", len(got), fresh.IndexedBranchNames)
	}
	if !got["main"] || !got["zzz-new"] {
		t.Fatalf("expected the default branch and the newest branch, got %q", fresh.IndexedBranchNames)
	}
}
