package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/secretbox"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), secretbox.MustNewCipher("codebeam-test-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestFirstUserBecomesAdmin(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	first, err := st.UpsertUserIdentity(ctx, "github", "1", "alice", "alice@x.test", "Alice", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsAdmin() {
		t.Fatalf("first user role = %q, want admin", first.Role)
	}
	second, err := st.UpsertUserIdentity(ctx, "github", "2", "bob", "bob@x.test", "Bob", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if second.IsAdmin() {
		t.Fatalf("second user role = %q, want member", second.Role)
	}
}

func TestSetUserRoleProtectsLastAdmin(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	admin, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")
	member, _ := st.UpsertUserIdentity(ctx, "github", "2", "bob", "", "", "", "")

	if err := st.SetUserRole(ctx, admin.ID, RoleMember); err == nil {
		t.Fatal("demoting the only admin should fail")
	}
	if err := st.SetUserRole(ctx, member.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserRole(ctx, admin.ID, RoleMember); err != nil {
		t.Fatalf("demoting with another admin present should work: %v", err)
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].ID != member.ID || !users[0].IsAdmin() {
		t.Fatalf("unexpected user list: %+v", users)
	}
}

func TestAPITokenLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	user, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")

	secret, token, err := st.CreateAPIToken(ctx, user.ID, "ci token", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "cbp_") {
		t.Fatalf("secret %q missing cbp_ prefix", secret)
	}
	resolved, err := st.UserForBearerToken(ctx, secret, TokenUsage{})
	if err != nil || resolved.ID != user.ID {
		t.Fatalf("token should resolve to its user: %v %+v", err, resolved)
	}
	if _, err := st.UserForBearerToken(ctx, "cbp_not-a-real-token", TokenUsage{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("bogus token should be ErrNoRows, got %v", err)
	}

	if err := st.RevokeAPIToken(ctx, user.ID, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserForBearerToken(ctx, secret, TokenUsage{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked token should be rejected, got %v", err)
	}
}

func TestAPITokenExpiry(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	user, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")

	secret, _, err := st.CreateAPIToken(ctx, user.ID, "short", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // expires_at has second precision
	if _, err := st.UserForBearerToken(ctx, secret, TokenUsage{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired token should be rejected, got %v", err)
	}
}

func TestRevokeStalePermissions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	user, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")
	other, _ := st.UpsertUserIdentity(ctx, "github", "2", "bob", "", "", "", "")

	mk := func(hostRepoID, provider string, private bool) *Repo {
		repo, err := st.UpsertRepo(ctx, Repo{
			HostProvider: provider,
			HostRepoID:   hostRepoID,
			Name:         hostRepoID,
			FullName:     provider + "/" + hostRepoID,
			Private:      private,
		}, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		return repo
	}

	keepPriv := mk("100", "github", true) // still accessible
	lostPriv := mk("200", "github", true) // no longer accessible
	pub := mk("300", "github", false)     // public: must never be pruned
	glPriv := mk("400", "gitlab", true)   // different provider: untouched
	// Bob also has access to the repo Alice is about to lose; his grant must survive.
	if _, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "200", Name: "200", FullName: "github/200", Private: true}, other.ID); err != nil {
		t.Fatal(err)
	}

	// The host now reports only repo 100 (and the public 300) as accessible.
	revoked, err := st.RevokeStalePermissions(ctx, user.ID, "github", []string{"100", "300"})
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("revoked = %d, want 1 (only the lost private repo)", revoked)
	}

	can := func(repo *Repo) bool {
		ok, err := st.UserCanAccessRepo(ctx, user.ID, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !can(keepPriv) {
		t.Error("still-accessible private repo must keep permission")
	}
	if can(lostPriv) {
		t.Error("lost private repo permission must be revoked")
	}
	if !can(pub) {
		t.Error("public repo must never be pruned")
	}
	if !can(glPriv) {
		t.Error("other-provider repo must be untouched")
	}
	if ok, _ := st.UserCanAccessRepo(ctx, other.ID, lostPriv.ID); !ok {
		t.Error("another user's permission on the same repo must survive")
	}

	// An empty accessible set revokes every remaining private github permission
	// (the user lost all access) but still spares public and other providers.
	revoked, err = st.RevokeStalePermissions(ctx, user.ID, "github", nil)
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatalf("empty-set revoked = %d, want 1 (repo 100)", revoked)
	}
	if can(keepPriv) {
		t.Error("private repo should be revoked when host reports no access")
	}
	if !can(pub) || !can(glPriv) {
		t.Error("public and other-provider repos must survive an empty accessible set")
	}
}

func TestRepoSelectionReferenceCounting(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	alice, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")
	bob, _ := st.UpsertUserIdentity(ctx, "github", "2", "bob", "", "", "", "")

	// A shared repo both users can access.
	repo, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "1", Name: "r", FullName: "github/r", Private: true}, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "1", Name: "r", FullName: "github/r", Private: true}, bob.ID); err != nil {
		t.Fatal(err)
	}

	selected := func() bool {
		r, err := st.GetRepo(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r.Selected
	}

	// Both select it.
	if err := st.SelectRepoForUser(ctx, alice.ID, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SelectRepoForUser(ctx, bob.ID, repo.ID); err != nil {
		t.Fatal(err)
	}
	if !selected() {
		t.Fatal("repo should be selected after either user selects it")
	}

	// Alice removes it: Bob still wants it, so it stays selected and the caller
	// is told not to drop the shared index.
	stillSelected, err := st.DeselectRepoForUser(ctx, alice.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stillSelected {
		t.Fatal("DeselectRepoForUser should report the repo is still wanted by Bob")
	}
	if !selected() {
		t.Fatal("repo must stay selected while Bob still wants it")
	}

	// Bob removes it too: now nobody wants it, so it is deselected and the
	// caller may remove the index.
	stillSelected, err = st.DeselectRepoForUser(ctx, bob.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillSelected {
		t.Fatal("DeselectRepoForUser should report no remaining selectors")
	}
	if selected() {
		t.Fatal("repo must be deselected once no user wants it")
	}
}

func TestGrantExistingRepoPermissions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	owner, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")
	newcomer, _ := st.UpsertUserIdentity(ctx, "github", "2", "bob", "", "", "", "")

	// Alice adds two private github repos; Bob has no access yet.
	shared, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "100", Name: "r", FullName: "github/r", Private: true}, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertRepo(ctx, Repo{HostProvider: "github", HostRepoID: "101", Name: "s", FullName: "github/s", Private: true}, owner.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.UserCanAccessRepo(ctx, newcomer.ID, shared.ID); ok {
		t.Fatal("newcomer should not have access before the grant")
	}

	// The host says Bob can now reach repo 100 and a repo 999 that isn't in
	// Codebeam. Only the existing repo gets a grant; nothing is imported.
	granted, err := st.GrantExistingRepoPermissions(ctx, newcomer.ID, "github", []string{"100", "999"})
	if err != nil {
		t.Fatal(err)
	}
	if granted != 1 {
		t.Fatalf("granted = %d, want 1 (only the existing repo 100)", granted)
	}
	if ok, _ := st.UserCanAccessRepo(ctx, newcomer.ID, shared.ID); !ok {
		t.Fatal("newcomer should have access after the grant")
	}
	// Re-running is idempotent (INSERT OR IGNORE).
	granted, err = st.GrantExistingRepoPermissions(ctx, newcomer.ID, "github", []string{"100"})
	if err != nil {
		t.Fatal(err)
	}
	if granted != 0 {
		t.Fatalf("re-grant should be a no-op, got %d", granted)
	}
	// No repo row was created for the not-present id 999.
	repos, err := st.ListReposForUser(ctx, newcomer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("newcomer should see exactly the one granted repo, got %d", len(repos))
	}
}

func TestOAuthCodeSingleUse(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	user, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")
	client, _, err := st.CreateOAuthClient(ctx, "Claude Code", []string{"http://127.0.0.1:5555/callback"}, false)
	if err != nil {
		t.Fatal(err)
	}

	code, err := st.CreateOAuthCode(ctx, OAuthCode{
		ClientID:      client.ClientID,
		UserID:        user.ID,
		RedirectURI:   "http://127.0.0.1:5555/callback",
		CodeChallenge: "challenge",
		Resource:      "http://localhost:8080/mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ConsumeOAuthCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if got.UserID != user.ID || got.CodeChallenge != "challenge" {
		t.Fatalf("unexpected code payload: %+v", got)
	}
	if _, err := st.ConsumeOAuthCode(ctx, code); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second consumption must fail, got %v", err)
	}
}

func TestOAuthTokenIssueAndRefreshRotation(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	user, _ := st.UpsertUserIdentity(ctx, "github", "1", "alice", "", "", "", "")
	client, _, err := st.CreateOAuthClient(ctx, "Claude Code", []string{"http://127.0.0.1:5555/callback"}, false)
	if err != nil {
		t.Fatal(err)
	}

	access, refresh, expiresIn, err := st.IssueOAuthTokens(ctx, client.ClientID, user.ID, "codebeam", "http://localhost:8080/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if expiresIn <= 0 || !strings.HasPrefix(access, "cbo_") || !strings.HasPrefix(refresh, "cbr_") {
		t.Fatalf("unexpected token shapes: %q %q %d", access, refresh, expiresIn)
	}
	resolved, err := st.UserForBearerToken(ctx, access, TokenUsage{IP: "203.0.113.7", UserAgent: "claude-code/2.0"})
	if err != nil || resolved.ID != user.ID {
		t.Fatalf("access token should resolve: %v", err)
	}

	newAccess, newRefresh, _, err := st.RefreshOAuthTokens(ctx, client.ClientID, refresh)
	if err != nil {
		t.Fatal(err)
	}
	if newAccess == access || newRefresh == refresh {
		t.Fatal("refresh must rotate both tokens")
	}
	if _, err := st.UserForBearerToken(ctx, access, TokenUsage{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old access token must stop working after rotation, got %v", err)
	}
	if _, _, _, err := st.RefreshOAuthTokens(ctx, client.ClientID, refresh); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old refresh token must stop working after rotation, got %v", err)
	}
	if _, _, _, err := st.RefreshOAuthTokens(ctx, "cbc_other-client", newRefresh); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("refresh bound to another client must fail, got %v", err)
	}

	grants, err := st.ListOAuthGrants(ctx, user.ID)
	if err != nil || len(grants) != 1 || grants[0].ClientName != "Claude Code" {
		t.Fatalf("grants: %v %+v", err, grants)
	}
	g := grants[0]
	if g.ClientID != client.ClientID || g.ClientIDSuffix() == "" {
		t.Fatalf("grant should carry its client id, got %+v", g)
	}
	if g.UseCount != 1 || g.LastUsedIP != "203.0.113.7" || g.LastUsedUA != "claude-code/2.0" {
		t.Fatalf("grant should record usage provenance, got %+v", g)
	}
	if err := st.RevokeOAuthGrant(ctx, user.ID, grants[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserForBearerToken(ctx, newAccess, TokenUsage{}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("revoked grant's access token must be rejected, got %v", err)
	}
}
