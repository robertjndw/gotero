package httpx

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadlineRecorder is a ResponseWriter that records SetWriteDeadline calls.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.deadlines = append(r.deadlines, t)
	return nil
}

func TestDeadlineWriterExtendsAtMostOncePerSecond(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	dw := NewDeadlineWriter(rec, time.Minute)
	if len(rec.deadlines) != 1 {
		t.Fatalf("want the initial deadline armed, got %d calls", len(rec.deadlines))
	}

	for range 100 {
		_, _ = dw.Write([]byte("x"))
	}
	if len(rec.deadlines) != 1 {
		t.Fatalf("writes within a second must not touch the deadline, got %d calls", len(rec.deadlines))
	}

	dw.last = time.Now().Add(-2 * time.Second)
	_, _ = dw.Write([]byte("x"))
	if len(rec.deadlines) != 2 {
		t.Fatalf("a write after a second must extend the deadline, got %d calls", len(rec.deadlines))
	}
	if got := time.Until(rec.deadlines[1]); got < 50*time.Second || got > time.Minute {
		t.Fatalf("deadline should be about one idle period ahead, got %v", got)
	}
	// The throttle restarts from the extension, not from when the writer
	// was created.
	_, _ = dw.Write([]byte("x"))
	if len(rec.deadlines) != 2 {
		t.Fatalf("a write right after an extension must not extend again, got %d calls", len(rec.deadlines))
	}

	dw.Release()
	if last := rec.deadlines[len(rec.deadlines)-1]; !last.IsZero() {
		t.Fatalf("Release must clear the deadline, got %v", last)
	}
}

// orderRecorder notes how many deadlines were set when each write arrived.
type orderRecorder struct {
	deadlineRecorder
	atWrite []int
}

func (r *orderRecorder) Write(p []byte) (int, error) {
	r.atWrite = append(r.atWrite, len(r.deadlines))
	return r.ResponseRecorder.Write(p)
}

// A slow query between flushes must not leave the next write with an
// expired deadline.
func TestDeadlineWriterExtendsBeforeWriting(t *testing.T) {
	rec := &orderRecorder{deadlineRecorder: deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}}
	dw := NewDeadlineWriter(rec, time.Minute)
	dw.last = time.Now().Add(-2 * time.Minute)
	_, _ = dw.Write([]byte("x"))
	if len(rec.atWrite) != 1 || rec.atWrite[0] != 2 {
		t.Fatalf("the deadline must be extended before the write reaches the connection, got %v", rec.atWrite)
	}
}

func TestDeadlineWriterCutsStalledReader(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := NewDeadlineWriter(w, 200*time.Millisecond)
		defer dw.Release()
		chunk := make([]byte, 1<<20)
		for {
			if _, err := dw.Write(chunk); err != nil {
				done <- err
				return
			}
		}
	}))
	defer srv.Close()

	// Send the request, then never read the response.
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a write error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write to a stalled reader never timed out")
	}
}

// slowPost sends headers and part of a body, then stalls.
func slowPost(t *testing.T, addr, path string) (time.Duration, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST "+path+" HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\nabc")
	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	return time.Since(start), string(b)
}

func TestBodyDeadlines(t *testing.T) {
	h := BodyDeadlines(200*time.Millisecond, 300*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "timeout", http.StatusRequestTimeout)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(Log(discardLogger())(h))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	for _, path := range []string{"/api/users/1/items", "/files/upload/k"} {
		took, resp := slowPost(t, addr, path)
		if took > 2*time.Second {
			t.Errorf("%s: stalled body held the connection for %v", path, took)
		}
		if strings.Contains(resp, " 200 ") {
			t.Errorf("%s: incomplete body accepted: %q", path, resp)
		}
	}
}

func TestIdleBodyKeepsProgressingUploadsAlive(t *testing.T) {
	h := BodyDeadlines(100*time.Millisecond, 400*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != 6 {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	srv := httptest.NewServer(Log(discardLogger())(h))
	defer srv.Close()
	pr, pw := io.Pipe()
	go func() {
		// Total time exceeds the "whole" deadline, but each chunk arrives
		// well within the idle deadline.
		for range 3 {
			time.Sleep(150 * time.Millisecond)
			_, _ = pw.Write([]byte("ab"))
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/files/upload/k", pr)
	req.ContentLength = 6
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("progressing upload was cut off: %d", resp.StatusCode)
	}
}

// A body deadline still set after the body is read makes net/http's
// background read time out and cancel the request context.
func TestBodyDeadlinesDoNotCancelLongRequests(t *testing.T) {
	h := BodyDeadlines(100*time.Millisecond, 100*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(300 * time.Millisecond)
		if r.Context().Err() != nil {
			http.Error(w, "cancelled", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(Log(discardLogger())(h))
	defer srv.Close()

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/files/download/t", ""},
		{http.MethodPost, "/api/users/1/items", "[]"},
		{http.MethodPost, "/files/upload/k", "abc"},
	} {
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: request context was cancelled (%d)", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// net/http starts its background read right away for requests without a
// body. A read deadline on such a request is never cleared (there is no EOF
// to clear it), so it would cancel the context of any handler that runs
// longer than the deadline, such as a large download.
func TestBodyDeadlinesLeaveBodilessRequestsAlone(t *testing.T) {
	h := BodyDeadlines(100*time.Millisecond, 100*time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		if r.Context().Err() != nil {
			http.Error(w, "cancelled", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/users/1/items")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET without a body was cancelled (%d)", resp.StatusCode)
	}
}

// Ordinary bodies get one deadline for the whole body; reads that make
// progress must not move it, and in particular must not collapse it.
func TestWholeBodyDeadlineSurvivesChunkedBodies(t *testing.T) {
	h := BodyDeadlines(2*time.Second, 0)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || string(b) != "abcdef" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	pr, pw := io.Pipe()
	go func() {
		for _, s := range []string{"ab", "cd", "ef"} {
			time.Sleep(50 * time.Millisecond)
			_, _ = pw.Write([]byte(s))
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/users/1/items", pr)
	req.ContentLength = 6
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("body delivered well within the deadline was cut off: %d", resp.StatusCode)
	}
}
