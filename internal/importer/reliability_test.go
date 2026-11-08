package importer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
)

func cursorOf(t testing.TB, j repo.ImportJob) cursor {
	t.Helper()
	var c cursor
	require.NoError(t, json.Unmarshal(j.Cursor, &c))
	return c
}

func isVersionsList(r *http.Request, typ string) bool {
	return strings.HasSuffix(r.URL.Path, "/"+typ) && r.URL.Query().Get("format") == "versions"
}

// A collection edited on zotero.org after the collections were read is
// noticed by the items listing. The job is interrupted before the catch-up
// pass; the resumed job must still know about the change, copy it and only
// then adopt the new library version.
func TestSourceChangeDuringImportSurvivesResume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)

	ctx, cancel := context.WithCancel(bg())
	defer cancel()
	var edited, interrupted atomic.Bool
	e.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if isVersionsList(r, "items") && !edited.Swap(true) {
			// Requests reach the fake one at a time in this phase, so the
			// fixture can change in place.
			src.version++
			c := src.collections[colRoot]
			c.version = src.version
			c.data["name"] = "Root renamed"
			src.collections[colRoot] = c
			return false
		}
		if r.URL.Query().Get("itemKey") != "" && !interrupted.Swap(true) {
			cancel()
			http.Error(w, "boom", http.StatusInternalServerError)
			return true
		}
		return false
	}
	job := e.enqueue(jobOpts{})
	stopped := e.runJob(ctx, job.ID)
	require.Equal(t, "queued", stopped.Status)
	st := cursorOf(t, stopped).Libs[0]
	assert.True(t, st.Drift, "the change is remembered across the interruption")
	assert.EqualValues(t, 50, st.Listed[phaseCollections])

	e.fz.hook = nil
	done := e.runJob(bg(), job.ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Empty(t, done.Error)

	cols := e.versions(e.user.LibraryID, repo.TypeCollection)
	assert.EqualValues(t, 51, cols[colRoot], "the collection edit was copied")
	got, err := e.reader(e.user.LibraryID).Get(bg(), repo.TypeCollection, []string{colRoot})
	require.NoError(t, err)
	assert.Contains(t, string(got[colRoot].Data), "Root renamed")
	assert.EqualValues(t, 51, e.reader(e.user.LibraryID).Version())
	assert.Equal(t, e.sourceVersions(src, repo.TypeItem), e.versions(e.user.LibraryID, repo.TypeItem))
	fin := cursorOf(t, done).Libs[0]
	assert.False(t, fin.Drift)
	assert.EqualValues(t, 51, fin.Copied)
}

// A library that changes during every pass is not adopted at a version whose
// changes were never all copied, and the job says so.
func TestStillChangingSourceIsNotAdopted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	e.fz.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if isVersionsList(r, "items") {
			// Someone keeps renaming a collection while the items are read.
			src.version++
			c := src.collections[colChild]
			c.version = src.version
			c.data["name"] = "Child " + string(rune('a'+src.version-50))
			src.collections[colChild] = c
		}
		return false
	}
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Contains(t, done.Error, "kept changing on zotero.org")

	st := cursorOf(t, done).Libs[0]
	assert.Equal(t, maxPasses-1, st.Pass)
	assert.True(t, st.Drift)
	assert.EqualValues(t, 53, src.version)
	assert.EqualValues(t, 53, st.LMV)
	assert.EqualValues(t, 52, st.Copied, "what every phase of the last pass saw")
	assert.EqualValues(t, 52, e.reader(e.user.LibraryID).Version(), "the newest source version is not adopted")
	assert.EqualValues(t, 52, e.versions(e.user.LibraryID, repo.TypeCollection)[colChild])
}

// A fresh import that failed half way stays a fresh import when it is
// started again, as long as nothing else wrote to the library meanwhile.
func TestRerunOfFailedFreshImportStaysFresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	addItems(src, 250)
	var batches atomic.Int64
	e.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("itemKey") != "" && batches.Add(1) == 2 {
			http.Error(w, "bad", http.StatusBadRequest)
			return true
		}
		return false
	}
	failed := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "failed", failed.Status)
	require.NotZero(t, e.reader(e.user.LibraryID).Version())

	e.fz.hook = nil
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Equal(t, modeFresh, cursorOf(t, done).Libs[0].Mode)
	assert.Equal(t, e.sourceVersions(src, repo.TypeItem), e.versions(e.user.LibraryID, repo.TypeItem),
		"zotero.org's versions are kept")
	assert.EqualValues(t, src.version, e.reader(e.user.LibraryID).Version())
}

// Once something else wrote to the library, a rerun must merge.
func TestRerunAfterOtherWritesMerges(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	addItems(src, 250)
	var batches atomic.Int64
	e.fz.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("itemKey") != "" && batches.Add(1) == 2 {
			http.Error(w, "bad", http.StatusBadRequest)
			return true
		}
		return false
	}
	require.Equal(t, "failed", e.runJob(bg(), e.enqueue(jobOpts{}).ID).Status)
	// A client write in between, as far as the library version tells.
	_, err := e.store.InLibrary(bg(), e.user.LibraryID, func(tx repo.LibraryTx) error {
		_, err := tx.NextVersion(bg())
		return err
	})
	require.NoError(t, err)

	e.fz.hook = nil
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	assert.Equal(t, modeMerge, cursorOf(t, done).Libs[0].Mode)
}

func TestEmbeddedImageIsImported(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	src := e.fz.lib("users", fakeUserID)
	png := []byte("\x89PNG\r\n\x1a\n fake image")
	const imgKey = "EEEEEEE2"
	src.items[imgKey] = fakeObj{46, map[string]any{
		"itemType": "attachment", "parentItem": itNote, "linkMode": "embedded_image",
		"contentType": "image/png", "md5": md5hex(png), "mtime": 1700000003000,
		"tags": []any{}, "collections": []string{}, "relations": map[string]any{},
		"dateAdded": "2024-01-08T00:00:00Z", "dateModified": "2024-01-08T00:00:00Z",
	}}
	src.version = 46
	src.files[imgKey] = fakeFile{body: png, md5: md5hex(png), mtime: 1700000003000}

	done := e.runJob(bg(), e.enqueue(jobOpts{files: true}).ID)
	require.Equal(t, "completed", done.Status, done.Error)
	af, err := e.reader(e.user.LibraryID).AttachmentFile(bg(), imgKey)
	require.NoError(t, err, "the image embedded in the note has its file")
	assert.Equal(t, md5hex(png), af.MD5)
	assert.Equal(t, png, e.blob(af.MD5))
}

func TestKeyWithoutNotesAccessIsRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.fz.access.User.Notes = false

	check, err := e.w.ValidateImportKey(bg(), fakeAPIKey)
	require.NoError(t, err)
	assert.True(t, check.Valid)
	assert.True(t, check.Library)
	assert.False(t, check.Notes)

	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	assert.Equal(t, "failed", done.Status)
	assert.Contains(t, done.Error, "cannot read your notes")
	assert.Empty(t, e.versions(e.user.LibraryID, repo.TypeItem), "nothing was imported")
}

// A worker whose lease went to another worker stops instead of importing
// alongside it, and does not touch the job anymore.
func TestWorkerStopsAfterLosingLease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.w.LeaseDuration = 200 * time.Millisecond
	src := e.fz.lib("users", fakeUserID)
	addItems(src, 400)
	job := e.enqueue(jobOpts{})

	var mu sync.Mutex
	var stolen bool
	var afterSteal int
	var snapshot repo.ImportJob
	e.fz.hook = func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("itemKey") == "" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if stolen {
			afterSteal++
			return false
		}
		stolen = true
		// The lease ran out and another worker claimed the job.
		_, err := e.pool.Exec(bg(), `UPDATE import_jobs SET claimed_by = 'thief' WHERE id = $1`, job.ID)
		require.NoError(t, err)
		snapshot, err = e.store.Imports().Get(bg(), job.ID)
		require.NoError(t, err)
		// Long enough for the heartbeat to notice.
		time.Sleep(300 * time.Millisecond)
		return false
	}
	got := e.runJob(bg(), job.ID)
	assert.Equal(t, "running", got.Status, "the job stays with the new owner")
	assert.Equal(t, "thief", got.ClaimedBy)
	assert.JSONEq(t, string(snapshot.Cursor), string(got.Cursor), "the old worker wrote nothing after losing the lease")
	assert.Nil(t, got.FinishedAt)
	mu.Lock()
	defer mu.Unlock()
	assert.LessOrEqual(t, afterSteal, 1, "the old worker stopped fetching")
}

// The command line runs exactly the job it queued, never someone else's.
func TestRunForegroundClaimsOnlyItsJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	bob, err := e.store.Users().Create(bg(), repo.User{Username: "bob", PasswordHash: "x"})
	require.NoError(t, err)
	other, err := e.store.Imports().Create(bg(), repo.ImportJob{UserID: bob.ID, SourceAPIKey: fakeAPIKey})
	require.NoError(t, err)
	mine := e.enqueue(jobOpts{})
	require.Greater(t, mine.ID, other.ID, "the other job is older and would be claimed first")

	var out strings.Builder
	require.NoError(t, e.w.RunForeground(bg(), mine.ID, &out))
	got, err := e.store.Imports().Get(bg(), mine.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
	untouched, err := e.store.Imports().Get(bg(), other.ID)
	require.NoError(t, err)
	assert.Equal(t, "queued", untouched.Status)
	assert.Nil(t, untouched.StartedAt)
}

// RunForeground follows a job another process runs, and takes it over once
// that process stopped renewing its lease.
func TestRunForegroundTakesOverExpiredJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	job := e.enqueue(jobOpts{})
	_, err := e.store.Imports().ClaimNext(bg(), "killed", 500*time.Millisecond)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(bg(), 30*time.Second)
	defer cancel()
	var out strings.Builder
	require.NoError(t, e.w.RunForeground(ctx, job.ID, &out))
	got, err := e.store.Imports().Get(bg(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", got.Status)
}

type panickingPublisher struct{}

func (panickingPublisher) Publish(zapi.Event) { panic("publisher exploded") }

func TestPanicFailsJobInsteadOfCrashing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.w.Publisher = panickingPublisher{}
	done := e.runJob(bg(), e.enqueue(jobOpts{}).ID)
	assert.Equal(t, "failed", done.Status)
	assert.Contains(t, done.Error, "internal error")
	assert.Contains(t, done.Error, "publisher exploded")

	// The failed job stays failed: nothing requeues it into a crash loop.
	n, err := e.store.Imports().RequeueExpired(bg())
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestParallelTurnsPanicIntoError(t *testing.T) {
	t.Parallel()
	err := parallel(bg(), 4, 10, func(_ context.Context, i int) error {
		if i == 3 {
			panic("worker exploded")
		}
		return nil
	})
	var pe *panicError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, err.Error(), "worker exploded")
}

// A response whose body stops arriving fails instead of blocking the worker.
func TestHTTPClientAbortsStalledBody(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	hc := newHTTPClient(200 * time.Millisecond)
	req, err := http.NewRequestWithContext(bg(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errBodyStalled), "got %v", err)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// Time spent between reads is the caller's, not a stall.
func TestHTTPClientIdleTimerOnlyRunsDuringReads(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 10)))
	}))
	defer srv.Close()
	hc := newHTTPClient(100 * time.Millisecond)
	req, err := http.NewRequestWithContext(bg(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := hc.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 5)
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	rest, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "xxxxx", string(rest))
}
