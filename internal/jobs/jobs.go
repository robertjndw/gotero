// Package jobs runs gotero's periodic maintenance: pruning expired rows,
// sweeping abandoned uploads and collecting orphaned files.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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

	// gcMu keeps a manual GC from the admin UI and the scheduled one from
	// running at the same time.
	gcMu sync.Mutex
}

func New(store repo.Store, files *files.Service, log *slog.Logger, keepDeleted time.Duration) *Runner {
	return &Runner{
		store: store, files: files, log: log, now: time.Now, keepDeleted: keepDeleted,
		blobTimeout: BlobGCTimeout,
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
	loop("prune", 10*time.Minute, r.Prune)
	loop("gc", 6*time.Hour, func(ctx context.Context) error {
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
	var errs []error
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
				if len(errs) == 0 {
					errs = append(errs, err)
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
	if res.OrphanBlobs > 0 || res.StagingFiles > 0 || res.Failed > 0 {
		r.log.Info("garbage collection", "dry_run", dryRun, "orphan_blobs", res.OrphanBlobs,
			"orphan_bytes", res.OrphanBytes, "staging_files", res.StagingFiles, "failed", res.Failed)
	}
	if res.Failed > 0 {
		errs[0] = fmt.Errorf("%d orphan blobs could not be removed, first: %w", res.Failed, errs[0])
	}
	return res, errors.Join(errs...)
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
