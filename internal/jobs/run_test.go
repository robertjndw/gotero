package jobs

import (
	"context"
	"errors"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
)

// faultyStore makes the first two runs of each job fail, one by panicking and
// one by returning an error, and only then lets them through to Postgres.
// The scheduler has to survive both and keep the other job going.
type faultyStore struct {
	repo.Store
	prunes atomic.Int64
	gcs    atomic.Int64
}

func (s *faultyStore) Prune(ctx context.Context, now time.Time, keep time.Duration) (int64, error) {
	switch s.prunes.Add(1) {
	case 1:
		panic("prune exploded")
	case 2:
		return 0, errors.New("prune failed")
	}
	return s.Store.Prune(ctx, now, keep)
}

func (s *faultyStore) Files() repo.FileRepo { return faultyFiles{s.Store.Files(), s} }

type faultyFiles struct {
	repo.FileRepo
	s *faultyStore
}

func (f faultyFiles) OrphanBlobs(ctx context.Context, olderThan time.Time) (iter.Seq2[repo.Blob, error], error) {
	switch f.s.gcs.Add(1) {
	case 1:
		panic("gc exploded")
	case 2:
		return nil, errors.New("gc failed")
	}
	return f.FileRepo.OrphanBlobs(ctx, olderThan)
}

func runEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runInBackground starts r.Run and returns a stop function that cancels it
// and fails the test unless Run returns promptly.
func runInBackground(t *testing.T, r *Runner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	stopped := false
	stop = func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestRunSchedulesBothJobsAndSurvivesFailures(t *testing.T) {
	e := setup(t)
	fs := &faultyStore{Store: e.store}
	e.run.store = fs
	e.run.pruneEvery = 20 * time.Millisecond
	e.run.gcEvery = 30 * time.Millisecond

	countSessions := func() int {
		var n int
		if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM web_sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var userID int64
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM users`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	expired := repo.WebSession{
		IDHash: []byte("expired-session"), UserID: userID, CSRFToken: "x",
		CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := e.store.WebSessions().Create(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	orphan := e.upload(t, []byte("scheduled orphan"))
	e.age(orphan)

	stop := runInBackground(t, e.run)

	// Each job's first run panics and its second errors, so seeing its
	// effect proves the loop outlived both and ran the job a third time.
	runEventually(t, "scheduled prune to remove the expired session", func() bool { return countSessions() == 0 })
	runEventually(t, "scheduled gc to remove the orphan", func() bool { return !e.exists(t, orphan) })

	// Later ticks keep coming: a second orphan is collected too, and prune
	// keeps being called.
	prunes := fs.prunes.Load()
	second := e.upload(t, []byte("second scheduled orphan"))
	e.age(second)
	runEventually(t, "gc on a later tick", func() bool { return !e.exists(t, second) })
	runEventually(t, "prune on a later tick", func() bool { return fs.prunes.Load() > prunes+1 })

	stop()
	// Nothing may run after Run has returned.
	p, g := fs.prunes.Load(), fs.gcs.Load()
	time.Sleep(100 * time.Millisecond) // several periods of both jobs
	if fs.prunes.Load() != p || fs.gcs.Load() != g {
		t.Fatalf("jobs ran after Run returned: prunes %d->%d gcs %d->%d", p, fs.prunes.Load(), g, fs.gcs.Load())
	}
}

// With production periods nothing is due for minutes; cancelling must not
// wait for the next tick.
func TestRunReturnsPromptlyOnCancel(t *testing.T) {
	e := setup(t)
	fs := &faultyStore{Store: e.store}
	e.run.store = fs
	stop := runInBackground(t, e.run)
	start := time.Now()
	stop()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Run took %v to return", d)
	}
	if fs.prunes.Load() != 0 || fs.gcs.Load() != 0 {
		t.Fatal("a job ran although none was due")
	}
}

func TestNewUsesProductionIntervals(t *testing.T) {
	r := New(nil, nil, nil, 0)
	if r.pruneEvery != 10*time.Minute || r.gcEvery != 6*time.Hour {
		t.Fatalf("prune every %v, gc every %v", r.pruneEvery, r.gcEvery)
	}
}
