package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	noReconnectLo = 4400
	noReconnectHi = 4499
)

// Close codes used by the server. The client does not reconnect after a code
// in 4400-4499 except CloseTooManyRequests.
const (
	// CloseBadRequest means the client sent something unparseable or invalid.
	CloseBadRequest = 4400
	// CloseForbidden means the API key or topic is not allowed.
	CloseForbidden = 4403
	// CloseTooManySubs means the connection exceeded MaxSubscriptions.
	CloseTooManySubs = 4413
	// CloseTooManyRequests asks the client to back off and reconnect later.
	CloseTooManyRequests = 4429
)

// MaxSubscriptions is the most subscriptions one connection may hold.
const MaxSubscriptions = 50

// MaxMessageSize is a sensible read limit for client frames.
const MaxMessageSize = 64 << 10

// NoReconnect reports whether a client will stay disconnected after the
// server closes with code.
func NoReconnect(code int) bool {
	return code >= noReconnectLo && code <= noReconnectHi && code != CloseTooManyRequests
}

// Client actions.
const (
	ActionCreateSubscriptions = "createSubscriptions"
	ActionDeleteSubscriptions = "deleteSubscriptions"
)

// Server event names.
const (
	EventConnected            = "connected"
	EventSubscriptionsCreated = "subscriptionsCreated"
	EventSubscriptionsDeleted = "subscriptionsDeleted"
	EventTopicUpdated         = "topicUpdated"
	EventTopicAdded           = "topicAdded"
	EventTopicRemoved         = "topicRemoved"
	EventLoginComplete        = "loginComplete"
)

// Subscription is one entry of a subscriptions list. For
// createSubscriptions an APIKey alone means every topic the key can see; for
// deleteSubscriptions Topic narrows the removal to one topic.
type Subscription struct {
	APIKey string   `json:"apiKey,omitempty"`
	Topics []string `json:"topics,omitempty"`
	Topic  string   `json:"topic,omitempty"`
}

// ClientMessage is a frame sent by the client. Clients use two shapes: an
// explicit action with a subscriptions list, or the short form with apiKey
// and topics at the top level, which means createSubscriptions.
type ClientMessage struct {
	Action        string         `json:"action,omitempty"`
	APIKey        string         `json:"apiKey,omitempty"`
	Topics        []string       `json:"topics,omitempty"`
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
}

// ErrBadMessage is returned for frames that are not valid client messages.
var ErrBadMessage = errors.New("stream: bad client message")

// ParseClientMessage decodes and validates a client frame.
func ParseClientMessage(data []byte) (ClientMessage, error) {
	var m ClientMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&m); err != nil {
		return ClientMessage{}, fmt.Errorf("%w: %w", ErrBadMessage, err)
	}
	if dec.More() {
		return ClientMessage{}, fmt.Errorf("%w: trailing data", ErrBadMessage)
	}
	if _, err := m.Normalize(); err != nil {
		return ClientMessage{}, err
	}
	return m, nil
}

// ErrTooManySubscriptions maps to the CloseTooManySubs close code.
var ErrTooManySubscriptions = errors.New("stream: too many subscriptions")

// maxAPIKeyLen guards against absurd values; real keys are 24 characters.
const maxAPIKeyLen = 128

// Kind returns the effective action: the short form counts as
// createSubscriptions.
func (m ClientMessage) Kind() string {
	if m.Action == "" {
		return ActionCreateSubscriptions
	}
	return m.Action
}

// Normalize validates the message and returns its subscriptions in one
// uniform list, folding the short top-level form into a single entry.
func (m ClientMessage) Normalize() ([]Subscription, error) {
	kind := m.Kind()
	if kind != ActionCreateSubscriptions && kind != ActionDeleteSubscriptions {
		return nil, fmt.Errorf("%w: unknown action %q", ErrBadMessage, truncate(kind))
	}
	subs := m.Subscriptions
	topLevel := m.APIKey != "" || len(m.Topics) > 0
	switch {
	case topLevel && len(subs) > 0:
		return nil, fmt.Errorf("%w: both subscriptions and top-level apiKey/topics", ErrBadMessage)
	case topLevel:
		subs = []Subscription{{APIKey: m.APIKey, Topics: m.Topics}}
	case len(subs) == 0:
		return nil, fmt.Errorf("%w: no subscriptions", ErrBadMessage)
	}
	if len(subs) > MaxSubscriptions {
		return nil, ErrTooManySubscriptions
	}
	for _, s := range subs {
		if len(s.APIKey) > maxAPIKeyLen {
			return nil, fmt.Errorf("%w: apiKey too long", ErrBadMessage)
		}
		if len(s.Topics) > MaxSubscriptions {
			return nil, ErrTooManySubscriptions
		}
		switch kind {
		case ActionCreateSubscriptions:
			if s.APIKey == "" && len(s.Topics) == 0 {
				return nil, fmt.Errorf("%w: subscription needs an apiKey or topics", ErrBadMessage)
			}
		case ActionDeleteSubscriptions:
			if s.APIKey == "" {
				return nil, fmt.Errorf("%w: subscription needs an apiKey", ErrBadMessage)
			}
		}
		for _, t := range s.Topics {
			if _, err := ParseTopic(t); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrBadMessage, err)
			}
		}
		if s.Topic != "" {
			if _, err := ParseTopic(s.Topic); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrBadMessage, err)
			}
		}
	}
	return subs, nil
}

// Event is implemented by every server-to-client message.
type Event interface {
	// EventName returns the value of the "event" member.
	EventName() string
}

// SubscriptionError reports why one subscription could not be created.
type SubscriptionError struct {
	APIKey string `json:"apiKey,omitempty"`
	Topic  string `json:"topic,omitempty"`
	Error  string `json:"error"`
}

// Connected is sent right after the socket opens. Retry is the reconnect
// delay in milliseconds the client should use.
type Connected struct {
	Retry int `json:"retry"`
}

// EventName implements Event.
func (Connected) EventName() string { return EventConnected }

// MarshalJSON adds the event member.
func (e Connected) MarshalJSON() ([]byte, error) {
	type plain Connected
	return json.Marshal(struct {
		Event string `json:"event"`
		plain
	}{EventConnected, plain(e)})
}

// SubscriptionsCreated answers createSubscriptions.
type SubscriptionsCreated struct {
	Subscriptions []Subscription      `json:"subscriptions"`
	Errors        []SubscriptionError `json:"errors"`
}

// EventName implements Event.
func (SubscriptionsCreated) EventName() string { return EventSubscriptionsCreated }

// MarshalJSON adds the event member and keeps both lists non-null; clients
// iterate "errors" unconditionally.
func (e SubscriptionsCreated) MarshalJSON() ([]byte, error) {
	if e.Subscriptions == nil {
		e.Subscriptions = []Subscription{}
	}
	if e.Errors == nil {
		e.Errors = []SubscriptionError{}
	}
	type plain SubscriptionsCreated
	return json.Marshal(struct {
		Event string `json:"event"`
		plain
	}{EventSubscriptionsCreated, plain(e)})
}

// SubscriptionsDeleted answers deleteSubscriptions.
type SubscriptionsDeleted struct{}

// EventName implements Event.
func (SubscriptionsDeleted) EventName() string { return EventSubscriptionsDeleted }

// MarshalJSON adds the event member.
func (SubscriptionsDeleted) MarshalJSON() ([]byte, error) {
	return []byte(`{"event":"subscriptionsDeleted"}`), nil
}

// TopicUpdated announces a new library version.
type TopicUpdated struct {
	Topic   string `json:"topic"`
	Version int64  `json:"version"`
}

// EventName implements Event.
func (TopicUpdated) EventName() string { return EventTopicUpdated }

// MarshalJSON adds the event member.
func (e TopicUpdated) MarshalJSON() ([]byte, error) {
	type plain TopicUpdated
	return json.Marshal(struct {
		Event string `json:"event"`
		plain
	}{EventTopicUpdated, plain(e)})
}

// TopicAdded tells a key it gained access to a library.
type TopicAdded struct {
	APIKey string `json:"apiKey"`
	Topic  string `json:"topic"`
}

// EventName implements Event.
func (TopicAdded) EventName() string { return EventTopicAdded }

// MarshalJSON adds the event member.
func (e TopicAdded) MarshalJSON() ([]byte, error) {
	type plain TopicAdded
	return json.Marshal(struct {
		Event string `json:"event"`
		plain
	}{EventTopicAdded, plain(e)})
}

// TopicRemoved tells a key it lost access to a library.
type TopicRemoved struct {
	APIKey string `json:"apiKey"`
	Topic  string `json:"topic"`
}

// EventName implements Event.
func (TopicRemoved) EventName() string { return EventTopicRemoved }

// MarshalJSON adds the event member.
func (e TopicRemoved) MarshalJSON() ([]byte, error) {
	type plain TopicRemoved
	return json.Marshal(struct {
		Event string `json:"event"`
		plain
	}{EventTopicRemoved, plain(e)})
}

// LoginComplete is published on a login-session topic once the browser login
// was approved. The client then polls the session endpoint for the key.
type LoginComplete struct {
	Topic string `json:"topic"`
}

// EventName implements Event.
func (LoginComplete) EventName() string { return EventLoginComplete }

// MarshalJSON adds the event member.
func (e LoginComplete) MarshalJSON() ([]byte, error) {
	type plain LoginComplete
	return json.Marshal(struct {
		Event string `json:"event"`
		plain
	}{EventLoginComplete, plain(e)})
}
