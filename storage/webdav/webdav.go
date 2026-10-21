// Package webdav stores blobs on a WebDAV server using a small hand-written
// client (PUT, GET, DELETE, MKCOL, MOVE and PROPFIND over net/http).
//
// Uploads are streamed with chunked transfer encoding into
// <base>/.staging/<random> and then MOVEd to their final path with
// "Overwrite: F", which makes the "first writer wins" dedup atomic on servers
// that honour the header.
//
//	b, err := webdav.New(webdav.Config{
//		URL:      "https://dav.example.com/gotero/",
//		Username: "user",
//		Password: "secret",
//	})
//
// # Chunked PUT compatibility
//
// Streaming needs the server to accept a PUT without Content-Length. Apache
// mod_dav, nginx with the dav module (version 1.3.9 and later, and only with
// chunked_transfer_encoding on), rclone and most application servers do.
// Some proxies and appliances answer 411 Length Required. For those, set
// Config.StageLocally: the body is first written to a temporary file and
// then sent with a Content-Length. That costs local disk space equal to the
// object size and delays the first byte to the server until the client has
// finished uploading, but it uses no more memory than streaming does.
package webdav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/robertjndw/gotero/storage"
)

// Config describes the WebDAV collection to store blobs in.
type Config struct {
	// URL of the root collection, for example https://host/dav/gotero/.
	// It must exist, or at least be creatable with a single MKCOL.
	URL      string
	Username string
	Password string
	// HTTPClient is used for all requests. The default has no overall
	// timeout, because uploads and downloads are streams; use the request
	// context for deadlines.
	HTTPClient *http.Client
	// StageLocally buffers each upload in a temporary file before sending it
	// with a Content-Length instead of streaming it chunked. See the package
	// documentation for the trade-off.
	StageLocally bool
	// TempDir is where StageLocally files go (default: os.TempDir()).
	TempDir string
}

// Backend is a storage.Backend on WebDAV.
type Backend struct {
	base    *url.URL // path without trailing slash
	user    string
	pass    string
	client  *http.Client
	local   bool
	tempDir string

	dirs sync.Map // known-existing collections (relative paths), a cache only
}

var _ storage.Backend = (*Backend)(nil)

// New validates cfg and returns a Backend. It does not contact the server.
func New(cfg Config) (*Backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("webdav: bad URL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("webdav: URL must be http(s)://host/path")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath, u.RawQuery, u.Fragment, u.User = "", "", "", nil
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{}
	}
	return &Backend{
		base: u, user: cfg.Username, pass: cfg.Password,
		client: c, local: cfg.StageLocally, tempDir: cfg.TempDir,
	}, nil
}

// urlFor returns the URL of the resource at rel (a validated key, a staging
// path or "" for the root). Collections get a trailing slash.
func (b *Backend) urlFor(rel string, collection bool) *url.URL {
	u := *b.base
	u.Path = b.base.Path + "/" + rel
	if rel == "" {
		u.Path = b.base.Path + "/"
	} else if collection {
		u.Path += "/"
	}
	return &u
}

func (b *Backend) newRequest(ctx context.Context, method string, u *url.URL, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if b.user != "" || b.pass != "" {
		req.SetBasicAuth(b.user, b.pass)
	}
	return req, nil
}

// do sends a bodiless request and returns the response; the caller closes it.
//
// 423 Locked is retried with a short backoff. Servers built on x/net/webdav
// (rclone among them) take a lock per resource while a request touches it, so
// concurrent MKCOLs of the same collection or a DELETE racing the tail of a
// cancelled PUT briefly see 423 although the operation is fine a moment later.
func (b *Backend) do(ctx context.Context, method string, u *url.URL, hdr map[string]string) (*http.Response, error) {
	delay := 20 * time.Millisecond
	for attempt := 0; ; attempt++ {
		req, err := b.newRequest(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := b.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusLocked || attempt >= 9 {
			return resp, err
		}
		drain(resp)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, time.Second)
	}
}

// drain empties and closes a body so the connection can be reused.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// statusError maps an unexpected response to an error. The caller still owns
// (and closes) the body.
func statusError(op string, resp *http.Response) error {
	var detail string
	if resp.Body != nil {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		detail = strings.Join(strings.Fields(string(snippet)), " ")
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: webdav %s: 404", storage.ErrNotFound, op)
	case http.StatusUnauthorized:
		return fmt.Errorf("webdav %s: authentication failed (401): check username and password", op)
	case http.StatusForbidden:
		return fmt.Errorf("webdav %s: access denied (403): account lacks permission for this path", op)
	case http.StatusLengthRequired:
		return fmt.Errorf("webdav %s: server requires Content-Length (411): enable Config.StageLocally", op)
	case http.StatusInsufficientStorage:
		return fmt.Errorf("webdav %s: server is out of storage (507)", op)
	}
	if detail != "" {
		return fmt.Errorf("webdav %s: unexpected status %s: %s", op, resp.Status, detail)
	}
	return fmt.Errorf("webdav %s: unexpected status %s", op, resp.Status)
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>`

// propfind lists rel (Depth 0 or 1) and streams the entries to fn. A missing
// resource yields an error wrapping storage.ErrNotFound.
func (b *Backend) propfind(ctx context.Context, rel string, collection bool, depth string, fn func(entry) error) error {
	u := b.urlFor(rel, collection)
	req, err := b.newRequest(ctx, "PROPFIND", u, bytes.NewReader([]byte(propfindBody)))
	if err != nil {
		return err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav PROPFIND: %w", err)
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusMultiStatus {
		return statusError("PROPFIND "+rel, resp)
	}
	return parseMultistatus(resp.Body, u, b.base.Path, fn)
}

// Stat implements storage.Backend.
func (b *Backend) Stat(ctx context.Context, key string) (storage.Info, error) {
	if err := storage.ValidateKey(key); err != nil {
		return storage.Info{}, err
	}
	var found *entry
	err := b.propfind(ctx, key, false, "0", func(e entry) error {
		if found == nil && e.Path == key {
			found = &e
		}
		return nil
	})
	if err != nil {
		return storage.Info{}, err
	}
	if found == nil {
		return storage.Info{}, fmt.Errorf("webdav: PROPFIND %s: no matching resource in answer", key)
	}
	if found.IsDir {
		return storage.Info{}, fmt.Errorf("%w: %s is a collection", storage.ErrNotFound, key)
	}
	return storage.Info{Key: key, Size: found.Size, ModTime: found.ModTime}, nil
}

// Delete implements storage.Backend.
func (b *Backend) Delete(ctx context.Context, key string) error {
	if err := storage.ValidateKey(key); err != nil {
		return err
	}
	return b.deletePath(ctx, key)
}

func (b *Backend) deletePath(ctx context.Context, rel string) error {
	resp, err := b.do(ctx, http.MethodDelete, b.urlFor(rel, false), nil)
	if err != nil {
		return fmt.Errorf("webdav DELETE: %w", err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted, http.StatusNotFound:
		return nil
	}
	return statusError("DELETE "+rel, resp)
}

// mkdirs creates every collection leading to the parent of rel.
func (b *Backend) mkdirs(ctx context.Context, rel string) error {
	i := strings.LastIndex(rel, "/")
	if i < 0 {
		return nil
	}
	return b.mkcolAll(ctx, rel[:i])
}

func (b *Backend) mkcolAll(ctx context.Context, dir string) error {
	if _, ok := b.dirs.Load(dir); ok {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(dir, "/") {
		if cur != "" {
			cur += "/"
		}
		cur += seg
		if _, ok := b.dirs.Load(cur); ok {
			continue
		}
		if err := b.mkcol(ctx, cur); err != nil {
			return err
		}
		b.dirs.Store(cur, struct{}{})
	}
	return nil
}

func (b *Backend) mkcol(ctx context.Context, dir string) error {
	resp, err := b.do(ctx, "MKCOL", b.urlFor(dir, true), nil)
	if err != nil {
		return fmt.Errorf("webdav MKCOL: %w", err)
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusMethodNotAllowed, http.StatusMovedPermanently:
		return nil // already exists
	}
	return statusError("MKCOL "+dir, resp)
}

// Walk implements storage.Backend. Collections are traversed breadth-first
// with Depth: 1 listings, so servers that refuse Depth: infinity work.
func (b *Backend) Walk(ctx context.Context, prefix string, fn func(storage.Info) error) error {
	if err := storage.ValidatePrefix(prefix); err != nil {
		return err
	}
	start := ""
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = prefix[:i]
	}
	queue := []string{start}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		var subdirs []string
		err := b.propfind(ctx, dir, true, "1", func(e entry) error {
			if e.Path == dir {
				return nil // the listed collection itself
			}
			if dir == "" && (e.Path == storage.StagingDir || strings.HasPrefix(e.Path, storage.StagingDir+"/")) {
				return nil
			}
			// Only direct children are expected; ignore anything else.
			if want := dir + "/"; dir != "" && !strings.HasPrefix(e.Path, want) {
				return nil
			}
			if e.IsDir {
				if strings.HasPrefix(e.Path+"/", prefix) || strings.HasPrefix(prefix, e.Path+"/") {
					subdirs = append(subdirs, e.Path)
				}
				return nil
			}
			// Foreign files that can never be valid keys are not ours.
			if !strings.HasPrefix(e.Path, prefix) || !validKey(e.Path) {
				return nil
			}
			return fn(storage.Info{Key: e.Path, Size: e.Size, ModTime: e.ModTime})
		})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue // missing start directory, or one that vanished mid-walk
			}
			return err
		}
		queue = append(queue, subdirs...)
	}
	return nil
}

// SweepStaging implements storage.Backend using the staging files'
// getlastmodified. Server and client clocks both matter here, so choose
// olderThan generously (hours, not seconds) in production.
func (b *Backend) SweepStaging(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	var stale []string
	err := b.propfind(ctx, storage.StagingDir, true, "1", func(e entry) error {
		if e.IsDir || e.Path == storage.StagingDir {
			return nil
		}
		if olderThan <= 0 || (!e.ModTime.IsZero() && e.ModTime.Before(cutoff)) {
			stale = append(stale, e.Path)
		}
		return nil
	})
	if errors.Is(err, storage.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range stale {
		if !strings.HasPrefix(p, storage.StagingDir+"/") {
			continue
		}
		if err := b.deletePath(ctx, p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func validKey(k string) bool { return storage.ValidateKey(k) == nil }
