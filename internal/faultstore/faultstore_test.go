package faultstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/faultstore"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
	"github.com/robertjndw/gotero/storage/storagetest"
)

func newLocal(t *testing.T) storage.Backend {
	t.Helper()
	b, err := local.New(t.TempDir(), local.WithNoSync())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Without rules the wrapper must be invisible, or every fault test would
// be testing the wrapper as much as the server.
func TestTransparentWithoutRules(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		return faultstore.New(newLocal(t), 1)
	}, storagetest.Options{LargeSize: 8 << 20})
}

// A latency-only rule on every stream exercises the wrapped reader and
// writer types against the whole contract, Seek included.
func TestTransparentWithLatencyRules(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		s := faultstore.New(newLocal(t), 1)
		s.Inject(faultstore.Rule{Op: faultstore.OpWrite, Delay: time.Nanosecond})
		s.Inject(faultstore.Rule{Op: faultstore.OpRead, Delay: time.Nanosecond})
		return s
	}, storagetest.Options{LargeSize: 1 << 20})
}

func put(ctx context.Context, b storage.Backend, key string, data []byte) error {
	up, err := b.Create(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = up.Abort(context.Background()) }()
	if _, err := io.Copy(up, bytes.NewReader(data)); err != nil {
		return err
	}
	return up.Commit(ctx, key)
}

func TestFailNthCall(t *testing.T) {
	ctx := context.Background()
	s := faultstore.New(newLocal(t), 1)
	s.Inject(faultstore.Rule{Op: faultstore.OpCommit, After: 1, Times: 1})
	if err := put(ctx, s, "a/1", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := put(ctx, s, "a/2", []byte("two")); !errors.Is(err, faultstore.ErrInjected) {
		t.Fatalf("second commit: %v", err)
	}
	if _, err := s.Stat(ctx, "a/2"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a failed commit must not publish: %v", err)
	}
	if err := put(ctx, s, "a/3", []byte("three")); err != nil {
		t.Fatal(err)
	}
	if s.Calls(faultstore.OpCommit) != 3 || s.Faults(faultstore.OpCommit) != 1 {
		t.Fatalf("calls %d faults %d", s.Calls(faultstore.OpCommit), s.Faults(faultstore.OpCommit))
	}
}

func TestKeyedRuleAndAppliedFault(t *testing.T) {
	ctx := context.Background()
	s := faultstore.New(newLocal(t), 1)
	s.Inject(faultstore.Rule{Op: faultstore.OpDelete, Key: "k/x", Err: faultstore.ErrTimeout, Applied: true})
	for _, k := range []string{"k/x", "k/y"} {
		if err := put(ctx, s, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, "k/y"); err != nil {
		t.Fatal(err)
	}
	err := s.Delete(ctx, "k/x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Stat(ctx, "k/x"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("an Applied fault must still perform the delete")
	}
}

func TestPartialWrite(t *testing.T) {
	ctx := context.Background()
	s := faultstore.New(newLocal(t), 1)
	s.Inject(faultstore.Rule{Op: faultstore.OpWrite, AtByte: 10, Err: faultstore.ErrDiskFull})
	up, err := s.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Abort(ctx) }()
	n, err := io.Copy(up, bytes.NewReader(make([]byte, 100)))
	if n != 10 || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("wrote %d, err %v", n, err)
	}
}

func TestPartialReadKeepsSeeker(t *testing.T) {
	ctx := context.Background()
	s := faultstore.New(newLocal(t), 1)
	if err := put(ctx, s, "r/1", bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}
	s.Inject(faultstore.Rule{Op: faultstore.OpRead, AtByte: 30})
	rd, _, err := s.Open(ctx, "r/1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rd.Close() }()
	if _, ok := rd.(io.Seeker); !ok {
		t.Fatal("a seekable reader must stay seekable")
	}
	got, err := io.ReadAll(rd)
	if len(got) != 30 || !errors.Is(err, faultstore.ErrInjected) {
		t.Fatalf("read %d, err %v", len(got), err)
	}
}

func TestProbabilityIsReproducible(t *testing.T) {
	ctx := context.Background()
	run := func() []bool {
		s := faultstore.New(newLocal(t), 42)
		s.Inject(faultstore.Rule{Op: faultstore.OpStat, Prob: 0.3})
		var out []bool
		for range 200 {
			_, err := s.Stat(ctx, "nope/x")
			out = append(out, errors.Is(err, faultstore.ErrInjected))
		}
		return out
	}
	a, b := run(), run()
	fired := 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("same seed, same calls, different faults")
		}
		if a[i] {
			fired++
		}
	}
	if fired < 30 || fired > 90 {
		t.Fatalf("p=0.3 fired %d of 200", fired)
	}
}

func TestDelayHonoursContext(t *testing.T) {
	s := faultstore.New(newLocal(t), 1)
	s.Inject(faultstore.Rule{Op: faultstore.OpOpen, Delay: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := s.Open(ctx, "a/b")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("open: %v after %v", err, time.Since(start))
	}
}

// An Abort fault leaves staging behind, which is what the tests of the
// server's sweep rely on.
func TestAbortFaultLeavesStaging(t *testing.T) {
	ctx := context.Background()
	s := faultstore.New(newLocal(t), 1)
	s.Inject(faultstore.Rule{Op: faultstore.OpAbort})
	up, err := s.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = up.Write([]byte("abandoned"))
	if err := up.Abort(ctx); !errors.Is(err, faultstore.ErrInjected) {
		t.Fatal(err)
	}
	n, err := s.SweepStaging(ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
}
