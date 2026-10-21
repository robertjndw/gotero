package zoteroclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is the hosted Zotero API.
	DefaultBaseURL = "https://api.zotero.org/"
	// DefaultMaxRetries is how often a request is retried after the first try.
	DefaultMaxRetries = 5

	apiVersion       = "3"
	defaultUserAgent = "zoteroclient/0 (+https://github.com/robertjndw/gotero)"
)

// Clock abstracts time so tests can run rate limiting paths without sleeping.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Client talks to a Zotero Web API v3 server. It is safe for concurrent use.
// A Backoff header received on any request pauses every later request made
// through the same Client, whichever goroutine sends it.
type Client struct {
	apiKey        string
	base          *url.URL
	hc            *http.Client // follows redirects, used for key-less fetches
	apiHC         *http.Client // never follows redirects, used for API calls
	ua            string
	schemaVersion int
	maxRetries    int
	log           *slog.Logger
	compressMin   int
	clock         Clock
	cfgErr        error

	mu           sync.Mutex
	backoffUntil time.Time
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at another server, e.g. a self-hosted gotero
// at "https://zotero.example.com/api/". A missing trailing slash is added.
func WithBaseURL(raw string) Option {
	return func(c *Client) {
		if !strings.HasSuffix(raw, "/") {
			raw += "/"
		}
		u, err := url.Parse(raw)
		if err == nil && (u.Scheme != "http" && u.Scheme != "https" || u.Host == "") {
			err = fmt.Errorf("need an absolute http(s) URL")
		}
		if err != nil {
			c.cfgErr = fmt.Errorf("zoteroclient: invalid base URL %q: %w", raw, err)
			return
		}
		c.base = u
	}
}

// WithHTTPClient sets the http.Client used for requests. The client is copied
// for API calls so that its CheckRedirect can be replaced: the API key must
// never follow a redirect to another host.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithUserAgent sets the User-Agent header.
func WithUserAgent(ua string) Option { return func(c *Client) { c.ua = ua } }

// WithSchemaVersion sends Zotero-Schema-Version with each API request, which
// asks servers to leave out fields newer than that schema.
func WithSchemaVersion(v int) Option { return func(c *Client) { c.schemaVersion = v } }

// WithMaxRetries sets how many times a request may be retried (default 5).
// Zero turns retrying off; Backoff is still honored.
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = max(n, 0) } }

// WithLogger sets the logger for retries, backoff pauses and debug traces.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithCompression gzips JSON request bodies of at least minBytes bytes.
// Zotero servers accept Content-Encoding: gzip, gotero included, but a
// third-party server might not, so it is opt-in. minBytes <= 0 disables it.
func WithCompression(minBytes int) Option { return func(c *Client) { c.compressMin = minBytes } }

// WithClock replaces the time source, mostly for tests.
func WithClock(clock Clock) Option {
	return func(c *Client) {
		if clock != nil {
			c.clock = clock
		}
	}
}

// New creates a Client that authenticates with apiKey (may be empty for the
// login endpoints). An invalid option value is reported by the first request.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:     apiKey,
		hc:         http.DefaultClient,
		ua:         defaultUserAgent,
		maxRetries: DefaultMaxRetries,
		log:        slog.New(slog.DiscardHandler),
		clock:      realClock{},
	}
	c.base, _ = url.Parse(DefaultBaseURL)
	for _, o := range opts {
		o(c)
	}
	api := *c.hc
	api.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.apiHC = &api
	return c
}

// request is one logical HTTP request, possibly sent several times.
type request struct {
	method     string
	url        string
	header     http.Header
	body       func() (io.Reader, error) // nil means no body
	length     int64
	replayable bool
	keyless    bool // no Zotero headers: the URL is self-authenticating
	idempotent bool
}

func (c *Client) newRequest(method, rel string, q url.Values, hdr http.Header) *request {
	u := c.base.String() + strings.TrimPrefix(rel, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	r := &request{method: method, url: u, header: hdr.Clone()}
	if r.header == nil {
		r.header = http.Header{}
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut:
		r.idempotent = true
	case http.MethodDelete:
		// A repeated delete only matches the first one's intent when a
		// precondition pins the library state.
		r.idempotent = r.header.Get("If-Unmodified-Since-Version") != "" || r.header.Get("If-Match") != ""
	}
	return r
}

func (r *request) setBytes(b []byte, contentType string) {
	r.header.Set("Content-Type", contentType)
	r.length = int64(len(b))
	r.replayable = true
	r.body = func() (io.Reader, error) { return bytes.NewReader(b), nil }
}

// setJSON sets a JSON body, gzipped when it reaches the compression threshold.
func (c *Client) setJSON(r *request, b []byte) error {
	if c.compressMin > 0 && len(b) >= c.compressMin {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(b); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		b = buf.Bytes()
		r.header.Set("Content-Encoding", "gzip")
	}
	r.setBytes(b, "application/json")
	return nil
}

// bodyFor makes a body replayable where the reader type allows it.
func (r *request) bodyFor(body io.Reader) {
	switch b := body.(type) {
	case nil:
	case *bytes.Buffer:
		r.setBytesKeepType(bytes.Clone(b.Bytes()))
	case io.ReadSeeker:
		pos, err := b.Seek(0, io.SeekCurrent)
		end, err2 := b.Seek(0, io.SeekEnd)
		_, err3 := b.Seek(pos, io.SeekStart)
		if err != nil || err2 != nil || err3 != nil {
			r.once(b)
			return
		}
		r.length = end - pos
		r.replayable = true
		r.body = func() (io.Reader, error) {
			_, err := b.Seek(pos, io.SeekStart)
			return b, err
		}
	default:
		r.once(b)
	}
}

func (r *request) setBytesKeepType(b []byte) {
	r.length = int64(len(b))
	r.replayable = true
	r.body = func() (io.Reader, error) { return bytes.NewReader(b), nil }
}

// once marks a body as readable a single time. Such requests are never retried.
func (r *request) once(b io.Reader) {
	r.length = -1
	r.body = func() (io.Reader, error) { return b, nil }
}

func (c *Client) build(ctx context.Context, r *request) (*http.Request, error) {
	hr, err := http.NewRequestWithContext(ctx, r.method, r.url, nil)
	if err != nil {
		return nil, err
	}
	hr.Header = r.header.Clone()
	hr.Header.Set("User-Agent", c.ua)
	if !r.keyless {
		hr.Header.Set("Zotero-API-Version", apiVersion)
		if c.apiKey != "" {
			hr.Header.Set("Zotero-API-Key", c.apiKey)
		}
		if c.schemaVersion > 0 {
			hr.Header.Set("Zotero-Schema-Version", fmt.Sprint(c.schemaVersion))
		}
	}
	if r.body != nil {
		b, err := r.body()
		if err != nil {
			return nil, err
		}
		if r.length == 0 {
			hr.Body = http.NoBody
		} else {
			hr.Body = io.NopCloser(b)
			if r.length > 0 {
				hr.ContentLength = r.length
			}
		}
	}
	return hr, nil
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.clock.After(d):
		return nil
	}
}

// waitBackoff blocks until any server requested Backoff has expired. The
// deadline can move while we wait, so it is re-read after every sleep.
func (c *Client) waitBackoff(ctx context.Context) error {
	for {
		c.mu.Lock()
		until := c.backoffUntil
		c.mu.Unlock()
		d := until.Sub(c.clock.Now())
		if d <= 0 {
			return ctx.Err()
		}
		c.log.Warn("zotero server asked for backoff, pausing", "wait", d)
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
	}
}

func (c *Client) noteBackoff(resp *http.Response) {
	v := resp.Header.Get("Backoff")
	if v == "" {
		return
	}
	d, ok := parseRetryAfter(v, c.clock.Now())
	if !ok || d <= 0 {
		return
	}
	until := c.clock.Now().Add(d)
	c.mu.Lock()
	if until.After(c.backoffUntil) {
		c.backoffUntil = until
	}
	c.mu.Unlock()
}

// retryDecision says whether to retry after resp and how long to wait first.
func (c *Client) retryDecision(r *request, resp *http.Response, attempt int) (time.Duration, bool) {
	if attempt >= c.maxRetries || !r.replayable && r.body != nil {
		return 0, false
	}
	switch s := resp.StatusCode; {
	case s == http.StatusTooManyRequests || s == http.StatusServiceUnavailable:
		// The server refused the request before acting on it, so even a
		// POST is safe to repeat.
		d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), c.clock.Now())
		if !ok {
			d = fallbackDelay(attempt)
		}
		return d, d <= maxRetryWait
	case s >= 500 && s != http.StatusNotImplemented && s != http.StatusHTTPVersionNotSupported && r.idempotent:
		return fallbackDelay(attempt), true
	}
	return 0, false
}

// roundTrip sends r with backoff and retry handling and returns the final
// response whatever its status. The caller closes the body.
func (c *Client) roundTrip(ctx context.Context, r *request) (*http.Response, error) {
	if c.cfgErr != nil {
		return nil, c.cfgErr
	}
	hc := c.apiHC
	if r.keyless {
		hc = c.hc
	}
	for attempt := 0; ; attempt++ {
		if err := c.waitBackoff(ctx); err != nil {
			return nil, err
		}
		hr, err := c.build(ctx, r)
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(hr)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			canRetry := r.idempotent && (r.body == nil || r.replayable) && attempt < c.maxRetries
			if !canRetry {
				return nil, err
			}
			d := fallbackDelay(attempt)
			c.log.Warn("zotero request failed, retrying", "method", r.method, "url", r.url, "err", err, "wait", d)
			if err := c.sleep(ctx, d); err != nil {
				return nil, err
			}
			continue
		}
		c.log.Debug("zotero response", "method", r.method, "url", r.url, "status", resp.StatusCode)
		c.noteBackoff(resp)
		d, retry := c.retryDecision(r, resp, attempt)
		if !retry {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		c.log.Warn("zotero server asked to retry", "method", r.method, "url", r.url,
			"status", resp.StatusCode, "attempt", attempt+1, "wait", d)
		if err := c.sleep(ctx, d); err != nil {
			return nil, err
		}
	}
}

// call is roundTrip plus status handling: 2xx returns the open response, 304
// returns ErrNotModified (with the response, body closed, so the version can
// still be read) and everything else becomes an *APIError.
func (c *Client) call(ctx context.Context, r *request) (*http.Response, error) {
	resp, err := c.roundTrip(ctx, r)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		_ = resp.Body.Close()
		return resp, ErrNotModified
	case resp.StatusCode >= 300:
		return nil, newAPIError(resp, r.method, r.url)
	}
	return resp, nil
}

// exec runs call and drains the body unless decode is wanted. The returned
// version is filled in on success, ErrNotModified and API errors alike.
func (c *Client) exec(ctx context.Context, r *request, out any) (int64, error) {
	resp, err := c.call(ctx, r)
	if err != nil {
		return versionOf(resp, err), err
	}
	defer func() { _ = resp.Body.Close() }()
	ver := versionHeader(resp.Header)
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return ver, nil
	}
	if err := decodeJSON(resp.Body, out); err != nil {
		return ver, fmt.Errorf("zoteroclient: decode %s %s: %w", r.method, r.url, err)
	}
	return ver, nil
}

func (c *Client) get(ctx context.Context, rel string, q url.Values, hdr http.Header, out any) (int64, error) {
	return c.exec(ctx, c.newRequest(http.MethodGet, rel, q, hdr), out)
}

func (c *Client) sendJSON(ctx context.Context, method, rel string, q url.Values, hdr http.Header, body []byte, out any) (int64, error) {
	r := c.newRequest(method, rel, q, hdr)
	if body != nil {
		if err := c.setJSON(r, body); err != nil {
			return 0, err
		}
	}
	return c.exec(ctx, r, out)
}

// Do sends an arbitrary request to the server with the client's headers,
// backoff and retry handling and returns the response whatever its status
// (no APIError conversion). path is relative to the base URL and may include
// a query. The caller must close the response body.
//
// A body can be retried only if it is a *bytes.Buffer, *bytes.Reader,
// *strings.Reader or other io.ReadSeeker; any other reader is sent once.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, hdr http.Header) (*http.Response, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		rel, ok := strings.CutPrefix(path, c.base.String())
		if !ok {
			return nil, errors.New("zoteroclient: Do: absolute URL is outside the base URL")
		}
		path = rel
	}
	r := c.newRequest(method, path, nil, hdr)
	r.bodyFor(body)
	return c.roundTrip(ctx, r)
}
