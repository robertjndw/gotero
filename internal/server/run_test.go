package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/config"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/zotero"
	zs "github.com/robertjndw/gotero/zotero/stream"
)

// runWait is the upper bound for anything that should happen "promptly".
// Tests poll against it, so it only matters when something is broken.
const runWait = 10 * time.Second

func runConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Listen: "127.0.0.1:0", LocalLogin: true, Secret: strings.Repeat("s", 32), MaxRequestBytes: 1 << 20,
		Storage:         config.Storage{Backend: "local", LocalPath: t.TempDir()},
		ShutdownTimeout: runWait,
	}
}

func runNewServer(t *testing.T, cfg *config.Config, pool *pgxpool.Pool) *Server {
	t.Helper()
	s, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), pool)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(runWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runSlowRoute puts a handler in front of the application that blocks until
// release is closed, so a test can hold a request in flight across shutdown.
// hold runs while the request is blocked; it gets the request context.
type runSlowRoute struct {
	started chan struct{}
	release chan struct{}
	hold    func(ctx context.Context) error
	// ctxDone is closed if the request context is cancelled while holding.
	ctxDone chan struct{}
}

const runSlowPath = "/run-test/slow"

func runInstallSlowRoute(s *Server) *runSlowRoute {
	sr := &runSlowRoute{started: make(chan struct{}), release: make(chan struct{}), ctxDone: make(chan struct{})}
	app := s.http.Handler
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != runSlowPath {
			app.ServeHTTP(w, r)
			return
		}
		// Headers and part of the body go out first, like a download that is
		// halfway through when the signal arrives.
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "first half,")
		w.(http.Flusher).Flush()
		close(sr.started)
		if sr.hold != nil {
			if err := sr.hold(r.Context()); err != nil && r.Context().Err() != nil {
				close(sr.ctxDone)
				return
			}
		}
		select {
		case <-sr.release:
		case <-r.Context().Done():
			close(sr.ctxDone)
			return
		}
		_, _ = io.WriteString(w, "second half")
	})
	return sr
}

type runResult struct {
	body string
	err  error
}

// runStart runs s in the background and returns its address, a cancel
// function standing in for SIGTERM, and a channel with Run's result.
func runStart(t *testing.T, s *Server) (addr string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	addrs := make(chan net.Addr, 1)
	s.onListen = func(a net.Addr) { addrs <- a }
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		errc <- s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(runWait):
			t.Error("Run did not return during cleanup")
		}
	})
	select {
	case a := <-addrs:
		return a.String(), cancel, errc
	case err := <-errc:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(runWait):
		t.Fatal("server did not start listening")
	}
	return "", nil, nil
}

func runGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// A fresh transport per request, so no pooled connection outlives the
	// listener and makes "new connections are refused" pass by accident.
	return (&http.Transport{DisableKeepAlives: true}).RoundTrip(req)
}

// runStartSlow issues a request to the slow route and waits until the
// handler is running.
func runStartSlow(t *testing.T, addr string, sr *runSlowRoute) <-chan runResult {
	t.Helper()
	out := make(chan runResult, 1)
	go func() {
		resp, err := runGet(context.Background(), "http://"+addr+runSlowPath)
		if err != nil {
			out <- runResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		if err == nil && resp.StatusCode != http.StatusOK {
			err = errors.New(resp.Status)
		}
		out <- runResult{body: string(b), err: err}
	}()
	select {
	case <-sr.started:
	case r := <-out:
		t.Fatalf("slow request ended early: %+v", r)
	case <-time.After(runWait):
		t.Fatal("slow request never reached the handler")
	}
	return out
}

func runDialRefused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return true
	}
	_ = c.Close()
	return false
}

// runBackgroundLoops lists which of Run's background goroutines are alive,
// judged by function names on all goroutine stacks.
func runBackgroundLoops() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	stacks := string(buf)
	var alive []string
	for _, fn := range []string{"jobs.(*Runner).Run", "importer.(*Worker).Run", "server.(*Server).revalidateStreams"} {
		if strings.Contains(stacks, fn) {
			alive = append(alive, fn)
		}
	}
	return alive
}

func TestRunServesAndShutsDownGracefully(t *testing.T) {
	s := runNewServer(t, runConfig(t), pgtest.New(t))
	sr := runInstallSlowRoute(s)
	addr, stop, done := runStart(t, s)

	resp, err := runGet(context.Background(), "http://"+addr+"/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var health struct{ Status string }
	err = json.NewDecoder(resp.Body).Decode(&health)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || health.Status != "ok" {
		t.Fatalf("healthz: %d %+v %v", resp.StatusCode, health, err)
	}
	if alive := runBackgroundLoops(); len(alive) != 3 {
		t.Fatalf("background loops running: %v", alive)
	}

	slow := runStartSlow(t, addr, sr)
	stop()

	runEventually(t, "listener to close", func() bool { return runDialRefused(addr) })
	select {
	case err := <-done:
		t.Fatalf("Run returned with a request in flight: %v", err)
	case r := <-slow:
		t.Fatalf("in-flight request ended before it was released: %+v", r)
	default:
	}

	close(sr.release)
	select {
	case r := <-slow:
		if r.err != nil || r.body != "first half,second half" {
			t.Fatalf("in-flight request: %q, %v", r.body, r.err)
		}
	case <-time.After(runWait):
		t.Fatal("in-flight request did not complete")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(runWait):
		t.Fatal("Run did not return after the last request finished")
	}
	if alive := runBackgroundLoops(); len(alive) != 0 {
		t.Fatalf("background loops still running after Run returned: %v", alive)
	}
	if resp, err := runGet(context.Background(), "http://"+addr+"/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server still answering after Run returned")
	}
}

func TestRunFailsWhenPortTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	cfg := runConfig(t)
	cfg.Listen = taken.Addr().String()
	s := runNewServer(t, cfg, pgtest.New(t))

	errc := make(chan error, 1)
	go func() { errc <- s.Run(context.Background()) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Run succeeded on a port that is in use")
		}
	case <-time.After(runWait):
		t.Fatal("Run did not fail on a port that is in use")
	}
	if alive := runBackgroundLoops(); len(alive) != 0 {
		t.Fatalf("background loops left running after a failed start: %v", alive)
	}
}

// A request that ignores shutdown must not hold the process past
// ShutdownTimeout. The request here is blocked in a database query, which is
// the case that matters: main closes the pool right after Run returns, and
// pgxpool.Close waits for every acquired connection to come back.
func TestRunShutdownTimeoutBoundsStuckRequests(t *testing.T) {
	pool := pgtest.New(t)
	cfg := runConfig(t)
	cfg.ShutdownTimeout = 300 * time.Millisecond
	s := runNewServer(t, cfg, pool)
	sr := runInstallSlowRoute(s)
	sr.hold = func(ctx context.Context) error {
		_, err := pool.Exec(ctx, `SELECT pg_sleep(30)`)
		return err
	}
	defer close(sr.release)
	addr, stop, done := runStart(t, s)
	slow := runStartSlow(t, addr, sr)

	start := time.Now()
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run: got %v, want the shutdown deadline", err)
		}
	case <-time.After(runWait):
		t.Fatal("Run did not return after ShutdownTimeout")
	}
	if took := time.Since(start); took < cfg.ShutdownTimeout || took > cfg.ShutdownTimeout+2*time.Second {
		t.Fatalf("Run returned after %v with a %v shutdown timeout", took, cfg.ShutdownTimeout)
	}

	// The stuck request is cut off and its context cancelled, which aborts
	// the query and hands the connection back.
	select {
	case <-sr.ctxDone:
	case <-time.After(runWait):
		t.Fatal("stuck request's context was never cancelled")
	}
	select {
	case r := <-slow:
		if r.err == nil {
			t.Fatalf("stuck request completed: %q", r.body)
		}
	case <-time.After(runWait):
		t.Fatal("client of the stuck request is still waiting")
	}
	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(runWait):
		t.Fatal("closing the pool hangs on a connection held by the stuck request")
	}
}

// Access removed behind the server's back (another process editing the
// database) never reaches AccessChanged; the periodic revalidation is what
// stops the stream from announcing that library.
func TestRevalidateStreamsDropsRevokedTopics(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.New(t)
	s := runNewServer(t, runConfig(t), pool)
	s.revalidateEvery = 20 * time.Millisecond

	store := pg.New(pool)
	alice, err := store.Users().Create(ctx, repo.User{Username: "alice", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := store.Users().Create(ctx, repo.User{Username: "bob", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.Groups().Create(ctx, repo.Group{OwnerID: bob.ID, Name: "Shared"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Groups().SetMember(ctx, g.ID, alice.ID, "member"); err != nil {
		t.Fatal(err)
	}
	key := zotero.NewAPIKey()
	if _, err := store.Keys().Create(ctx, repo.APIKey{
		UserID: alice.ID, Hash: auth.HashToken(key), Prefix: key[:4], Name: "t",
		Access: repo.KeyAccess{Library: true, Groups: "all"},
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	loopCtx, stopLoop := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		s.revalidateStreams(loopCtx)
	}()
	t.Cleanup(func() {
		stopLoop()
		<-loopDone
	})

	wsCtx, cancelWS := context.WithTimeout(ctx, runWait)
	defer cancelWS()
	ws, _, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/stream", //nolint:bodyclose
		&websocket.DialOptions{HTTPHeader: http.Header{"Zotero-API-Key": {key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	next := func() map[string]any {
		t.Helper()
		_, msg, err := ws.Read(wsCtx)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(msg, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := next(); m["event"] != "connected" {
		t.Fatalf("got %v", m)
	}

	userTopic, groupTopic := zs.UserTopic(alice.ID), zs.GroupTopic(g.ID)
	s.hub.Publish(groupTopic, 1)
	if m := next(); m["event"] != "topicUpdated" || m["topic"] != groupTopic {
		t.Fatalf("before revocation: got %v", m)
	}

	// Revalidation runs many times while access is unchanged; none of
	// those passes may announce anything. The removal below must be the
	// next event.
	if _, err := pool.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, g.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	if m := next(); m["event"] != "topicRemoved" || m["topic"] != groupTopic {
		t.Fatalf("after revocation: got %v", m)
	}

	// Updates for the group no longer reach the stream; the user's own
	// library still does. Events are delivered in order, so a leaked group
	// update would arrive first.
	s.hub.Publish(groupTopic, 2)
	s.hub.Publish(userTopic, 2)
	if m := next(); m["event"] != "topicUpdated" || m["topic"] != userTopic {
		t.Fatalf("after revocation: got %v", m)
	}

	stopLoop()
	select {
	case <-loopDone:
	case <-time.After(runWait):
		t.Fatal("revalidateStreams did not return after cancel")
	}
}

func TestNewUsesProductionRevalidateInterval(t *testing.T) {
	s := runNewServer(t, runConfig(t), pgtest.New(t))
	if s.revalidateEvery != time.Minute {
		t.Fatalf("revalidateEvery = %v", s.revalidateEvery)
	}
}
