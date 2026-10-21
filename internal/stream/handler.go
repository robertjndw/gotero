package stream

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	zs "github.com/robertjndw/gotero/zotero/stream"
)

// reconnectDelay is what we tell clients to wait before reconnecting, in ms.
const reconnectDelay = 10_000

// Handler upgrades the request and serves one client until it disconnects.
func (h *Hub) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		h.serve(c.Writer, c.Request)
	}
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Subscriptions are authorized by API keys inside messages, not by
		// cookies, so a cross-origin page gains nothing it could not do with
		// the key directly. Desktop clients do not send a browser Origin.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept already wrote the HTTP error
	}
	ws.SetReadLimit(zs.MaxMessageSize)
	c := newConn(ws, h.maxQueue)
	h.add(c)
	defer h.remove(c)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Single-key mode: a key in the handshake subscribes to all its topics.
	if key := r.Header.Get("Zotero-API-Key"); key != "" {
		if code, reason := h.create(ctx, c, []zs.Subscription{{APIKey: key}}, false); code != 0 {
			_ = ws.Close(websocket.StatusCode(code), reason)
			return
		}
	}
	c.enqueue(zs.Connected{Retry: reconnectDelay})

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		h.writeLoop(ctx, c)
		cancel()
	}()

	code, reason := h.readLoop(ctx, c)
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	cancel()
	<-writerDone
	if code != 0 {
		_ = ws.Close(websocket.StatusCode(code), reason)
	} else {
		_ = ws.CloseNow()
	}
}

func (h *Hub) readLoop(ctx context.Context, c *conn) (int, string) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if errors.As(err, &ce) || errors.Is(err, context.Canceled) {
				return 0, ""
			}
			if errors.Is(err, websocket.ErrMessageTooBig) {
				return zs.CloseBadRequest, "Message too large"
			}
			return 0, ""
		}
		if typ != websocket.MessageText {
			return zs.CloseBadRequest, "Text frames only"
		}
		msg, err := zs.ParseClientMessage(data)
		if err != nil {
			return zs.CloseBadRequest, "Invalid message"
		}
		subs, err := msg.Normalize()
		if err != nil {
			return zs.CloseBadRequest, "Invalid message"
		}
		switch msg.Kind() {
		case zs.ActionCreateSubscriptions:
			if code, reason := h.create(ctx, c, subs, true); code != 0 {
				return code, reason
			}
		case zs.ActionDeleteSubscriptions:
			c.deleteSubs(subs)
			c.enqueue(zs.SubscriptionsDeleted{})
		default:
			return zs.CloseBadRequest, "Unknown action"
		}
	}
}

// create adds subscriptions and, if reply is set, answers with
// subscriptionsCreated. A non-zero code means the connection must close.
func (h *Hub) create(ctx context.Context, c *conn, req []zs.Subscription, reply bool) (int, string) {
	created := []zs.Subscription{}
	errs := []zs.SubscriptionError{}
	var add []*subscription

	for _, s := range req {
		if s.APIKey == "" {
			// Without a key only public and login-session topics make sense.
			var ok []string
			for _, t := range s.Topics {
				pt, err := zs.ParseTopic(t)
				if err == nil && (pt.Kind == zs.TopicPublic || pt.Kind == zs.TopicLoginSession) {
					ok = append(ok, t)
				} else {
					errs = append(errs, zs.SubscriptionError{Topic: t, Error: "Topic is not valid without an API key"})
				}
			}
			if len(ok) > 0 {
				add = append(add, &subscription{topics: ok})
				created = append(created, zs.Subscription{Topics: ok})
			}
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		userID, allowed, err := h.resolver.KeyTopics(rctx, s.APIKey)
		cancel()
		if errors.Is(err, ErrInvalidKey) {
			if !reply {
				return zs.CloseForbidden, "Invalid API key"
			}
			errs = append(errs, zs.SubscriptionError{APIKey: s.APIKey, Error: "Invalid key"})
			continue
		}
		if err != nil {
			h.log.Warn("stream: resolve key", "err", err)
			errs = append(errs, zs.SubscriptionError{APIKey: s.APIKey, Error: "Temporary server error"})
			continue
		}
		sub := &subscription{apiKey: s.APIKey, userID: userID}
		if len(s.Topics) == 0 {
			sub.all = true
			sub.topics = slices.Clone(allowed)
		} else {
			for _, t := range s.Topics {
				if pt, err := zs.ParseTopic(t); err == nil && pt.Kind == zs.TopicPublic {
					sub.topics = append(sub.topics, t)
				} else if slices.Contains(allowed, t) {
					sub.topics = append(sub.topics, t)
				} else {
					errs = append(errs, zs.SubscriptionError{APIKey: s.APIKey, Topic: t, Error: "Topic is not valid for provided API key"})
				}
			}
		}
		add = append(add, sub)
		created = append(created, zs.Subscription{APIKey: s.APIKey, Topics: sub.topics})
	}

	c.mu.Lock()
	total := c.topicCount()
	for _, s := range add {
		total += len(s.topics)
	}
	if total > zs.MaxSubscriptions {
		c.mu.Unlock()
		return zs.CloseTooManySubs, "Too many subscriptions"
	}
	c.subs = append(c.subs, add...)
	c.mu.Unlock()

	if reply {
		c.enqueue(zs.SubscriptionsCreated{Subscriptions: created, Errors: errs})
	}
	return 0, ""
}

func (c *conn) deleteSubs(req []zs.Subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range req {
		kept := c.subs[:0]
		for _, s := range c.subs {
			if s.apiKey != r.APIKey {
				kept = append(kept, s)
				continue
			}
			if r.Topic == "" {
				continue // drop the whole subscription
			}
			s.topics = slices.DeleteFunc(s.topics, func(t string) bool { return t == r.Topic })
			if len(s.topics) > 0 {
				kept = append(kept, s)
			}
		}
		c.subs = kept
	}
}

func (h *Hub) writeLoop(ctx context.Context, c *conn) {
	ping := time.NewTicker(h.pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, h.writeTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-c.signal:
			events, overflow := c.drain()
			for _, ev := range events {
				b, err := marshalEvent(ev)
				if err != nil {
					h.log.Error("stream: marshal event", "err", err)
					continue
				}
				wctx, cancel := context.WithTimeout(ctx, h.writeTimeout)
				err = c.ws.Write(wctx, websocket.MessageText, b)
				cancel()
				if err != nil {
					return
				}
			}
			if overflow {
				// 4429 is the one 44xx code clients reconnect after, which
				// is exactly what a client that fell behind should do.
				_ = c.ws.Close(zs.CloseTooManyRequests, "Too many queued events")
				return
			}
		}
	}
}
