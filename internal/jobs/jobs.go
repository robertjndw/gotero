// Package jobs runs gotero's periodic maintenance: pruning expired rows,
// sweeping abandoned uploads and collecting orphaned files.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"path"
	"runtime/debug"
	"sync"
	"time"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
)

// Defaults chosen so maintenance is invisible under normal use.
const (
	// OrphanGrace protects files that were just uploaded but whose
	// registration is still in flight or about to be retried.
	OrphanGrace = 24 * time.Hour
	// StagingGrace is how old an abandoned staging object must be before it
	// is removed; long uploads over slow links must not be cut off.
	StagingGrace = 24 * time.Hour
	// BlobGCTimeout bounds removing one orphan, storage round trip included.
	BlobGCTimeout = 2 * time.Minute
	// PruneInterval and GCInterval are the mean periods of the scheduled
	// jobs; each wait is jittered between half and the full period.
	PruneInterval = 10 * time.Minute
	GCInterval    = 6 * time.Hour
)

// GCResult describes one garbage collection pass.
type GCResult struct {
	OrphanBlobs  int64
	OrphanBytes  int64
	StagingFiles int
	Deleted      bool
	// Failed counts orphans whose removal failed; they are retried next pass.
	Failed int64
}

// Runner owns the maintenance loops.
type Runner struct {
	store repo.Store
	files *files.Service
	log   *slog.Logger
	now   func() time.Time
	// keepDeleted is the delete-log retention; <= 0 keeps the log forever.
	keepDeleted time.Duration
	blobTimeout time.Duration
	// Scheduling periods, fields only so tests can run the loops quickly.
	pruneEvery time.Duration
	gcEvery    time.Duration

	// gcMu keeps a manual GC from the admin UI and the scheduled one from
	// running at the same time.
	gcMu sync.Mutex
}

func New(store repo.Store, files *files.Service, log *slog.Logger, keepDeleted time.Duration) *Runner {
	return &Runner{
		store: store, files: files, log: log, now: time.Now, keepDeleted: keepDeleted,
		blobTimeout: BlobGCTimeout, pruneEvery: PruneInterval, gcEvery: GCInterval,
	}
}

// Run blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loop := func(name string, every time.Duration, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Jitter so restarts of many instances don't align, and so the
				// first run doesn't compete with startup.
				wait := every/2 + rand.N(every/2) //nolint:gosec // scheduling jitter
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				if err := r.runOnce(ctx, name, fn); err != nil && !errors.Is(err, context.Canceled) {
					r.log.Error("maintenance job failed", "job", name, "err", err)
				}
			}
		}()
	}
	loop("prune", r.pruneEvery, r.Prune)
	loop("gc", r.gcEvery, func(ctx context.Context) error {
		_, err := r.GC(ctx, false)
		return err
	})
	wg.Wait()
}

// runOnce turns a panic in one run of a job into an error, so a bug in
// maintenance takes down that run rather than the whole server, and the next
// tick tries again.
func (r *Runner) runOnce(ctx context.Context, name string, fn func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("maintenance job panicked", "job", name, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("job %s panicked: %v", name, p)
		}
	}()
	return fn(ctx)
}

// Prune removes expired sessions and tokens, and delete-log entries older
// than the configured retention when there is one.
func (r *Runner) Prune(ctx context.Context) error {
	n, err := r.store.Prune(ctx, r.now(), r.keepDeleted)
	if err != nil {
		return err
	}
	if n > 0 {
		r.log.Info("pruned expired rows", "rows", n)
	}
	return nil
}

// GC removes blobs no attachment references and abandoned staging uploads.
// With dryRun it only reports what it would remove.
func (r *Runner) GC(ctx context.Context, dryRun bool) (GCResult, error) {
	r.gcMu.Lock()
	defer r.gcMu.Unlock()

	res := GCResult{Deleted: !dryRun}
	orphans, err := r.store.Files().OrphanBlobs(ctx, r.now().Add(-OrphanGrace))
	if err != nil {
		return res, err
	}
	// Collect first: deleting while the iterator holds its query open would
	// keep a connection busy for the whole sweep.
	var victims []repo.Blob
	for b, err := range orphans {
		if err != nil {
			return res, err
		}
		victims = append(victims, b)
		res.OrphanBlobs++
		res.OrphanBytes += b.Size
	}
	var (
		errs []error
		// Kept out of errs so the summary below only ever wraps a blob error.
		firstFail error
	)
	if !dryRun {
		res.OrphanBlobs, res.OrphanBytes = 0, 0
		cutoff := r.now().Add(-OrphanGrace)
		for _, b := range victims {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			// One unreachable or broken object must not stop the rest of the
			// sweep. A failed blob keeps its row, so the next pass retries it.
			removed, err := r.removeOrphan(ctx, b.MD5, cutoff)
			if err != nil {
				res.Failed++
				if firstFail == nil {
					firstFail = err
				}
				r.log.Warn("garbage collection: could not remove orphan blob", "md5", b.MD5, "err", err)
				continue
			}
			if removed {
				res.OrphanBlobs++
				res.OrphanBytes += b.Size
			}
		}
		n, err := r.files.Backend().SweepStaging(ctx, StagingGrace)
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep staging: %w", err))
		}
		res.StagingFiles = n
	}
	if err := r.collectUntracked(ctx, dryRun, &res, &firstFail); err != nil {
		errs = append(errs, err)
	}
	if res.OrphanBlobs > 0 || res.StagingFiles > 0 || res.Failed > 0 {
		r.log.Info("garbage collection", "dry_run", dryRun, "orphan_blobs", res.OrphanBlobs,
			"orphan_bytes", res.OrphanBytes, "staging_files", res.StagingFiles, "failed", res.Failed)
	}
	if firstFail != nil {
		errs = append(errs, fmt.Errorf("%w: %d failed, first: %w", ErrBlobsNotRemoved, res.Failed, firstFail))
	}
	return res, errors.Join(errs...)
}

// ErrBlobsNotRemoved marks the part of a GC error that is only about single
// blobs whose removal failed. The rest of the sweep still ran, and those
// blobs are retried on the next pass.
var ErrBlobsNotRemoved = errors.New("jobs: orphan blobs could not be removed")

// OnlyBlobFailures reports whether every error GC returned is an
// ErrBlobsNotRemoved, so the pass counts as done apart from those blobs.
func OnlyBlobFailures(err error) bool {
	if err == nil {
		return false
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range j.Unwrap() {
			if !OnlyBlobFailures(e) {
				return false
			}
		}
		return true
	}
	return errors.Is(err, ErrBlobsNotRemoved)
}

// ErrUntrackedSuspicious stops the untracked sweep when so much of the store
// is unknown to the database that the database is more likely wrong than the
// files: gotero pointed at an empty or restored-from-old database, or a second
// instance sharing the bucket. Deleting in that state would wipe real files.
var ErrUntrackedSuspicious = errors.New("jobs: too many stored files are unknown to the database; " +
	"not deleting any, check that GOTERO_DATABASE_URL and the storage settings belong together")

// untrackedLimit is how many untracked blobs a sweep may delete out of
// scanned. A crash leaks a handful at a time, so anything near a tenth of
// the store means the database and the store don't belong together.
func untrackedLimit(scanned int) int {
	return max(20, scanned/10)
}

// collectUntracked removes bytes that have no blobs row at all, which the
// row-driven pass above can never see. They are counted as orphans.
// Per-blob failures go to firstFail and res.Failed; the returned error is
// for anything that stopped the sweep itself.
func (r *Runner) collectUntracked(ctx context.Context, dryRun bool, res *GCResult, firstFail *error) error {
	cutoff := r.now().Add(-OrphanGrace)
	untracked, scanned, err := r.files.UntrackedBlobs(ctx, cutoff)
	if err != nil {
		return err
	}
	if len(untracked) > untrackedLimit(scanned) {
		r.log.Error("garbage collection: refusing to delete untracked blobs",
			"untracked", len(untracked), "scanned", scanned, "err", ErrUntrackedSuspicious)
		return ErrUntrackedSuspicious
	}
	for _, info := range untracked {
		if dryRun {
			res.OrphanBlobs++
			res.OrphanBytes += info.Size
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m := path.Base(info.Key)
		bctx, cancel := context.WithTimeout(ctx, r.blobTimeout)
		removed, err := r.files.RemoveUntracked(bctx, m, cutoff)
		cancel()
		if err != nil {
			res.Failed++
			if *firstFail == nil {
				*firstFail = err
			}
			r.log.Warn("garbage collection: could not remove untracked blob", "md5", m, "err", err)
			continue
		}
		if removed {
			res.OrphanBlobs++
			res.OrphanBytes += info.Size
		}
	}
	return nil
}

// removeOrphan bounds one blob's removal. RemoveOrphan keeps a database
// connection and the blob lock while the backend deletes the bytes, and
// uploads of the same content wait on that lock, so a hung storage request
// must not hold them indefinitely.
func (r *Runner) removeOrphan(ctx context.Context, md5hex string, cutoff time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.blobTimeout)
	defer cancel()
	return r.files.RemoveOrphan(ctx, md5hex, cutoff)
}
