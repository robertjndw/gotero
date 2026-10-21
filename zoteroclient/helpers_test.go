package zoteroclient_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/robertjndw/gotero/zoteroclient"
)

// fakeClock never sleeps: After advances the clock by the requested amount
// and fires at once, recording what was asked for.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.sleeps = append(f.sleeps, d)
	ch := make(chan time.Time, 1)
	ch <- f.now
	return ch
}

func (f *fakeClock) Sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.sleeps...)
}

// setup starts a server for h and returns a client with key "KEY" that talks
// to it using a fake clock.
func setup(t *testing.T, h http.HandlerFunc, opts ...zoteroclient.Option) (*zoteroclient.Client, *fakeClock, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	clk := newFakeClock()
	all := append([]zoteroclient.Option{
		zoteroclient.WithBaseURL(srv.URL + "/"),
		zoteroclient.WithClock(clk),
	}, opts...)
	return zoteroclient.New("KEY", all...), clk, srv
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// readBody returns the request body, gunzipped if the client compressed it.
func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("bad gzip body: %v", err)
			return ""
		}
		rd = zr
	}
	b, err := io.ReadAll(rd)
	if err != nil {
		t.Errorf("read body: %v", err)
	}
	return string(b)
}
