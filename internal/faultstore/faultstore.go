// Package faultstore wraps a storage.Backend and makes it fail on purpose.
//
// The HTTP-level fault tests (conformance/syncsim) and the process-level ops
// test only ever see a storage layer that either works or is gone. Real
// object stores and disks fail in between: a PUT dies after half the bytes,
// a commit times out after the copy went through, the disk fills up, a
// delete is refused. This package lets tests program exactly those failures
// into any backend, so they can check that gotero keeps its invariants: no
// row points at missing bytes, no bytes are leaked forever, and the client
// always gets an answer it can retry.
//
// Faults are described by Rules. Every call to the wrapped backend is
// matched against the rules in the order they were added and the first one
// that fires decides what happens. Randomness comes from a seeded generator,
// so a failing run can be replayed with the same seed (as long as the calls
// arrive in the same order).
//
//	fs := faultstore.New(local, 1)
//	fs.Inject(faultstore.Rule{Op: faultstore.OpWrite, AtByte: 1 << 20, Err: faultstore.ErrDiskFull})
//	fs.Inject(faultstore.Rule{Op: faultstore.OpCommit, After: 1, Times: 1})
//
// The package is meant for tests only; nothing in the server imports it.
package faultstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"sync"
	"syscall"
	"time"

	"github.com/robertjndw/gotero/storage"
)

// Op names one kind of backend call.
type Op string

const (
	OpCreate Op = "create"
	// OpWrite is an upload stream. A rule matches once per upload (at
	// Create), so After and Times count uploads, not Write calls. The fault
	// itself hits the Write that crosses AtByte.
	OpWrite  Op = "write"
	OpCommit Op = "commit"
	OpAbort  Op = "abort"
	OpOpen   Op = "open"
	// OpRead is a download stream, matched once per Open like OpWrite.
	OpRead   Op = "read"
	OpStat   Op = "stat"
	OpDelete Op = "delete"
	OpWalk   Op = "walk"
	OpSweep  Op = "sweep"
)

// Errors for the common failure shapes. Rules may return any error; these
// look like what the real backends produce, so error classification in the
// server is exercised the same way.
var (
	// ErrInjected is the default fault: an unclassified backend error.
	ErrInjected = errors.New("faultstore: injected fault")
	// ErrTimeout is a backend request that ran out of time. It is not the
	// caller's context expiring, which is why it wraps DeadlineExceeded
	// without the caller's ctx being done.
	ErrTimeout = fmt.Errorf("faultstore: backend request timed out: %w", context.DeadlineExceeded)
	// ErrDiskFull is what the local backend surfaces when the filesystem
	// is full.
	ErrDiskFull error = &fs.PathError{Op: "write", Path: "faultstore", Err: syscall.ENOSPC}
)

// Rule describes one fault. The zero value of every field means "no
// restriction", so Rule{Op: OpCommit} fails every commit with ErrInjected.
type Rule struct {
	Op Op
	// Key restricts the rule to one key. Ignored for OpCreate and OpWrite,
	// which do not know their key yet, and for OpWalk and OpSweep.
	Key string
	// After lets this many matching calls through before the rule starts
	// firing; After: n-1, Times: 1 fails exactly the nth call.
	After int
	// Times caps how often the rule fires. 0 means no cap.
	Times int
	// Prob, when > 0, makes each eligible call fire only with this
	// probability, drawn from the store's seeded generator.
	Prob float64
	// AtByte applies to OpWrite and OpRead: this many bytes of the stream
	// get through, then the call that would cross it returns the error.
	AtByte int64
	// Err is the error to return. With Err nil and Delay > 0 the rule only
	// adds latency; with both unset it returns ErrInjected.
	Err error
	// Delay is added before the call, or before every Write/Read of a
	// selected stream. It gives up early if the call's context ends.
	Delay time.Duration
	// Applied runs the real call first and keeps its effect, then reports
	// the error anyway: a commit or delete that went through but whose
	// reply was lost. Only meaningful for OpCommit, OpAbort, OpDelete,
	// OpStat, OpWalk and OpSweep.
	Applied bool
}

func (r *Rule) err() error {
	switch {
	case r.Err != nil:
		return r.Err
	case r.Delay > 0:
		return nil
	default:
		return ErrInjected
	}
}

type rule struct {
	Rule
	seen  int
	fired int
}

// Store is a storage.Backend that injects faults into another one. It is
// safe for concurrent use.
type Store struct {
	inner storage.Backend

	mu     sync.Mutex
	rng    *rand.Rand
	rules  []*rule
	calls  map[Op]int
	faults map[Op]int
}

var _ storage.Backend = (*Store)(nil)

// New wraps inner. seed makes probabilistic rules reproducible.
func New(inner storage.Backend, seed uint64) *Store {
	return &Store{
		inner:  inner,
		rng:    rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), //nolint:gosec // reproducible test faults
		calls:  map[Op]int{},
		faults: map[Op]int{},
	}
}

// Inner returns the wrapped backend, for checking what really got stored.
func (s *Store) Inner() storage.Backend { return s.inner }

// Inject adds a rule. Rules added earlier take precedence.
func (s *Store) Inject(r Rule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &rule{Rule: r})
}

// Clear removes every rule. Counters are kept.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = nil
}

// Calls reports how many times op was attempted. Streams count once.
func (s *Store) Calls(op Op) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[op]
}

// Faults reports how many faults were injected into op, latency-only rules
// excluded.
func (s *Store) Faults(op Op) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults[op]
}

// pick returns a copy of the rule that fires for this call, if any.
func (s *Store) pick(op Op, key string) (Rule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[op]++
	for _, r := range s.rules {
		if r.Op != op || (r.Key != "" && key != "" && r.Key != key) {
			continue
		}
		r.seen++
		if r.seen <= r.After || (r.Times > 0 && r.fired >= r.Times) {
			continue
		}
		if r.Prob > 0 && s.rng.Float64() >= r.Prob {
			continue
		}
		r.fired++
		if r.err() != nil {
			s.faults[op]++
		}
		return r.Rule, true
	}
	return Rule{}, false
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// before handles a plain call: it waits out any delay and returns the
// error to report. With applied true the caller still runs the real call
// and reports the error afterwards; otherwise the error replaces the call.
func (s *Store) before(ctx context.Context, op Op, key string) (applied bool, err error) {
	r, ok := s.pick(op, key)
	if !ok {
		return false, nil
	}
	if err := sleep(ctx, r.Delay); err != nil {
		return false, err
	}
	return r.Applied, r.err()
}

// Create implements storage.Backend.
func (s *Store) Create(ctx context.Context) (storage.Upload, error) {
	if _, err := s.before(ctx, OpCreate, ""); err != nil {
		return nil, err
	}
	up, err := s.inner.Create(ctx)
	if err != nil {
		return nil, err
	}
	u := &upload{s: s, inner: up, ctx: ctx}
	if r, ok := s.pick(OpWrite, ""); ok {
		u.stream = &stream{rule: r}
	}
	return u, nil
}

// Open implements storage.Backend.
func (s *Store) Open(ctx context.Context, key string) (storage.Reader, storage.Info, error) {
	if _, err := s.before(ctx, OpOpen, key); err != nil {
		return nil, storage.Info{}, err
	}
	rd, info, err := s.inner.Open(ctx, key)
	if err != nil {
		return nil, info, err
	}
	r, ok := s.pick(OpRead, key)
	if !ok {
		return rd, info, nil
	}
	fr := &reader{inner: rd, ctx: ctx, stream: stream{rule: r}}
	// Keep io.Seeker visible when the real reader has it: the download
	// handler takes a different path for seekable readers and both need
	// covering.
	if _, ok := rd.(io.Seeker); ok {
		return &seekReader{fr}, info, nil
	}
	return fr, info, nil
}

// Stat implements storage.Backend.
func (s *Store) Stat(ctx context.Context, key string) (storage.Info, error) {
	applied, err := s.before(ctx, OpStat, key)
	if err != nil && !applied {
		return storage.Info{}, err
	}
	info, ierr := s.inner.Stat(ctx, key)
	if err != nil {
		return storage.Info{}, err
	}
	return info, ierr
}

// Delete implements storage.Backend.
func (s *Store) Delete(ctx context.Context, key string) error {
	applied, err := s.before(ctx, OpDelete, key)
	if err != nil && !applied {
		return err
	}
	if ierr := s.inner.Delete(ctx, key); ierr != nil {
		return ierr
	}
	return err
}

// Walk implements storage.Backend.
func (s *Store) Walk(ctx context.Context, prefix string, fn func(storage.Info) error) error {
	applied, err := s.before(ctx, OpWalk, "")
	if err != nil && !applied {
		return err
	}
	if ierr := s.inner.Walk(ctx, prefix, fn); ierr != nil {
		return ierr
	}
	return err
}

// SweepStaging implements storage.Backend.
func (s *Store) SweepStaging(ctx context.Context, olderThan time.Duration) (int, error) {
	applied, err := s.before(ctx, OpSweep, "")
	if err != nil && !applied {
		return 0, err
	}
	n, ierr := s.inner.SweepStaging(ctx, olderThan)
	if ierr != nil {
		return n, ierr
	}
	return n, err
}

// stream is the byte budget of one selected upload or download.
type stream struct {
	rule Rule
	done int64
}

// limit returns how much of p may pass and the error to report after it.
// A latency-only rule never cuts the stream.
func (st *stream) limit(ctx context.Context, p []byte) (int, error) {
	if err := sleep(ctx, st.rule.Delay); err != nil {
		return 0, err
	}
	err := st.rule.err()
	if err == nil {
		return len(p), nil
	}
	left := st.rule.AtByte - st.done
	if left < 0 {
		left = 0
	}
	if int64(len(p)) <= left {
		return len(p), nil
	}
	return int(left), err
}

type upload struct {
	s      *Store
	inner  storage.Upload
	ctx    context.Context
	stream *stream
}

func (u *upload) Write(p []byte) (int, error) {
	if u.stream == nil {
		return u.inner.Write(p)
	}
	allow, ferr := u.stream.limit(u.ctx, p)
	n, err := u.inner.Write(p[:allow])
	u.stream.done += int64(n)
	if err != nil {
		return n, err
	}
	if ferr != nil {
		return n, ferr
	}
	return n, nil
}

func (u *upload) Commit(ctx context.Context, key string) error {
	applied, err := u.s.before(ctx, OpCommit, key)
	if err != nil && !applied {
		return err
	}
	if ierr := u.inner.Commit(ctx, key); ierr != nil {
		return ierr
	}
	return err
}

// Abort is the call that cleans up staging. A fault here that is not
// Applied leaves the staged bytes behind on purpose, so tests can check
// that SweepStaging eventually removes them.
func (u *upload) Abort(ctx context.Context) error {
	applied, err := u.s.before(ctx, OpAbort, "")
	if err != nil && !applied {
		return err
	}
	if ierr := u.inner.Abort(ctx); ierr != nil {
		return ierr
	}
	return err
}

type reader struct {
	inner  storage.Reader
	ctx    context.Context
	stream stream
}

func (r *reader) Read(p []byte) (int, error) {
	allow, ferr := r.stream.limit(r.ctx, p)
	if ferr != nil && allow == 0 {
		return 0, ferr
	}
	n, err := r.inner.Read(p[:allow])
	r.stream.done += int64(n)
	if err != nil {
		return n, err
	}
	if ferr != nil && n == allow {
		return n, ferr
	}
	return n, nil
}

func (r *reader) Close() error { return r.inner.Close() }

type seekReader struct{ *reader }

func (r *seekReader) Seek(offset int64, whence int) (int64, error) {
	return r.inner.(io.Seeker).Seek(offset, whence)
}
