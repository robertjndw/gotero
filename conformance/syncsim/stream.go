package syncsim

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// listener holds a client's streaming connection, the way Zotero keeps one
// open and syncs when a topicUpdated arrives.
type listener struct {
	mu       sync.Mutex
	latest   map[string]int64 // topic -> newest version announced
	subs     int              // subscriptionsCreated received
	problems []string
	// wake is signalled on every topicUpdated and reconnect.
	wake chan struct{}
}

func newListener() *listener {
	return &listener{latest: map[string]int64{}, wake: make(chan struct{}, 1)}
}

func (s *listener) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *listener) version(topic string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest[topic]
}

func (s *listener) subscriptionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs
}

func (s *listener) isSubscribed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs > 0
}

func (s *listener) problem(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

// run keeps the connection up until ctx ends, reconnecting after drops.
func (c *client) runStream(ctx context.Context, streamURL string) {
	for ctx.Err() == nil {
		err := c.streamOnce(ctx, streamURL)
		if ctx.Err() != nil {
			return
		}
		c.trace.add("stream: %v, reconnecting", err)
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

func (c *client) streamOnce(ctx context.Context, streamURL string) error {
	s := c.stream
	ws, _, err := websocket.Dial(ctx, streamURL, nil) //nolint:bodyclose // the library closes it
	if err != nil {
		return err
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(1 << 20)

	read := func() (map[string]any, error) {
		_, b, err := ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := decode(b, &m); err != nil {
			s.problem("stream: undecodable message %q", b)
			return nil, err
		}
		c.trace.add("stream <- %s", clip(b, 160))
		return m, nil
	}
	m, err := read()
	if err != nil {
		return err
	}
	if m["event"] != "connected" {
		s.problem("stream: first message is %v, want connected", m)
	}
	sub, _ := json.Marshal(map[string]any{
		"action": "createSubscriptions", "subscriptions": []any{map[string]any{"apiKey": c.user.APIKey}},
	})
	if err := ws.Write(ctx, websocket.MessageText, sub); err != nil {
		return err
	}
	for {
		m, err := read()
		if err != nil {
			return err
		}
		switch m["event"] {
		case "subscriptionsCreated":
			if errs, _ := m["errors"].([]any); len(errs) > 0 {
				s.problem("stream: subscription errors %v", errs)
			}
			s.mu.Lock()
			s.subs++
			s.mu.Unlock()
			// Zotero syncs after (re)connecting, since events may have been
			// missed while it was away.
			s.signal()
		case "topicUpdated":
			topic, _ := m["topic"].(string)
			n, _ := m["version"].(json.Number)
			v, err := n.Int64()
			if topic == "" || err != nil {
				s.problem("stream: malformed topicUpdated %v", m)
				continue
			}
			s.mu.Lock()
			s.latest[topic] = max(s.latest[topic], v)
			s.mu.Unlock()
			s.signal()
		}
	}
}
