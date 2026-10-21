// Package stream implements Zotero's streaming WebSocket endpoint. Clients
// subscribe to library topics and get topicUpdated pushes, which lets Zotero
// sync seconds after a change elsewhere instead of polling.
//
// The hub is in-process: gotero runs as a single instance, so there is no
// need for an external broker.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	zs "github.com/robertjndw/gotero/zotero/stream"
)

// ErrInvalidKey is returned by a Resolver for unknown or revoked keys.
var ErrInvalidKey = errors.New("stream: invalid API key")

// Resolver tells the hub which library topics an API key may see.
type Resolver interface {
	KeyTopics(ctx context.Context, apiKey string) (userID int64, topics []string, err error)
}

// Hub fans events out to connected clients.
type Hub struct {
	resolver Resolver
	log      *slog.Logger

	mu    sync.RWMutex
	conns map[*conn]struct{}

	// Tunables, overridable in tests.
	pingInterval time.Duration
	writeTimeout time.Duration
	maxQueue     int
}

func NewHub(resolver Resolver, log *slog.Logger) *Hub {
	return &Hub{
		resolver:     resolver,
		log:          log,
		conns:        map[*conn]struct{}{},
		pingInterval: 30 * time.Second,
		writeTimeout: 10 * time.Second,
		maxQueue:     256,
	}
}

// Publish announces a new library version. It never blocks: events are
// queued per connection and topicUpdated events for the same topic coalesce,
// since only the newest version matters to a client.
func (h *Hub) Publish(topic string, version int64) {
	h.each(func(c *conn) {
		if c.wants(topic) {
			c.enqueueUpdate(topic, version)
		}
	})
}

// LoginCompleted notifies a Zotero client waiting on a browser login.
func (h *Hub) LoginCompleted(token string) {
	topic := zs.LoginSessionTopic(token)
	h.each(func(c *conn) {
		if c.wants(topic) {
			c.enqueue(zs.LoginComplete{Topic: topic})
		}
	})
}

// AccessChanged must be called when a user's set of readable libraries
// changes (group created, joined, left or deleted) and when their keys are
// revoked or the account is disabled. Subscriptions in "all topics" mode
// gain and lose topics; explicit subscriptions only ever lose them, so a
// revoked key stops receiving updates immediately.
func (h *Hub) AccessChanged(userID int64) {
	var affected []*conn
	h.each(func(c *conn) {
		if c.hasUser(userID) {
			affected = append(affected, c)
		}
	})
	if len(affected) == 0 {
		return
	}
	// Resolving keys hits the database; keep it off the caller's request.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, c := range affected {
			c.refreshUser(ctx, h.resolver, userID)
		}
	}()
}

func (h *Hub) each(fn func(*conn)) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.conns {
		fn(c)
	}
}

func (h *Hub) add(c *conn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) remove(c *conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// Connections reports the number of open sockets, for stats and tests.
func (h *Hub) Connections() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

// subscription is one apiKey (or key-less public) subscription.
type subscription struct {
	apiKey string
	userID int64
	all    bool // created without explicit topics: follows access changes
	topics []string
}

type conn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	subs    []*subscription
	queue   []zs.Event
	pending map[string]int // topic -> index of a queued topicUpdated
	signal  chan struct{}
	closed  bool
	// overflow is set when a client stops reading; the writer then closes
	// the socket so the client reconnects and re-syncs.
	overflow bool
	maxQueue int
}

func newConn(ws *websocket.Conn, maxQueue int) *conn {
	return &conn{ws: ws, pending: map[string]int{}, signal: make(chan struct{}, 1), maxQueue: maxQueue}
}

func (c *conn) wants(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		if slices.Contains(s.topics, topic) {
			return true
		}
	}
	return false
}

func (c *conn) hasUser(userID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		if s.apiKey != "" && s.userID == userID {
			return true
		}
	}
	return false
}

func (c *conn) topicCount() int {
	n := 0
	for _, s := range c.subs {
		n += len(s.topics)
	}
	return n
}

func (c *conn) enqueueUpdate(topic string, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if i, ok := c.pending[topic]; ok {
		if u, _ := c.queue[i].(zs.TopicUpdated); u.Version < version {
			c.queue[i] = zs.TopicUpdated{Topic: topic, Version: version}
		}
		return
	}
	if c.pushLocked(zs.TopicUpdated{Topic: topic, Version: version}) {
		c.pending[topic] = len(c.queue) - 1
	}
}

func (c *conn) enqueue(ev zs.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pushLocked(ev)
}

func (c *conn) pushLocked(ev zs.Event) bool {
	if c.closed {
		return false
	}
	if len(c.queue) >= c.maxQueue {
		c.overflow = true
	} else {
		c.queue = append(c.queue, ev)
	}
	select {
	case c.signal <- struct{}{}:
	default:
	}
	return !c.overflow
}

// drain takes everything queued so far.
func (c *conn) drain() ([]zs.Event, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := c.queue
	c.queue = nil
	clear(c.pending)
	return q, c.overflow
}

func (c *conn) refreshUser(ctx context.Context, r Resolver, userID int64) {
	c.mu.Lock()
	var keys []string
	for _, s := range c.subs {
		if s.apiKey != "" && s.userID == userID && !slices.Contains(keys, s.apiKey) {
			keys = append(keys, s.apiKey)
		}
	}
	c.mu.Unlock()

	for _, key := range keys {
		_, topics, err := r.KeyTopics(ctx, key)
		if errors.Is(err, ErrInvalidKey) {
			topics = nil // revoked: everything is removed
		} else if err != nil {
			continue // transient; the next change or a reconnect fixes it
		}
		c.mu.Lock()
		for _, s := range c.subs {
			if s.apiKey != key {
				continue
			}
			for _, t := range topics {
				if s.all && !slices.Contains(s.topics, t) && c.topicCount() < zs.MaxSubscriptions {
					s.topics = append(s.topics, t)
					c.pushLocked(zs.TopicAdded{APIKey: key, Topic: t})
				}
			}
			kept := s.topics[:0]
			for _, t := range s.topics {
				// Public topics (styles, translators) never depend on access.
				if pt, err := zs.ParseTopic(t); slices.Contains(topics, t) || (err == nil && pt.Kind == zs.TopicPublic) {
					kept = append(kept, t)
				} else {
					c.pushLocked(zs.TopicRemoved{APIKey: key, Topic: t})
				}
			}
			s.topics = kept
		}
		c.mu.Unlock()
	}
}

func marshalEvent(ev zs.Event) ([]byte, error) {
	return json.Marshal(ev)
}
