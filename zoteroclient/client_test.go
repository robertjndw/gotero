package zoteroclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

func TestHeadersSent(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		for k, want := range map[string]string{
			"Zotero-API-Version":    "3",
			"Zotero-API-Key":        "KEY",
			"User-Agent":            "test-agent/1",
			"Zotero-Schema-Version": "33",
		} {
			if got := r.Header.Get(k); got != want {
				t.Errorf("%s = %q, want %q", k, got, want)
			}
		}
		if r.URL.Path != "/keys/current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, 200, `{"key":"KEY","userID":7,"username":"u","displayName":"U","access":{"user":{"library":true},"groups":{}}}`)
	}, zoteroclient.WithUserAgent("test-agent/1"), zoteroclient.WithSchemaVersion(33))

	ki, err := c.CurrentKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ki.UserID != 7 || !ki.Access.User.Library {
		t.Errorf("unexpected key info %+v", ki)
	}
}

func TestBackoffPausesLaterRequests(t *testing.T) {
	var n atomic.Int32
	c, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Backoff", "7")
		}
		w.Header().Set("Last-Modified-Version", "1")
		writeJSON(w, 200, `{}`)
	})
	lib := c.User(1)
	for range 3 {
		if _, _, err := lib.Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{}); err != nil {
			t.Fatal(err)
		}
	}
	got := clk.Sleeps()
	if len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("sleeps = %v, want exactly one 7s pause before the second request", got)
	}
}

// The pause is shared by every goroutine using the client.
func TestBackoffSharedAcrossGoroutines(t *testing.T) {
	var n atomic.Int32
	c, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Backoff", "5")
		}
		writeJSON(w, 200, `{}`)
	})
	if _, _, err := c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{})
		}()
	}
	wg.Wait()
	if len(clk.Sleeps()) == 0 {
		t.Error("no goroutine waited for the backoff")
	}
	for _, d := range clk.Sleeps() {
		if d > 5*time.Second {
			t.Errorf("slept %v, more than the backoff", d)
		}
	}
}

func TestBackoffRespectsContext(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Backoff", "3600")
		writeJSON(w, 200, `{}`)
	}))
	defer srv.Close()
	c := zoteroclient.New("KEY", zoteroclient.WithBaseURL(srv.URL))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := c.User(1).Versions(ctx, zotero.TypeItem, zoteroclient.VersionsQuery{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, _, err := c.User(1).Versions(ctx, zotero.TypeItem, zoteroclient.VersionsQuery{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 2*time.Second || hits.Load() != 1 {
		t.Errorf("waited %v with %d hits", time.Since(start), hits.Load())
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	var n atomic.Int32
	c, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			writeJSON(w, 429, `slow down`)
			return
		}
		writeJSON(w, 200, `{"A":1}`)
	})
	m, _, err := c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if m["A"] != 1 || n.Load() != 2 {
		t.Errorf("m=%v n=%d", m, n.Load())
	}
	if s := clk.Sleeps(); len(s) != 1 || s[0] != 3*time.Second {
		t.Errorf("sleeps = %v", s)
	}
}

func TestRetryAfterHTTPDate(t *testing.T) {
	var n atomic.Int32
	var clk *fakeClock
	c, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", clk.Now().Add(12*time.Second).UTC().Format(http.TimeFormat))
			writeJSON(w, 429, ``)
			return
		}
		writeJSON(w, 200, `{}`)
	})
	if _, _, err := c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{}); err != nil {
		t.Fatal(err)
	}
	if s := clk.Sleeps(); len(s) != 1 || s[0] != 12*time.Second {
		t.Errorf("sleeps = %v", s)
	}
}

func TestRetry503WithoutHeaderUsesJitter(t *testing.T) {
	var n atomic.Int32
	c, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			writeJSON(w, 503, ``)
			return
		}
		writeJSON(w, 200, `{}`)
	})
	if _, _, err := c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{}); err != nil {
		t.Fatal(err)
	}
	s := clk.Sleeps()
	if len(s) != 2 {
		t.Fatalf("sleeps = %v", s)
	}
	if s[0] < 500*time.Millisecond || s[0] > time.Second || s[1] < time.Second || s[1] > 2*time.Second {
		t.Errorf("jittered delays out of range: %v", s)
	}
}

func TestRetriesExhausted(t *testing.T) {
	var n atomic.Int32
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "1")
		writeJSON(w, 429, `nope`)
	}, zoteroclient.WithMaxRetries(2))
	_, _, err := c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{})
	var ae *zoteroclient.APIError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusTooManyRequests || ae.Message != "nope" {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 3 {
		t.Errorf("requests = %d, want 3", n.Load())
	}
}

func TestServerErrorRetryRules(t *testing.T) {
	var gets, posts, puts atomic.Int32
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Method {
		case http.MethodGet:
			if gets.Add(1) == 1 {
				writeJSON(w, 500, `boom`)
				return
			}
			writeJSON(w, 200, `{}`)
		case http.MethodPost:
			posts.Add(1)
			writeJSON(w, 500, `boom`)
		case http.MethodPut:
			if puts.Add(1) == 1 {
				writeJSON(w, 502, `boom`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()
	lib := c.User(1)
	if _, _, err := lib.Versions(ctx, zotero.TypeItem, zoteroclient.VersionsQuery{}); err != nil || gets.Load() != 2 {
		t.Errorf("GET: err=%v gets=%d", err, gets.Load())
	}
	if _, err := lib.SetFulltext(ctx, "ABCD2345", zotero.FulltextContent{Content: "x"}); err != nil || puts.Load() != 2 {
		t.Errorf("PUT: err=%v puts=%d", err, puts.Load())
	}
	objs := []zotero.OrderedMap{{}}
	if _, _, err := lib.Write(ctx, zotero.TypeItem, objs, zoteroclient.WriteOptions{}); err == nil || posts.Load() != 1 {
		t.Errorf("POST: err=%v posts=%d, want one failed attempt", err, posts.Load())
	}
}

func TestNotModified(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-Modified-Since-Version"); got != "5" {
			t.Errorf("If-Modified-Since-Version = %q", got)
		}
		if got := r.URL.Query().Get("since"); got != "5" {
			t.Errorf("since = %q", got)
		}
		w.Header().Set("Last-Modified-Version", "5")
		w.WriteHeader(http.StatusNotModified)
	})
	m, ver, err := c.User(1).Versions(context.Background(), zotero.TypeItem, zoteroclient.VersionsQuery{Since: 5})
	if !errors.Is(err, zoteroclient.ErrNotModified) || !zoteroclient.IsNotModified(err) {
		t.Fatalf("err = %v", err)
	}
	if m != nil || ver != 5 {
		t.Errorf("m=%v ver=%d", m, ver)
	}
}

func TestPreconditionFailedIsTyped(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified-Version", "42")
		writeJSON(w, 412, "Library has been modified since specified version")
	})
	_, ver, err := c.User(1).Write(context.Background(), zotero.TypeItem, []zotero.OrderedMap{{}}, zoteroclient.WriteOptions{})
	if !zoteroclient.IsPreconditionFailed(err) {
		t.Fatalf("err = %v", err)
	}
	var ae *zoteroclient.APIError
	if !errors.As(err, &ae) || !strings.Contains(ae.Message, "modified since") || ae.Header.Get("Last-Modified-Version") != "42" || ae.LibraryVersion() != 42 {
		t.Errorf("api error = %+v", ae)
	}
	if ver != 42 {
		t.Errorf("ver = %d", ver)
	}
	if zoteroclient.IsNotFound(err) || zoteroclient.IsForbidden(err) || zoteroclient.IsNotModified(err) {
		t.Error("wrong classification")
	}
}

func TestErrorClassification(t *testing.T) {
	for status, check := range map[int]func(error) bool{
		403: zoteroclient.IsForbidden,
		404: zoteroclient.IsNotFound,
		409: zoteroclient.IsConflict,
	} {
		c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, status, "x") })
		_, err := c.CurrentKey(context.Background())
		if !check(err) {
			t.Errorf("status %d: not classified: %v", status, err)
		}
	}
}

func TestAPIKeyNotForwardedOnRedirect(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(true)
	}))
	defer other.Close()
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	})
	resp, err := c.Do(context.Background(), http.MethodGet, "users/1/items", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound || leaked.Load() {
		t.Errorf("status=%d leaked=%v", resp.StatusCode, leaked.Load())
	}
}

func TestDoRetriesReplayableBody(t *testing.T) {
	var n atomic.Int32
	var bodies []string
	var mu sync.Mutex
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, ``)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	h := http.Header{"X-Custom": {"1"}}
	resp, err := c.Do(context.Background(), http.MethodPost, "/users/1/whatever", strings.NewReader("hello"), h)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(bodies) != 2 || bodies[0] != "hello" || bodies[1] != "hello" {
		t.Errorf("status=%d bodies=%q", resp.StatusCode, bodies)
	}
}

func TestDoNonReplayableBodyIsNotRetried(t *testing.T) {
	var n atomic.Int32
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		writeJSON(w, 429, ``)
	})
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("x")); _ = pw.Close() }()
	resp, err := c.Do(context.Background(), http.MethodPost, "x", pr, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || n.Load() != 1 {
		t.Errorf("status=%d n=%d", resp.StatusCode, n.Load())
	}
}

func TestInvalidBaseURL(t *testing.T) {
	c := zoteroclient.New("k", zoteroclient.WithBaseURL("not a url"))
	if _, err := c.CurrentKey(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid base URL") {
		t.Errorf("err = %v", err)
	}
}

func TestCompression(t *testing.T) {
	var sawGzip, sawPlain atomic.Bool
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if r.Header.Get("Content-Encoding") == "gzip" {
			sawGzip.Store(true)
		} else {
			sawPlain.Store(true)
		}
		if !strings.HasPrefix(body, "[") || !strings.HasSuffix(body, "]") {
			t.Errorf("body after decoding = %.60q", body)
		}
		writeJSON(w, 200, `{"successful":{},"success":{},"unchanged":[],"failed":[]}`)
	}, zoteroclient.WithCompression(100))

	small := zotero.OrderedMap{}
	_ = small.SetValue("a", 1)
	big := zotero.OrderedMap{}
	_ = big.SetValue("note", strings.Repeat("lorem ipsum ", 100))
	for _, o := range []zotero.OrderedMap{small, big} {
		if _, _, err := c.User(1).Write(context.Background(), zotero.TypeItem, []zotero.OrderedMap{o}, zoteroclient.WriteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if !sawGzip.Load() || !sawPlain.Load() {
		t.Errorf("gzip=%v plain=%v, want both", sawGzip.Load(), sawPlain.Load())
	}
}
