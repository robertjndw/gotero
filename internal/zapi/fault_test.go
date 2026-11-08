package zapi_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/faultdb"
	"github.com/robertjndw/gotero/internal/faultstore"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/jobs"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
)

// faultEnv is a testEnv whose blob store and database connections can be
// made to fail on demand.
type faultEnv struct {
	*testEnv
	fs   *faultstore.Store
	kill *faultdb.Killer
	root string
}

// newFaultEnv wraps the local backend in a fault store. wrap, if given, sits
// between the two, e.g. to hide io.Seeker.
func newFaultEnv(t *testing.T, wrap func(storage.Backend) storage.Backend) *faultEnv {
	t.Helper()
	return newSeededFaultEnv(t, wrap, 1)
}

func newSeededFaultEnv(t *testing.T, wrap func(storage.Backend) storage.Backend, seed uint64) *faultEnv {
	t.Helper()
	root := t.TempDir()
	be, err := local.New(root, local.WithNoSync())
	require.NoError(t, err)
	var inner storage.Backend = be
	if wrap != nil {
		inner = wrap(inner)
	}
	fe := &faultEnv{fs: faultstore.New(inner, seed), root: root}
	var svc *files.Service
	e := newEnv(t,
		withPool(func(tb testing.TB) *pgxpool.Pool {
			dsn := pgtest.NewDSN(tb)
			cfg, err := pgxpool.ParseConfig(dsn)
			require.NoError(tb, err)
			fe.kill = faultdb.New(dsn)
			cfg.ConnConfig.Tracer = fe.kill
			pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
			require.NoError(tb, err)
			tb.Cleanup(pool.Close)
			return pool
		}),
		withDeps(func(d *zapi.Deps) {
			svc = files.NewService(fe.fs, d.Store.Files(), "0123456789abcdef0123456789abcdef")
			d.Files = svc
		}))
	e.Files = svc
	fe.testEnv = e
	return fe
}

func (e *faultEnv) staging(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(e.root, storage.StagingDir))
	require.NoError(t, err)
	return len(ents)
}

func (e *faultEnv) hasBytes(t *testing.T, m string) bool {
	t.Helper()
	_, err := e.fs.Inner().Stat(ctx(), files.BlobKey(m))
	if err != nil {
		require.ErrorIs(t, err, storage.ErrNotFound)
	}
	return err == nil
}

func (e *faultEnv) hasRow(t *testing.T, m string) bool {
	t.Helper()
	ok, err := e.Store.Files().BlobExists(ctx(), m)
	require.NoError(t, err)
	return ok
}

func (e *faultEnv) tokenState(t *testing.T, uploadKey string) string {
	t.Helper()
	var s string
	require.NoError(t, e.Pool.QueryRow(ctx(), `SELECT state FROM upload_tokens WHERE upload_key = $1`, uploadKey).Scan(&s))
	return s
}

// expectKilled checks that the connection killer fired exactly once, so a
// test cannot pass because its fragment never matched.
func (e *faultEnv) expectKilled(t *testing.T) {
	t.Helper()
	n, err := e.kill.Killed()
	require.NoError(t, err)
	require.Equal(t, 1, n, "expected exactly one connection to be killed")
}

// gc runs a real garbage collection pass with every grace period already
// expired: blob rows and the files on disk are backdated first.
func (e *faultEnv) gc(t *testing.T) (jobs.GCResult, error) {
	t.Helper()
	_, err := e.Pool.Exec(ctx(), `UPDATE blobs SET created_at = now() - interval '3 days'`)
	require.NoError(t, err)
	old := time.Now().Add(-72 * time.Hour)
	require.NoError(t, filepath.WalkDir(filepath.Join(e.root, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, old, old)
	}))
	run := jobs.New(e.Store, e.Files, slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	return run.GC(ctx(), false)
}

// fileState is everything a client could observe about an attachment's file.
type fileState struct {
	libVersion  int64
	itemVersion any
	md5         any
	used        int64
	fileStatus  int
}

func (e *faultEnv) fileState(t *testing.T, l *lib, u user, key string) fileState {
	t.Helper()
	d := l.dataOf("items", key)
	return fileState{
		libVersion: l.version(), itemVersion: d["version"], md5: d["md5"],
		used: e.used(t, u), fileStatus: l.get("items/" + key + "/file").Status,
	}
}

// retryable is what a Zotero client treats as "try again later" rather than
// as a broken request: any 5xx. A 2xx would be a lie and a 4xx would make
// the client give up on the file.
func retryable(t *testing.T, status int, body []byte) {
	t.Helper()
	t.Logf("fault answered %d %q", status, body)
	require.GreaterOrEqual(t, status, 500, "status %d: %s", status, body)
	require.Less(t, status, 600, "status %d: %s", status, body)
}

func TestUploadStorageFaults(t *testing.T) {
	cases := []struct {
		name   string
		rule   faultstore.Rule
		status int
		// published: the fault strikes after the bytes became visible.
		published bool
	}{
		{name: "create fails", rule: faultstore.Rule{Op: faultstore.OpCreate}, status: 500},
		{name: "write fails mid-stream", rule: faultstore.Rule{Op: faultstore.OpWrite, AtByte: 400 << 10}, status: 500},
		{name: "write times out", rule: faultstore.Rule{Op: faultstore.OpWrite, AtByte: 100 << 10, Err: faultstore.ErrTimeout}, status: 500},
		{name: "disk full mid-stream", rule: faultstore.Rule{Op: faultstore.OpWrite, AtByte: 400 << 10, Err: faultstore.ErrDiskFull}, status: 507},
		{name: "disk full on commit", rule: faultstore.Rule{Op: faultstore.OpCommit, Err: faultstore.ErrDiskFull}, status: 507},
		{name: "commit fails", rule: faultstore.Rule{Op: faultstore.OpCommit}, status: 500},
		{name: "commit reply lost", rule: faultstore.Rule{Op: faultstore.OpCommit, Err: faultstore.ErrTimeout, Applied: true}, status: 500, published: true},
		{name: "stat after commit fails", rule: faultstore.Rule{Op: faultstore.OpStat}, status: 500, published: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newFaultEnv(t, nil)
			u := e.newUser(t, "alice", fullAccess)
			l := e.userLib(t, u)
			key := e.attachment(t, l, "f.pdf")
			// Small enough that what is left of the body after the fault
			// fits in what net/http drains, so the client reliably reads
			// our answer instead of a reset.
			p := newPayload("f.pdf", bytes.Repeat([]byte("gotero!"), 600<<10/7))
			a := e.authorize(t, l, key, p)
			before := e.fileState(t, l, u, key)

			e.fs.Inject(c.rule)
			r := e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data)))
			require.Equal(t, 1, e.fs.Faults(c.rule.Op))
			retryable(t, r.Status, r.Body)
			assert.Equal(t, c.status, r.Status, string(r.Body))
			e.fs.Clear()

			assert.Equal(t, "pending", e.tokenState(t, a.UploadKey), "the client must be able to send again")
			assert.Equal(t, 400, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
			assert.Equal(t, before, e.fileState(t, l, u, key), "a failed upload changed what clients see")
			assert.False(t, e.hasRow(t, p.md5))
			assert.Equal(t, c.published, e.hasBytes(t, p.md5))
			assert.Zero(t, e.staging(t), "staging left behind")

			require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
			require.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
			after := e.fileState(t, l, u, key)
			assert.Equal(t, p.md5, after.md5)
			assert.EqualValues(t, len(p.data), after.used)
			assert.Equal(t, p.data, e.download(t, l, key))
		})
	}
}

// download fetches an attachment's file through the redirect.
func (e *faultEnv) download(t *testing.T, l *lib, key string) []byte {
	t.Helper()
	r := l.get("items/" + key + "/file")
	require.Equal(t, 302, r.Status)
	res := e.fetch(t, r.Header.Get("Location"), nil)
	require.NoError(t, res.err)
	require.Equal(t, 200, res.status)
	return res.body
}

type fetched struct {
	status int
	header http.Header
	body   []byte
	err    error
}

func (e *faultEnv) fetch(t *testing.T, loc string, hdr map[string]string) fetched {
	t.Helper()
	hr, err := http.NewRequestWithContext(ctx(), http.MethodGet, loc, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		hr.Header.Set(k, v)
	}
	res, err := e.client().Do(hr)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	return fetched{status: res.StatusCode, header: res.Header, body: b, err: err}
}

// The bytes reach storage, but the connection recording them dies. Nothing
// may point at them, the client must be told to retry, and the bytes must
// neither be collected while an upload can still claim them nor kept
// forever once nobody can.
func TestUploadDBFaultAfterCommit(t *testing.T) {
	for _, retry := range []bool{true, false} {
		t.Run("retry="+strconv.FormatBool(retry), func(t *testing.T) {
			e := newFaultEnv(t, nil)
			u := e.newUser(t, "alice", fullAccess)
			l := e.userLib(t, u)
			key := e.attachment(t, l, "d.pdf")
			p := newPayload("d.pdf", bytes.Repeat([]byte("db fault "), 20_000))
			a := e.authorize(t, l, key, p)
			before := e.fileState(t, l, u, key)

			e.kill.KillOn("INSERT INTO blobs", 1)
			r := e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data)))
			e.expectKilled(t)
			retryable(t, r.Status, r.Body)
			assert.Equal(t, "pending", e.tokenState(t, a.UploadKey))
			assert.Equal(t, before, e.fileState(t, l, u, key))
			assert.False(t, e.hasRow(t, p.md5))
			require.True(t, e.hasBytes(t, p.md5))

			// The upload token is still live, so GC must keep the bytes.
			res, err := e.gc(t)
			require.NoError(t, err)
			assert.Zero(t, res.OrphanBlobs)
			require.True(t, e.hasBytes(t, p.md5), "GC removed bytes a live upload can still claim")

			if retry {
				require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
				require.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
				_, err = e.Pool.Exec(ctx(), `UPDATE upload_tokens SET expires_at = now() - interval '1 minute'`)
				require.NoError(t, err)
				res, err = e.gc(t)
				require.NoError(t, err)
				assert.Zero(t, res.OrphanBlobs)
				assert.Equal(t, p.data, e.download(t, l, key), "GC removed referenced bytes")
				return
			}
			// The client gave up. Once its token expires the bytes belong to
			// nobody and the next pass collects them.
			_, err = e.Pool.Exec(ctx(), `UPDATE upload_tokens SET expires_at = now() - interval '1 minute'`)
			require.NoError(t, err)
			res, err = e.gc(t)
			require.NoError(t, err)
			assert.EqualValues(t, 1, res.OrphanBlobs)
			assert.False(t, e.hasBytes(t, p.md5), "untracked bytes leaked")
			assert.Equal(t, before, e.fileState(t, l, u, key))
		})
	}
}

// Killing the connection at any statement of the register transaction must
// leave the library exactly as it was, and the same register call must then
// go through.
func TestRegisterDBFaults(t *testing.T) {
	for _, stmt := range []string{
		"FOR UPDATE", // the library lock
		"UPDATE libraries SET version",
		"INSERT INTO items",
		"INSERT INTO attachment_files",
		"UPDATE libraries SET storage_used",
		"UPDATE upload_tokens SET state",
		"commit",
	} {
		t.Run(stmt, func(t *testing.T) {
			e := newFaultEnv(t, nil)
			u := e.newUser(t, "alice", fullAccess)
			l := e.userLib(t, u)
			key := e.attachment(t, l, "r.pdf")
			p := newPayload("r.pdf", []byte("registered after a crash"))
			a := e.authorize(t, l, key, p)
			require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
			before := e.fileState(t, l, u, key)

			e.kill.KillOn(stmt, 1)
			r := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
			e.expectKilled(t)
			retryable(t, r.Status, r.Body)
			assert.Equal(t, before, e.fileState(t, l, u, key), "half a registration is visible")
			assert.Equal(t, "uploaded", e.tokenState(t, a.UploadKey))

			require.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
			after := e.fileState(t, l, u, key)
			assert.Equal(t, before.libVersion+1, after.libVersion)
			assert.Equal(t, p.md5, after.md5)
			assert.EqualValues(t, len(p.data), after.used)
			assert.Equal(t, p.data, e.download(t, l, key))
		})
	}
}

// The {"exists":1} shortcut writes the same rows without a token.
func TestAttachExistingDBFault(t *testing.T) {
	e := newFaultEnv(t, nil)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	k1, k2 := e.attachment(t, l, "1.pdf"), e.attachment(t, l, "2.pdf")
	p := newPayload("1.pdf", []byte("shared content"))
	e.store(t, l, k1, p)
	before := e.fileState(t, l, u, k2)

	e.kill.KillOn("INSERT INTO attachment_files", 1)
	r := e.form(t, l, k2, p.form(), "If-None-Match", "*")
	e.expectKilled(t)
	retryable(t, r.Status, r.Body)
	assert.Equal(t, before, e.fileState(t, l, u, k2))

	r = e.form(t, l, k2, p.form(), "If-None-Match", "*")
	require.Equal(t, 200, r.Status, string(r.Body))
	assert.EqualValues(t, 1, r.json(t)["exists"])
	assert.Equal(t, p.data, e.download(t, l, k2))
}

// A multi-object write is one transaction: losing the connection anywhere in
// it must leave no object and no version bump behind.
func TestItemWriteDBFaults(t *testing.T) {
	for _, stmt := range []string{
		"FOR UPDATE",
		"UPDATE libraries SET version",
		"INSERT INTO items",
		"INSERT INTO item_tags",
		"commit",
	} {
		t.Run(stmt, func(t *testing.T) {
			e := newFaultEnv(t, nil)
			u := e.newUser(t, "alice", fullAccess)
			l := e.userLib(t, u)
			l.create("items", m{"itemType": "book", "title": "existing"})
			v0 := l.version()
			keysBefore := l.get("items?format=keys").Body

			batch := make([]m, 5)
			for i := range batch {
				batch[i] = m{"itemType": "book", "title": "b" + strconv.Itoa(i), "tags": []m{{"tag": "t" + strconv.Itoa(i)}}}
			}
			e.kill.KillOn(stmt, 1)
			r := l.post("items", batch, "If-Unmodified-Since-Version", strconv.FormatInt(v0, 10))
			e.expectKilled(t)
			retryable(t, r.Status, r.Body)
			assert.Equal(t, v0, l.version())
			assert.Equal(t, string(keysBefore), string(l.get("items?format=keys").Body))
			assert.Equal(t, "0", l.get("tags?limit=1").Header.Get("Total-Results"))

			r = l.post("items", batch, "If-Unmodified-Since-Version", strconv.FormatInt(v0, 10))
			require.Equal(t, 200, r.Status, string(r.Body))
			assert.Len(t, r.json(t)["success"], 5)
			assert.Equal(t, v0+1, l.version())
		})
	}
}

// noSeek hides io.Seeker so downloads take the plain copy path.
type noSeek struct{ storage.Backend }

func (b noSeek) Open(ctx context.Context, key string) (storage.Reader, storage.Info, error) {
	rd, info, err := b.Backend.Open(ctx, key)
	if err != nil {
		return nil, info, err
	}
	return struct{ io.ReadCloser }{rd}, info, nil
}

func TestDownloadStorageFaults(t *testing.T) {
	const size = 1 << 20
	for _, seekable := range []bool{true, false} {
		var wrap func(storage.Backend) storage.Backend
		if !seekable {
			wrap = func(b storage.Backend) storage.Backend { return noSeek{b} }
		}
		t.Run("seekable="+strconv.FormatBool(seekable), func(t *testing.T) {
			e := newFaultEnv(t, wrap)
			u := e.newUser(t, "alice", fullAccess)
			l := e.userLib(t, u)
			key := e.attachment(t, l, "dl.pdf")
			p := newPayload("dl.pdf", bytes.Repeat([]byte("0123456789abcdef"), size/16))
			e.store(t, l, key, p)
			loc := func() string {
				r := l.get("items/" + key + "/file")
				require.Equal(t, 302, r.Status)
				return r.Header.Get("Location")
			}

			t.Run("open fails", func(t *testing.T) {
				for _, c := range []struct {
					err    error
					status int
				}{{faultstore.ErrInjected, 500}, {faultstore.ErrTimeout, 500}, {storage.ErrNotFound, 404}} {
					e.fs.Inject(faultstore.Rule{Op: faultstore.OpOpen, Err: c.err, Times: 1})
					res := e.fetch(t, loc(), nil)
					require.NoError(t, res.err)
					assert.Equal(t, c.status, res.status, "%v", c.err)
					assert.NotEqual(t, p.data, res.body)
				}
			})

			for _, at := range []int64{0, 1, 300 << 10, size - 1} {
				t.Run("read fails at "+strconv.FormatInt(at, 10), func(t *testing.T) {
					e.fs.Inject(faultstore.Rule{Op: faultstore.OpRead, AtByte: at, Times: 1})
					res := e.fetch(t, loc(), nil)
					// Headers went out before the failure, so the status
					// cannot change any more. What must hold is that the
					// client notices: the body falls short of Content-Length
					// and the connection is cut, which every HTTP client
					// reports as an error.
					assert.Equal(t, 200, res.status)
					assert.Equal(t, strconv.Itoa(size), res.header.Get("Content-Length"))
					require.Error(t, res.err, "a truncated download looked complete")
					assert.Less(t, len(res.body), size)
				})
			}

			if seekable {
				t.Run("range read fails", func(t *testing.T) {
					e.fs.Inject(faultstore.Rule{Op: faultstore.OpRead, AtByte: 10, Times: 1})
					res := e.fetch(t, loc(), map[string]string{"Range": "bytes=100-199"})
					assert.Equal(t, 206, res.status)
					require.Error(t, res.err)
				})
			}

			// The file itself is fine once storage recovers.
			e.fs.Clear()
			assert.Equal(t, p.data, e.download(t, l, key))
		})
	}
}

// Deleting an attachment orphans its blob. A storage delete that fails must
// keep the row so the next pass retries, and referenced blobs must survive
// every pass.
func TestGCDeleteFaults(t *testing.T) {
	e := newFaultEnv(t, nil)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	keep, drop := e.attachment(t, l, "keep.pdf"), e.attachment(t, l, "drop.pdf")
	pk, pd := newPayload("keep.pdf", []byte("still referenced")), newPayload("drop.pdf", []byte("about to be orphaned"))
	e.store(t, l, keep, pk)
	e.store(t, l, drop, pd)
	r := l.send("DELETE", "items/"+drop, nil, "If-Unmodified-Since-Version", strconv.FormatInt(l.version(), 10))
	require.Equal(t, 204, r.Status, string(r.Body))
	_, err := e.Pool.Exec(ctx(), `UPDATE upload_tokens SET expires_at = now() - interval '1 minute'`)
	require.NoError(t, err)

	for _, rule := range []faultstore.Rule{
		{Op: faultstore.OpDelete, Times: 1},
		{Op: faultstore.OpDelete, Err: faultstore.ErrTimeout, Times: 1},
	} {
		e.fs.Inject(rule)
		res, err := e.gc(t)
		require.Error(t, err)
		assert.EqualValues(t, 1, res.Failed)
		assert.True(t, e.hasRow(t, pd.md5), "row dropped although its bytes are still there")
		assert.True(t, e.hasBytes(t, pd.md5))
	}

	// The walk for untracked bytes failing must not stop the row pass.
	e.fs.Inject(faultstore.Rule{Op: faultstore.OpWalk, Times: 1})
	res, err := e.gc(t)
	require.Error(t, err)
	assert.EqualValues(t, 1, res.OrphanBlobs)
	assert.False(t, e.hasRow(t, pd.md5))
	assert.False(t, e.hasBytes(t, pd.md5))

	res, err = e.gc(t)
	require.NoError(t, err)
	assert.Zero(t, res.OrphanBlobs)
	assert.Equal(t, pk.data, e.download(t, l, keep), "GC touched a referenced blob")
}

// TestRandomStorageFaultsConverge runs concurrent clients against a backend
// that fails a fraction of every kind of call, each client retrying 5xx the
// way Zotero does. Once storage recovers every file must be there exactly
// once: downloadable with the right bytes, charged once, with no row
// missing its bytes, no bytes missing a row, and nothing left in staging
// after a sweep. GOTERO_FAULT_SEED replays the fault pattern (call order
// still varies between runs, since the clients are concurrent).
func TestRandomStorageFaultsConverge(t *testing.T) {
	seed := uint64(20261108)
	if s := os.Getenv("GOTERO_FAULT_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		require.NoError(t, err)
		seed = v
	}
	t.Logf("GOTERO_FAULT_SEED=%d", seed)

	e := newSeededFaultEnv(t, nil, seed)

	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	const n = 12
	keys := make([]string, n)
	payloads := make([]payload, n)
	for i := range n {
		keys[i] = e.attachment(t, l, "r"+strconv.Itoa(i)+".pdf")
		// Every third file shares its content with another, so dedup and
		// the exists shortcut run under faults too.
		content := bytes.Repeat([]byte("content "+strconv.Itoa(i%8)+" "), 2000+i%8*500)
		payloads[i] = newPayload("r"+strconv.Itoa(i)+".pdf", content)
	}

	const p = 0.15
	for _, r := range []faultstore.Rule{
		{Op: faultstore.OpCreate, Prob: p},
		{Op: faultstore.OpWrite, Prob: p, AtByte: 4 << 10},
		{Op: faultstore.OpWrite, Prob: p / 2, AtByte: 0, Err: faultstore.ErrDiskFull},
		{Op: faultstore.OpCommit, Prob: p, Err: faultstore.ErrTimeout, Applied: true},
		{Op: faultstore.OpCommit, Prob: p},
		{Op: faultstore.OpAbort, Prob: 0.3},
		{Op: faultstore.OpStat, Prob: p},
		{Op: faultstore.OpOpen, Prob: p},
		{Op: faultstore.OpRead, Prob: p, AtByte: 3 << 10},
	} {
		e.fs.Inject(r)
	}

	// until retries op while the server answers 5xx, like Zotero's backoff.
	until := func(t *testing.T, what string, op func() (int, []byte), want ...int) {
		t.Helper()
		for attempt := 0; attempt < 60; attempt++ {
			status, body := op()
			for _, w := range want {
				if status == w {
					return
				}
			}
			require.GreaterOrEqual(t, status, 500, "%s: %d %s", what, status, body)
		}
		t.Fatalf("%s: still failing after 60 attempts", what)
	}

	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < n; i += 4 {
				key, pl := keys[i], payloads[i]
				var a authz
				exists := false
				until(t, "authorize", func() (int, []byte) {
					r := e.form(t, l, key, pl.form(), "If-None-Match", "*")
					if r.Status == 200 {
						j := r.json(t)
						if j["exists"] != nil {
							exists = true
						} else {
							a = authz{URL: j["url"].(string), UploadKey: j["uploadKey"].(string)}
						}
					}
					return r.Status, r.Body
				}, 200)
				if exists {
					continue
				}
				until(t, "upload", func() (int, []byte) {
					r := e.put(t, a.URL, bytes.NewReader(pl.data), int64(len(pl.data)))
					return r.Status, r.Body
				}, 201)
				until(t, "register", func() (int, []byte) {
					r := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
					return r.Status, r.Body
				}, 204)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	// Downloads under faults: an error is fine, wrong bytes never are.
	for i, key := range keys {
		r := l.get("items/" + key + "/file")
		require.Equal(t, 302, r.Status)
		for attempt := 0; ; attempt++ {
			require.Less(t, attempt, 60, "download never succeeded")
			res := e.fetch(t, r.Header.Get("Location"), nil)
			if res.err != nil || res.status >= 500 {
				continue
			}
			require.Equal(t, 200, res.status)
			require.Equal(t, payloads[i].data, res.body, "download returned wrong bytes")
			break
		}
	}
	t.Logf("faults injected: create=%d write=%d commit=%d abort=%d stat=%d open=%d read=%d",
		e.fs.Faults(faultstore.OpCreate), e.fs.Faults(faultstore.OpWrite), e.fs.Faults(faultstore.OpCommit),
		e.fs.Faults(faultstore.OpAbort), e.fs.Faults(faultstore.OpStat), e.fs.Faults(faultstore.OpOpen),
		e.fs.Faults(faultstore.OpRead))

	e.fs.Clear()
	var want int64
	unique := map[string]bool{}
	for i, key := range keys {
		assert.Equal(t, payloads[i].md5, l.dataOf("items", key)["md5"])
		want += int64(len(payloads[i].data))
		unique[payloads[i].md5] = true
	}
	assert.Equal(t, want, e.used(t, u), "storage charged wrongly")

	_, err := e.fs.SweepStaging(ctx(), 0)
	require.NoError(t, err)
	assert.Zero(t, e.staging(t))
	_, err = e.Pool.Exec(ctx(), `UPDATE upload_tokens SET expires_at = now() - interval '1 minute'`)
	require.NoError(t, err)
	res, err := e.gc(t)
	require.NoError(t, err)
	assert.Zero(t, res.OrphanBlobs, "GC found something to remove although every blob is referenced")

	rows := map[string]bool{}
	rs, err := e.Pool.Query(ctx(), `SELECT md5 FROM blobs`)
	require.NoError(t, err)
	for rs.Next() {
		var m string
		require.NoError(t, rs.Scan(&m))
		rows[m] = true
	}
	require.NoError(t, rs.Err())
	stored := map[string]bool{}
	require.NoError(t, e.fs.Inner().Walk(ctx(), "blobs/", func(info storage.Info) error {
		stored[filepath.Base(info.Key)] = true
		return nil
	}))
	assert.Equal(t, unique, rows, "blob rows")
	assert.Equal(t, unique, stored, "stored bytes")
	for i, key := range keys {
		assert.Equal(t, payloads[i].data, e.download(t, l, key))
	}
}
