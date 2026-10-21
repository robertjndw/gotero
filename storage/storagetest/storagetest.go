// Package storagetest is a conformance suite for storage.Backend
// implementations. Every bundled backend runs it, and third-party backends
// should too:
//
//	func TestMyBackend(t *testing.T) {
//		storagetest.Run(t, func(t *testing.T) storage.Backend {
//			return newMyBackendInEmptyNamespace(t)
//		})
//	}
//
// The factory is called once per subtest and must return a backend that
// starts out empty and is isolated from other calls (a fresh directory,
// bucket prefix, and so on). Cleanup should be registered with t.Cleanup.
package storagetest

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- content checksum for test data only
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertjndw/gotero/storage"
)

// Options tunes the suite for backends with unusual behaviour.
type Options struct {
	// SweepNeedsBytes is how many bytes an abandoned upload must have
	// received before SweepStaging can see it. Backends that stage straight
	// to storage (local, WebDAV) leave this at 0 (meaning 1 byte). S3 only
	// materializes a staging object once a whole part has been sent.
	SweepNeedsBytes int64

	// SweepMayMissAbandoned relaxes SweepStaging checks for servers that
	// cannot list in-flight uploads: the sweep must still succeed and leave
	// committed data alone, but it is not required to report the abandoned
	// upload.
	SweepMayMissAbandoned bool

	// LargeSize overrides the size of the streaming test (default 64 MiB).
	LargeSize int64
}

// Run executes the full suite with default options.
func Run(t *testing.T, newBackend func(t *testing.T) storage.Backend) {
	RunWithOptions(t, newBackend, Options{})
}

// RunWithOptions executes the full suite.
func RunWithOptions(t *testing.T, newBackend func(t *testing.T) storage.Backend, opts Options) {
	if opts.SweepNeedsBytes <= 0 {
		opts.SweepNeedsBytes = 1
	}
	if opts.LargeSize <= 0 {
		opts.LargeSize = 64 << 20
	}
	s := &suite{newBackend: newBackend, opts: opts}
	for _, c := range []struct {
		name string
		fn   func(*testing.T)
	}{
		{"RoundTrip", s.roundTrip},
		{"NestedKeys", s.nestedKeys},
		{"EmptyObject", s.emptyObject},
		{"MissingObject", s.missing},
		{"DeleteMissing", s.deleteMissing},
		{"Delete", s.deleteExisting},
		{"InvalidKeys", s.invalidKeys},
		{"CommitExistingKeepsContent", s.commitExisting},
		{"Abort", s.abort},
		{"AbortAfterCommit", s.abortAfterCommit},
		{"Walk", s.walk},
		{"WalkStopsOnError", s.walkStops},
		{"SweepStaging", s.sweep},
		{"Seeker", s.seeker},
		{"ContextCancel", s.ctxCancel},
		{"ConcurrentDistinctKeys", s.concurrentDistinct},
		{"ConcurrentSameKey", s.concurrentSame},
		{"LargeStreaming", s.large},
	} {
		t.Run(c.name, c.fn)
	}
}

type suite struct {
	newBackend func(t *testing.T) storage.Backend
	opts       Options
}

func (s *suite) backend(t *testing.T) storage.Backend {
	t.Helper()
	b := s.newBackend(t)
	if b == nil {
		t.Fatal("newBackend returned nil")
	}
	return b
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return c
}

// Put stores data under key through the streaming API.
func Put(c context.Context, b storage.Backend, key string, data []byte) error {
	return PutReader(c, b, key, bytes.NewReader(data))
}

// PutReader streams r into key.
func PutReader(c context.Context, b storage.Backend, key string, r io.Reader) error {
	up, err := b.Create(c)
	if err != nil {
		return err
	}
	if _, err := io.Copy(up, r); err != nil {
		_ = up.Abort(context.WithoutCancel(c))
		return err
	}
	if err := up.Commit(c, key); err != nil {
		_ = up.Abort(context.WithoutCancel(c))
		return err
	}
	return nil
}

// ReadAll opens key and returns its content and reported Info.
func ReadAll(c context.Context, b storage.Backend, key string) ([]byte, storage.Info, error) {
	r, info, err := b.Open(c, key)
	if err != nil {
		return nil, info, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	return data, info, err
}

// Pattern returns n deterministic pseudo-random bytes derived from seed.
func Pattern(seed uint64, n int) []byte {
	buf := make([]byte, n)
	_, _ = io.ReadFull(NewReader(seed, int64(n)), buf)
	return buf
}

// NewReader returns a reader producing n deterministic pseudo-random bytes
// without holding them in memory.
func NewReader(seed uint64, n int64) io.Reader {
	var key [32]byte
	for i := range 8 {
		key[i] = byte(seed >> (8 * i))
	}
	return &genReader{rng: rand.NewChaCha8(key), left: n}
}

type genReader struct {
	rng  *rand.ChaCha8
	left int64
}

func (g *genReader) Read(p []byte) (int, error) {
	if g.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > g.left {
		p = p[:g.left]
	}
	n, _ := g.rng.Read(p)
	g.left -= int64(n)
	return n, nil
}

func requireNoStaging(t *testing.T, b storage.Backend) {
	t.Helper()
	n, err := b.SweepStaging(ctx(t), 0)
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected clean staging area, sweep removed %d objects", n)
	}
}

func walkAll(t *testing.T, b storage.Backend, prefix string) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	err := b.Walk(ctx(t), prefix, func(i storage.Info) error {
		if _, dup := got[i.Key]; dup {
			t.Errorf("Walk reported %q twice", i.Key)
		}
		got[i.Key] = i.Size
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(%q): %v", prefix, err)
	}
	return got
}

func (s *suite) roundTrip(t *testing.T) {
	b := s.backend(t)
	data := Pattern(1, 300_000)
	if err := Put(ctx(t), b, "ab/key1", data); err != nil {
		t.Fatal(err)
	}
	got, info, err := ReadAll(ctx(t), b, "ab/key1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(data))
	}
	if info.Size != int64(len(data)) || info.Key != "ab/key1" {
		t.Fatalf("Open info = %+v", info)
	}
	st, err := b.Stat(ctx(t), "ab/key1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != int64(len(data)) || st.Key != "ab/key1" {
		t.Fatalf("Stat info = %+v", st)
	}
	if st.ModTime.IsZero() {
		t.Error("Stat returned zero ModTime")
	}
	requireNoStaging(t, b)
}

func (s *suite) nestedKeys(t *testing.T) {
	b := s.backend(t)
	sum := md5.Sum([]byte("nested")) // #nosec G401
	hexsum := hex.EncodeToString(sum[:])
	keys := []string{
		hexsum[:2] + "/" + hexsum,
		"a/b/c/d/e/deep",
		"with space/and+plus/%25pct",
		"unicode/über-日本",
	}
	for i, k := range keys {
		data := Pattern(uint64(10+i), 1000+i)
		if err := Put(ctx(t), b, k, data); err != nil {
			t.Fatalf("Put(%q): %v", k, err)
		}
		got, _, err := ReadAll(ctx(t), b, k)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read back %q: err=%v equal=%v", k, err, bytes.Equal(got, data))
		}
	}
	all := walkAll(t, b, "")
	if len(all) != len(keys) {
		t.Fatalf("Walk returned %v, want %d keys", all, len(keys))
	}
}

func (s *suite) emptyObject(t *testing.T) {
	b := s.backend(t)
	if err := Put(ctx(t), b, "ee/empty", nil); err != nil {
		t.Fatal(err)
	}
	got, info, err := ReadAll(ctx(t), b, "ee/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || info.Size != 0 {
		t.Fatalf("got %d bytes, info %+v", len(got), info)
	}
	st, err := b.Stat(ctx(t), "ee/empty")
	if err != nil || st.Size != 0 {
		t.Fatalf("Stat = %+v, %v", st, err)
	}
	requireNoStaging(t, b)
}

func (s *suite) missing(t *testing.T) {
	b := s.backend(t)
	if _, _, err := b.Open(ctx(t), "no/such"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open missing: %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(ctx(t), "no/such"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat missing: %v, want ErrNotFound", err)
	}
	// A "directory" is not an object.
	if err := Put(ctx(t), b, "dir/obj", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx(t), "dir"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat of a key prefix: %v, want ErrNotFound", err)
	}
	if _, _, err := b.Open(ctx(t), "dir"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Open of a key prefix: %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(ctx(t), "dir/obj/below"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Stat below an object: %v, want ErrNotFound", err)
	}
}

func (s *suite) deleteMissing(t *testing.T) {
	b := s.backend(t)
	if err := b.Delete(ctx(t), "no/such"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
	if err := b.Delete(ctx(t), "no/such"); err != nil {
		t.Fatalf("second Delete missing: %v", err)
	}
}

func (s *suite) deleteExisting(t *testing.T) {
	b := s.backend(t)
	if err := Put(ctx(t), b, "de/x", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx(t), "de/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx(t), "de/x"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat after Delete: %v", err)
	}
	// The key can be reused afterwards.
	if err := Put(ctx(t), b, "de/x", []byte("again")); err != nil {
		t.Fatal(err)
	}
	got, _, err := ReadAll(ctx(t), b, "de/x")
	if err != nil || string(got) != "again" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func (s *suite) invalidKeys(t *testing.T) {
	b := s.backend(t)
	for _, k := range []string{"", "/abs", "a/../b", "..", "a//b", "a\\b", ".staging/x", ".staging", "a/./b", "a/"} {
		if _, err := b.Stat(ctx(t), k); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Stat(%q): %v, want ErrInvalidKey", k, err)
		}
		if _, _, err := b.Open(ctx(t), k); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Open(%q): %v, want ErrInvalidKey", k, err)
		}
		if err := b.Delete(ctx(t), k); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Delete(%q): %v, want ErrInvalidKey", k, err)
		}
		up, err := b.Create(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = up.Write([]byte("data"))
		if err := up.Commit(ctx(t), k); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("Commit(%q): %v, want ErrInvalidKey", k, err)
		}
		if err := up.Abort(ctx(t)); err != nil {
			t.Errorf("Abort after rejected Commit: %v", err)
		}
	}
	if got := walkAll(t, b, ""); len(got) != 0 {
		t.Fatalf("invalid commits left objects behind: %v", got)
	}
	requireNoStaging(t, b)
}

func (s *suite) commitExisting(t *testing.T) {
	b := s.backend(t)
	orig := Pattern(3, 5000)
	if err := Put(ctx(t), b, "dd/same", orig); err != nil {
		t.Fatal(err)
	}
	other := Pattern(4, 7000)
	if err := Put(ctx(t), b, "dd/same", other); err != nil {
		t.Fatalf("second Commit to existing key must succeed: %v", err)
	}
	got, _, err := ReadAll(ctx(t), b, "dd/same")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("existing object was overwritten by a later Commit")
	}
	requireNoStaging(t, b)
}

func (s *suite) abort(t *testing.T) {
	b := s.backend(t)
	up, err := b.Create(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.Write(Pattern(5, 100_000)); err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(ctx(t)); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := up.Abort(ctx(t)); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if got := walkAll(t, b, ""); len(got) != 0 {
		t.Fatalf("aborted upload visible: %v", got)
	}
	requireNoStaging(t, b)

	// Abort without any write must work too.
	up, err = b.Create(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(ctx(t)); err != nil {
		t.Fatalf("Abort of untouched upload: %v", err)
	}
	requireNoStaging(t, b)
}

func (s *suite) abortAfterCommit(t *testing.T) {
	b := s.backend(t)
	up, err := b.Create(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	data := Pattern(6, 4096)
	if _, err := up.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := up.Commit(ctx(t), "ac/obj"); err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(ctx(t)); err != nil {
		t.Fatalf("Abort after Commit must be a no-op: %v", err)
	}
	got, _, err := ReadAll(ctx(t), b, "ac/obj")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("object damaged by Abort after Commit: %v", err)
	}
}

func (s *suite) walk(t *testing.T) {
	b := s.backend(t)
	want := map[string]int64{}
	for i, k := range []string{"ab/abc1", "ab/abd2", "ab/zzz", "cd/cde3", "ef/efg4", "top"} {
		n := 100 * (i + 1)
		if err := Put(ctx(t), b, k, Pattern(uint64(20+i), n)); err != nil {
			t.Fatal(err)
		}
		want[k] = int64(n)
	}
	// An in-flight upload must not show up.
	up, err := b.Create(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Abort(context.Background()) }()
	_, _ = up.Write([]byte("in flight"))

	check := func(prefix string) {
		t.Helper()
		exp := map[string]int64{}
		for k, v := range want {
			if strings.HasPrefix(k, prefix) {
				exp[k] = v
			}
		}
		got := walkAll(t, b, prefix)
		if len(got) != len(exp) {
			keys := func(m map[string]int64) []string {
				var o []string
				for k := range m {
					o = append(o, k)
				}
				sort.Strings(o)
				return o
			}
			t.Fatalf("Walk(%q) = %v, want %v", prefix, keys(got), keys(exp))
		}
		for k, v := range exp {
			if got[k] != v {
				t.Errorf("Walk(%q): %s size %d, want %d", prefix, k, got[k], v)
			}
		}
	}
	for _, p := range []string{"", "ab/", "ab", "ab/ab", "ab/abc", "cd/", "top", "nope/", "ab/abc1"} {
		check(p)
	}
}

func (s *suite) walkStops(t *testing.T) {
	b := s.backend(t)
	for i := range 5 {
		if err := Put(ctx(t), b, fmt.Sprintf("w/%d", i), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := errors.New("stop")
	calls := 0
	err := b.Walk(ctx(t), "", func(storage.Info) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk returned %v, want sentinel", err)
	}
	if calls != 1 {
		t.Fatalf("fn called %d times after returning an error", calls)
	}
}

func (s *suite) sweep(t *testing.T) {
	b := s.backend(t)
	if err := Put(ctx(t), b, "sw/keep", []byte("committed")); err != nil {
		t.Fatal(err)
	}
	up, err := b.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Written in chunks: some backends only notice it after a full part.
	chunk := Pattern(7, 1<<20)
	for written := int64(0); written < s.opts.SweepNeedsBytes; written += int64(len(chunk)) {
		n := min(int64(len(chunk)), s.opts.SweepNeedsBytes-written)
		if _, err := up.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
	}
	// Give object stores with coarse (1s) timestamps a chance to age it.
	time.Sleep(1100 * time.Millisecond)

	// Fresh staging must survive a sweep with a generous threshold.
	if n, err := b.SweepStaging(ctx(t), time.Hour); err != nil || n != 0 {
		t.Fatalf("SweepStaging(1h) = %d, %v; want 0, nil", n, err)
	}
	n, err := b.SweepStaging(ctx(t), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("SweepStaging: %v", err)
	}
	if n < 1 && !s.opts.SweepMayMissAbandoned {
		t.Fatalf("SweepStaging removed %d, want at least 1 abandoned upload", n)
	}
	got, _, err := ReadAll(ctx(t), b, "sw/keep")
	if err != nil || string(got) != "committed" {
		t.Fatalf("committed object affected by sweep: %q, %v", got, err)
	}
	// The sweeper pulled the rug out; the owner must still be able to clean up.
	if err := up.Abort(ctx(t)); err != nil {
		t.Fatalf("Abort after sweep: %v", err)
	}
	requireNoStaging(t, b)
	if all := walkAll(t, b, ""); len(all) != 1 {
		t.Fatalf("unexpected objects after sweep: %v", all)
	}
}

func (s *suite) seeker(t *testing.T) {
	b := s.backend(t)
	data := Pattern(8, 200_000)
	if err := Put(ctx(t), b, "sk/obj", data); err != nil {
		t.Fatal(err)
	}
	r, _, err := b.Open(ctx(t), "sk/obj")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	sk, ok := r.(io.Seeker)
	if !ok {
		t.Skip("reader does not implement io.Seeker")
	}
	read := func(n int) []byte {
		t.Helper()
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Fatalf("read: %v", err)
		}
		return buf
	}
	mid := int64(len(data) / 2)
	if pos, err := sk.Seek(mid, io.SeekStart); err != nil || pos != mid {
		t.Fatalf("Seek start: %d, %v", pos, err)
	}
	if !bytes.Equal(read(1000), data[mid:mid+1000]) {
		t.Fatal("bytes after SeekStart differ")
	}
	if pos, err := sk.Seek(500, io.SeekCurrent); err != nil || pos != mid+1500 {
		t.Fatalf("Seek current: %d, %v", pos, err)
	}
	if !bytes.Equal(read(100), data[mid+1500:mid+1600]) {
		t.Fatal("bytes after SeekCurrent differ")
	}
	if pos, err := sk.Seek(-50, io.SeekEnd); err != nil || pos != int64(len(data))-50 {
		t.Fatalf("Seek end: %d, %v", pos, err)
	}
	tail, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(tail, data[len(data)-50:]) {
		t.Fatalf("tail read: %v", err)
	}
	if _, err := sk.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(64), data[:64]) {
		t.Fatal("bytes after rewind differ")
	}
}

func (s *suite) ctxCancel(t *testing.T) {
	b := s.backend(t)
	cctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	up, err := b.Create(cctx)
	if err != nil {
		t.Fatal(err)
	}
	chunk := Pattern(9, 256<<10)
	if _, err := up.Write(chunk); err != nil {
		t.Fatalf("first write: %v", err)
	}
	cancel()

	// Either a later Write or the Commit must report the cancellation.
	var failed bool
	for range 400 { // up to ~100 MiB, far more than any buffering allows
		if _, err := up.Write(chunk); err != nil {
			failed = true
			break
		}
	}
	if cerr := up.Commit(cctx, "cx/never"); cerr == nil && !failed {
		t.Fatal("neither Write nor Commit failed after context cancellation")
	} else if cerr == nil {
		t.Fatal("Commit succeeded on a cancelled context")
	}
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("Abort after cancellation: %v", err)
	}
	if _, err := b.Stat(ctx(t), "cx/never"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("object visible after cancelled upload: %v", err)
	}
	requireNoStaging(t, b)

	// Cancelled contexts are honoured by the read-side methods as well.
	if _, err := b.Stat(cctx, "cx/never"); err == nil {
		t.Error("Stat with cancelled context succeeded")
	}
}

func (s *suite) concurrentDistinct(t *testing.T) {
	b := s.backend(t)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Put(ctx(t), b, fmt.Sprintf("cc/%02d", i), Pattern(uint64(100+i), 50_000+i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	for i := range n {
		got, _, err := ReadAll(ctx(t), b, fmt.Sprintf("cc/%02d", i))
		if err != nil || !bytes.Equal(got, Pattern(uint64(100+i), 50_000+i)) {
			t.Fatalf("object %d corrupt: %v", i, err)
		}
	}
	if got := walkAll(t, b, "cc/"); len(got) != n {
		t.Fatalf("Walk saw %d objects, want %d", len(got), n)
	}
	requireNoStaging(t, b)
}

func (s *suite) concurrentSame(t *testing.T) {
	b := s.backend(t)
	const n = 16
	data := Pattern(200, 120_000)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = Put(ctx(t), b, "cs/same", data)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	got, _, err := ReadAll(ctx(t), b, "cs/same")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content corrupt after racing commits: %v", err)
	}
	if all := walkAll(t, b, ""); len(all) != 1 {
		t.Fatalf("expected exactly one object, got %v", all)
	}
	requireNoStaging(t, b)
}

func (s *suite) large(t *testing.T) {
	if testing.Short() {
		t.Skip("large streaming test skipped in -short mode")
	}
	b := s.backend(t)
	size := s.opts.LargeSize
	h := md5.New() // #nosec G401
	src := io.TeeReader(NewReader(300, size), h)
	if err := PutReader(ctx(t), b, "lg/big", src); err != nil {
		t.Fatal(err)
	}
	want := h.Sum(nil)
	r, info, err := b.Open(ctx(t), "lg/big")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if info.Size != size {
		t.Fatalf("size %d, want %d", info.Size, size)
	}
	h2 := md5.New() // #nosec G401
	n, err := io.Copy(h2, r)
	if err != nil || n != size {
		t.Fatalf("read %d bytes, err %v", n, err)
	}
	if !bytes.Equal(h2.Sum(nil), want) {
		t.Fatal("md5 mismatch after round trip")
	}
	requireNoStaging(t, b)
}

// HeapGrowth uploads size bytes through b while sampling the heap, and fails
// the test when the in-use heap grows more than maxGrowth beyond the starting
// point. It is exported so backends can reuse it for their own memory tests.
// It returns the observed peak growth in bytes.
func HeapGrowth(t *testing.T, b storage.Backend, size int64, maxGrowth uint64) uint64 {
	t.Helper()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapInuse

	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak {
					peak = m.HeapInuse
				}
			}
		}
	}()
	err := PutReader(ctx(t), b, "mem/big", NewReader(400, size))
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	var growth uint64
	if peak > base {
		growth = peak - base
	}
	t.Logf("uploaded %d MiB, peak heap growth %d MiB (limit %d MiB)", size>>20, growth>>20, maxGrowth>>20)
	if growth > maxGrowth {
		t.Fatalf("heap grew by %d MiB while uploading, limit %d MiB", growth>>20, maxGrowth>>20)
	}
	return growth
}
