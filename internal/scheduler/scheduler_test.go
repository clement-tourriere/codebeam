package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ctourriere/codebeam/internal/store"
)

type fakeStore struct {
	candidates  []store.ReindexCandidate
	listErr     error
	enabled     bool
	interval    time.Duration
	settingsErr error
}

func (f fakeStore) AutoIndexSettings(_ context.Context, defEnabled bool, defInterval time.Duration) (bool, time.Duration, error) {
	if f.settingsErr != nil {
		return defEnabled, defInterval, f.settingsErr
	}
	return f.enabled, f.interval, nil
}

func (f fakeStore) ListRemoteReindexCandidates(_ context.Context, _ int64) ([]store.ReindexCandidate, error) {
	return f.candidates, f.listErr
}

type fakeReindexer struct {
	calls []store.ReindexCandidate
	err   error
}

func (f *fakeReindexer) EnqueueReindex(_ context.Context, repoID, userID int64) (int64, error) {
	f.calls = append(f.calls, store.ReindexCandidate{RepoID: repoID, UserID: userID})
	return 0, f.err
}

func TestRefreshDueEnqueuesEachCandidate(t *testing.T) {
	st := fakeStore{enabled: true, interval: time.Minute, candidates: []store.ReindexCandidate{{RepoID: 1, UserID: 10}, {RepoID: 2, UserID: 20}}}
	rx := &fakeReindexer{}
	r := &RemoteRefresher{store: st, indexer: rx}

	r.refreshDue(context.Background())

	if len(rx.calls) != 2 {
		t.Fatalf("expected 2 enqueues, got %#v", rx.calls)
	}
	if rx.calls[0] != (store.ReindexCandidate{RepoID: 1, UserID: 10}) || rx.calls[1] != (store.ReindexCandidate{RepoID: 2, UserID: 20}) {
		t.Fatalf("unexpected enqueues: %#v", rx.calls)
	}
}

func TestRefreshDueSkipsWhenDisabled(t *testing.T) {
	st := fakeStore{enabled: false, interval: time.Minute, candidates: []store.ReindexCandidate{{RepoID: 1, UserID: 10}}}
	rx := &fakeReindexer{}
	r := &RemoteRefresher{store: st, indexer: rx}

	r.refreshDue(context.Background())
	if len(rx.calls) != 0 {
		t.Fatalf("expected no enqueues when auto-index is disabled, got %#v", rx.calls)
	}
}

func TestRefreshDueToleratesAlreadyIndexing(t *testing.T) {
	st := fakeStore{enabled: true, interval: time.Minute, candidates: []store.ReindexCandidate{{RepoID: 1, UserID: 10}}}
	rx := &fakeReindexer{err: errors.New("repository is already indexing")}
	r := &RemoteRefresher{store: st, indexer: rx}

	// Must not panic and must still attempt the candidate.
	r.refreshDue(context.Background())
	if len(rx.calls) != 1 {
		t.Fatalf("expected the candidate to be attempted once, got %#v", rx.calls)
	}
}
