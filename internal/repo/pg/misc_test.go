package pg_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestLoginSessions(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	required := u.ID
	hash := []byte("tok")
	require.NoError(t, s.LoginSessions().Create(ctx(), repo.LoginSession{
		TokenHash: hash, RequiredUserID: &required, ClientName: "Zotero", ExpiresAt: exp,
	}))
	assert.ErrorIs(t, s.LoginSessions().Create(ctx(), repo.LoginSession{TokenHash: hash, ExpiresAt: exp}), repo.ErrConflict)

	got, err := s.LoginSessions().Get(ctx(), hash)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, &required, got.RequiredUserID)
	assert.Nil(t, got.UserID)
	assert.Empty(t, got.APIKey)
	assert.True(t, exp.Equal(got.ExpiresAt))

	require.NoError(t, s.LoginSessions().Complete(ctx(), hash, u.ID, "KEY123"))
	got, _ = s.LoginSessions().Get(ctx(), hash)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, "KEY123", got.APIKey)
	require.NotNil(t, got.UserID)

	assert.ErrorIs(t, s.LoginSessions().Complete(ctx(), hash, u.ID, "OTHER"), repo.ErrConflict)
	assert.ErrorIs(t, s.LoginSessions().Complete(ctx(), []byte("ghost"), u.ID, "K"), repo.ErrNotFound)
	_, err = s.LoginSessions().Get(ctx(), []byte("ghost"))
	assert.ErrorIs(t, err, repo.ErrNotFound)

	h2 := []byte("tok2")
	require.NoError(t, s.LoginSessions().Create(ctx(), repo.LoginSession{TokenHash: h2, ExpiresAt: exp}))
	require.NoError(t, s.LoginSessions().Cancel(ctx(), h2))
	require.NoError(t, s.LoginSessions().Cancel(ctx(), h2), "cancel is idempotent")
	got, _ = s.LoginSessions().Get(ctx(), h2)
	assert.Equal(t, "cancelled", got.Status)
	assert.ErrorIs(t, s.LoginSessions().Complete(ctx(), h2, u.ID, "K"), repo.ErrConflict)
	assert.ErrorIs(t, s.LoginSessions().Cancel(ctx(), []byte("ghost")), repo.ErrNotFound)
}

func TestWebSessions(t *testing.T) {
	s, _ := newStore(t)
	u, v := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	for _, ws := range []repo.WebSession{
		{IDHash: []byte("a1"), UserID: u.ID, CSRFToken: "c1", ExpiresAt: exp},
		{IDHash: []byte("a2"), UserID: u.ID, CSRFToken: "c2", ExpiresAt: exp},
		{IDHash: []byte("b1"), UserID: v.ID, CSRFToken: "c3", ExpiresAt: exp},
	} {
		require.NoError(t, s.WebSessions().Create(ctx(), ws))
	}
	got, err := s.WebSessions().Get(ctx(), []byte("a1"))
	require.NoError(t, err)
	assert.Equal(t, "c1", got.CSRFToken)
	assert.Equal(t, u.ID, got.UserID)

	require.NoError(t, s.WebSessions().Delete(ctx(), []byte("a1")))
	_, err = s.WebSessions().Get(ctx(), []byte("a1"))
	assert.ErrorIs(t, err, repo.ErrNotFound)

	require.NoError(t, s.WebSessions().DeleteForUser(ctx(), u.ID))
	_, err = s.WebSessions().Get(ctx(), []byte("a2"))
	assert.ErrorIs(t, err, repo.ErrNotFound)
	_, err = s.WebSessions().Get(ctx(), []byte("b1"))
	assert.NoError(t, err)
}

func TestInvites(t *testing.T) {
	s, _ := newStore(t)
	admin := mustUser(t, s, "admin")
	user := mustUser(t, s, "newbie")
	now := time.Now().UTC().Truncate(time.Microsecond)

	inv, err := s.Invites().Create(ctx(), repo.Invite{
		TokenHash: []byte("t1"), Email: "n@example.com", IsAdmin: true, CreatedBy: &admin.ID, ExpiresAt: now.Add(time.Hour),
	})
	require.NoError(t, err)
	assert.NotZero(t, inv.ID)
	got, err := s.Invites().GetByHash(ctx(), []byte("t1"))
	require.NoError(t, err)
	assert.Equal(t, inv, got)
	assert.Equal(t, "n@example.com", got.Email)
	_, err = s.Invites().GetByHash(ctx(), []byte("nope"))
	assert.ErrorIs(t, err, repo.ErrNotFound)

	noEmail, err := s.Invites().Create(ctx(), repo.Invite{TokenHash: []byte("t2"), ExpiresAt: now.Add(-time.Hour)})
	require.NoError(t, err)
	assert.Empty(t, noEmail.Email)

	list, err := s.Invites().List(ctx(), 10, 0)
	require.NoError(t, err)
	assert.Len(t, list, 2)
	list, _ = s.Invites().List(ctx(), 10, inv.ID)
	assert.Len(t, list, 1)

	assert.ErrorIs(t, s.Invites().Use(ctx(), noEmail.ID, user.ID, now), repo.ErrConflict, "expired")
	assert.ErrorIs(t, s.Invites().Use(ctx(), 999, user.ID, now), repo.ErrConflict, "unknown")

	// Only one of several racing users can redeem an invite.
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Invites().Use(ctx(), inv.ID, user.ID, now) == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, wins)
	got, _ = s.Invites().GetByHash(ctx(), []byte("t1"))
	require.NotNil(t, got.UsedBy)
	assert.Equal(t, user.ID, *got.UsedBy)
	require.NotNil(t, got.UsedAt)

	require.NoError(t, s.Invites().Delete(ctx(), inv.ID))
	assert.ErrorIs(t, s.Invites().Delete(ctx(), inv.ID), repo.ErrNotFound)
}

func TestInstanceSettings(t *testing.T) {
	s, _ := newStore(t)
	type cfg struct {
		Open  bool     `json:"open"`
		Names []string `json:"names"`
	}
	var got cfg
	ok, err := s.Settings().Get(ctx(), "signup", &got)
	require.NoError(t, err)
	assert.False(t, ok)

	require.NoError(t, s.Settings().Set(ctx(), "signup", cfg{Open: true, Names: []string{"a"}}))
	ok, err = s.Settings().Get(ctx(), "signup", &got)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, cfg{Open: true, Names: []string{"a"}}, got)

	require.NoError(t, s.Settings().Set(ctx(), "signup", cfg{}))
	got = cfg{}
	ok, _ = s.Settings().Get(ctx(), "signup", &got)
	assert.True(t, ok)
	assert.False(t, got.Open)

	var n int
	require.NoError(t, s.Settings().Set(ctx(), "count", 42))
	ok, err = s.Settings().Get(ctx(), "count", &n)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 42, n)

	var wrong cfg
	_, err = s.Settings().Get(ctx(), "count", &wrong)
	assert.Error(t, err, "type mismatch is reported")
	assert.Error(t, s.Settings().Set(ctx(), "bad", make(chan int)))
}

func TestBlobsAndUploadTokens(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	f := s.Files()
	const md5 = "dddddddddddddddddddddddddddddddd"

	ok, err := f.BlobExists(ctx(), md5)
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, f.PutBlob(ctx(), repo.Blob{MD5: md5, Size: 7}))
	require.NoError(t, f.PutBlob(ctx(), repo.Blob{MD5: md5, Size: 7}), "idempotent")
	ok, _ = f.BlobExists(ctx(), md5)
	assert.True(t, ok)
	assert.Error(t, f.PutBlob(ctx(), repo.Blob{MD5: "NOTHEX", Size: 1}), "md5 format is enforced")

	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil)}))
		return tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "AAAAAAAA", MD5: md5, Filename: "a", Size: 7})
	})
	in, err := f.BlobReferencedIn(ctx(), md5, []int64{u.LibraryID})
	require.NoError(t, err)
	assert.True(t, in)
	in, _ = f.BlobReferencedIn(ctx(), md5, []int64{u.LibraryID + 100})
	assert.False(t, in, "other libraries' files are not visible")
	in, _ = f.BlobReferencedIn(ctx(), md5, nil)
	assert.False(t, in)

	deleted, err := f.DeleteOrphanBlob(ctx(), md5, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, deleted, "referenced blobs are not deleted")

	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	tok := repo.UploadToken{
		UploadKey: "key1", LibraryID: u.LibraryID, ItemKey: "AAAAAAAA", MD5: md5, Filename: "a.pdf",
		Size: 7, MtimeMS: 1700000000000, Compressed: true, ExpiresAt: exp,
	}
	require.NoError(t, f.CreateUploadToken(ctx(), tok))
	got, err := f.GetUploadToken(ctx(), "key1")
	require.NoError(t, err)
	assert.Equal(t, "pending", got.State)
	assert.Equal(t, tok.Filename, got.Filename)
	assert.True(t, got.Compressed)
	assert.EqualValues(t, 1700000000000, got.MtimeMS)
	moved, err := f.SetUploadTokenStateFrom(ctx(), "key1", "pending", "uploaded")
	require.NoError(t, err)
	assert.True(t, moved)
	got, _ = f.GetUploadToken(ctx(), "key1")
	assert.Equal(t, "uploaded", got.State)
	moved, err = f.SetUploadTokenStateFrom(ctx(), "key1", "pending", "registered")
	require.NoError(t, err)
	assert.False(t, moved, "a token that already left from is not moved again")
	moved, err = f.SetUploadTokenStateFrom(ctx(), "ghost", "pending", "uploaded")
	require.NoError(t, err)
	assert.False(t, moved)
	_, err = f.GetUploadToken(ctx(), "ghost")
	assert.ErrorIs(t, err, repo.ErrNotFound)
	deleted, err = f.DeleteOrphanBlob(ctx(), "ffffffffffffffffffffffffffffffff", time.Now())
	assert.NoError(t, err, "deleting a missing blob is a no-op")
	assert.False(t, deleted)
}

func TestOrphanBlobs(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	f := s.Files()
	md5s := map[string]string{
		"referenced": "11111111111111111111111111111111",
		"orphan":     "22222222222222222222222222222222",
		"liveToken":  "33333333333333333333333333333333",
		"deadToken":  "44444444444444444444444444444444",
		"young":      "55555555555555555555555555555555",
	}
	for name, m := range md5s {
		require.NoError(t, f.PutBlob(ctx(), repo.Blob{MD5: m, Size: 10}))
		if name != "young" {
			_, err := pool.Exec(ctx(), `UPDATE blobs SET created_at = now() - interval '2 days' WHERE md5 = $1`, m)
			require.NoError(t, err)
		}
	}
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil)}))
		return tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "AAAAAAAA", MD5: md5s["referenced"], Filename: "a", Size: 10})
	})
	tok := func(key, md5 string, exp time.Time) {
		require.NoError(t, f.CreateUploadToken(ctx(), repo.UploadToken{
			UploadKey: key, LibraryID: u.LibraryID, ItemKey: "AAAAAAAA", MD5: md5, Filename: "x", ExpiresAt: exp,
		}))
	}
	tok("live", md5s["liveToken"], time.Now().Add(time.Hour))
	tok("dead", md5s["deadToken"], time.Now().Add(-time.Hour))

	collect := func(olderThan time.Time) []string {
		seq, err := f.OrphanBlobs(ctx(), olderThan)
		require.NoError(t, err)
		var out []string
		for b, err := range seq {
			require.NoError(t, err)
			out = append(out, b.MD5)
		}
		return out
	}
	got := collect(time.Now().Add(-time.Hour))
	assert.ElementsMatch(t, []string{md5s["orphan"], md5s["deadToken"]}, got)
	assert.Len(t, collect(time.Now().Add(time.Hour)), 3, "young blobs qualify once old enough")

	// Stopping early releases the connection.
	seq, err := f.OrphanBlobs(ctx(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	for range seq {
		break
	}
	assert.Zero(t, pool.Stat().AcquiredConns())

	st, err := f.Stats(ctx())
	require.NoError(t, err)
	assert.Equal(t, repo.FileStats{BlobCount: 5, BlobBytes: 50, OrphanBytes: 40}, st)

	cutoff := time.Now().Add(-time.Hour)
	for name, want := range map[string]bool{"referenced": false, "liveToken": false, "young": false, "orphan": true} {
		deleted, err := f.DeleteOrphanBlob(ctx(), md5s[name], cutoff)
		require.NoError(t, err)
		assert.Equal(t, want, deleted, name)
	}
	ok, _ := f.BlobExists(ctx(), md5s["orphan"])
	assert.False(t, ok)

	// Re-uploading an old orphan restarts its grace period.
	require.NoError(t, f.PutBlob(ctx(), repo.Blob{MD5: md5s["deadToken"], Size: 10}))
	deleted, err := f.DeleteOrphanBlob(ctx(), md5s["deadToken"], cutoff)
	require.NoError(t, err)
	assert.False(t, deleted, "freshly re-uploaded blob must survive")

	empty, err := newEmptyStats(t)
	require.NoError(t, err)
	assert.Equal(t, repo.FileStats{}, empty)
}

func newEmptyStats(t *testing.T) (repo.FileStats, error) {
	s, _ := newStore(t)
	return s.Files().Stats(ctx())
}

func TestImportJobs(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	im := s.Imports()

	_, err := im.ClaimNext(ctx())
	assert.ErrorIs(t, err, repo.ErrNotFound)

	j1, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID, SourceAPIKey: "zotkey", IncludeGroups: true})
	require.NoError(t, err)
	j2, err := im.Create(ctx(), repo.ImportJob{UserID: u.ID, IncludeFiles: true})
	require.NoError(t, err)
	assert.Equal(t, "queued", j1.Status)
	assert.JSONEq(t, `{}`, string(j1.Cursor))
	assert.Equal(t, "zotkey", j1.SourceAPIKey)
	assert.True(t, j1.IncludeGroups)
	assert.Empty(t, j2.SourceAPIKey)

	claimed, err := im.ClaimNext(ctx())
	require.NoError(t, err)
	assert.Equal(t, j1.ID, claimed.ID, "oldest first")
	assert.Equal(t, "running", claimed.Status)
	assert.NotNil(t, claimed.StartedAt)

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

	list, err := im.ListByUser(ctx(), u.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, j2.ID, list[0].ID, "newest first")
	_, err = im.Get(ctx(), 999)
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestImportClaimNextConcurrent(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	const jobs = 4
	for range jobs {
		_, err := s.Imports().Create(ctx(), repo.ImportJob{UserID: u.ID})
		require.NoError(t, err)
	}
	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.Imports().ClaimNext(ctx())
			if err != nil {
				assert.ErrorIs(t, err, repo.ErrNotFound)
				return
			}
			mu.Lock()
			claimed[j.ID]++
			mu.Unlock()
		}()
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

	ok, err := im.SaveProgress(ctx(), j.ID, []byte(`{"a":1}`), []byte(`{"objects":2}`))
	require.NoError(t, err)
	assert.False(t, ok, "a queued job is not running")

	claimed, err := im.ClaimNext(ctx())
	require.NoError(t, err)
	ok, err = im.SaveProgress(ctx(), claimed.ID, []byte(`{"a":1}`), []byte(`{"objects":2}`))
	require.NoError(t, err)
	assert.True(t, ok)
	got, _ := im.Get(ctx(), j.ID)
	assert.JSONEq(t, `{"a":1}`, string(got.Cursor))
	assert.JSONEq(t, `{"objects":2}`, string(got.Progress))

	require.NoError(t, im.Requeue(ctx(), j.ID))
	got, _ = im.Get(ctx(), j.ID)
	assert.Equal(t, "queued", got.Status)
	assert.Equal(t, "zotkey", got.SourceAPIKey)
	assert.JSONEq(t, `{"a":1}`, string(got.Cursor), "resume point survives")

	_, err = im.ClaimNext(ctx())
	require.NoError(t, err)
	n, err := im.RequeueRunning(ctx())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	_, err = im.ClaimNext(ctx())
	require.NoError(t, err)
	at := time.Now().UTC()
	ok, err = im.Finish(ctx(), j.ID, "completed", "careful", at)
	require.NoError(t, err)
	assert.True(t, ok)
	got, _ = im.Get(ctx(), j.ID)
	assert.Equal(t, "completed", got.Status)
	assert.Equal(t, "careful", got.Error)
	assert.Empty(t, got.SourceAPIKey)
	assert.NotNil(t, got.FinishedAt)

	ok, err = im.Finish(ctx(), j.ID, "failed", "late", at)
	require.NoError(t, err)
	assert.False(t, ok, "a finished job stays finished")
}
