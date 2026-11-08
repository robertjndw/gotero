package files_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // md5 is Zotero's file identity
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/faultdb"
	"github.com/robertjndw/gotero/internal/faultstore"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// faultEnv is a files.Service on real Postgres and a local backend, with a
// fault store in between and a connection killer on the pool.
type faultEnv struct {
	svc   *files.Service
	fs    *faultstore.Store
	kill  *faultdb.Killer
	store *pg.Store
	pool  *pgxpool.Pool
	root  string
}

func newFaultEnv(t *testing.T) *faultEnv {
	t.Helper()
	dsn := pgtest.NewDSN(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	kill := faultdb.New(dsn)
	cfg.ConnConfig.Tracer = kill
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	root := t.TempDir()
	be, err := local.New(root, local.WithNoSync())
	if err != nil {
		t.Fatal(err)
	}
	fs := faultstore.New(be, 1)
	store := pg.New(pool)
	return &faultEnv{
		svc: files.NewService(fs, store.Files(), "0123456789abcdef0123456789abcdef"),
		fs:  fs, kill: kill, store: store, pool: pool, root: root,
	}
}

func md5hex(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // see import
	return hex.EncodeToString(s[:])
}

func (e *faultEnv) staging(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.root, storage.StagingDir))
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

func (e *faultEnv) hasBytes(t *testing.T, m string) bool {
	t.Helper()
	_, err := e.fs.Inner().Stat(context.Background(), files.BlobKey(m))
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func (e *faultEnv) hasRow(t *testing.T, m string) bool {
	t.Helper()
	ok, err := e.store.Files().BlobExists(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// sweepUntracked is what GC does for bytes without a row. The cutoff is in
// the future so freshly written test files count as old.
func (e *faultEnv) sweepUntracked(t *testing.T) (removed int, err error) {
	t.Helper()
	ctx := context.Background()
	cutoff := time.Now().Add(time.Hour)
	list, _, err := e.svc.UntrackedBlobs(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	for _, info := range list {
		ok, err := e.svc.RemoveUntracked(ctx, filepath.Base(info.Key), cutoff)
		if err != nil {
			return removed, err
		}
		if ok {
			removed++
		}
	}
	return removed, nil
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ seed
	}
	return b
}

// Every way storage can fail during an upload must leave neither a row nor
// a staging file, and the same upload must succeed once storage recovers.
func TestReceiveSurvivesStorageFaults(t *testing.T) {
	cases := []struct {
		name string
		rule faultstore.Rule
		// published: the fault strikes after the bytes became visible.
		published bool
		full      bool
	}{
		{name: "create fails", rule: faultstore.Rule{Op: faultstore.OpCreate}},
		{name: "first write fails", rule: faultstore.Rule{Op: faultstore.OpWrite}},
		{name: "write fails mid-stream", rule: faultstore.Rule{Op: faultstore.OpWrite, AtByte: 300 << 10}},
		{name: "write times out", rule: faultstore.Rule{Op: faultstore.OpWrite, AtByte: 1 << 20, Err: faultstore.ErrTimeout}},
		{name: "disk full mid-stream", rule: faultstore.Rule{Op: faultstore.OpWrite, AtByte: 1 << 20, Err: faultstore.ErrDiskFull}, full: true},
		{name: "disk full on create", rule: faultstore.Rule{Op: faultstore.OpCreate, Err: faultstore.ErrDiskFull}, full: true},
		{name: "commit fails", rule: faultstore.Rule{Op: faultstore.OpCommit}},
		{name: "commit disk full", rule: faultstore.Rule{Op: faultstore.OpCommit, Err: faultstore.ErrDiskFull}, full: true},
		{name: "commit reply lost", rule: faultstore.Rule{Op: faultstore.OpCommit, Err: faultstore.ErrTimeout, Applied: true}, published: true},
		{name: "post-commit stat fails", rule: faultstore.Rule{Op: faultstore.OpStat}, published: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newFaultEnv(t)
			ctx := context.Background()
			data := payload(3<<20, 7)
			m := md5hex(data)
			e.fs.Inject(c.rule)

			err := e.svc.Receive(ctx, bytes.NewReader(data), m, int64(len(data)))
			if err == nil {
				t.Fatal("upload succeeded through a storage fault")
			}
			if e.fs.Faults(c.rule.Op) != 1 {
				t.Fatalf("fault fired %d times", e.fs.Faults(c.rule.Op))
			}
			if got := errors.Is(err, files.ErrStorageFull); got != c.full {
				t.Fatalf("ErrStorageFull = %v for %v", got, err)
			}
			if e.hasRow(t, m) {
				t.Fatal("a failed upload recorded a blob row")
			}
			if e.hasBytes(t, m) != c.published {
				t.Fatalf("bytes present = %v, want %v", !c.published, c.published)
			}
			if n := e.staging(t); n != 0 {
				t.Fatalf("%d staging files left behind", n)
			}

			e.fs.Clear()
			if err := e.svc.Receive(ctx, bytes.NewReader(data), m, int64(len(data))); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if !e.hasRow(t, m) || !e.hasBytes(t, m) {
				t.Fatal("retry did not store the blob")
			}
			rd, _, err := e.svc.Open(ctx, m)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rd)
			_ = rd.Close()
			if !bytes.Equal(got, data) {
				t.Fatal("stored bytes differ after retry")
			}
		})
	}
}

// When even the cleanup fails, the staged bytes stay until SweepStaging.
func TestFailedAbortIsSweptLater(t *testing.T) {
	e := newFaultEnv(t)
	ctx := context.Background()
	data := payload(1<<20, 3)
	e.fs.Inject(faultstore.Rule{Op: faultstore.OpWrite, AtByte: 1000})
	e.fs.Inject(faultstore.Rule{Op: faultstore.OpAbort})
	if err := e.svc.Receive(ctx, bytes.NewReader(data), md5hex(data), int64(len(data))); err == nil {
		t.Fatal("expected failure")
	}
	if e.staging(t) != 1 {
		t.Fatalf("staging files: %d", e.staging(t))
	}
	e.fs.Clear()
	n, err := e.fs.SweepStaging(ctx, 0)
	if err != nil || n != 1 || e.staging(t) != 0 {
		t.Fatalf("sweep: %d %v, left %d", n, err, e.staging(t))
	}
}

// The bytes are committed but the connection recording the row dies. The
// client sees an error, nothing references the bytes, and GC has to find
// them by walking storage, since no row leads to them.
func TestDBFaultAfterCommitLeavesCollectableBytes(t *testing.T) {
	for _, stmt := range []string{"pg_advisory_xact_lock", "INSERT INTO blobs", "commit"} {
		t.Run(stmt, func(t *testing.T) {
			e := newFaultEnv(t)
			ctx := context.Background()
			data := payload(64<<10, 9)
			m := md5hex(data)
			e.kill.KillOn(stmt, 1)

			err := e.svc.Receive(ctx, bytes.NewReader(data), m, int64(len(data)))
			if n, kerr := e.kill.Killed(); n != 1 || kerr != nil {
				t.Fatalf("killer: %d %v", n, kerr)
			}
			if err == nil {
				t.Fatal("upload reported success with its connection killed")
			}
			if e.hasRow(t, m) {
				t.Fatal("row recorded although the transaction died")
			}
			if !e.hasBytes(t, m) {
				t.Fatal("expected the committed bytes to remain")
			}

			removed, err := e.sweepUntracked(t)
			if err != nil || removed != 1 {
				t.Fatalf("sweep: %d %v", removed, err)
			}
			if e.hasBytes(t, m) {
				t.Fatal("untracked bytes survived GC")
			}
			// And the client's retry still works afterwards.
			if err := e.svc.Receive(ctx, bytes.NewReader(data), m, int64(len(data))); err != nil {
				t.Fatal(err)
			}
			if !e.hasRow(t, m) || !e.hasBytes(t, m) {
				t.Fatal("retry did not store the blob")
			}
		})
	}
}

// The untracked sweep must never touch bytes that a row or a live upload
// token claims, nor anything that is not a blob, nor young bytes.
func TestUntrackedSweepSparesClaimedBytes(t *testing.T) {
	e := newFaultEnv(t)
	ctx := context.Background()

	tracked := payload(1000, 1)
	if err := e.svc.Receive(ctx, bytes.NewReader(tracked), md5hex(tracked), int64(len(tracked))); err != nil {
		t.Fatal(err)
	}

	// Bytes without a row, but an upload for them is in flight.
	inflight := payload(1000, 2)
	putRaw(t, e.fs.Inner(), files.BlobKey(md5hex(inflight)), inflight)
	u, err := e.store.Users().Create(ctx, repo.User{Username: "u", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.Files().CreateUploadToken(ctx, repo.UploadToken{
		UploadKey: "k1", LibraryID: u.LibraryID, ItemKey: "AAAAAAAA", MD5: md5hex(inflight),
		Filename: "f", Size: 1000, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Something that is not a blob key at all.
	putRaw(t, e.fs.Inner(), "blobs/zz/not-a-blob", []byte("x"))
	// A stray blob that a real sweep must keep while it is young.
	young := payload(1000, 3)
	putRaw(t, e.fs.Inner(), files.BlobKey(md5hex(young)), young)
	list, _, err := e.svc.UntrackedBlobs(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(list) != 0 {
		t.Fatalf("young bytes listed: %v %v", list, err)
	}
	if ok, err := e.svc.RemoveUntracked(ctx, md5hex(young), time.Now().Add(-time.Hour)); ok || err != nil {
		t.Fatalf("young bytes removed: %v %v", ok, err)
	}

	removed, err := e.sweepUntracked(t)
	if err != nil || removed != 1 {
		t.Fatalf("sweep: %d %v", removed, err)
	}
	for name, m := range map[string]string{"tracked": md5hex(tracked), "in flight": md5hex(inflight)} {
		if !e.hasBytes(t, m) {
			t.Fatalf("%s bytes were deleted", name)
		}
	}
	if e.hasBytes(t, md5hex(young)) {
		t.Fatal("the stray blob should be gone once old")
	}
	if _, err := e.fs.Inner().Stat(ctx, "blobs/zz/not-a-blob"); err != nil {
		t.Fatal("a foreign object was deleted")
	}
}

// A refused or timed out delete keeps the row, so the next pass retries;
// a delete that went through but reported an error is cleaned up by the
// next pass too.
func TestRemoveOrphanStorageFaults(t *testing.T) {
	for name, rule := range map[string]faultstore.Rule{
		"refused":    {Op: faultstore.OpDelete},
		"timeout":    {Op: faultstore.OpDelete, Err: faultstore.ErrTimeout},
		"reply lost": {Op: faultstore.OpDelete, Err: faultstore.ErrTimeout, Applied: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newFaultEnv(t)
			ctx := context.Background()
			data := payload(2000, 5)
			m := md5hex(data)
			if err := e.svc.Receive(ctx, bytes.NewReader(data), m, int64(len(data))); err != nil {
				t.Fatal(err)
			}
			cutoff := time.Now().Add(time.Hour)
			e.fs.Inject(rule)
			if ok, err := e.svc.RemoveOrphan(ctx, m, cutoff); ok || err == nil {
				t.Fatalf("remove through a failing delete: %v %v", ok, err)
			}
			if !e.hasRow(t, m) {
				t.Fatal("row dropped although the delete failed")
			}
			if e.hasBytes(t, m) == rule.Applied {
				t.Fatalf("bytes present = %v", !rule.Applied)
			}
			e.fs.Clear()
			if ok, err := e.svc.RemoveOrphan(ctx, m, cutoff); !ok || err != nil {
				t.Fatalf("retry: %v %v", ok, err)
			}
			if e.hasRow(t, m) || e.hasBytes(t, m) {
				t.Fatal("retry left something behind")
			}
		})
	}
}

// The database dying in the middle of GC's row delete must not lose the
// bytes of a row that survives, nor keep a row whose bytes are gone.
func TestRemoveOrphanDBFault(t *testing.T) {
	for _, stmt := range []string{"DELETE FROM blobs", "commit"} {
		t.Run(stmt, func(t *testing.T) {
			e := newFaultEnv(t)
			ctx := context.Background()
			data := payload(2000, 6)
			m := md5hex(data)
			if err := e.svc.Receive(ctx, bytes.NewReader(data), m, int64(len(data))); err != nil {
				t.Fatal(err)
			}
			e.kill.KillOn(stmt, 1)
			ok, err := e.svc.RemoveOrphan(ctx, m, time.Now().Add(time.Hour))
			if n, kerr := e.kill.Killed(); n != 1 || kerr != nil {
				t.Fatalf("killer: %d %v", n, kerr)
			}
			if ok || err == nil {
				t.Fatalf("remove with a dead connection: %v %v", ok, err)
			}
			row, b := e.hasRow(t, m), e.hasBytes(t, m)
			switch {
			case row && !b:
				// The documented crash window: an orphan row whose bytes are
				// gone. Receive re-checks bytes, and the next pass drops it.
			case row && b:
			default:
				t.Fatalf("row %v bytes %v", row, b)
			}
			if ok, err := e.svc.RemoveOrphan(ctx, m, time.Now().Add(time.Hour)); !ok || err != nil {
				t.Fatalf("next pass: %v %v", ok, err)
			}
			if e.hasRow(t, m) || e.hasBytes(t, m) {
				t.Fatal("next pass left something behind")
			}
		})
	}
}

func putRaw(t *testing.T, b storage.Backend, key string, data []byte) {
	t.Helper()
	ctx := context.Background()
	up, err := b.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Abort(ctx) }()
	if _, err := up.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := up.Commit(ctx, key); err != nil {
		t.Fatal(fmt.Errorf("commit %s: %w", key, err))
	}
}
