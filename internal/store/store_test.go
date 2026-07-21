package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/secretbox"
)

func TestListRemoteReindexCandidates(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	// A user with a GitHub token (so GitHub repos are fetchable).
	user, err := st.UpsertUserIdentity(ctx, "github", "gh-1", "gh", "gh@example.com", "GH", "", "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	selectRepo := func(repo Repo) *Repo {
		r, err := st.UpsertRepo(ctx, repo, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SelectRepoForUser(ctx, user.ID, r.ID); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// Candidate: selected GitHub repo, user has a token.
	ghRepo := selectRepo(Repo{HostProvider: "github", HostRepoID: "r1", Name: "r1", FullName: "github/r1", CloneURL: "https://x/r1", DefaultBranch: "main"})
	// Excluded: local repos are kept fresh by the watcher, not the scheduler.
	selectRepo(Repo{HostProvider: "local", HostRepoID: "/p", Name: "l", FullName: "local/l", CloneURL: "/p", LocalPath: "/p", DefaultBranch: "HEAD"})
	// Excluded: selected private GitLab repo but the user has no GitLab token to fetch it.
	selectRepo(Repo{HostProvider: "gitlab", HostRepoID: "g1", Name: "g1", FullName: "gitlab/g1", CloneURL: "https://x/g1", DefaultBranch: "main", Private: true})
	// Excluded: not selected.
	if _, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "r2", Name: "r2", FullName: "github/r2", CloneURL: "https://x/r2", DefaultBranch: "main"}, user.ID); err != nil {
		t.Fatal(err)
	}

	candidates, err := st.ListRemoteReindexCandidates(ctx, UnixNow()+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected exactly the credentialed GitHub repo, got %#v", candidates)
	}
	if candidates[0].RepoID != ghRepo.ID || candidates[0].UserID != user.ID {
		t.Fatalf("candidate %#v, want repo %d / user %d", candidates[0], ghRepo.ID, user.ID)
	}

	// Backoff: once an attempt has been made within the window, the repo is no
	// longer due, so a failing repo isn't retried every poll.
	if _, err := st.CreateIndexJob(ctx, ghRepo.ID, "queued"); err != nil {
		t.Fatal(err)
	}
	candidates, err = st.ListRemoteReindexCandidates(ctx, UnixNow()-60)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("expected no candidates after a recent attempt, got %#v", candidates)
	}
}

func TestIndexingSettingsFallBackToDefaults(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	// Unset: return the caller's default.
	if n, err := st.MaxIndexedBranches(ctx, 100); err != nil || n != 100 {
		t.Fatalf("MaxIndexedBranches unset = %d, %v; want 100, nil", n, err)
	}
	if v, err := st.AutoExcludeInaccessible(ctx, true); err != nil || v != true {
		t.Fatalf("AutoExcludeInaccessible unset = %v, %v; want true, nil", v, err)
	}

	// Set: the stored value wins over the default, including 0 (unlimited).
	if err := st.SetMaxIndexedBranches(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := st.MaxIndexedBranches(ctx, 100); err != nil || n != 0 {
		t.Fatalf("MaxIndexedBranches after set 0 = %d, %v; want 0, nil", n, err)
	}
	if err := st.SetMaxIndexedBranches(ctx, 25); err != nil {
		t.Fatal(err)
	}
	if n, err := st.MaxIndexedBranches(ctx, 100); err != nil || n != 25 {
		t.Fatalf("MaxIndexedBranches after set 25 = %d, %v; want 25, nil", n, err)
	}
	if err := st.SetAutoExcludeInaccessible(ctx, false); err != nil {
		t.Fatal(err)
	}
	if v, err := st.AutoExcludeInaccessible(ctx, true); err != nil || v != false {
		t.Fatalf("AutoExcludeInaccessible after set false = %v, %v; want false, nil", v, err)
	}
}

func TestExcludeRepoForAccessFailure(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	userA, err := st.UpsertUserIdentity(ctx, "gitlab", "gl-a", "a", "a@example.com", "A", "", "tok-a")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := st.UpsertUserIdentity(ctx, "gitlab", "gl-b", "b", "b@example.com", "B", "", "tok-b")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, Repo{HostProvider: "gitlab", HostRepoID: "g1", Name: "g1", FullName: "gitlab/g1", CloneURL: "https://x/g1", DefaultBranch: "main", Private: true}, userA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRepo(ctx, Repo{HostProvider: "gitlab", HostRepoID: "g1", Name: "g1", FullName: "gitlab/g1", CloneURL: "https://x/g1", DefaultBranch: "main", Private: true}, userB.ID); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []int64{userA.ID, userB.ID} {
		if err := st.SelectRepoForUser(ctx, uid, repo.ID); err != nil {
			t.Fatal(err)
		}
	}

	// User A's token fails: only A's intent is dropped; B still wants it, so it
	// stays selected but carries the recorded reason.
	still, err := st.ExcludeRepoForAccessFailure(ctx, userA.ID, repo.ID, "Excluded: no access.")
	if err != nil {
		t.Fatal(err)
	}
	if !still {
		t.Fatal("repo should still be selected while user B wants it")
	}
	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Selected || fresh.LastIndexError != "Excluded: no access." {
		t.Fatalf("after A excluded: selected=%v err=%q", fresh.Selected, fresh.LastIndexError)
	}

	// User B's token fails too: no one wants it now, so it is fully deselected and
	// index state cleared, which removes it from the scheduler's candidate set.
	still, err = st.ExcludeRepoForAccessFailure(ctx, userB.ID, repo.ID, "Excluded: no access.")
	if err != nil {
		t.Fatal(err)
	}
	if still {
		t.Fatal("repo should be fully deselected once no user wants it")
	}
	fresh, err = st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Selected {
		t.Fatal("repo should be deselected")
	}
	if fresh.IndexedAt != 0 {
		t.Fatalf("indexed_at should be cleared, got %d", fresh.IndexedAt)
	}
	candidates, err := st.ListRemoteReindexCandidates(ctx, UnixNow()+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("excluded repo must not be a reindex candidate, got %#v", candidates)
	}
}

func TestPublicRemoteReindexCandidateDoesNotRequireToken(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "public", Name: "zoekt", FullName: "github.com/sourcegraph/zoekt", CloneURL: "https://github.com/sourcegraph/zoekt.git", DefaultBranch: "main", Selected: true}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SelectRepoForUser(ctx, user.ID, repo.ID); err != nil {
		t.Fatal(err)
	}

	candidates, err := st.ListRemoteReindexCandidates(ctx, UnixNow()+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].RepoID != repo.ID || candidates[0].UserID != user.ID {
		t.Fatalf("public repo should be refreshable without provider token, got %#v", candidates)
	}
}

func TestDeleteSourceForUserRemovesIdentityReposAndJobs(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	user, err := st.UpsertUserIdentity(ctx, "github", "gh-1", "gh", "gh@example.com", "GH", "", "tok-123")
	if err != nil {
		t.Fatal(err)
	}
	ghRepo, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "r1", Name: "r1", FullName: "github/r1", CloneURL: "https://x/r1", DefaultBranch: "main", Selected: true}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	glRepo, err := st.UpsertRepo(ctx, Repo{HostProvider: "gitlab", HostRepoID: "g1", Name: "g1", FullName: "gitlab/g1", CloneURL: "https://x/g1", DefaultBranch: "main", Selected: true}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIndexJob(ctx, ghRepo.ID, "queued"); err != nil {
		t.Fatal(err)
	}

	result, err := st.DeleteSourceForUser(ctx, user.ID, "github")
	if err != nil {
		t.Fatal(err)
	}
	if !result.IdentityDeleted {
		t.Fatal("expected GitHub identity to be deleted")
	}
	if len(result.Repos) != 1 || len(result.DeletedRepos) != 1 || result.DeletedRepos[0].ID != ghRepo.ID {
		t.Fatalf("unexpected delete result: %#v", result)
	}
	if _, err := st.GetAccessToken(ctx, user.ID, "github"); err == nil {
		t.Fatal("expected GitHub token to be removed")
	}
	if _, err := st.GetRepo(ctx, ghRepo.ID); err == nil {
		t.Fatal("expected GitHub repo row to be deleted")
	}
	if _, err := st.GetRepo(ctx, glRepo.ID); err != nil {
		t.Fatalf("GitLab repo should remain: %v", err)
	}
	var jobs int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM index_jobs WHERE repo_id = ?`, ghRepo.ID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("expected GitHub index jobs to cascade, got %d", jobs)
	}
}

func TestDeleteSourceForUserKeepsSharedRepo(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	user1, err := st.UpsertUserIdentity(ctx, "github", "u1", "u1", "u1@example.com", "U1", "", "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	user2, err := st.UpsertUserIdentity(ctx, "github", "u2", "u2", "u2@example.com", "U2", "", "tok-2")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "shared", Name: "shared", FullName: "github/shared", CloneURL: "https://x/shared", DefaultBranch: "main", Selected: true}, user1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "shared", Name: "shared", FullName: "github/shared", CloneURL: "https://x/shared", DefaultBranch: "main", Selected: true}, user2.ID); err != nil {
		t.Fatal(err)
	}

	result, err := st.DeleteSourceForUser(ctx, user1.ID, "github")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DeletedRepos) != 0 || len(result.SharedRepos) != 1 || result.SharedRepos[0].ID != repo.ID {
		t.Fatalf("unexpected shared delete result: %#v", result)
	}
	if _, err := st.GetRepo(ctx, repo.ID); err != nil {
		t.Fatalf("shared repo should remain: %v", err)
	}
	if ok, err := st.UserCanAccessRepo(ctx, user1.ID, repo.ID); err != nil || ok {
		t.Fatalf("user1 should no longer access repo, ok=%v err=%v", ok, err)
	}
	if ok, err := st.UserCanAccessRepo(ctx, user2.ID, repo.ID); err != nil || !ok {
		t.Fatalf("user2 should still access repo, ok=%v err=%v", ok, err)
	}
}

func TestListIndexJobsForRepo(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repoA, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "a", Name: "a", FullName: "github/a", CloneURL: "https://x/a", DefaultBranch: "main"}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "b", Name: "b", FullName: "github/b", CloneURL: "https://x/b", DefaultBranch: "main"}, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.CreateIndexJob(ctx, repoA.ID, "queued"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateIndexJob(ctx, repoB.ID, "queued"); err != nil {
		t.Fatal(err)
	}

	jobs, err := st.ListIndexJobsForRepo(ctx, user.ID, repoA.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("limit not applied: got %d jobs, want 2", len(jobs))
	}
	for _, j := range jobs {
		if j.RepoID != repoA.ID {
			t.Fatalf("repo %d job leaked into repo %d results", j.RepoID, repoA.ID)
		}
		if j.RepoName != "github/a" {
			t.Fatalf("RepoName = %q, want github/a", j.RepoName)
		}
	}
	if jobs[0].ID < jobs[1].ID {
		t.Fatalf("jobs not newest-first: ids %d then %d", jobs[0].ID, jobs[1].ID)
	}
}

func TestBranchListNormalizesBranches(t *testing.T) {
	got := NormalizeBranchList(" main, origin/dev\nrefs/heads/release/1  main ")
	if got != "main,dev,release/1" {
		t.Fatalf("got %q", got)
	}
}

func TestBranchPolicySupportsAllAndExclusions(t *testing.T) {
	policy := NormalizeBranchPolicy(" all, release/*, !wip/*, !refs/heads/tmp ")
	if policy != "*,release/*,!wip/*,!tmp" {
		t.Fatalf("policy = %q", policy)
	}
	branches, missing := ResolveBranchPolicy(policy, "main", []string{"wip/demo", "release/1", "release/2024/01", "main", "tmp", "feature"})
	if len(missing) != 0 {
		t.Fatalf("missing = %#v", missing)
	}
	if got, want := NormalizeBranchList(strings.Join(branches, ",")), "main,feature,release/1,release/2024/01"; got != want {
		t.Fatalf("branches = %q, want %q", got, want)
	}
}

func TestBranchesToIndexIgnoresStoredActualBranches(t *testing.T) {
	repo := Repo{DefaultBranch: "trunk", IndexedBranchNames: "main,dev"}
	if got, want := strings.Join(repo.BranchesToIndex(), ","), "trunk"; got != want {
		t.Fatalf("BranchesToIndex = %q, want %q", got, want)
	}
}

func TestResolveBranchPolicyReportsMissingExplicitBranches(t *testing.T) {
	branches, missing := ResolveBranchPolicy("main,release/*,missing", "main", []string{"main", "release/1"})
	if got, want := NormalizeBranchList(strings.Join(branches, ",")), "main,release/1"; got != want {
		t.Fatalf("branches = %q, want %q", got, want)
	}
	if len(missing) != 1 || missing[0] != "missing" {
		t.Fatalf("missing = %#v", missing)
	}
}

func TestMarkRepoIndexSucceededPersistsIndexResult(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	user, err := st.CreateDevUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "r1", Name: "r1", FullName: "github/r1", CloneURL: "https://x/r1", DefaultBranch: "main", Selected: true}, user.ID)
	if err != nil {
		t.Fatal(err)
	}

	statsJSON := `{"files":3,"lines":120,"bytes":4096,"languages":{"Go":{"files":2,"lines":100,"bytes":3800},"":{"files":1,"lines":20,"bytes":296}}}`
	err = st.MarkRepoIndexSucceeded(ctx, repo.ID, IndexResult{
		IndexedBranches: "main",
		Commit:          "abcdef0123456789",
		LastCommitAt:    1750000000,
		ContentStats:    statsJSON,
	})
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.IndexedCommit != "abcdef0123456789" || fresh.LastCommitAt != 1750000000 {
		t.Fatalf("commit provenance not persisted: %#v", fresh)
	}
	stats, ok := fresh.DecodeContentStats()
	if !ok {
		t.Fatalf("content stats should decode, raw = %q", fresh.ContentStats)
	}
	if stats.Files != 3 || stats.Lines != 120 || stats.Bytes != 4096 {
		t.Fatalf("stats totals = %#v", stats)
	}
	if lang := stats.Languages["Go"]; lang.Files != 2 || lang.Bytes != 3800 {
		t.Fatalf("Go language stats = %#v", stats.Languages)
	}

	if err := st.MarkRepoUnindexed(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err = st.GetRepo(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.LastCommitAt != 0 || fresh.ContentStats != "" {
		t.Fatalf("unindexing should clear stats: %#v", fresh)
	}
	if _, ok := fresh.DecodeContentStats(); ok {
		t.Fatal("DecodeContentStats should report missing stats after unindex")
	}
}

func TestAccessTokenEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	const token = "ghp_secret_at_rest_123"
	user, err := st.UpsertUserIdentity(ctx, "github", "gh-1", "gh", "gh@example.com", "GH", "", token)
	if err != nil {
		t.Fatal(err)
	}

	// The raw column must be ciphertext, never the plaintext token — this is the
	// whole point: SELECT access_token FROM identities no longer leaks credentials.
	var raw string
	if err := st.db.QueryRowContext(ctx, `SELECT access_token FROM identities WHERE provider = 'github'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == token || !secretbox.IsEncrypted(raw) {
		t.Fatalf("access_token not encrypted at rest: %q", raw)
	}

	// GetAccessToken transparently decrypts back to the original.
	got, err := st.GetAccessToken(ctx, user.ID, "github")
	if err != nil {
		t.Fatal(err)
	}
	if got != token {
		t.Fatalf("GetAccessToken = %q, want %q", got, token)
	}
}

func TestExpiredOAuthCredentialRefreshesAndRotatesAtomically(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	user, err := st.UpsertUserIdentityWithCredential(ctx, "gitlab", "gl-1", "gitlab-user", "gl@example.com", "GitLab User", "", CodeHostCredential{
		AccessToken: "access-old", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var rawAccess, rawRefresh string
	if err := st.db.QueryRowContext(ctx, `SELECT access_token, refresh_token FROM identities WHERE provider = 'gitlab'`).Scan(&rawAccess, &rawRefresh); err != nil {
		t.Fatal(err)
	}
	if rawAccess == "access-old" || rawRefresh == "refresh-old" || !secretbox.IsEncrypted(rawAccess) || !secretbox.IsEncrypted(rawRefresh) {
		t.Fatalf("OAuth credential was not encrypted at rest")
	}

	refreshes := 0
	refresher := func(_ context.Context, refreshToken string) (CodeHostCredential, error) {
		refreshes++
		if refreshToken != "refresh-old" {
			t.Fatalf("refresh token = %q", refreshToken)
		}
		return CodeHostCredential{
			AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		}, nil
	}
	got, err := st.GetAccessTokenWithRefresh(ctx, user.ID, "gitlab", refresher)
	if err != nil {
		t.Fatal(err)
	}
	if got != "access-new" || refreshes != 1 {
		t.Fatalf("first access = %q, refreshes = %d", got, refreshes)
	}
	got, err = st.GetAccessTokenWithRefresh(ctx, user.ID, "gitlab", refresher)
	if err != nil {
		t.Fatal(err)
	}
	if got != "access-new" || refreshes != 1 {
		t.Fatalf("second access = %q, refreshes = %d; fresh credential should be reused", got, refreshes)
	}
	credential, err := st.GetCodeHostCredential(ctx, user.ID, "gitlab")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access-new" || credential.RefreshToken != "refresh-new" || credential.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("stored rotated credential = %#v", credential)
	}
}

func TestEmptyTokenStaysEmpty(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	// A token-less identity (dev/OIDC) must stay literally empty so the
	// "WHERE access_token != ''" sentinels keep treating it as "no token".
	if _, err := st.UpsertUserIdentity(ctx, "dev", "local", "dev", "dev@codebeam.local", "Dev", "", ""); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.db.QueryRowContext(ctx, `SELECT access_token FROM identities WHERE provider = 'dev'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		t.Fatalf("empty token stored as %q, want empty", raw)
	}
	syncable, err := st.ListSyncableIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(syncable) != 0 {
		t.Fatalf("token-less identity treated as syncable: %#v", syncable)
	}
}

func TestEncryptExistingTokensMigration(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "codebeam.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() // nolint:errcheck

	// Seed an identity, then overwrite the column with raw plaintext to simulate a
	// row written before encryption-at-rest existed.
	user, err := st.UpsertUserIdentity(ctx, "github", "gh-1", "gh", "gh@example.com", "GH", "", "placeholder")
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "ghp_legacy_plaintext_token"
	if _, err := st.db.ExecContext(ctx, `UPDATE identities SET access_token = ? WHERE provider = 'github'`, legacy); err != nil {
		t.Fatal(err)
	}

	if err := st.encryptExistingTokens(ctx); err != nil {
		t.Fatal(err)
	}

	var raw string
	if err := st.db.QueryRowContext(ctx, `SELECT access_token FROM identities WHERE provider = 'github'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !secretbox.IsEncrypted(raw) {
		t.Fatalf("migration did not encrypt legacy token: %q", raw)
	}
	if got, err := st.GetAccessToken(ctx, user.ID, "github"); err != nil || got != legacy {
		t.Fatalf("GetAccessToken after migration = %q, %v; want %q", got, err, legacy)
	}

	// Idempotent: a second run must leave the already-encrypted value untouched.
	if err := st.encryptExistingTokens(ctx); err != nil {
		t.Fatal(err)
	}
	var raw2 string
	if err := st.db.QueryRowContext(ctx, `SELECT access_token FROM identities WHERE provider = 'github'`).Scan(&raw2); err != nil {
		t.Fatal(err)
	}
	if raw2 != raw {
		t.Fatalf("migration not idempotent: %q -> %q", raw, raw2)
	}
}

func TestCountUnreadableTokens(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codebeam.db")

	st, err := Open(ctx, dbPath, secretbox.MustNewCipher("key-one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertUserIdentity(ctx, "github", "gh-1", "gh", "gh@example.com", "GH", "", "tok-123"); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountUnreadableTokens(ctx); err != nil || n != 0 {
		t.Fatalf("CountUnreadableTokens with matching key = %d, %v; want 0", n, err)
	}
	st.Close() // nolint:errcheck

	// Reopen the same DB under a different key: the stored token is now unreadable.
	// (The startup migration leaves already-encrypted values alone, so it stays
	// sealed under key-one.)
	st2, err := Open(ctx, dbPath, secretbox.MustNewCipher("key-two"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close() // nolint:errcheck
	if n, err := st2.CountUnreadableTokens(ctx); err != nil || n != 1 {
		t.Fatalf("CountUnreadableTokens with wrong key = %d, %v; want 1", n, err)
	}
}
