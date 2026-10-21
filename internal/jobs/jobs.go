// Package jobs runs gotero's periodic maintenance: pruning expired rows,
// sweeping abandoned uploads and collecting orphaned files.
package jobs

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
)

// Defaults chosen so maintenance is invisible under normal use.
const (
	// KeepDeleted is how long delete-log entries are kept. A client that has
	// not synced for longer gets a 409 and does a full sync, which is safe.
	KeepDeleted = 180 * 24 * time.Hour
	// OrphanGrace protects files that were just uploaded but whose
	// registration is still in flight or about to be retried.
	OrphanGrace = 24 * time.Hour
	// StagingGrace is how old an abandoned staging object must be before it
	// is removed; long uploads over slow links must not be cut off.
	StagingGrace = 24 * time.Hour
)

// GCResult describes one garbage collection pass.
type GCResult struct {
	OrphanBlobs  int64
	OrphanBytes  int64
	StagingFiles int
	Deleted      bool
}

// Runner owns the maintenance loops.
type Runner struct {
	store repo.Store
	files *files.Service
	log   *slog.Logger
	now   func() time.Time

	// gcMu keeps a manual GC from the admin UI and the scheduled one from
	// running at the same time.
	gcMu sync.Mutex
}

func New(store repo.Store, files *files.Service, log *slog.Logger) *Runner {
	return &Runner{store: store, files: files, log: log, now: time.Now}
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
				if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
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

// Prune removes expired sessions, tokens and old delete-log entries.
func (r *Runner) Prune(ctx context.Context) error {
	n, err := r.store.Prune(ctx, r.now(), KeepDeleted)
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
	if !dryRun {
		res.OrphanBlobs, res.OrphanBytes = 0, 0
		cutoff := r.now().Add(-OrphanGrace)
		for _, b := range victims {
			removed, err := r.files.RemoveOrphan(ctx, b.MD5, cutoff)
			if err != nil {
				return res, err
			}
			if removed {
				res.OrphanBlobs++
				res.OrphanBytes += b.Size
			}
		}
		n, err := r.files.Backend().SweepStaging(ctx, StagingGrace)
		if err != nil {
			return res, err
		}
		res.StagingFiles = n
	}
	if res.OrphanBlobs > 0 || res.StagingFiles > 0 {
		r.log.Info("garbage collection", "dry_run", dryRun, "orphan_blobs", res.OrphanBlobs,
			"orphan_bytes", res.OrphanBytes, "staging_files", res.StagingFiles)
	}
	return res, nil
}
