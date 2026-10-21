package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Listen: ":0", Secret: strings.Repeat("s", 32), MaxRequestBytes: 1 << 20,
		Storage: config.Storage{Backend: "local", LocalPath: t.TempDir()},
	}
	s, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), pgtest.New(t))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The streaming websocket goes through every middleware. This guards
// against wrappers that hide http.Hijacker.
func TestStreamThroughMiddleware(t *testing.T) {
	srv := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/stream", nil) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	_, msg, err := ws.Read(ctx)
	if err != nil || !strings.Contains(string(msg), `"connected"`) {
		t.Fatalf("got %q, %v", msg, err)
	}
}

func TestRoutes(t *testing.T) {
	srv := newTestServer(t)
	for path, want := range map[string]int{
		"/healthz":            http.StatusOK,
		"/api/keys/current":   http.StatusForbidden,
		"/admin/api/me":       http.StatusUnauthorized,
		"/admin/api/setup":    http.StatusOK,
		"/":                   http.StatusOK,
		"/groups/12":          http.StatusOK, // SPA route
		"/api/does/not/exist": http.StatusNotFound,
	} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: got %d want %d", path, resp.StatusCode, want)
		}
	}
}
