package conformance

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

type response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *response) String() string {
	b := string(r.Body)
	if len(b) > 400 {
		b = b[:400] + "..."
	}
	return strconv.Itoa(r.Status) + " " + b
}

type request struct {
	header http.Header
	body   io.Reader
	gzip   bool
}

type reqOpt func(*request)

func withKey(k string) reqOpt {
	return func(r *request) {
		if k == "" {
			r.header.Del("Zotero-API-Key")
			return
		}
		r.header.Set("Zotero-API-Key", k)
	}
}

func withHeader(k, v string) reqOpt { return func(r *request) { r.header.Set(k, v) } }
func withGzip() reqOpt              { return func(r *request) { r.gzip = true } }

func withIfUnmodified(v int64) reqOpt {
	return withHeader("If-Unmodified-Since-Version", strconv.FormatInt(v, 10))
}

func withRaw(b []byte, ctype string) reqOpt {
	return func(r *request) {
		r.body = bytes.NewReader(b)
		r.header.Set("Content-Type", ctype)
	}
}

func withReader(rd io.Reader, ctype string) reqOpt {
	return func(r *request) {
		r.body = rd
		r.header.Set("Content-Type", ctype)
	}
}

func withJSON(v any) reqOpt {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return withRaw(b, "application/json")
}

func withForm(v url.Values) reqOpt {
	return withRaw([]byte(v.Encode()), "application/x-www-form-urlencoded")
}

// do sends a request. rel is relative to the API base unless it is absolute.
func (e *env) do(t testing.TB, method, rel string, opts ...reqOpt) *response {
	t.Helper()
	r := &request{header: http.Header{}}
	r.header.Set("Zotero-API-Version", "3")
	for _, o := range opts {
		o(r)
	}
	target := rel
	if !strings.HasPrefix(rel, "http://") && !strings.HasPrefix(rel, "https://") {
		target = e.base + strings.TrimLeft(rel, "/")
	}
	body := r.body
	if r.gzip && body != nil {
		raw, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		body = &buf
		r.header.Set("Content-Encoding", "gzip")
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = r.header
	resp, err := e.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", method, target, err)
	}
	out := &response{Status: resp.StatusCode, Header: resp.Header, Body: b}
	checkUniversal(t, method, target, out)
	return out
}

// checkUniversal enforces rules that hold for every response.
func checkUniversal(t testing.TB, method, target string, r *response) {
	t.Helper()
	for _, h := range []string{"Retry-After", "Backoff"} {
		if v := r.Header.Get(h); v != "" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || (h == "Retry-After" && n < 1) {
				t.Errorf("%s %s: %s must be a whole number of seconds, got %q", method, target, h, v)
			}
		}
	}
}

func mustStatus(t testing.TB, r *response, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status = %s, want %d", r, want)
	}
}

func mustStatusAny(t testing.TB, r *response, want ...int) {
	t.Helper()
	for _, w := range want {
		if r.Status == w {
			return
		}
	}
	t.Fatalf("status = %s, want one of %v", r, want)
}

func mustHeader(t testing.TB, r *response, name string) string {
	t.Helper()
	v := r.Header.Get(name)
	if v == "" {
		t.Fatalf("missing header %s in response %s", name, r)
	}
	return v
}

// versionHeader returns Last-Modified-Version, failing if absent: the client
// throws without it on any library response.
func versionHeader(t testing.TB, r *response) int64 {
	t.Helper()
	v := mustHeader(t, r, "Last-Modified-Version")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		t.Fatalf("Last-Modified-Version = %q, want non-negative integer", v)
	}
	return n
}
