package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var streamTests = []testCase{
	{"ConnectedEvent", testStreamConnected},
	{"CreateSubscriptionsWithAPIKey", testStreamSubscribe},
	{"LegacyTopicsForm", testStreamLegacy},
	{"TopicUpdatedAfterWrite", testStreamTopicUpdated},
	{"InvalidKeyRejected", testStreamInvalidKey},
	{"DeleteSubscriptions", testStreamDelete},
	{"StylesAndTranslatorsTopics", testStreamStyles},
}

const streamWait = 5 * time.Second

type wsConn struct {
	t *testing.T
	c *websocket.Conn
}

func (e *env) needStream(t *testing.T) {
	if e.cfg.StreamURL == "" {
		t.Skip("Config.StreamURL not set")
	}
}

// dialStream connects and returns the connection plus the first message.
func (e *env) dialStream(t *testing.T) (*wsConn, obj) {
	e.needStream(t)
	ctx, cancel := context.WithTimeout(ctxOf(t), streamWait)
	defer cancel()
	// coder/websocket already closes the handshake response body.
	c, _, err := websocket.Dial(ctx, e.cfg.StreamURL, nil) //nolint:bodyclose
	if err != nil {
		t.Fatalf("dial %s: %v", e.cfg.StreamURL, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	w := &wsConn{t: t, c: c}
	return w, w.read(streamWait)
}

func (w *wsConn) send(v any) {
	w.t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(ctxOf(w.t), streamWait)
	defer cancel()
	if err := w.c.Write(ctx, websocket.MessageText, b); err != nil {
		w.t.Fatalf("stream write: %v", err)
	}
}

func (w *wsConn) readErr(d time.Duration) (obj, error) {
	ctx, cancel := context.WithTimeout(ctxOf(w.t), d)
	defer cancel()
	_, b, err := w.c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var o obj
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("non-JSON stream message %q: %w", b, err)
	}
	return o, nil
}

func (w *wsConn) read(d time.Duration) obj {
	w.t.Helper()
	o, err := w.readErr(d)
	if err != nil {
		w.t.Fatalf("stream read: %v", err)
	}
	return o
}

// waitEvent skips unrelated messages until the named event arrives.
func (w *wsConn) waitEvent(name string, d time.Duration, match func(obj) bool) obj {
	w.t.Helper()
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			w.t.Fatalf("no %q event within %s", name, d)
		}
		o := w.read(left)
		if o["event"] == name && (match == nil || match(o)) {
			return o
		}
	}
}

func subscribe(t *testing.T, e *env, u User) *wsConn {
	w, _ := e.dialStream(t)
	w.send(obj{"action": "createSubscriptions", "subscriptions": []obj{{"apiKey": u.APIKey}}})
	ev := w.waitEvent("subscriptionsCreated", streamWait, nil)
	if errs := at[[]any](t, ev, "errors"); len(errs) != 0 {
		t.Fatalf("subscription errors: %v", errs)
	}
	return w
}

func testStreamConnected(t *testing.T, e *env) {
	_, first := e.dialStream(t)
	if first["event"] != "connected" {
		t.Fatalf("first message = %v, want connected", first)
	}
	if atInt(t, first, "retry") <= 0 {
		t.Errorf("retry = %v", first["retry"])
	}
}

func testStreamSubscribe(t *testing.T, e *env) {
	e.needStream(t)
	u := e.newUser(t)
	w, _ := e.dialStream(t)
	w.send(obj{"action": "createSubscriptions", "subscriptions": []obj{{"apiKey": u.APIKey}}})
	ev := w.waitEvent("subscriptionsCreated", streamWait, nil)
	at[[]any](t, ev, "errors") // must be present even when empty
	subs := at[[]any](t, ev, "subscriptions")
	if len(subs) == 0 {
		t.Fatalf("no subscriptions in %v", ev)
	}
	topics := strs(at[[]any](t, subs[0].(obj), "topics"))
	if !slices.Contains(topics, fmt.Sprintf("/users/%d", u.ID)) {
		t.Errorf("topics %v lack the user library", topics)
	}
}

func testStreamLegacy(t *testing.T, e *env) {
	e.needStream(t)
	u := e.newUser(t)
	w, _ := e.dialStream(t)
	w.send(obj{"apiKey": u.APIKey, "topics": []string{fmt.Sprintf("/users/%d", u.ID)}})
	ev := w.waitEvent("subscriptionsCreated", streamWait, nil)
	at[[]any](t, ev, "errors")
	at[[]any](t, ev, "subscriptions")
}

func testStreamTopicUpdated(t *testing.T, e *env) {
	e.needStream(t)
	u := e.newUser(t)
	w := subscribe(t, e, u)
	l := e.libFor(u)
	r := l.post(t, "items", []obj{bookItem("streamed")})
	mustStatus(t, r, 200)
	v := versionHeader(t, r)
	topic := fmt.Sprintf("/users/%d", u.ID)
	ev := w.waitEvent("topicUpdated", streamWait, func(o obj) bool { return o["topic"] == topic })
	if got := atInt(t, ev, "version"); got != v {
		t.Errorf("topicUpdated version = %d, want %d", got, v)
	}
}

func testStreamInvalidKey(t *testing.T, e *env) {
	e.needStream(t)
	w, _ := e.dialStream(t)
	w.send(obj{"action": "createSubscriptions", "subscriptions": []obj{{"apiKey": "ABCDEFGHIJKLMNOPQRSTUVWX"}}})
	ev, err := w.readErr(streamWait)
	if err != nil {
		if websocket.CloseStatus(err) != 4403 {
			t.Fatalf("closed with %v, want status 4403", err)
		}
		return
	}
	if errs, ok := ev["errors"].([]any); !ok || len(errs) == 0 {
		t.Errorf("invalid key accepted: %v", ev)
	}
}

func testStreamDelete(t *testing.T, e *env) {
	e.needStream(t)
	u := e.newUser(t)
	w := subscribe(t, e, u)
	w.send(obj{"action": "deleteSubscriptions", "subscriptions": []obj{{"apiKey": u.APIKey}}})
	w.waitEvent("subscriptionsDeleted", streamWait, nil)
	// Deleting again must be ignored rather than closing the socket.
	w.send(obj{"action": "deleteSubscriptions", "subscriptions": []obj{{"apiKey": u.APIKey}}})
	w.waitEvent("subscriptionsDeleted", streamWait, nil)
}

func testStreamStyles(t *testing.T, e *env) {
	e.needStream(t)
	w, _ := e.dialStream(t)
	w.send(obj{"topics": []string{"styles", "translators"}})
	ev := w.waitEvent("subscriptionsCreated", streamWait, nil)
	at[[]any](t, ev, "errors")
}
