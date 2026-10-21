package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/robertjndw/gotero/storage"
)

var errAborted = errors.New("webdav: upload aborted")

type upload struct {
	b      *Backend
	ctx    context.Context // Create's context
	cancel context.CancelFunc
	rel    string // staging path relative to the base URL

	// streaming mode
	pw   *io.PipeWriter
	done chan struct{}
	err  error // PUT result, valid after done is closed

	// stage-locally mode
	tmp *os.File

	mu       sync.Mutex
	finished bool
}

// Create implements storage.Backend. The upload runs on ctx: cancelling it
// aborts the transfer, so pass a context that outlives the whole upload.
func (b *Backend) Create(ctx context.Context) (storage.Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := storage.NewStagingName()
	if err != nil {
		return nil, err
	}
	if err := b.mkcolAll(ctx, storage.StagingDir); err != nil {
		return nil, err
	}
	uctx, cancel := context.WithCancel(ctx)
	u := &upload{b: b, ctx: ctx, cancel: cancel, rel: storage.StagingDir + "/" + name}
	if b.local {
		f, err := os.CreateTemp(b.tempDir, "gotero-webdav-*")
		if err != nil {
			cancel()
			return nil, fmt.Errorf("webdav: temp file: %w", err)
		}
		u.tmp = f
		return u, nil
	}
	pr, pw := io.Pipe()
	u.pw, u.done = pw, make(chan struct{})
	go func() {
		defer close(u.done)
		err := b.put(uctx, u.rel, pr, -1)
		u.err = err
		if err == nil {
			err = io.ErrClosedPipe // the server answered before the body ended
		}
		pr.CloseWithError(err)
	}()
	return u, nil
}

// put sends body to rel. size < 0 selects chunked transfer encoding.
func (b *Backend) put(ctx context.Context, rel string, body io.Reader, size int64) error {
	req, err := b.newRequest(ctx, http.MethodPut, b.urlFor(rel, false), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav PUT: %w", err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	}
	return statusError("PUT "+rel, resp)
}

func (u *upload) Write(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	if u.tmp != nil {
		return u.tmp.Write(p)
	}
	return u.pw.Write(p)
}

func (u *upload) Commit(ctx context.Context, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finished {
		return errors.New("webdav: upload already finished")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	if err := u.finishPut(ctx); err != nil {
		return err
	}
	b := u.b
	if err := b.mkdirs(ctx, key); err != nil {
		return err
	}
	status, err := b.move(ctx, u.rel, key)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		// Someone removed a collection we had cached; recreate and retry once.
		b.forgetDirs()
		if err := b.mkdirs(ctx, key); err != nil {
			return err
		}
		if status, err = b.move(ctx, u.rel, key); err != nil {
			return err
		}
	}
	switch status {
	case http.StatusCreated, http.StatusNoContent, http.StatusOK:
	case http.StatusPreconditionFailed:
		// Destination exists: content-addressed dedup, so drop our copy.
	default:
		return fmt.Errorf("webdav MOVE %s -> %s: unexpected status %d", u.rel, key, status)
	}
	u.finished = true
	u.cancel()
	if status == http.StatusPreconditionFailed {
		_ = b.deletePath(context.WithoutCancel(ctx), u.rel)
	}
	return nil
}

// finishPut makes sure the staging file is complete on the server.
func (u *upload) finishPut(ctx context.Context) error {
	if u.tmp != nil {
		size, err := u.tmp.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		if _, err := u.tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return u.b.put(ctx, u.rel, u.tmp, size)
	}
	_ = u.pw.Close()
	select {
	case <-u.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if u.err != nil {
		return fmt.Errorf("webdav: upload staging file: %w", u.err)
	}
	return nil
}

// move renames src to dst without overwriting and returns the HTTP status.
func (b *Backend) move(ctx context.Context, src, dst string) (int, error) {
	resp, err := b.do(ctx, "MOVE", b.urlFor(src, false), map[string]string{
		"Destination": b.urlFor(dst, false).String(),
		"Overwrite":   "F",
	})
	if err != nil {
		return 0, fmt.Errorf("webdav MOVE: %w", err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return 0, statusError("MOVE "+src, resp)
	}
	return resp.StatusCode, nil
}

func (b *Backend) forgetDirs() {
	b.dirs.Range(func(k, _ any) bool { b.dirs.Delete(k); return true })
}

func (u *upload) Abort(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.finished {
		return nil
	}
	u.finished = true
	if u.tmp != nil {
		name := u.tmp.Name()
		_ = u.tmp.Close()
		_ = os.Remove(name)
	} else {
		_ = u.pw.CloseWithError(errAborted)
	}
	u.cancel()
	if u.done != nil {
		<-u.done
	}
	// Remove whatever part of the staging file reached the server. The
	// caller's ctx may be the cancelled one, cleanup must still run.
	return u.b.deletePath(context.WithoutCancel(ctx), u.rel)
}

var _ storage.Upload = (*upload)(nil)
