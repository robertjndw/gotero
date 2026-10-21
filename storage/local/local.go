// Package local stores blobs as plain files below a root directory.
//
// Uploads are written to <root>/.staging/<random> and moved into place on
// Commit, so readers never observe a partially written object.
//
//	b, err := local.New("/var/lib/gotero/files")
//	if err != nil { ... }
//	up, _ := b.Create(ctx)
//	defer up.Abort(ctx)
//	io.Copy(up, src)
//	err = up.Commit(ctx, "ab/abcdef0123456789")
package local

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/robertjndw/gotero/storage"
)

const (
	dirPerm  = 0o750
	filePerm = 0o640
)

// Option configures a Backend.
type Option func(*Backend)

// WithNoSync disables fsync calls. Only meant for tests and benchmarks: a
// crash can then lose or truncate committed objects.
func WithNoSync() Option { return func(b *Backend) { b.noSync = true } }

// Backend is a storage.Backend on the local filesystem.
type Backend struct {
	root    string
	staging string
	noSync  bool
}

var _ storage.Backend = (*Backend)(nil)

// New returns a Backend rooted at root, creating the directory (and its
// staging directory) if needed.
func New(root string, opts ...Option) (*Backend, error) {
	if root == "" {
		return nil, errors.New("local: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	b := &Backend{root: abs, staging: filepath.Join(abs, storage.StagingDir)}
	for _, o := range opts {
		o(b)
	}
	if err := os.MkdirAll(b.staging, dirPerm); err != nil {
		return nil, fmt.Errorf("local: create root: %w", err)
	}
	return b, nil
}

// path maps a validated key to a filesystem path.
func (b *Backend) path(key string) (string, error) {
	if err := storage.ValidateKey(key); err != nil {
		return "", err
	}
	return filepath.Join(b.root, filepath.FromSlash(key)), nil
}

// Create implements storage.Backend.
func (b *Backend) Create(ctx context.Context) (storage.Upload, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := storage.NewStagingName()
	if err != nil {
		return nil, err
	}
	p := filepath.Join(b.staging, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm) // #nosec G304 -- generated name
	if err != nil {
		return nil, fmt.Errorf("local: create staging file: %w", err)
	}
	return &upload{ctx: ctx, b: b, f: f, path: p}, nil
}

// Open implements storage.Backend. The returned reader is an *os.File, so it
// also implements io.Seeker and io.ReaderAt.
func (b *Backend) Open(ctx context.Context, key string) (storage.Reader, storage.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, storage.Info{}, err
	}
	p, err := b.path(key)
	if err != nil {
		return nil, storage.Info{}, err
	}
	f, err := os.Open(p) // #nosec G304 -- key validated
	if err != nil {
		return nil, storage.Info{}, mapErr(err)
	}
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		_ = f.Close()
		if err == nil {
			err = storage.ErrNotFound
		}
		return nil, storage.Info{}, mapErr(err)
	}
	return f, storage.Info{Key: key, Size: st.Size(), ModTime: st.ModTime()}, nil
}

// Stat implements storage.Backend.
func (b *Backend) Stat(ctx context.Context, key string) (storage.Info, error) {
	if err := ctx.Err(); err != nil {
		return storage.Info{}, err
	}
	p, err := b.path(key)
	if err != nil {
		return storage.Info{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return storage.Info{}, mapErr(err)
	}
	if st.IsDir() {
		return storage.Info{}, storage.ErrNotFound
	}
	return storage.Info{Key: key, Size: st.Size(), ModTime: st.ModTime()}, nil
}

// Delete implements storage.Backend. Empty parent directories are left in
// place; they are cheap and removing them would race with concurrent Commits.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Walk implements storage.Backend. Order is lexical by path.
func (b *Backend) Walk(ctx context.Context, prefix string, fn func(storage.Info) error) error {
	if err := storage.ValidatePrefix(prefix); err != nil {
		return err
	}
	return filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // raced with a Delete
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if key == "." {
			return nil
		}
		if key == storage.StagingDir {
			return filepath.SkipDir
		}
		if d.IsDir() {
			// Prune subtrees that can't contain a match.
			dirKey := key + "/"
			if !strings.HasPrefix(dirKey, prefix) && !strings.HasPrefix(prefix, dirKey) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasPrefix(key, prefix) || !d.Type().IsRegular() {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		return fn(storage.Info{Key: key, Size: st.Size(), ModTime: st.ModTime()})
	})
}

// SweepStaging implements storage.Backend, using file mtime as the age.
func (b *Backend) SweepStaging(ctx context.Context, olderThan time.Duration) (int, error) {
	entries, err := os.ReadDir(b.staging)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		if olderThan > 0 && st.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(b.staging, e.Name())); err == nil {
			n++
		} else if !errors.Is(err, fs.ErrNotExist) {
			return n, err
		}
	}
	return n, nil
}

func mapErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
	}
	// A path component that is a regular file (ENOTDIR) also means "no such key".
	var pe *fs.PathError
	if errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "not a directory") {
		return fmt.Errorf("%w: %w", storage.ErrNotFound, err)
	}
	return err
}
