// Package importer copies a zotero.org library into a gotero account: the
// personal library, optionally the groups the key can read, and the attachment
// files. It runs as a background worker that claims queued import jobs.
//
// Into an empty library, objects keep their zotero.org keys and versions and
// are written straight through the repository, so a Zotero client that synced
// with zotero.org before sees the same versions on gotero. Into a library that
// already has content, including every rerun, objects get fresh library
// versions like any API write, and an object edited in gotero since the
// importer wrote it is left alone.
package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/robertjndw/gotero/internal/adminapi"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zoteroclient"
)

const (
	defaultPollInterval     = 3 * time.Second
	defaultProgressInterval = time.Second
	defaultRetryDelay       = 2 * time.Second
	// fileWorkers is how many attachments download at once. zotero.org rate
	// limits per key, so this stays modest.
	fileWorkers = 4
)

// Deps are the collaborators of the Worker.
type Deps struct {
	Store repo.Store
	// Files stores attachment bytes. Without it files are skipped.
	Files *files.Service
	Log   *slog.Logger
	// BaseURL is the zotero.org API root, https://api.zotero.org/ by default.
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
	// DefaultQuota returns the instance-wide quota in bytes, nil for
	// unlimited. When unset there is no instance default.
	DefaultQuota func(ctx context.Context) (*int64, error)
	// Publisher, if set, is told when an import moved a library to a new
	// version so connected clients sync right away.
	Publisher zapi.Publisher
	// Notifier, if set, is told when the import created or attached groups
	// for the user, so connected clients learn about their new libraries.
	Notifier Notifier

	// PollInterval and ProgressInterval default to 3s and 1s.
	PollInterval     time.Duration
	ProgressInterval time.Duration
}

// Notifier tells connected clients that a user's library access changed.
type Notifier interface {
	AccessChanged(userID int64)
}

// Worker processes import jobs one at a time.
type Worker struct {
	Deps
	// retryDelay separates the two attempts at a failing file.
	retryDelay time.Duration
}

var _ adminapi.ImportValidator = (*Worker)(nil)

// New builds a Worker. Optional dependencies get defaults.
func New(d Deps) *Worker {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.BaseURL == "" {
		d.BaseURL = zoteroclient.DefaultBaseURL
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.PollInterval <= 0 {
		d.PollInterval = defaultPollInterval
	}
	if d.ProgressInterval <= 0 {
		d.ProgressInterval = defaultProgressInterval
	}
	return &Worker{Deps: d, retryDelay: defaultRetryDelay}
}

func (w *Worker) newClient(key string, opts ...zoteroclient.Option) *zoteroclient.Client {
	opts = append(opts, zoteroclient.WithBaseURL(w.BaseURL), zoteroclient.WithLogger(w.Log))
	if w.HTTPClient != nil {
		opts = append(opts, zoteroclient.WithHTTPClient(w.HTTPClient))
	}
	return zoteroclient.New(key, opts...)
}

// ValidateImportKey implements adminapi.ImportValidator. A key zotero.org
// rejects is a normal answer; only transport problems and unexpected
// statuses are errors.
func (w *Worker) ValidateImportKey(ctx context.Context, apiKey string) (adminapi.ImportKeyCheck, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// A short retry budget: the admin UI request is waiting on this.
	info, err := w.newClient(apiKey, zoteroclient.WithMaxRetries(1)).CurrentKey(ctx)
	switch {
	case zoteroclient.IsForbidden(err):
		return adminapi.ImportKeyCheck{}, nil
	case err != nil:
		return adminapi.ImportKeyCheck{}, err
	}
	return adminapi.ImportKeyCheck{Valid: true, Library: info.Access.User.Library, Files: info.Access.User.Files}, nil
}

// Run claims and processes jobs until ctx ends. Jobs left running by a
// previous process are put back in the queue first; this assumes one
// importer per database.
func (w *Worker) Run(ctx context.Context) {
	if n, err := w.Store.Imports().RequeueRunning(ctx); err != nil {
		w.Log.ErrorContext(ctx, "importer: requeue running jobs", "err", err)
	} else if n > 0 {
		w.Log.InfoContext(ctx, "importer: resuming interrupted jobs", "count", n)
	}
	for ctx.Err() == nil {
		did, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.ErrorContext(ctx, "importer: run job", "err", err)
		}
		if did && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.PollInterval):
		}
	}
}

// RunOnce claims the oldest queued job and processes it to the end. It
// reports whether there was a job. The returned error is about the queue
// itself; a failing import is recorded on its job instead.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, err := w.Store.Imports().ClaimNext(ctx)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim import job: %w", err)
	}
	w.process(ctx, job)
	return true, nil
}

func (w *Worker) process(ctx context.Context, job repo.ImportJob) {
	log := w.Log.With("job", job.ID, "user", job.UserID)
	r := newRun(w, job, log)
	warning, err := r.execute(ctx)

	// Shutdown must not use the cancelled ctx, or the job state is lost.
	bg := context.WithoutCancel(ctx)
	switch {
	case errors.Is(err, errCancelled):
		log.InfoContext(bg, "importer: job cancelled")
	case err != nil && ctx.Err() != nil:
		// Shutting down mid-import: the cursor was saved with the last
		// checkpoint, so the job simply continues after the restart.
		r.checkpointQuiet(bg)
		if rerr := w.Store.Imports().Requeue(bg, job.ID); rerr != nil {
			log.ErrorContext(bg, "importer: requeue job", "err", rerr)
		}
	case err != nil:
		log.ErrorContext(bg, "importer: job failed", "err", err)
		r.checkpointQuiet(bg)
		w.finish(bg, log, job.ID, "failed", describe(err))
	default:
		log.InfoContext(bg, "importer: job completed", "warning", warning)
		r.checkpointQuiet(bg)
		w.finish(bg, log, job.ID, "completed", warning)
	}
}

func (w *Worker) finish(ctx context.Context, log *slog.Logger, id int64, status, msg string) {
	if _, err := w.Store.Imports().Finish(ctx, id, status, msg, w.Now()); err != nil {
		log.ErrorContext(ctx, "importer: finish job", "err", err)
	}
}

// describe turns an error into text the web UI can show to the user.
func describe(err error) string {
	var ae *zoteroclient.APIError
	switch {
	case zoteroclient.IsForbidden(err):
		return "zotero.org refused access. Check that the API key allows reading your library, notes, files and groups."
	case errors.As(err, &ae):
		return fmt.Sprintf("zotero.org answered HTTP %d for %s %s.", ae.StatusCode, ae.Method, ae.URL)
	case errors.Is(err, repo.ErrBusy):
		return "The library is busy. Start the import again in a moment."
	}
	return err.Error()
}

// parallel runs fn for every index below n with at most limit calls at once.
// It stops handing out work after the first error, which it returns.
func parallel(ctx context.Context, limit, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  int
	)
	for range min(limit, n) {
		wg.Go(func() {
			for {
				mu.Lock()
				if first != nil || next >= n || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				if err := fn(ctx, i); err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}
