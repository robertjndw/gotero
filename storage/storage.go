// Package storage defines a small, streaming blob store abstraction used by
// gotero to keep attachment files. Backends live in subpackages (local, s3,
// webdav) and are validated by the shared suite in storagetest.
//
// Keys are opaque slash-separated strings chosen by the caller. Backends must
// never buffer a whole object in memory: uploads are written incrementally to
// a staging location and only become visible under their final key on Commit.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned (possibly wrapped) when a key does not exist.
var ErrNotFound = errors.New("storage: object not found")

// Info describes a stored object.
type Info struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Reader is returned by Open. Backends that can seek cheaply (local files,
// S3 range requests) also implement io.Seeker, which lets HTTP handlers serve
// Range requests via http.ServeContent.
type Reader interface {
	io.ReadCloser
}

// Backend is a blob store.
type Backend interface {
	// Create starts a new upload into a staging location. The caller must
	// finish it with exactly one of Commit or Abort.
	Create(ctx context.Context) (Upload, error)

	// Open returns a reader for key, or ErrNotFound.
	Open(ctx context.Context, key string) (Reader, Info, error)

	// Stat returns metadata for key, or ErrNotFound.
	Stat(ctx context.Context, key string) (Info, error)

	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error

	// Walk calls fn for every committed object whose key starts with prefix.
	// Staging objects are not included. Returning an error from fn stops the
	// walk and Walk returns that error.
	Walk(ctx context.Context, prefix string, fn func(Info) error) error

	// SweepStaging removes abandoned staging objects older than olderThan
	// and reports how many were removed.
	SweepStaging(ctx context.Context, olderThan time.Duration) (int, error)
}

// Upload is an in-progress write.
type Upload interface {
	io.Writer

	// Commit makes the written bytes visible under key. If key already exists
	// Commit succeeds and discards the staged copy: content-addressed callers
	// rely on this to deduplicate without a race.
	Commit(ctx context.Context, key string) error

	// Abort discards the staged bytes. Calling Abort after Commit is a no-op,
	// so callers can always defer it.
	Abort(ctx context.Context) error
}
