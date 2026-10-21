package jobs

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // test checksum
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type env struct {
	store *pg.Store
	fs    *files.Service
	be    storage.Backend
	run   *Runner
	lib   int64
	exec  func(sql string, args ...any)
}

func setup(t *testing.T) *env {
	t.Helper()
	pool := pgtest.New(t)
	store := pg.New(pool)
	be, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := files.NewService(be, store.Files(), strings.Repeat("s", 32))
	u, err := store.Users().Create(context.Background(), repo.User{Username: "alice", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return &env{
		store: store, fs: fs, be: be, lib: u.LibraryID,
		run: New(store, fs, slog.New(slog.NewTextHandler(io.Discard, nil))),
		exec: func(sql string, args ...any) {
			if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
				t.Fatal(err)
			}
		},
	}
}

func sum(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // test
	return hex.EncodeToString(s[:])
}

func (e *env) upload(t *testing.T, data []byte) string {
	t.Helper()
	m := sum(data)
	if err := e.fs.Receive(context.Background(), bytes.NewReader(data), m, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *env) age(md5hex string) {
	e.exec(`UPDATE blobs SET created_at = now() - interval '3 days' WHERE md5 = $1`, md5hex)
}

func (e *env) attach(t *testing.T, key, md5hex string) {
	t.Helper()
	_, err := e.store.InLibrary(context.Background(), e.lib, func(tx repo.LibraryTx) error {
		if err := tx.PutObjects(context.Background(), repo.TypeItem, []repo.Object{{
			Key: key, Version: 1, ItemType: "attachment", ContentHash: []byte(key),
			Data: json.RawMessage(`{"itemType":"attachment"}`),
		}}); err != nil {
			return err
		}
		return tx.PutAttachmentFile(context.Background(), repo.AttachmentFile{ItemKey: key, MD5: md5hex, Filename: "f", Size: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) exists(t *testing.T, md5hex string) bool {
	t.Helper()
	_, err := e.be.Stat(context.Background(), files.BlobKey(md5hex))
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func TestGCRemovesOnlyOldOrphans(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	orphan := e.upload(t, []byte("orphan"))
	young := e.upload(t, []byte("young"))
	kept := e.upload(t, []byte("referenced"))
	e.attach(t, "AAAAAAAA", kept)
	e.age(orphan)
	e.age(kept)

	dry, err := e.run.GC(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.OrphanBlobs != 1 || dry.Deleted || !e.exists(t, orphan) {
		t.Fatalf("dry run: %+v", dry)
	}

	res, err := e.run.GC(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanBlobs != 1 || res.OrphanBytes != int64(len("orphan")) {
		t.Fatalf("gc: %+v", res)
	}
	if e.exists(t, orphan) {
		t.Fatal("orphan bytes survived")
	}
	if !e.exists(t, young) || !e.exists(t, kept) {
		t.Fatal("young or referenced blob removed")
	}
}

func TestGCDoesNotEatReUploads(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	data := []byte("same content uploaded again")
	m := e.upload(t, data)
	e.age(m)

	// GC lists the orphan, then a client re-uploads identical bytes before
	// the deletion happens. The fresh upload must win.
	seq, err := e.store.Files().OrphanBlobs(ctx, time.Now().Add(-OrphanGrace))
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for b, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, b.MD5)
	}
	if len(listed) != 1 {
		t.Fatalf("expected one orphan, got %v", listed)
	}
	e.upload(t, data)
	removed, err := e.fs.RemoveOrphan(ctx, m, time.Now().Add(-OrphanGrace))
	if err != nil {
		t.Fatal(err)
	}
	if removed || !e.exists(t, m) {
		t.Fatal("re-uploaded blob was garbage collected")
	}
}

func TestGCRespectsLiveUploadTokens(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	m := e.upload(t, []byte("in flight"))
	e.age(m)
	if err := e.store.Files().CreateUploadToken(ctx, repo.UploadToken{
		UploadKey: "k", LibraryID: e.lib, ItemKey: "AAAAAAAA", MD5: m, Filename: "f",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.run.GC(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanBlobs != 0 || !e.exists(t, m) {
		t.Fatalf("blob with live upload token removed: %+v", res)
	}
}

func TestGCSweepsStaging(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	up, err := e.be.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = up.Write([]byte("abandoned"))
	// Not committed and never aborted: simulate a crashed upload whose
	// staging file is old enough to collect.
	e.run.now = func() time.Time { return time.Now() }
	n, err := e.be.SweepStaging(ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}

func TestPrune(t *testing.T) {
	e := setup(t)
	if err := e.run.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
}
