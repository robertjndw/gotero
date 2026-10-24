package stream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	zs "github.com/robertjndw/gotero/zotero/stream"
)

type fakeResolver struct {
	mu   sync.Mutex
	keys map[string][]string // key -> topics
	down bool                // every lookup fails, like an unreachable database
}

func (f *fakeResolver) KeyTopics(_ context.Context, key string) (int64, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return 0, nil, errors.New("database unreachable")
	}
	t, ok := f.keys[key]
	if !ok {
		return 0, nil, ErrInvalidKey
	}
	return 1, append([]string(nil), t...), nil
}

func (f *fakeResolver) set(key string, topics ...string) {
	f.mu.Lock()
	f.keys[key] = topics
	f.mu.Unlock()
}

func setup(t *testing.T) (*Hub, *fakeResolver, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := &fakeResolver{keys: map[string][]string{"KEY": {"/users/1", "/groups/5"}}}
	h := NewHub(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e := gin.New()
	e.GET("/stream", h.Handler())
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return h, r, "ws" + strings.TrimPrefix(srv.URL, "http") + "/stream"
}

type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func dial(t *testing.T, url string, hdr http.Header) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr}) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &client{t: t, ws: ws}
}

func (c *client) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if err := c.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *client) expectEvent(name string) map[string]any {
	c.t.Helper()
	m := c.recv()
	if m["event"] != name {
		c.t.Fatalf("want %s, got %v", name, m)
	}
	return m
}

func (c *client) closeCode() websocket.StatusCode {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubscribeAndUpdate(t *testing.T) {
	h, _, url := setup(t)
	c := dial(t, url, nil)
	if m := c.expectEvent("connected"); m["retry"] != float64(10000) {
		t.Fatalf("retry: %v", m)
	}
	c.send(map[string]any{"action": "createSubscriptions", "subscriptions": []any{map[string]any{"apiKey": "KEY"}}})
	m := c.expectEvent("subscriptionsCreated")
	if errs, ok := m["errors"].([]any); !ok || len(errs) != 0 {
		t.Fatalf("errors must be an empty array: %v", m)
	}
	h.Publish("/users/1", 7)
	h.Publish("/users/99", 8) // not subscribed
	if m := c.expectEvent("topicUpdated"); m["topic"] != "/users/1" || m["version"] != float64(7) {
		t.Fatalf("update: %v", m)
	}
	h.Publish("/groups/5", 3)
	if m := c.expectEvent("topicUpdated"); m["topic"] != "/groups/5" {
		t.Fatalf("group update: %v", m)
	}
}

func TestInvalidKeyIsReportedNotFatal(t *testing.T) {
	_, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"action": "createSubscriptions", "subscriptions": []any{map[string]any{"apiKey": "NOPE"}}})
	m := c.expectEvent("subscriptionsCreated")
	if errs := m["errors"].([]any); len(errs) != 1 {
		t.Fatalf("want one error: %v", m)
	}
}

func TestHandshakeKey(t *testing.T) {
	h, _, url := setup(t)
	c := dial(t, url, http.Header{"Zotero-API-Key": {"KEY"}})
	c.expectEvent("connected")
	h.Publish("/users/1", 2)
	c.expectEvent("topicUpdated")

	bad := dial(t, url, http.Header{"Zotero-API-Key": {"NOPE"}})
	if code := bad.closeCode(); code != zs.CloseForbidden {
		t.Fatalf("close code %d", code)
	}
}

// A key that cannot be checked right now must not leave the connection open
// without its subscription: the client would never hear about changes.
func TestTemporaryResolveFailureClosesRetryably(t *testing.T) {
	_, r, url := setup(t)
	r.mu.Lock()
	r.down = true
	r.mu.Unlock()

	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"action": "createSubscriptions", "subscriptions": []any{map[string]any{"apiKey": "KEY"}}})
	if code := c.closeCode(); code != websocket.StatusTryAgainLater {
		t.Fatalf("close code %d", code)
	}

	hs := dial(t, url, http.Header{"Zotero-API-Key": {"KEY"}})
	if code := hs.closeCode(); code != websocket.StatusTryAgainLater {
		t.Fatalf("handshake key: close code %d", code)
	}
}

func TestPublicTopicsAndForeignTopics(t *testing.T) {
	_, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"topics": []string{"styles", "translators"}})
	m := c.expectEvent("subscriptionsCreated")
	if len(m["errors"].([]any)) != 0 {
		t.Fatalf("public topics rejected: %v", m)
	}
	c.send(map[string]any{"apiKey": "KEY", "topics": []string{"/users/1", "/users/2"}})
	m = c.expectEvent("subscriptionsCreated")
	if len(m["errors"].([]any)) != 1 {
		t.Fatalf("foreign topic must be an error: %v", m)
	}
}

func TestDeleteSubscriptions(t *testing.T) {
	h, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"apiKey": "KEY"})
	c.expectEvent("subscriptionsCreated")
	c.send(map[string]any{"action": "deleteSubscriptions", "subscriptions": []any{map[string]any{"apiKey": "KEY"}}})
	c.expectEvent("subscriptionsDeleted")
	// Deleting something that does not exist is harmless.
	c.send(map[string]any{"action": "deleteSubscriptions", "subscriptions": []any{map[string]any{"apiKey": "OTHER"}}})
	c.expectEvent("subscriptionsDeleted")
	h.Publish("/users/1", 9)
	h.LoginCompleted("tok")
	c.send(map[string]any{"topics": []string{"styles"}})
	// The next frame must be the reply, not a stale topicUpdated.
	c.expectEvent("subscriptionsCreated")
}

func TestLoginComplete(t *testing.T) {
	h, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"topics": []string{"login-session:abc"}})
	c.expectEvent("subscriptionsCreated")
	h.LoginCompleted("abc")
	if m := c.expectEvent("loginComplete"); m["topic"] != "login-session:abc" {
		t.Fatalf("got %v", m)
	}
}

func TestAccessChanged(t *testing.T) {
	h, r, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"apiKey": "KEY"})
	c.expectEvent("subscriptionsCreated")

	r.set("KEY", "/users/1", "/groups/6")
	h.AccessChanged(1)
	got := map[string]string{}
	for range 2 {
		m := c.recv()
		got[m["event"].(string)] = m["topic"].(string)
	}
	if got["topicAdded"] != "/groups/6" || got["topicRemoved"] != "/groups/5" {
		t.Fatalf("got %v", got)
	}
	h.Publish("/groups/6", 1)
	c.expectEvent("topicUpdated")
}

// A key revoked outside this process (the CLI) never calls AccessChanged;
// the periodic revalidation has to drop its topics.
func TestRevalidateDropsRevokedKey(t *testing.T) {
	h, r, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"apiKey": "KEY"})
	c.expectEvent("subscriptionsCreated")

	r.mu.Lock()
	delete(r.keys, "KEY")
	r.mu.Unlock()
	h.Revalidate(context.Background())
	removed := map[string]bool{}
	for range 2 {
		m := c.expectEvent("topicRemoved")
		removed[m["topic"].(string)] = true
	}
	if !removed["/users/1"] || !removed["/groups/5"] {
		t.Fatalf("removed %v", removed)
	}
}

func TestBadMessagesClose(t *testing.T) {
	_, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	_ = c.ws.Write(context.Background(), websocket.MessageText, []byte("{nope"))
	if code := c.closeCode(); code != zs.CloseBadRequest {
		t.Fatalf("close code %d", code)
	}
}

func TestTooManySubscriptions(t *testing.T) {
	_, r, url := setup(t)
	var topics []string
	for i := range 51 {
		topics = append(topics, zs.GroupTopic(int64(i+1)))
	}
	r.set("BIG", topics...)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"apiKey": "BIG"})
	if code := c.closeCode(); code != zs.CloseTooManySubs {
		t.Fatalf("close code %d", code)
	}
}

func TestUpdatesCoalesceAndSlowClientsDoNotBlock(t *testing.T) {
	h, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"apiKey": "KEY"})
	c.expectEvent("subscriptionsCreated")
	// Publishing must never block, however many events pile up.
	done := make(chan struct{})
	go func() {
		for v := range 100_000 {
			h.Publish("/users/1", int64(v+1))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked")
	}
	// Whatever arrives, versions only move forward and end at the last one.
	var last float64
	for last < 100_000 {
		m := c.expectEvent("topicUpdated")
		v := m["version"].(float64)
		if v <= last {
			t.Fatalf("version went backwards: %v after %v", v, last)
		}
		last = v
	}
}

func TestOverflowClosesWithRetryableCode(t *testing.T) {
	h, _, url := setup(t)
	h.maxQueue = 4
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"topics": []string{"login-session:a"}})
	c.expectEvent("subscriptionsCreated")
	// loginComplete events do not coalesce, so they can overflow the queue
	// while the client is not reading.
	waitFor(t, func() bool { return h.Connections() == 1 })
	for range 50 {
		h.LoginCompleted("a")
	}
	if code := c.closeCode(); code != zs.CloseTooManyRequests {
		t.Fatalf("close code %d", code)
	}
	waitFor(t, func() bool { return h.Connections() == 0 })
}

func TestDisconnectCleansUp(t *testing.T) {
	h, _, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	waitFor(t, func() bool { return h.Connections() == 1 })
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, func() bool { return h.Connections() == 0 })
}

func TestRevocationRemovesExplicitTopics(t *testing.T) {
	h, r, url := setup(t)
	c := dial(t, url, nil)
	c.expectEvent("connected")
	c.send(map[string]any{"apiKey": "KEY", "topics": []string{"/groups/5"}})
	c.expectEvent("subscriptionsCreated")

	r.mu.Lock()
	delete(r.keys, "KEY") // key revoked
	r.mu.Unlock()
	h.AccessChanged(1)
	if m := c.expectEvent("topicRemoved"); m["topic"] != "/groups/5" {
		t.Fatalf("got %v", m)
	}
	h.Publish("/groups/5", 9)
	c.send(map[string]any{"topics": []string{"styles"}})
	// The reply arrives, not a topicUpdated for the revoked topic.
	c.expectEvent("subscriptionsCreated")
}
