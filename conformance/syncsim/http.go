package syncsim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errNetwork is a failed round trip, real or injected. The sync that hit it
// stops and is retried later, as in Zotero.
var errNetwork = errors.New("network error")

// faultTransport drops requests before they reach the server, or drops the
// response after the server has acted on it. The second kind is the
// interesting one: the client has to recover from a write it does not know
// went through.
type faultTransport struct {
	base http.RoundTripper

	mu   sync.Mutex
	rng  *rand.Rand
	rate float64
}

func (f *faultTransport) setRate(r float64) {
	f.mu.Lock()
	f.rate = r
	f.mu.Unlock()
}

// roll returns 0 for no fault, 1 to drop the request, 2 to drop the response.
func (f *faultTransport) roll() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rate == 0 || f.rng.Float64() >= f.rate {
		return 0
	}
	return 1 + f.rng.IntN(2)
}

func (f *faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	switch f.roll() {
	case 1:
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: injected: request dropped", errNetwork)
	case 2:
		resp, err := f.base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: injected: response to %s %s dropped (status %d)",
			errNetwork, req.Method, req.URL.Path, resp.StatusCode)
	}
	return f.base.RoundTrip(req)
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// lmv is the Last-Modified-Version header, -1 if absent.
func (r *response) lmv() int64 {
	v, err := strconv.ParseInt(r.header.Get("Last-Modified-Version"), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

func (r *response) String() string {
	b := string(r.body)
	if len(b) > 300 {
		b = b[:300] + "..."
	}
	return fmt.Sprintf("%d %s", r.status, b)
}

// protocolError is a response no correct server sends a correct client. It
// fails the run.
type protocolError struct{ msg string }

func (e *protocolError) Error() string { return e.msg }

func protoErr(format string, args ...any) error {
	return &protocolError{msg: fmt.Sprintf(format, args...)}
}

type reqOpts struct {
	query  url.Values
	header map[string]string
	body   []byte
	ctype  string
	noKey  bool // download targets authenticate with their own token
	abs    bool // path is an absolute URL
}

// trace is a ring of recent requests, dumped when a run fails.
type trace struct {
	mu    sync.Mutex
	lines []string
	next  int
}

const traceLen = 400

func (t *trace) add(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	line := time.Now().Format("15:04:05.000 ") + fmt.Sprintf(format, args...)
	if len(t.lines) < traceLen {
		t.lines = append(t.lines, line)
		return
	}
	t.lines[t.next] = line
	t.next = (t.next + 1) % traceLen
}

func (t *trace) dump() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append(append([]string(nil), t.lines[t.next:]...), t.lines[:t.next]...)
}

// do sends one API request. 429 and 503 are retried after Retry-After
// (scaled down so tests stay fast), like the Zotero client does.
func (c *client) do(ctx context.Context, method, path string, o reqOpts) (*response, error) {
	u := path
	if !o.abs {
		u = c.sim.base + path
	}
	if len(o.query) > 0 {
		u += "?" + o.query.Encode()
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if o.body != nil {
			body = bytes.NewReader(o.body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Zotero-API-Version", "3")
		if !o.noKey {
			req.Header.Set("Zotero-API-Key", c.user.APIKey)
		}
		if o.ctype != "" {
			req.Header.Set("Content-Type", o.ctype)
		}
		for k, v := range o.header {
			req.Header.Set(k, v)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			c.trace.add("%s %s -> %v", method, shortURL(u), err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errNetwork) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %w", errNetwork, err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: reading body: %w", errNetwork, err)
		}
		r := &response{status: resp.StatusCode, header: resp.Header, body: b}
		c.trace.add("%s %s -> %d lmv=%s %s", method, shortURL(u), r.status,
			resp.Header.Get("Last-Modified-Version"), clip(b, 160))
		if (r.status == http.StatusServiceUnavailable || r.status == http.StatusTooManyRequests) && attempt < 20 {
			c.sim.stats.retries.Add(1)
			secs, err := strconv.Atoi(r.header.Get("Retry-After"))
			if err != nil || secs < 1 {
				return nil, protoErr("%s %s: %d without an integer Retry-After (%q)", method, path, r.status, r.header.Get("Retry-After"))
			}
			d := time.Duration(float64(time.Duration(secs)*time.Second) * c.sim.cfg.RetryAfterScale)
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		if r.status >= 500 {
			return nil, protoErr("%s %s: server error %s", method, path, r)
		}
		return r, nil
	}
}

func shortURL(u string) string {
	if i := strings.Index(u, "/api/"); i >= 0 {
		return u[i+4:]
	}
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "/"); j >= 0 {
			return u[i+3+j:]
		}
	}
	return u
}

func clip(b []byte, n int) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// libResponse checks the Last-Modified-Version every library response must
// carry and that it never goes backwards for this client. Reads are not
// locked, but each one starts after the previous one finished, so a lower
// version would mean the server lost a committed write.
func (c *client) libResponse(l *library, method, path string, r *response) (int64, error) {
	v := r.lmv()
	if v < 0 {
		return 0, protoErr("%s %s: %d without Last-Modified-Version", method, path, r.status)
	}
	c.lmvMu.Lock()
	defer c.lmvMu.Unlock()
	if seen := c.maxLMV[l.path]; v < seen {
		return 0, protoErr("%s %s: Last-Modified-Version went backwards from %d to %d", method, path, seen, v)
	}
	c.maxLMV[l.path] = v
	return v, nil
}
