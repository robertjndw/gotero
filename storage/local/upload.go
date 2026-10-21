package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/robertjndw/gotero/storage"
)

type upload struct {
	ctx  context.Context // Create's context; cancelling it fails further writes
	b    *Backend
	f    *os.File
	path string

	mu   sync.Mutex
	done bool
}

func (u *upload) Write(p []byte) (int, error) {
	if err := u.ctx.Err(); err != nil {
		return 0, err
	}
	return u.f.Write(p)
}

// Commit publishes the staged file. Hard-linking (rather than renaming) makes
// "first writer wins" atomic: os.Link fails with EEXIST instead of silently
// replacing an object a concurrent uploader just committed. Filesystems
// without hard links fall back to a rename guarded by an existence check.
func (u *upload) Commit(ctx context.Context, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return errors.New("local: upload already finished")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dst, err := u.b.path(key)
	if err != nil {
		return err
	}
	if !u.b.noSync {
		if err := u.f.Sync(); err != nil {
			return fmt.Errorf("local: fsync: %w", err)
		}
	}
	if err := u.f.Close(); err != nil {
		return fmt.Errorf("local: close: %w", err)
	}
	u.done = true // from here the staging file is ours to clean up

	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		_ = os.Remove(u.path)
		return fmt.Errorf("local: mkdir: %w", err)
	}
	err = os.Link(u.path, dst)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		// Content-addressed dedup: someone else already stored this key.
		_ = os.Remove(u.path)
		return nil
	default:
		if _, serr := os.Lstat(dst); serr == nil {
			_ = os.Remove(u.path)
			return nil
		}
		// No hard link support; a rename is the best we can do.
		if rerr := os.Rename(u.path, dst); rerr != nil {
			_ = os.Remove(u.path)
			return fmt.Errorf("local: publish: %w", rerr)
		}
		u.syncDir(dir)
		return nil
	}
	_ = os.Remove(u.path)
	u.syncDir(dir)
	return nil
}

func (u *upload) syncDir(dir string) {
	if u.b.noSync {
		return
	}
	d, err := os.Open(dir) // #nosec G304 -- derived from validated key
	if err != nil {
		return
	}
	// Some filesystems refuse fsync on directories; the data is still durable
	// enough in practice, so the error is not fatal.
	_ = d.Sync()
	_ = d.Close()
}

func (u *upload) Abort(context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return nil
	}
	u.done = true
	_ = u.f.Close()
	if err := os.Remove(u.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

var _ storage.Upload = (*upload)(nil)
