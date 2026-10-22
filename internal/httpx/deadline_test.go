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

	dw.Release()
	if last := rec.deadlines[len(rec.deadlines)-1]; !last.IsZero() {
		t.Fatalf("Release must clear the deadline, got %v", last)
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

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
