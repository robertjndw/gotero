package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/robertjndw/gotero/storage"
)

// Open implements storage.Backend. The reader implements io.Seeker: every
// Seek that moves the position drops the current response, and the next Read
// issues a ranged GET. Servers that ignore Range are handled by skipping
// bytes, which is correct but slow.
func (b *Backend) Open(ctx context.Context, key string) (storage.Reader, storage.Info, error) {
	info, err := b.Stat(ctx, key) // also rejects collections, which GET would happily serve as HTML
	if err != nil {
		return nil, storage.Info{}, err
	}
	if info.Size < 0 {
		return nil, storage.Info{}, fmt.Errorf("webdav: server did not report a size for %s", key)
	}
	r := &reader{ctx: ctx, b: b, key: key, size: info.Size}
	if info.Size > 0 {
		// Open the stream now so a 404 race surfaces here and not on Read.
		if err := r.connect(); err != nil {
			return nil, storage.Info{}, err
		}
	}
	return r, info, nil
}

type reader struct {
	ctx  context.Context
	b    *Backend
	key  string
	size int64
	pos  int64
	body io.ReadCloser
}

func (r *reader) connect() error {
	req, err := r.b.newRequest(r.ctx, http.MethodGet, r.b.urlFor(r.key, false), nil)
	if err != nil {
		return err
	}
	if r.pos > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
	}
	resp, err := r.b.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav GET: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if r.pos > 0 {
			// Range ignored; fast-forward.
			if _, err := io.CopyN(io.Discard, resp.Body, r.pos); err != nil {
				_ = resp.Body.Close()
				return fmt.Errorf("webdav GET: skip to offset: %w", err)
			}
		}
	case http.StatusPartialContent:
	default:
		defer drain(resp)
		return statusError("GET "+r.key, resp)
	}
	r.body = resp.Body
	return nil
}

func (r *reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		if err := r.connect(); err != nil {
			return 0, err
		}
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	if errors.Is(err, io.EOF) && r.pos < r.size {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *reader) Seek(offset int64, whence int) (int64, error) {
	var np int64
	switch whence {
	case io.SeekStart:
		np = offset
	case io.SeekCurrent:
		np = r.pos + offset
	case io.SeekEnd:
		np = r.size + offset
	default:
		return 0, errors.New("webdav: invalid whence")
	}
	if np < 0 {
		return 0, errors.New("webdav: negative seek position")
	}
	if np != r.pos && r.body != nil {
		_ = r.body.Close()
		r.body = nil
	}
	r.pos = np
	return np, nil
}

func (r *reader) Close() error {
	if r.body == nil {
		return nil
	}
	err := r.body.Close()
	r.body = nil
	return err
}
