package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/storage"
)

// hookBackend lets a test step into Delete.
type hookBackend struct {
	storage.Backend
	onDelete func(ctx context.Context, key string) error
}

func (b *hookBackend) Delete(ctx context.Context, key string) error {
	if b.onDelete != nil {
		if err := b.onDelete(ctx, key); err != nil {
			return err
		}
	}
	return b.Backend.Delete(ctx, key)
}

func (e *env) rowExists(t *testing.T, md5hex string) bool {
	t.Helper()
	ok, err := e.store.Files().BlobExists(context.Background(), md5hex)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// waitForBlobLockWaiter polls until some session is queued on a blob lock,
// or gives up after a while. Giving up is not an error: without a database
// lock the uploader never waits, which is the bug the caller checks for.
func (e *env) waitForBlobLockWaiter(t *testing.T, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(done) > 0 {
			return
		}
		var n int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Two processes share the database and the blob store: one runs GC, the
// other (say the import CLI) uploads the same content. GC has already
// removed the row and is about to remove the bytes when the upload lands.
// The upload must not end up with a row, and an attachment, whose bytes
// are gone.
func TestGCAndUploadInAnotherProcessNeverLoseReferencedBytes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	data := []byte("content GC and an upload fight over")
	m := e.upload(t, data)
	e.age(m)

	atDelete := make(chan struct{})
	proceed := make(chan struct{})
	gcBackend := &hookBackend{Backend: e.be, onDelete: func(context.Context, string) error {
		close(atDelete)
		<-proceed
		return nil
	}}
	// Separate services, so nothing in-process can serialize them.
	gcProc := files.NewService(gcBackend, e.store.Files(), strings.Repeat("s", 32))
	uploadProc := files.NewService(e.be, e.store.Files(), strings.Repeat("s", 32))

	gcDone := make(chan error, 1)
	go func() {
		_, err := gcProc.RemoveOrphan(ctx, m, time.Now().Add(-OrphanGrace))
		gcDone <- err
	}()
	<-atDelete

	upDone := make(chan error, 1)
	go func() { upDone <- uploadProc.Receive(ctx, bytes.NewReader(data), m, int64(len(data))) }()
	e.waitForBlobLockWaiter(t, upDone)
	close(proceed)
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	upErr := <-upDone

	if e.rowExists(t, m) && !e.exists(t, m) {
		t.Fatal("blob row exists but its bytes were garbage collected")
	}
	if upErr != nil {
		if !errors.Is(upErr, files.ErrBlobCollected) || !errors.Is(upErr, repo.ErrBusy) {
			t.Fatalf("upload should fail retryably, got %v", upErr)
		}
		// The client retries and the blob is back for good.
		if err := uploadProc.Receive(ctx, bytes.NewReader(data), m, int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	e.attach(t, "AAAAAAAA", m)
	if !e.rowExists(t, m) || !e.exists(t, m) {
		t.Fatal("attached blob is missing its row or bytes")
	}
}

func TestGCContinuesPastFailingBlobs(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	bad := e.upload(t, []byte("storage refuses to delete me"))
	good := e.upload(t, []byte("deletable"))
	e.age(bad)
	e.age(good)

	broken := &hookBackend{Backend: e.be, onDelete: func(_ context.Context, key string) error {
		if key == files.BlobKey(bad) {
			return errors.New("storage is having a bad day")
		}
		return nil
	}}
	run := New(e.store, files.NewService(broken, e.store.Files(), strings.Repeat("s", 32)), discard(), 0)

	res, err := run.GC(ctx, false)
	if err == nil {
		t.Fatal("a failed blob should be reported")
	}
	if res.Failed != 1 || res.OrphanBlobs != 1 {
		t.Fatalf("gc: %+v", res)
	}
	if e.exists(t, good) || e.rowExists(t, good) {
		t.Fatal("the deletable orphan was not removed")
	}
	// The row stays with its bytes, so the next pass tries again rather than
	// leaking the bytes forever.
	if !e.rowExists(t, bad) || !e.exists(t, bad) {
		t.Fatal("failed removal must leave both row and bytes")
	}

	res, err = e.run.GC(ctx, false)
	if err != nil || res.OrphanBlobs != 1 || e.exists(t, bad) || e.rowExists(t, bad) {
		t.Fatalf("retry pass: %+v %v", res, err)
	}
}

func TestGCBoundsEachBlob(t *testing.T) {
	e := setup(t)
	m := e.upload(t, []byte("on a storage server that hangs"))
	e.age(m)
	hung := &hookBackend{Backend: e.be, onDelete: func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	run := New(e.store, files.NewService(hung, e.store.Files(), strings.Repeat("s", 32)), discard(), 0)
	run.blobTimeout = 100 * time.Millisecond

	start := time.Now()
	res, err := run.GC(context.Background(), false)
	if err == nil || res.Failed != 1 {
		t.Fatalf("gc: %+v %v", res, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a hung delete stalled the pass")
	}
	if !e.rowExists(t, m) || !e.exists(t, m) {
		t.Fatal("timed out removal must leave both row and bytes")
	}
}

func TestJobPanicIsRecovered(t *testing.T) {
	var logs bytes.Buffer
	r := New(nil, nil, slog.New(slog.NewTextHandler(&logs, nil)), 0)
	err := r.runOnce(context.Background(), "boom", func(context.Context) error { panic("kaboom") })
	if err == nil || !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("panic should come back as an error, got %v", err)
	}
	if !strings.Contains(logs.String(), "runOnce") {
		t.Fatalf("the log should carry a stack trace: %s", logs.String())
	}
	if err := r.runOnce(context.Background(), "ok", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
