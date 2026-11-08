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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
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
	defaultLeaseDuration    = 2 * time.Minute
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
	BaseURL string
	// HTTPClient talks to zotero.org and its file hosts. The default bounds
	// connecting, waiting for headers and stalls while reading a body.
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
	// LeaseDuration is how long a claimed job stays with this worker without
	// a renewal, 2 minutes by default. The lease is renewed four times per
	// duration, and a job whose lease ran out is requeued.
	LeaseDuration time.Duration
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
	if d.HTTPClient == nil {
		d.HTTPClient = newHTTPClient(bodyIdleTimeout)
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
	if d.LeaseDuration <= 0 {
		d.LeaseDuration = defaultLeaseDuration
	}
	return &Worker{Deps: d, retryDelay: defaultRetryDelay}
}

func (w *Worker) newClient(key string, opts ...zoteroclient.Option) *zoteroclient.Client {
	opts = append(opts, zoteroclient.WithBaseURL(w.BaseURL), zoteroclient.WithLogger(w.Log),
		zoteroclient.WithHTTPClient(w.HTTPClient))
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
	u := info.Access.User
	return adminapi.ImportKeyCheck{Valid: true, Library: u.Library, Notes: u.Notes, Files: u.Files}, nil
}

// Run claims and processes jobs until ctx ends. Now and then it puts jobs
// whose lease ran out back in the queue, whichever process held them, so a
// crashed server or a killed command line import does not leave a job stuck.
func (w *Worker) Run(ctx context.Context) {
	var lastRequeue time.Time
	for ctx.Err() == nil {
		if time.Since(lastRequeue) >= w.LeaseDuration/2 {
			w.requeueExpired(ctx)
			lastRequeue = time.Now()
		}
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

func (w *Worker) requeueExpired(ctx context.Context) {
	if n, err := w.Store.Imports().RequeueExpired(ctx); err != nil {
		if ctx.Err() == nil {
			w.Log.ErrorContext(ctx, "importer: requeue expired jobs", "err", err)
		}
	} else if n > 0 {
		w.Log.InfoContext(ctx, "importer: resuming interrupted jobs", "count", n)
	}
}

// RunOnce claims the oldest queued job and processes it to the end. It
// reports whether there was a job. The returned error is about the queue
// itself; a failing import is recorded on its job instead.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	token := newToken()
	job, err := w.Store.Imports().ClaimNext(ctx, token, w.LeaseDuration)
	if errors.Is(err, repo.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim import job: %w", err)
	}
	w.process(ctx, job, token)
	return true, nil
}

// newToken names one claim of a job. It is unique per claim rather than per
// process, so even a worker that claims the same job again after losing it
// cannot mistake the old lease for its own.
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var errLeaseLost = errors.New("importer: lease lost")

// panicError is a panic inside an import, turned into an error so the job
// fails instead of the whole server.
type panicError struct {
	value any
	stack []byte
}

func (p *panicError) Error() string { return fmt.Sprintf("panic: %v", p.value) }

func recoverPanic(err *error) {
	if v := recover(); v != nil {
		*err = &panicError{value: v, stack: debug.Stack()}
	}
}

func (w *Worker) process(ctx context.Context, job repo.ImportJob, token string) {
	log := w.Log.With("job", job.ID, "user", job.UserID)
	runCtx, stop := context.WithCancelCause(ctx)
	beat := w.heartbeat(runCtx, stop, job.ID, token, log)
	r := newRun(w, job, token, log)
	warning, err := r.safeExecute(runCtx)
	lost := errors.Is(context.Cause(runCtx), errLeaseLost)
	stop(nil)
	<-beat

	// Shutdown must not use the cancelled ctx, or the job state is lost.
	bg := context.WithoutCancel(ctx)
	var pe *panicError
	switch {
	case lost:
		log.WarnContext(bg, "importer: job taken over or cancelled, stopping")
	case errors.Is(err, errCancelled):
		log.InfoContext(bg, "importer: job cancelled or taken over")
	case err != nil && ctx.Err() != nil:
		// Shutting down mid-import: the cursor was saved with the last
		// checkpoint, so the job simply continues after the restart.
		r.checkpointQuiet(bg)
		if rerr := w.Store.Imports().Requeue(bg, job.ID, token); rerr != nil {
			log.ErrorContext(bg, "importer: requeue job", "err", rerr)
		}
	case errors.As(err, &pe):
		// Failing the job keeps a bug from taking the server down, and a
		// failed job is not requeued, so it cannot crash-loop either.
		log.ErrorContext(bg, "importer: job panicked", "panic", fmt.Sprint(pe.value), "stack", string(pe.stack))
		r.checkpointQuiet(bg)
		w.finish(bg, log, job.ID, token, "failed", "The import stopped because of an internal error ("+pe.Error()+"). Please report it.")
	case err != nil:
		log.ErrorContext(bg, "importer: job failed", "err", err)
		r.checkpointQuiet(bg)
		w.finish(bg, log, job.ID, token, "failed", describe(err))
	default:
		log.InfoContext(bg, "importer: job completed", "warning", warning)
		r.checkpointQuiet(bg)
		w.finish(bg, log, job.ID, token, "completed", warning)
	}
}

// safeExecute is execute with panics turned into errors. Panics in the
// helper goroutines of parallel arrive here as errors already.
func (r *run) safeExecute(ctx context.Context) (warning string, err error) {
	defer recoverPanic(&err)
	return r.execute(ctx)
}

// heartbeat renews the lease until ctx ends. Once the lease is gone, the job
// was cancelled or another worker took it over, and stop ends the run. The
// returned channel closes when the heartbeat has stopped.
func (w *Worker) heartbeat(ctx context.Context, stop context.CancelCauseFunc, id int64, token string, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(w.LeaseDuration / 4)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			ok, err := w.Store.Imports().Renew(ctx, id, token, w.LeaseDuration)
			switch {
			case err != nil:
				// The next tick tries again. If the database stays away
				// past the lease, another worker may take over, and the
				// refused writes stop this one.
				if ctx.Err() == nil {
					log.WarnContext(ctx, "importer: renew lease", "err", err)
				}
			case !ok:
				stop(errLeaseLost)
				return
			}
		}
	}()
	return done
}

func (w *Worker) finish(ctx context.Context, log *slog.Logger, id int64, token, status, msg string) {
	ok, err := w.Store.Imports().Finish(ctx, id, token, status, msg, w.Now())
	switch {
	case err != nil:
		log.ErrorContext(ctx, "importer: finish job", "err", err)
	case !ok:
		log.WarnContext(ctx, "importer: job was cancelled or taken over before it finished", "status", status)
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
// It stops handing out work after the first error, which it returns. A panic
// in fn becomes that error: in a goroutine of its own it would end the
// process before process() could recover it.
func parallel(ctx context.Context, limit, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		next  int
	)
	call := func(i int) (err error) {
		defer recoverPanic(&err)
		return fn(ctx, i)
	}
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
				if err := call(i); err != nil {
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
