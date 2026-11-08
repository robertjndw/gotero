package pg_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

const lease = time.Minute

func TestImportJobs(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	bob := mustUser(t, s, "bob")
	im := s.Imports()

	_, err := im.ClaimNext(ctx(), "w1", lease)
	assert.ErrorIs(t, err, repo.ErrNotFound)

	j1, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID, SourceAPIKey: "zotkey", IncludeGroups: true})
	require.NoError(t, err)
	j2, err := im.Create(ctx(), repo.ImportJob{UserID: bob.ID, IncludeFiles: true})
	require.NoError(t, err)
	assert.Equal(t, "queued", j1.Status)
	assert.JSONEq(t, `{}`, string(j1.Cursor))
	assert.Equal(t, "zotkey", j1.SourceAPIKey)
	assert.True(t, j1.IncludeGroups)
	assert.Empty(t, j2.SourceAPIKey)

	_, err = im.Create(ctx(), repo.ImportJob{UserID: u.ID})
	assert.ErrorIs(t, err, repo.ErrImportActive, "one active import per account")
	assert.ErrorIs(t, err, repo.ErrConflict)
	active, err := im.ActiveForUser(ctx(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, j1.ID, active.ID)

	claimed, err := im.ClaimNext(ctx(), "w1", lease)
	require.NoError(t, err)
	assert.Equal(t, j1.ID, claimed.ID, "oldest first")
	assert.Equal(t, "running", claimed.Status)
	assert.NotNil(t, claimed.StartedAt)
	assert.Equal(t, "w1", claimed.ClaimedBy)
	require.NotNil(t, claimed.LeaseExpiresAt)
	assert.WithinDuration(t, time.Now().Add(lease), *claimed.LeaseExpiresAt, 30*time.Second)

	claimed.Cursor = []byte(`{"after":"x"}`)
	claimed.Progress = []byte(`{"items":10}`)
	claimed.Status = "completed"
	claimed.SourceAPIKey = ""
	fin := time.Now().UTC().Truncate(time.Microsecond)
	claimed.FinishedAt = &fin
	require.NoError(t, im.Update(ctx(), claimed))
	got, err := im.Get(ctx(), j1.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	assert.JSONEq(t, `{"after":"x"}`, string(got.Cursor))
	assert.JSONEq(t, `{"items":10}`, string(got.Progress))
	assert.Empty(t, got.SourceAPIKey, "the zotero.org key is wiped")
	assert.True(t, fin.Equal(*got.FinishedAt))

	claimed.Error = "boom"
	claimed.Status = "failed"
	require.NoError(t, im.Update(ctx(), claimed))
	got, _ = im.Get(ctx(), j1.ID)
	assert.Equal(t, "boom", got.Error)
	assert.ErrorIs(t, im.Update(ctx(), repo.ImportJob{ID: 999, Status: "failed"}), repo.ErrNotFound)

	_, err = im.ActiveForUser(ctx(), u.ID)
	assert.ErrorIs(t, err, repo.ErrNotFound)
	j3, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID})
	require.NoError(t, err, "a new import once the previous one ended")

	list, err := im.ListByUser(ctx(), u.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, j3.ID, list[0].ID, "newest first")
	_, err = im.Get(ctx(), 999)
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestImportClaimNextConcurrent(t *testing.T) {
	s, _ := newStore(t)
	const jobs = 4
	for i := range jobs {
		u := mustUser(t, s, "user"+string(rune('a'+i)))
		_, err := s.Imports().Create(ctx(), repo.ImportJob{UserID: u.ID})
		require.NoError(t, err)
	}
	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			j, err := s.Imports().ClaimNext(ctx(), "w", lease)
			if err != nil {
				assert.ErrorIs(t, err, repo.ErrNotFound)
				return
			}
			mu.Lock()
			claimed[j.ID]++
			mu.Unlock()
		})
	}
	wg.Wait()
	assert.Len(t, claimed, jobs)
	for id, n := range claimed {
		assert.Equalf(t, 1, n, "job %d claimed once", id)
	}
}

func TestImportProgressFinishAndRequeue(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	im := s.Imports()
	j, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID, SourceAPIKey: "zotkey"})
	require.NoError(t, err)

	ok, err := im.SaveProgress(ctx(), j.ID, "w1", []byte(`{"a":1}`), []byte(`{"objects":2}`))
	require.NoError(t, err)
	assert.False(t, ok, "a queued job is not running")

	claimed, err := im.ClaimNext(ctx(), "w1", lease)
	require.NoError(t, err)
	ok, err = im.SaveProgress(ctx(), claimed.ID, "w2", []byte(`{"b":1}`), []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, ok, "only the lease holder writes progress")
	ok, err = im.SaveProgress(ctx(), claimed.ID, "w1", []byte(`{"a":1}`), []byte(`{"objects":2}`))
	require.NoError(t, err)
	assert.True(t, ok)
	got, _ := im.Get(ctx(), j.ID)
	assert.JSONEq(t, `{"a":1}`, string(got.Cursor))
	assert.JSONEq(t, `{"objects":2}`, string(got.Progress))

	require.NoError(t, im.Requeue(ctx(), j.ID, "w2"))
	got, _ = im.Get(ctx(), j.ID)
	assert.Equal(t, "running", got.Status, "a stranger cannot requeue the job")
	require.NoError(t, im.Requeue(ctx(), j.ID, "w1"))
	got, _ = im.Get(ctx(), j.ID)
	assert.Equal(t, "queued", got.Status)
	assert.Empty(t, got.ClaimedBy)
	assert.Equal(t, "zotkey", got.SourceAPIKey)
	assert.JSONEq(t, `{"a":1}`, string(got.Cursor), "resume point survives")

	_, err = im.ClaimNext(ctx(), "w1", lease)
	require.NoError(t, err)
	at := time.Now().UTC()
	ok, err = im.Finish(ctx(), j.ID, "w2", "failed", "not mine", at)
	require.NoError(t, err)
	assert.False(t, ok, "only the lease holder finishes the job")
	ok, err = im.Finish(ctx(), j.ID, "w1", "completed", "careful", at)
	require.NoError(t, err)
	assert.True(t, ok)
	got, _ = im.Get(ctx(), j.ID)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, "careful", got.Error)
	assert.Empty(t, got.SourceAPIKey)
	assert.NotNil(t, got.FinishedAt)

	ok, err = im.Finish(ctx(), j.ID, "w1", "failed", "late", at)
	require.NoError(t, err)
	assert.False(t, ok, "a finished job stays finished")
}

// Only a lease that ran out is taken away; a live worker keeps its job.
func TestImportLeaseExpiry(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	im := s.Imports()
	j, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID})
	require.NoError(t, err)
	_, err = im.ClaimNext(ctx(), "w1", lease)
	require.NoError(t, err)

	n, err := im.RequeueExpired(ctx())
	require.NoError(t, err)
	assert.Zero(t, n, "a live lease is left alone")
	ok, err := im.Renew(ctx(), j.ID, "w1", lease)
	require.NoError(t, err)
	assert.True(t, ok)

	_, err = pool.Exec(ctx(), `UPDATE import_jobs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, j.ID)
	require.NoError(t, err)
	n, err = im.RequeueExpired(ctx())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	_, err = im.Claim(ctx(), j.ID, "w2", lease)
	require.NoError(t, err)
	ok, err = im.Renew(ctx(), j.ID, "w1", lease)
	require.NoError(t, err)
	assert.False(t, ok, "the old worker lost the job")
	ok, err = im.SaveProgress(ctx(), j.ID, "w1", []byte(`{}`), []byte(`{}`))
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = im.Claim(ctx(), j.ID, "w3", lease)
	assert.ErrorIs(t, err, repo.ErrNotFound, "a running job cannot be claimed")

	// Jobs left running by a version without leases count as expired.
	_, err = pool.Exec(ctx(), `UPDATE import_jobs SET lease_expires_at = NULL, claimed_by = NULL WHERE id = $1`, j.ID)
	require.NoError(t, err)
	n, err = im.RequeueExpired(ctx())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

func TestImportCancelIsConditional(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	im := s.Imports()
	j, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID, SourceAPIKey: "zotkey"})
	require.NoError(t, err)
	_, err = im.ClaimNext(ctx(), "w1", lease)
	require.NoError(t, err)

	// The worker finishes between the API's read and its cancel.
	ok, err := im.Finish(ctx(), j.ID, "w1", "completed", "", time.Now())
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = im.Cancel(ctx(), j.ID, time.Now())
	require.NoError(t, err)
	assert.False(t, ok)
	got, _ := im.Get(ctx(), j.ID)
	assert.Equal(t, "completed", got.Status, "a finished job is not relabelled")

	j2, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID, SourceAPIKey: "zotkey"})
	require.NoError(t, err)
	_, err = im.ClaimNext(ctx(), "w1", lease)
	require.NoError(t, err)
	ok, err = im.Cancel(ctx(), j2.ID, time.Now())
	require.NoError(t, err)
	assert.True(t, ok)
	got, _ = im.Get(ctx(), j2.ID)
	assert.Equal(t, "cancelled", got.Status)
	assert.Empty(t, got.SourceAPIKey)
	assert.NotNil(t, got.FinishedAt)
	ok, err = im.Finish(ctx(), j2.ID, "w1", "completed", "", time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "the worker cannot overwrite the cancel")
}
