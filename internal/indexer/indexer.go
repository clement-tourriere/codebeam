package indexer

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ctourriere/codebeam/internal/codehost"
	"github.com/ctourriere/codebeam/internal/config"
	"github.com/ctourriere/codebeam/internal/store"
	"github.com/sourcegraph/zoekt"
	zindex "github.com/sourcegraph/zoekt/index"
)

const (
	indexTimeout       = 20 * time.Minute
	maxIndexedFileSize = 2 << 20
)

type branchRevision struct {
	Name    string
	Ref     string
	Version string
}

type gitTreeEntry struct {
	Path string
	Blob string
	Size int64
}

type gitBranchDocument struct {
	Path     string
	Blob     string
	Branches []string
}

type Indexer struct {
	cfg          config.Config
	store        *store.Store
	ctagsPath    string
	jobSlots     chan struct{}
	activeMu     sync.Mutex
	activeByRepo map[int64]context.CancelFunc
}

func New(cfg config.Config, store *store.Store) *Indexer {
	ctagsPath := resolveCTagsPath(cfg.CTagsPath)
	if ctagsPath != "" {
		slog.Info("symbol indexing enabled", "ctags", ctagsPath)
	}
	indexConcurrency := cfg.IndexConcurrency
	if indexConcurrency <= 0 {
		indexConcurrency = 1
	}
	return &Indexer{
		cfg:          cfg,
		store:        store,
		ctagsPath:    ctagsPath,
		jobSlots:     make(chan struct{}, indexConcurrency),
		activeByRepo: map[int64]context.CancelFunc{},
	}
}

// resolveCTagsPath finds a Universal Ctags binary so the Zoekt builder can index
// symbols. An explicit path (from CODEBEAM_CTAGS_PATH / CTAGS_COMMAND) wins;
// otherwise we probe PATH and verify the binary really is Universal Ctags — the
// macOS system `ctags` is BSD ctags, which Zoekt cannot use. Returning "" leaves
// symbol indexing off (the feature degrades gracefully).
func resolveCTagsPath(explicit string) string {
	if explicit != "" {
		if isUniversalCtags(explicit) {
			return explicit
		}
		slog.Warn("configured ctags is not Universal Ctags; symbol indexing disabled", "path", explicit)
		return ""
	}
	for _, name := range []string{"universal-ctags", "ctags"} {
		if path, err := exec.LookPath(name); err == nil && isUniversalCtags(path) {
			return path
		}
	}
	return ""
}

func isUniversalCtags(path string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	return err == nil && bytes.Contains(out, []byte("Universal Ctags"))
}

// FileSymbol is a definition (function, type, method, …) found in a single file.
type FileSymbol struct {
	Name  string
	Kind  string
	Line  int
	Scope string
}

// SymbolsEnabled reports whether a usable Universal Ctags binary was found, so
// callers can hide symbol-dependent UI when it isn't.
func (i *Indexer) SymbolsEnabled() bool {
	return i.ctagsPath != ""
}

// FileSymbols runs ctags over a single file and returns its symbol definitions in
// line order, for the file-viewer outline. absPath must already be validated by
// the caller. It returns nil (never an error) when ctags is unavailable or fails,
// so the file view degrades gracefully.
func (i *Indexer) FileSymbols(ctx context.Context, absPath string) []FileSymbol {
	if i.ctagsPath == "" || absPath == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, i.ctagsPath,
		"--output-format=json", "--fields=+nK", "--sort=no", "-f", "-", absPath).Output()
	if err != nil {
		return nil
	}
	return parseCtagsJSON(out)
}

func parseCtagsJSON(out []byte) []FileSymbol {
	var symbols []FileSymbol
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var tag struct {
			Type  string `json:"_type"`
			Name  string `json:"name"`
			Line  int    `json:"line"`
			Kind  string `json:"kind"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(line, &tag); err != nil {
			continue
		}
		if tag.Type != "tag" || tag.Name == "" || tag.Line <= 0 {
			continue
		}
		symbols = append(symbols, FileSymbol{Name: tag.Name, Kind: tag.Kind, Line: tag.Line, Scope: tag.Scope})
	}
	sort.SliceStable(symbols, func(i, j int) bool { return symbols[i].Line < symbols[j].Line })
	return symbols
}

func (i *Indexer) SourceRoot(repo store.Repo) string {
	if repo.HostProvider == "local" {
		return repo.LocalPath
	}
	return filepath.Join(i.cfg.RepoDir, strconv.FormatInt(repo.ID, 10), "work")
}

func (i *Indexer) CancelReindex(repoID int64) bool {
	i.activeMu.Lock()
	cancel, ok := i.activeByRepo[repoID]
	i.activeMu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

func (i *Indexer) RemoveIndex(ctx context.Context, repoID int64) error {
	if err := removeRepoShards(i.cfg.IndexDir, repoID); err != nil {
		return err
	}
	if i.store == nil {
		return nil
	}
	return i.store.MarkRepoUnindexed(ctx, repoID)
}

// RemoveRepository removes every Codebeam-managed artifact for a repository:
// Zoekt shards, index state, and the managed clone under RepoDir. It never
// removes original local repository paths.
func (i *Indexer) RemoveRepository(ctx context.Context, repoID int64) error {
	if err := i.RemoveIndex(ctx, repoID); err != nil {
		return err
	}
	if i.cfg.RepoDir == "" {
		return nil
	}
	return os.RemoveAll(filepath.Join(i.cfg.RepoDir, strconv.FormatInt(repoID, 10)))
}

// ReconcileMissingIndexes marks repositories whose database row says "indexed"
// but whose Zoekt shard is gone as needing reindex. This protects users from
// silent zero-result searches after index-directory cleanup bugs or manual file
// deletion.
func (i *Indexer) ReconcileMissingIndexes(ctx context.Context) (int, error) {
	if i.store == nil {
		return 0, nil
	}
	repos, err := i.store.ListIndexedRepos(ctx)
	if err != nil {
		return 0, err
	}
	missing := 0
	for _, repo := range repos {
		hasShard, err := hasRepoShard(i.cfg.IndexDir, repo.ID)
		if err != nil {
			return missing, err
		}
		if hasShard {
			continue
		}
		if err := i.store.MarkRepoIndexFinished(ctx, repo.ID, "Index shard missing; reindex required."); err != nil {
			return missing, err
		}
		missing++
	}
	return missing, nil
}

func (i *Indexer) isActive(repoID int64) bool {
	i.activeMu.Lock()
	defer i.activeMu.Unlock()
	_, ok := i.activeByRepo[repoID]
	return ok
}

func (i *Indexer) registerActive(repoID int64, cancel context.CancelFunc) bool {
	i.activeMu.Lock()
	defer i.activeMu.Unlock()
	if _, ok := i.activeByRepo[repoID]; ok {
		return false
	}
	i.activeByRepo[repoID] = cancel
	return true
}

func (i *Indexer) clearActive(repoID int64) {
	i.activeMu.Lock()
	delete(i.activeByRepo, repoID)
	i.activeMu.Unlock()
}

func (i *Indexer) EnqueueReindex(ctx context.Context, repoID, userID int64) (int64, error) {
	repo, err := i.store.GetRepoForUser(ctx, repoID, userID)
	if err != nil {
		return 0, err
	}
	return i.enqueueRepo(ctx, *repo, userID)
}

// EnqueueRepoReindex queues a background reindex for a repository without
// requiring a user-scoped token lookup. It is intended for local repositories
// whose source is already on disk and can be refreshed by background services
// such as filesystem watchers.
func (i *Indexer) EnqueueRepoReindex(ctx context.Context, repoID int64) (int64, error) {
	repo, err := i.store.GetRepo(ctx, repoID)
	if err != nil {
		return 0, err
	}
	return i.enqueueRepo(ctx, *repo, 0)
}

func (i *Indexer) enqueueRepo(ctx context.Context, repo store.Repo, userID int64) (int64, error) {
	if i.isActive(repo.ID) {
		return 0, errors.New("repository is already indexing")
	}
	jobID, err := i.store.CreateIndexJob(ctx, repo.ID, "Queued for indexing")
	if err != nil {
		return 0, err
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	if !i.registerActive(repo.ID, cancel) {
		cancel()
		_ = i.store.FinishIndexJob(context.Background(), jobID, "cancelled", "Repository is already indexing.")
		return 0, errors.New("repository is already indexing")
	}
	go func() {
		defer cancel()
		defer i.clearActive(repo.ID)
		_ = i.runReindexJob(workerCtx, repo, userID, jobID)
	}()
	return jobID, nil
}

func (i *Indexer) Reindex(ctx context.Context, repoID, userID int64) error {
	repo, err := i.store.GetRepoForUser(ctx, repoID, userID)
	if err != nil {
		return err
	}
	if i.isActive(repo.ID) {
		return errors.New("repository is already indexing")
	}

	jobID, err := i.store.CreateIndexJob(ctx, repo.ID, "Waiting for index worker")
	if err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	if !i.registerActive(repo.ID, cancel) {
		cancel()
		_ = i.store.FinishIndexJob(context.Background(), jobID, "cancelled", "Repository is already indexing.")
		return errors.New("repository is already indexing")
	}
	defer cancel()
	defer i.clearActive(repo.ID)
	return i.runReindexJob(workerCtx, *repo, userID, jobID)
}

func (i *Indexer) runReindexJob(ctx context.Context, repo store.Repo, userID, jobID int64) error {
	if err := i.store.UpdateIndexJob(ctx, jobID, "queued", "Waiting for index worker"); err != nil {
		return err
	}
	release, err := i.acquireJobSlot(ctx)
	if err != nil {
		message := indexErrorMessage(err)
		_ = i.store.FinishIndexJob(context.Background(), jobID, indexFailureStatus(err), message)
		return err
	}
	defer release()

	jobCtx, cancel := context.WithTimeout(ctx, indexTimeout)
	defer cancel()

	progress := func(message string) {
		_ = i.store.UpdateIndexJob(context.Background(), jobID, "running", message)
	}

	if err := i.store.UpdateIndexJob(jobCtx, jobID, "running", "Preparing repository"); err != nil {
		return err
	}
	if err := i.store.MarkRepoIndexStarted(jobCtx, repo.ID); err != nil {
		return err
	}

	result, runErr := i.reindexRepo(jobCtx, repo, userID, progress)
	if runErr != nil {
		message := indexErrorMessage(runErr)
		// A remote repository the caller's token cannot read would keep failing on
		// every background refresh. When enabled, drop it from the selection so the
		// churn stops; the user can re-select it once access is restored.
		if i.shouldExcludeInaccessible(repo, userID, runErr) {
			reason := accessExclusionReason(message)
			if _, err := i.store.ExcludeRepoForAccessFailure(context.Background(), userID, repo.ID, reason); err == nil {
				_ = i.store.FinishIndexJob(context.Background(), jobID, "failed", reason)
				slog.Info("excluded repository after access failure", "repo", repo.FullName, "repo_id", repo.ID, "user_id", userID)
				return runErr
			}
		}
		_ = i.store.MarkRepoIndexFinished(context.Background(), repo.ID, message)
		_ = i.store.FinishIndexJob(context.Background(), jobID, indexFailureStatus(runErr), message)
		return runErr
	}

	if err := i.store.MarkRepoIndexSucceeded(jobCtx, repo.ID, result); err != nil {
		return err
	}
	return i.store.FinishIndexJob(jobCtx, jobID, "succeeded", "Indexed successfully")
}

func (i *Indexer) acquireJobSlot(ctx context.Context) (func(), error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case i.jobSlots <- struct{}{}:
		return func() { <-i.jobSlots }, nil
	}
}

func (i *Indexer) reindexRepo(ctx context.Context, repo store.Repo, userID int64, progress func(string)) (store.IndexResult, error) {
	if err := os.MkdirAll(i.cfg.IndexDir, 0o755); err != nil {
		return store.IndexResult{}, err
	}
	if err := os.MkdirAll(i.cfg.RepoDir, 0o755); err != nil {
		return store.IndexResult{}, err
	}

	sourceRoot, branches, err := i.prepareSource(ctx, repo, userID, progress)
	if err != nil {
		return store.IndexResult{}, err
	}
	if len(branches) == 0 {
		branches = []branchRevision{{Name: "HEAD", Ref: "HEAD", Version: "WORKTREE"}}
	}
	// Record the primary (first) branch's commit and its committer timestamp
	// for provenance and freshness; the WORKTREE sentinel for a non-git source
	// has neither.
	commit := branches[0].Version
	if commit == "WORKTREE" {
		commit = ""
	}
	lastCommitAt := commitTimestamp(ctx, sourceRoot, commit)
	stats, err := i.buildIndex(ctx, repo, sourceRoot, branches, lastCommitAt, progress)
	if err != nil {
		return store.IndexResult{}, err
	}
	indexedBranches := make([]string, 0, len(branches))
	for _, branch := range branches {
		indexedBranches = append(indexedBranches, branch.Name)
	}
	return store.IndexResult{
		IndexedBranches: strings.Join(indexedBranches, ","),
		Commit:          commit,
		LastCommitAt:    lastCommitAt,
		ContentStats:    encodeContentStats(stats),
	}, nil
}

// commitTimestamp returns the committer timestamp (unix seconds) of commit in
// worktree, or 0 when the source is not a git repository or the commit cannot
// be resolved. Shallow clones keep the tip commit object, so this works for
// the depth-1 clones Codebeam manages.
func commitTimestamp(ctx context.Context, worktree, commit string) int64 {
	if commit == "" {
		return 0
	}
	out, err := gitOutput(ctx, worktree, "", "log", "-1", "--format=%ct", commit)
	if err != nil {
		return 0
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || ts <= 0 {
		return 0
	}
	return ts
}

func encodeContentStats(stats store.RepoContentStats) string {
	encoded, err := json.Marshal(stats)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func (i *Indexer) prepareSource(ctx context.Context, repo store.Repo, userID int64, progress func(string)) (root string, branches []branchRevision, err error) {
	if repo.HostProvider == "local" {
		sendProgress(progress, "Inspecting local repository")
		if repo.LocalPath == "" {
			return "", nil, errors.New("local repository path is empty")
		}
		info, err := os.Stat(repo.LocalPath)
		if err != nil {
			return "", nil, err
		}
		if !info.IsDir() {
			return "", nil, fmt.Errorf("%s is not a directory", repo.LocalPath)
		}
		branch := firstBranch(repo.BranchesToIndex())
		if branch == "" {
			branch = "HEAD"
		}
		if branch == "HEAD" {
			if current, err := gitOutput(ctx, repo.LocalPath, "", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
				if current = strings.TrimSpace(current); current != "" && current != "HEAD" {
					branch = current
				}
			}
		}
		version, _ := gitOutput(ctx, repo.LocalPath, "", "rev-parse", "HEAD")
		return repo.LocalPath, []branchRevision{{Name: branch, Ref: "HEAD", Version: strings.TrimSpace(version)}}, nil
	}

	token, err := i.accessTokenForRepo(ctx, repo, userID)
	if err != nil {
		return "", nil, err
	}
	authHeader := gitAuthHeader(repo.HostProvider, token)
	candidates, err := i.resolveBranchesToIndex(ctx, repo, authHeader, progress)
	if err != nil {
		return "", nil, err
	}
	if len(candidates) == 0 {
		return "", nil, errors.New("branch selection matched no remote branches")
	}
	worktree := i.SourceRoot(repo)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return "", nil, err
	}

	// Establish a git directory first (clone the primary branch on a fresh run),
	// so the branch cap can read commit dates from the remote before we pay to
	// fetch every branch's file contents.
	primary := firstBranch(candidates)
	customBranches := strings.TrimSpace(repo.IndexedBranches) != ""
	existing := false
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err == nil {
		existing = true
	} else {
		sendProgress(progress, "Cloning repository")
		if err := os.RemoveAll(worktree); err != nil {
			return "", nil, err
		}
		if err := i.clone(ctx, worktree, authHeader, repo, primary, !customBranches); err != nil {
			return "", nil, err
		}
	}

	branchesToIndex, err := i.limitBranches(ctx, worktree, authHeader, repo, candidates, progress)
	if err != nil {
		return "", nil, err
	}
	primary = firstBranch(branchesToIndex)

	if existing {
		sendProgress(progress, fmt.Sprintf("Fetching latest changes (%d branches)", len(branchesToIndex)))
		if err := i.fetchBranches(ctx, worktree, authHeader, branchesToIndex); err != nil {
			return "", nil, err
		}
	} else if len(branchesToIndex) > 1 || primary == "HEAD" {
		if err := i.fetchBranches(ctx, worktree, authHeader, branchesToIndex); err != nil {
			return "", nil, err
		}
	}

	sendProgress(progress, "Resolving branch revisions")
	revisions, err := i.resolveBranchRevisions(ctx, worktree, authHeader, branchesToIndex)
	if err != nil {
		return "", nil, err
	}
	if len(revisions) > 0 {
		if err := checkoutBranch(ctx, worktree, authHeader, revisions[0]); err != nil {
			return "", nil, err
		}
	}
	return worktree, revisions, nil
}

func (i *Indexer) resolveBranchesToIndex(ctx context.Context, repo store.Repo, authHeader string, progress func(string)) ([]string, error) {
	if !store.BranchPolicyRequiresDiscovery(repo.IndexedBranches) {
		branches := repo.BranchesToIndex()
		if len(branches) == 0 {
			return []string{"HEAD"}, nil
		}
		return branches, nil
	}

	sendProgress(progress, "Discovering remote branches")
	available, err := listRemoteBranches(ctx, repo.CloneURL, authHeader)
	if err != nil {
		return nil, err
	}
	branches, missing := store.ResolveBranchPolicy(repo.IndexedBranches, repo.DefaultBranch, available)
	if len(missing) > 0 {
		return nil, fmt.Errorf("branch selection references missing branches: %s", strings.Join(missing, ", "))
	}
	if len(branches) == 0 {
		return nil, fmt.Errorf("branch selection %q matched no remote branches", store.BranchPolicyLabel(repo.IndexedBranches))
	}
	return branches, nil
}

func listRemoteBranches(ctx context.Context, cloneURL, authHeader string) ([]string, error) {
	out, err := gitOutput(ctx, "", authHeader, "ls-remote", "--heads", cloneURL)
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(fields[1], "refs/heads/")
		if branch != "" {
			branches = append(branches, branch)
		}
	}
	return branches, nil
}

// maxIndexedBranches returns the effective branch cap: the runtime setting when
// present, else the env/config default, always clamped to Zoekt's 64-branch
// shard limit. 0 (and anything above the Zoekt limit) means "the maximum Zoekt
// supports" — there is no true unlimited, because a shard's branch mask is a
// uint64 and the builder fails with "too many branches" past 64.
func (i *Indexer) maxIndexedBranches(ctx context.Context) int {
	limit := i.cfg.MaxIndexedBranches
	if i.store != nil {
		if fromDB, err := i.store.MaxIndexedBranches(ctx, limit); err == nil {
			limit = fromDB
		}
	}
	if limit <= 0 || limit > config.ZoektMaxBranches {
		return config.ZoektMaxBranches
	}
	return limit
}

// limitBranches caps candidates to the configured maximum, keeping the most
// recently updated branches; the default branch, when part of the set, always
// survives the cut. It returns the input unchanged when no cap applies. Recency
// is learned from a metadata-only fetch of the branch tips; when that fetch
// fails (e.g. the host rejects the request) it falls back to the discovery order
// so indexing still succeeds, just without recency ranking.
//
// Truncation only applies to discovery policies (globs, "*", excludes), where
// the user asked for "whatever matches". A selection of explicitly named
// branches that exceeds the limit fails with a clear error instead — silently
// dropping branches someone listed by hand would be worse than refusing.
func (i *Indexer) limitBranches(ctx context.Context, worktree, authHeader string, repo store.Repo, candidates []string, progress func(string)) ([]string, error) {
	limit := i.maxIndexedBranches(ctx)
	if len(candidates) <= limit {
		return candidates, nil
	}
	if !store.BranchPolicyRequiresDiscovery(repo.IndexedBranches) {
		return nil, fmt.Errorf("branch selection names %d branches; at most %d can be indexed per repository", len(candidates), limit)
	}
	total := len(candidates)
	sendProgress(progress, fmt.Sprintf("Discovered %d branches; selecting the %d most recently updated", total, limit))

	ordered, err := rankBranchesByRecency(ctx, worktree, authHeader, candidates)
	if err != nil {
		slog.Warn("rank branches by recency; keeping discovery order", "repo", repo.FullName, "error", err)
		ordered = candidates
	}
	ordered = withDefaultBranchFirst(ordered, repo.DefaultBranch)
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	sendProgress(progress, fmt.Sprintf("Indexing the %d most recently updated of %d branches (branch limit)", len(ordered), total))
	return ordered, nil
}

// rankBranchesByRecency orders branches by their tip commit date, newest first.
// It fetches only commit metadata (no file contents) so ranking a repo with many
// branches stays cheap, then reads committer dates from the remote-tracking refs.
func rankBranchesByRecency(ctx context.Context, worktree, authHeader string, branches []string) ([]string, error) {
	if err := fetchBranchMetadata(ctx, worktree, authHeader, branches); err != nil {
		return nil, err
	}
	out, err := gitOutput(ctx, worktree, "", "for-each-ref", "--sort=-committerdate", "--format=%(refname:lstrip=3)", "refs/remotes/origin")
	if err != nil {
		return nil, err
	}
	want := make(map[string]struct{}, len(branches))
	for _, branch := range branches {
		want[branch] = struct{}{}
	}
	seen := make(map[string]struct{}, len(branches))
	ranked := make([]string, 0, len(branches))
	for _, line := range strings.Split(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if _, ok := want[name]; !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		ranked = append(ranked, name)
	}
	// Keep any branch for-each-ref didn't report (e.g. one whose tip could not be
	// fetched) at the end in discovery order, so nothing is silently dropped
	// before the cap is applied.
	for _, branch := range branches {
		if _, ok := seen[branch]; !ok {
			ranked = append(ranked, branch)
			seen[branch] = struct{}{}
		}
	}
	return ranked, nil
}

// fetchBranchMetadata fetches only the tip commit object of each branch (no trees
// or blobs) into remote-tracking refs, so committer dates are available locally.
// On hosts that support partial clone this transfers a few hundred bytes per
// branch; on those that don't, git transparently falls back to a normal shallow
// fetch of the tips, which still yields the dates.
func fetchBranchMetadata(ctx context.Context, worktree, authHeader string, branches []string) error {
	var refspecs []string
	for _, branch := range branches {
		if branch == "" || branch == "HEAD" {
			continue
		}
		refspecs = append(refspecs, "+refs/heads/"+branch+":"+remoteBranchRef(branch))
	}
	if len(refspecs) == 0 {
		return nil
	}
	const chunkSize = 200
	for start := 0; start < len(refspecs); start += chunkSize {
		end := min(start+chunkSize, len(refspecs))
		args := append([]string{"fetch", "--no-tags", "--depth=1", "--filter=tree:0", "origin"}, refspecs[start:end]...)
		if _, err := gitOutput(ctx, worktree, authHeader, args...); err != nil {
			return err
		}
	}
	return nil
}

// withDefaultBranchFirst moves the default branch to the front when it is part of
// branches, so it survives a later cap. It never injects the default branch that
// a policy deliberately excluded.
func withDefaultBranchFirst(branches []string, defaultBranch string) []string {
	def := strings.TrimSpace(defaultBranch)
	if def == "" {
		return branches
	}
	idx := slices.Index(branches, def)
	if idx <= 0 {
		return branches
	}
	out := make([]string, 0, len(branches))
	out = append(out, def)
	out = append(out, branches[:idx]...)
	out = append(out, branches[idx+1:]...)
	return out
}

func (i *Indexer) accessTokenForRepo(ctx context.Context, repo store.Repo, userID int64) (string, error) {
	if userID == 0 {
		if repo.Private {
			return "", fmt.Errorf("%s requires credentials", repo.FullName)
		}
		return "", nil
	}
	token, err := i.store.GetAccessToken(ctx, userID, repo.HostProvider)
	if err == nil {
		if token == "" && repo.Private {
			return "", fmt.Errorf("%s requires credentials", repo.FullName)
		}
		return token, nil
	}
	if !repo.Private && errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return "", fmt.Errorf("load %s token: %w", repo.HostProvider, err)
}

func (i *Indexer) clone(ctx context.Context, worktree, authHeader string, repo store.Repo, branch string, allowFallback bool) error {
	args := []string{"clone", "--no-tags", "--depth=1"}
	if branch != "" && branch != "HEAD" {
		args = append(args, "--branch", branch)
	}
	args = append(args, repo.CloneURL, worktree)
	if _, err := gitOutput(ctx, "", authHeader, args...); err == nil {
		return nil
	} else if !allowFallback || branch == "" || branch == "HEAD" {
		return err
	}

	// Some hosts report stale default branch metadata. Retry without --branch when
	// the user did not explicitly choose custom branches.
	_ = os.RemoveAll(worktree)
	args = []string{"clone", "--no-tags", "--depth=1", repo.CloneURL, worktree}
	_, err := gitOutput(ctx, "", authHeader, args...)
	return err
}

func (i *Indexer) fetchBranches(ctx context.Context, worktree, authHeader string, branches []string) error {
	var refspecs []string
	fetchHead := false
	for _, branch := range branches {
		if branch == "" || branch == "HEAD" {
			fetchHead = true
			continue
		}
		refspecs = append(refspecs, "+refs/heads/"+branch+":"+remoteBranchRef(branch))
	}
	if fetchHead {
		if _, err := gitOutput(ctx, worktree, authHeader, "fetch", "--prune", "--depth=1", "origin"); err != nil {
			return err
		}
		if _, err := gitOutput(ctx, worktree, authHeader, "checkout", "--force", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	const chunkSize = 100
	for start := 0; start < len(refspecs); start += chunkSize {
		end := min(start+chunkSize, len(refspecs))
		args := append([]string{"fetch", "--prune", "--depth=1", "origin"}, refspecs[start:end]...)
		if _, err := gitOutput(ctx, worktree, authHeader, args...); err != nil {
			return err
		}
	}
	return nil
}

func (i *Indexer) resolveBranchRevisions(ctx context.Context, worktree, authHeader string, branches []string) ([]branchRevision, error) {
	revisions := make([]branchRevision, 0, len(branches))
	for _, branch := range branches {
		if branch == "" {
			branch = "HEAD"
		}
		ref := branch
		if branch != "HEAD" {
			ref = remoteBranchRef(branch)
		}
		version, err := gitOutput(ctx, worktree, authHeader, "rev-parse", ref)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, branchRevision{Name: branch, Ref: ref, Version: strings.TrimSpace(version)})
	}
	return revisions, nil
}

func checkoutBranch(ctx context.Context, worktree, authHeader string, branch branchRevision) error {
	ref := branch.Ref
	if ref == "" {
		ref = "HEAD"
	}
	_, err := gitOutput(ctx, worktree, authHeader, "checkout", "--force", ref)
	return err
}

func remoteBranchRef(branch string) string {
	return "refs/remotes/origin/" + branch
}

func firstBranch(branches []string) string {
	if len(branches) == 0 {
		return ""
	}
	return branches[0]
}

func (i *Indexer) fileConcurrency() int {
	if i.cfg.IndexFileConcurrency > 0 {
		return i.cfg.IndexFileConcurrency
	}
	return 1
}

func (i *Indexer) buildIndex(ctx context.Context, repo store.Repo, root string, branches []branchRevision, lastCommitAt int64, progress func(string)) (store.RepoContentStats, error) {
	sendProgress(progress, "Building Zoekt index")
	if err := removeRepoShards(i.cfg.IndexDir, repo.ID); err != nil {
		return store.RepoContentStats{}, err
	}

	repoBranches := make([]zoekt.RepositoryBranch, 0, len(branches))
	for _, branch := range branches {
		version := branch.Version
		if version == "" {
			version = "WORKTREE"
		}
		repoBranches = append(repoBranches, zoekt.RepositoryBranch{Name: branch.Name, Version: version})
	}
	if len(repoBranches) == 0 {
		repoBranches = []zoekt.RepositoryBranch{{Name: "HEAD", Version: "WORKTREE"}}
	}
	latestCommitDate := time.Now()
	if lastCommitAt > 0 {
		latestCommitDate = time.Unix(lastCommitAt, 0)
	}
	documentConcurrency := i.fileConcurrency()
	opts := zindex.Options{
		IndexDir:            i.cfg.IndexDir,
		ShardPrefixOverride: shardPrefix(repo.ID),
		Parallelism:         documentConcurrency,
		// When empty, Zoekt still auto-detects `universal-ctags` on PATH; when set
		// (e.g. Homebrew's keg-only ctags) it indexes symbols for sym: search.
		CTagsPath: i.ctagsPath,
		RepositoryDescription: zoekt.Repository{
			Name:                 repo.FullName,
			URL:                  repo.WebURL,
			Source:               root,
			Branches:             repoBranches,
			FileURLTemplate:      fileURLTemplate(repo),
			LineFragmentTemplate: "#L{{.LineNumber}}",
			LatestCommitDate:     latestCommitDate,
		},
	}
	builder, err := zindex.NewBuilder(opts)
	if err != nil {
		return store.RepoContentStats{}, err
	}
	finished := false
	defer func() {
		if !finished {
			_ = builder.Finish()
		}
	}()

	stats := newContentStatsAccumulator(repoBranches[0].Name)
	var fileCount int
	if repo.HostProvider == "local" {
		branchName := repoBranches[0].Name
		fileCount, err = addFilesystemDocuments(ctx, builder, root, branchName, documentConcurrency, stats, progress)
	} else {
		fileCount, err = addGitBranchDocuments(ctx, builder, root, branches, documentConcurrency, stats, progress)
	}
	if err != nil {
		return store.RepoContentStats{}, err
	}
	if len(branches) > 1 {
		sendProgress(progress, fmt.Sprintf("Writing Zoekt shard (%d file versions across %d branches)", fileCount, len(branches)))
	} else {
		sendProgress(progress, fmt.Sprintf("Writing Zoekt shard (%d files)", fileCount))
	}
	if err := builder.Finish(); err != nil {
		return store.RepoContentStats{}, err
	}
	finished = true
	return stats.stats, nil
}

// contentStatsAccumulator aggregates file/line/byte totals per language over
// the documents of the primary indexed branch, so the stats describe one
// coherent file set (the branch indexed_commit points at) even when several
// branches share the shard. It is only touched from the single-threaded
// document consumer in addDocumentsWithWorkers.
type contentStatsAccumulator struct {
	primaryBranch string
	stats         store.RepoContentStats
}

func newContentStatsAccumulator(primaryBranch string) *contentStatsAccumulator {
	return &contentStatsAccumulator{
		primaryBranch: primaryBranch,
		stats:         store.RepoContentStats{Languages: map[string]store.LanguageStat{}},
	}
}

func (a *contentStatsAccumulator) add(doc zindex.Document) {
	if !slices.Contains(doc.Branches, a.primaryBranch) {
		return
	}
	lines := int64(bytes.Count(doc.Content, []byte{'\n'}))
	if len(doc.Content) > 0 && doc.Content[len(doc.Content)-1] != '\n' {
		lines++
	}
	size := int64(len(doc.Content))
	a.stats.Files++
	a.stats.Lines += lines
	a.stats.Bytes += size
	lang := a.stats.Languages[doc.Language]
	lang.Files++
	lang.Lines += lines
	lang.Bytes += size
	a.stats.Languages[doc.Language] = lang
}

type filesystemDocument struct {
	Path string
	Name string
}

type documentResult struct {
	Index int
	Doc   zindex.Document
	Err   error
}

func addFilesystemDocuments(ctx context.Context, builder *zindex.Builder, root, branch string, workers int, stats *contentStatsAccumulator, progress func(string)) (int, error) {
	var files []filesystemDocument
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxIndexedFileSize {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filesystemDocument{Path: path, Name: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return 0, err
	}

	lastProgress := time.Now()
	return addDocumentsWithWorkers(ctx, builder, len(files), workers, func(ctx context.Context, jobs <-chan int, results chan<- documentResult) {
		for idx := range jobs {
			content, err := os.ReadFile(files[idx].Path)
			doc := zindex.Document{Name: files[idx].Name, Content: content, Branches: []string{branch}}
			// Detect the language here, in the parallel workers, with the same
			// detector the Zoekt builder runs; pre-setting it makes the builder
			// skip its own pass and lets the stats accumulator reuse it.
			zindex.DetermineLanguageIfUnknown(&doc)
			if !sendDocumentResult(ctx, results, documentResult{Index: idx, Doc: doc, Err: err}) {
				return
			}
		}
	}, func(doc zindex.Document, count int) {
		stats.add(doc)
		if count%500 == 0 && time.Since(lastProgress) > 2*time.Second {
			sendProgress(progress, fmt.Sprintf("Building Zoekt index (%d files)", count))
			lastProgress = time.Now()
		}
	})
}

func addGitBranchDocuments(ctx context.Context, builder *zindex.Builder, worktree string, branches []branchRevision, workers int, stats *contentStatsAccumulator, progress func(string)) (int, error) {
	docs := map[string]*gitBranchDocument{}
	var order []string
	branchEntries, err := listGitTrees(ctx, worktree, branches, workers, progress)
	if err != nil {
		return 0, err
	}
	for branchIndex, entries := range branchEntries {
		branch := branches[branchIndex]
		for _, entry := range entries {
			key := entry.Blob + "\x00" + entry.Path
			doc, ok := docs[key]
			if !ok {
				doc = &gitBranchDocument{Path: entry.Path, Blob: entry.Blob}
				docs[key] = doc
				order = append(order, key)
			}
			doc.Branches = append(doc.Branches, branch.Name)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		left, right := docs[order[i]], docs[order[j]]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		return strings.Join(left.Branches, ",") < strings.Join(right.Branches, ",")
	})

	orderedDocs := make([]*gitBranchDocument, 0, len(order))
	for _, key := range order {
		orderedDocs = append(orderedDocs, docs[key])
	}
	lastProgress := time.Now()
	return addDocumentsWithWorkers(ctx, builder, len(orderedDocs), workers, func(ctx context.Context, jobs <-chan int, results chan<- documentResult) {
		batch, err := newGitCatFileBatch(ctx, worktree)
		if err != nil {
			if idx, ok := <-jobs; ok {
				_ = sendDocumentResult(ctx, results, documentResult{Index: idx, Err: err})
			}
			return
		}
		defer batch.Close()
		for idx := range jobs {
			entry := orderedDocs[idx]
			content, err := batch.Cat(ctx, entry.Blob)
			doc := zindex.Document{Name: entry.Path, Content: content, Branches: entry.Branches}
			// Same pre-detection as the filesystem path: one enry pass, shared
			// by the builder and the stats accumulator.
			zindex.DetermineLanguageIfUnknown(&doc)
			if !sendDocumentResult(ctx, results, documentResult{Index: idx, Doc: doc, Err: err}) {
				return
			}
			if err != nil {
				return
			}
		}
	}, func(doc zindex.Document, count int) {
		stats.add(doc)
		if count%500 == 0 && time.Since(lastProgress) > 2*time.Second {
			sendProgress(progress, fmt.Sprintf("Building Zoekt index (%d file versions)", count))
			lastProgress = time.Now()
		}
	})
}

func addDocumentsWithWorkers(ctx context.Context, builder *zindex.Builder, count, workers int, worker func(context.Context, <-chan int, chan<- documentResult), onAdded func(zindex.Document, int)) (int, error) {
	if count == 0 {
		return 0, nil
	}
	workers = max(1, min(workers, count))
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan int)
	results := make(chan documentResult, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			worker(workerCtx, jobs, results)
		}()
	}
	go func() {
		defer close(jobs)
		for idx := 0; idx < count; idx++ {
			select {
			case <-workerCtx.Done():
				return
			case jobs <- idx:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := map[int]zindex.Document{}
	next := 0
	added := 0
	var firstErr error
	for result := range results {
		if result.Err != nil && firstErr == nil {
			firstErr = result.Err
			cancel()
		}
		if firstErr != nil {
			continue
		}
		pending[result.Index] = result.Doc
		for {
			doc, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if err := builder.Add(doc); err != nil {
				firstErr = err
				cancel()
				break
			}
			added++
			next++
			if onAdded != nil {
				onAdded(doc, added)
			}
		}
	}
	if firstErr != nil {
		return added, firstErr
	}
	if added != count {
		return added, io.ErrUnexpectedEOF
	}
	return added, nil
}

func sendDocumentResult(ctx context.Context, results chan<- documentResult, result documentResult) bool {
	select {
	case <-ctx.Done():
		return false
	case results <- result:
		return true
	}
}

func listGitTrees(ctx context.Context, worktree string, branches []branchRevision, workers int, progress func(string)) ([][]gitTreeEntry, error) {
	if len(branches) == 0 {
		return nil, nil
	}
	workers = max(1, min(workers, len(branches)))
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type treeResult struct {
		Index   int
		Entries []gitTreeEntry
		Err     error
	}
	jobs := make(chan int)
	results := make(chan treeResult, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for idx := range jobs {
				branch := branches[idx]
				entries, err := listGitTree(workerCtx, worktree, branch.Ref)
				select {
				case <-workerCtx.Done():
					return
				case results <- treeResult{Index: idx, Entries: entries, Err: err}:
				}
				if err != nil {
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for idx, branch := range branches {
			sendProgress(progress, fmt.Sprintf("Reading branch %s (%d/%d)", branch.Name, idx+1, len(branches)))
			select {
			case <-workerCtx.Done():
				return
			case jobs <- idx:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	out := make([][]gitTreeEntry, len(branches))
	var firstErr error
	for result := range results {
		if result.Err != nil && firstErr == nil {
			firstErr = result.Err
			cancel()
		}
		if firstErr == nil {
			out[result.Index] = result.Entries
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

type gitCatFileBatch struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *bytes.Buffer
}

func newGitCatFileBatch(ctx context.Context, worktree string) (*gitCatFileBatch, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir = worktree
	cmd.Env = gitEnv("")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	return &gitCatFileBatch{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdoutPipe), stderr: stderr}, nil
}

func (b *gitCatFileBatch) Cat(ctx context.Context, blob string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintln(b.stdin, blob); err != nil {
		return nil, err
	}
	header, err := b.stdout.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(strings.TrimSpace(header))
	if len(fields) == 2 && fields[1] == "missing" {
		return nil, fmt.Errorf("git object %s is missing", fields[0])
	}
	if len(fields) != 3 || fields[1] != "blob" {
		return nil, fmt.Errorf("unexpected git cat-file header %q", strings.TrimSpace(header))
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 || size > maxIndexedFileSize {
		return nil, fmt.Errorf("invalid git blob size %q", fields[2])
	}
	content := make([]byte, size)
	if _, err := io.ReadFull(b.stdout, content); err != nil {
		return nil, err
	}
	if trailing, err := b.stdout.ReadByte(); err != nil {
		return nil, err
	} else if trailing != '\n' {
		return nil, fmt.Errorf("unexpected git cat-file separator %q", trailing)
	}
	return content, nil
}

func (b *gitCatFileBatch) Close() error {
	if b == nil || b.cmd == nil {
		return nil
	}
	_ = b.stdin.Close()
	if err := b.cmd.Wait(); err != nil && b.stderr != nil && b.stderr.Len() > 0 {
		return fmt.Errorf("git cat-file failed: %w: %s", err, strings.TrimSpace(b.stderr.String()))
	}
	return nil
}

func listGitTree(ctx context.Context, worktree, ref string) ([]gitTreeEntry, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := gitOutput(ctx, worktree, "", "ls-tree", "-r", "-l", "-z", ref)
	if err != nil {
		return nil, err
	}
	var entries []gitTreeEntry
	for _, raw := range bytes.Split([]byte(out), []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		tab := bytes.IndexByte(raw, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(string(raw[:tab]))
		if len(meta) < 4 || meta[1] != "blob" || meta[0] == "120000" {
			continue
		}
		size, err := strconv.ParseInt(meta[3], 10, 64)
		if err != nil || size > maxIndexedFileSize {
			continue
		}
		path := string(raw[tab+1:])
		if shouldSkipPath(path) {
			continue
		}
		entries = append(entries, gitTreeEntry{Path: path, Blob: meta[2], Size: size})
	}
	return entries, nil
}

func shouldSkipPath(filePath string) bool {
	parts := strings.Split(filepath.ToSlash(filePath), "/")
	for _, part := range parts[:max(0, len(parts)-1)] {
		if shouldSkipDir(part) {
			return true
		}
	}
	return false
}

func removeRepoShards(indexDir string, repoID int64) error {
	matches, err := repoShardMatches(indexDir, repoID)
	if err != nil {
		return err
	}
	for _, match := range matches {
		if err := os.Remove(match); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func hasRepoShard(indexDir string, repoID int64) (bool, error) {
	matches, err := repoShardMatches(indexDir, repoID)
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func repoShardMatches(indexDir string, repoID int64) ([]string, error) {
	// Zoekt writes shards as <prefix>_vNN.NNNNN.zoekt. Include the underscore
	// separator in the glob so repo_35 never matches repo_350, repo_351, etc.
	return filepath.Glob(filepath.Join(indexDir, shardPrefix(repoID)+"_*.zoekt"))
}

func shardPrefix(repoID int64) string {
	return "repo_" + strconv.FormatInt(repoID, 10)
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "target", "dist", "build", ".venv", "venv", ".cache", ".codebeam":
		return true
	default:
		return false
	}
}

// ShouldSkipDir reports whether a directory is excluded from indexing. It is
// exported so the file-tree browser hides the same directories the indexer does.
func ShouldSkipDir(name string) bool {
	return shouldSkipDir(name)
}

func fileURLTemplate(repo store.Repo) string {
	if repo.WebURL == "" {
		return ""
	}
	if codehost.IsGitLabProvider(repo.HostProvider) {
		return strings.TrimRight(repo.WebURL, "/") + "/-/blob/{{.Version}}/{{.Path}}"
	}
	if repo.HostProvider == "github" {
		return strings.TrimRight(repo.WebURL, "/") + "/blob/{{.Version}}/{{.Path}}"
	}
	return ""
}

func gitOutput(ctx context.Context, dir, authHeader string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv(authHeader)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s failed: %w: %s", safeGitArgs(args), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitEnv(authHeader string) []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, item := range os.Environ() {
		switch {
		case strings.HasPrefix(item, "GIT_TERMINAL_PROMPT="):
			continue
		case strings.HasPrefix(item, "GIT_CONFIG_COUNT="):
			continue
		case strings.HasPrefix(item, "GIT_CONFIG_KEY_0="):
			continue
		case strings.HasPrefix(item, "GIT_CONFIG_VALUE_0="):
			continue
		default:
			env = append(env, item)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if authHeader != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0="+authHeader,
		)
	}
	return env
}

func gitAuthHeader(provider, token string) string {
	if token == "" {
		return ""
	}
	username := "oauth2"
	if provider == string(codehost.GitHub) {
		username = "x-access-token"
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	return "Authorization: Basic " + encoded
}

func safeGitArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) > 4 {
		args = args[:4]
	}
	return strings.Join(args, " ")
}

func sendProgress(progress func(string), message string) {
	if progress != nil {
		progress(message)
	}
}

func indexFailureStatus(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "failed"
}

// autoExcludeInaccessible reads the auto-exclude toggle from the runtime setting,
// falling back to the env/config default.
func (i *Indexer) autoExcludeInaccessible(ctx context.Context) bool {
	def := i.cfg.AutoExcludeInaccessible
	if i.store == nil {
		return def
	}
	enabled, err := i.store.AutoExcludeInaccessible(ctx, def)
	if err != nil {
		return def
	}
	return enabled
}

// shouldExcludeInaccessible reports whether a failed run should deselect the
// repository: only remote repos, only when a token-bearing user drove the run,
// only for access/permission errors, and only when the toggle is on.
func (i *Indexer) shouldExcludeInaccessible(repo store.Repo, userID int64, err error) bool {
	if repo.HostProvider == "local" || userID == 0 {
		return false
	}
	if !isAccessError(err) {
		return false
	}
	return i.autoExcludeInaccessible(context.Background())
}

// isAccessError reports whether a git error looks like a permission/authentication
// failure (as opposed to a transient network error, cancellation, or timeout),
// using the messages GitHub and GitLab return over HTTP.
func isAccessError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	signatures := []string{
		"authentication failed",
		"could not read username",
		"could not read password",
		"invalid username or password",
		"http basic: access denied",
		"access denied",
		"error: 403",
		"error: 401",
		"403 forbidden",
		"401 unauthorized",
		"not authorized",
		"you are not allowed",
		"permission denied",
		"don't have permission",
		"do not have permission",
		"could not be found or you don",
		"repository not found",
		"remote: not found",
	}
	for _, sig := range signatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// accessExclusionReason renders the message stored on an auto-excluded repo. The
// underlying git error is appended for context and the whole thing is trimmed.
func accessExclusionReason(detail string) string {
	base := "Excluded automatically: your account cannot access this repository. Re-select it to retry once access is restored."
	if detail = strings.TrimSpace(detail); detail == "" {
		return base
	}
	return trimMessage(base + " (" + detail + ")")
}

func indexErrorMessage(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "Indexing cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("Indexing timed out after %s.", indexTimeout)
	default:
		return trimMessage(err.Error())
	}
}

func trimMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 1200 {
		return message
	}
	return message[:1200] + "..."
}
