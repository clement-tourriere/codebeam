package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/ctourriere/codebeam/internal/store"
)

type fakePermStore struct {
	identities []store.Identity
	err        error
}

func (f fakePermStore) ListSyncableIdentities(context.Context) ([]store.Identity, error) {
	return f.identities, f.err
}

type fakeResolver struct {
	calls  []string // "userID:provider"
	errFor map[int64]error
}

func (f *fakeResolver) ReconcileRepoPermissions(_ context.Context, userID int64, provider string) error {
	f.calls = append(f.calls, provider)
	if f.errFor != nil {
		return f.errFor[userID]
	}
	return nil
}

func TestSyncAllReconcilesEachIdentity(t *testing.T) {
	st := fakePermStore{identities: []store.Identity{
		{UserID: 1, Provider: "github"},
		{UserID: 2, Provider: "gitlab"},
	}}
	rs := &fakeResolver{}
	p := &PermissionSyncer{store: st, resolver: rs}

	p.syncAll(context.Background())

	if len(rs.calls) != 2 || rs.calls[0] != "github" || rs.calls[1] != "gitlab" {
		t.Fatalf("expected both identities reconciled, got %#v", rs.calls)
	}
}

func TestSyncAllContinuesPastOneFailure(t *testing.T) {
	st := fakePermStore{identities: []store.Identity{
		{UserID: 1, Provider: "github"},
		{UserID: 2, Provider: "gitlab"},
	}}
	rs := &fakeResolver{errFor: map[int64]error{1: errors.New("token expired")}}
	p := &PermissionSyncer{store: st, resolver: rs}

	// One user's failure must not stop the others.
	p.syncAll(context.Background())
	if len(rs.calls) != 2 {
		t.Fatalf("expected both identities attempted despite one error, got %#v", rs.calls)
	}
}

func TestSyncAllToleratesListError(t *testing.T) {
	st := fakePermStore{err: errors.New("db down")}
	rs := &fakeResolver{}
	p := &PermissionSyncer{store: st, resolver: rs}

	p.syncAll(context.Background()) // must not panic
	if len(rs.calls) != 0 {
		t.Fatalf("expected no reconciles when listing fails, got %#v", rs.calls)
	}
}
