package stream

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TopicKind says what a topic addresses.
type TopicKind int

// The topic kinds.
const (
	TopicUser TopicKind = iota + 1
	TopicGroup
	TopicLoginSession
	// TopicPublic covers "styles" and "translators". Clients subscribe to them
	// but nothing is ever published there.
	TopicPublic
)

const loginSessionPrefix = "login-session:"

// Topic is a parsed topic string.
type Topic struct {
	Kind  TopicKind
	ID    int64  // user or group ID
	Token string // login session token, or the public topic name
}

// String returns the wire form of the topic.
func (t Topic) String() string {
	switch t.Kind {
	case TopicUser:
		return UserTopic(t.ID)
	case TopicGroup:
		return GroupTopic(t.ID)
	case TopicLoginSession:
		return LoginSessionTopic(t.Token)
	default:
		return t.Token
	}
}

// UserTopic returns the topic for a user's library, e.g. "/users/1".
func UserTopic(id int64) string { return "/users/" + strconv.FormatInt(id, 10) }

// GroupTopic returns the topic for a group library, e.g. "/groups/2".
func GroupTopic(id int64) string { return "/groups/" + strconv.FormatInt(id, 10) }

// LoginSessionTopic returns the topic a client watches during browser login.
func LoginSessionTopic(token string) string { return loginSessionPrefix + token }

// ErrInvalidTopic is returned for topics that match no known form.
var ErrInvalidTopic = errors.New("stream: invalid topic")

// maxTokenLen caps login session tokens so a topic can't be used to push
// arbitrarily large strings into server maps.
const maxTokenLen = 128

// ParseTopic parses a topic string.
func ParseTopic(s string) (Topic, error) {
	switch {
	case s == "styles" || s == "translators":
		return Topic{Kind: TopicPublic, Token: s}, nil
	case strings.HasPrefix(s, "/users/"):
		id, err := parseID(s[len("/users/"):])
		if err != nil {
			return Topic{}, fmt.Errorf("%w: %q", ErrInvalidTopic, truncate(s))
		}
		return Topic{Kind: TopicUser, ID: id}, nil
	case strings.HasPrefix(s, "/groups/"):
		id, err := parseID(s[len("/groups/"):])
		if err != nil {
			return Topic{}, fmt.Errorf("%w: %q", ErrInvalidTopic, truncate(s))
		}
		return Topic{Kind: TopicGroup, ID: id}, nil
	case strings.HasPrefix(s, loginSessionPrefix):
		tok := s[len(loginSessionPrefix):]
		if tok == "" || len(tok) > maxTokenLen || !tokenChars(tok) {
			return Topic{}, fmt.Errorf("%w: %q", ErrInvalidTopic, truncate(s))
		}
		return Topic{Kind: TopicLoginSession, Token: tok}, nil
	}
	return Topic{}, fmt.Errorf("%w: %q", ErrInvalidTopic, truncate(s))
}

func parseID(s string) (int64, error) {
	// ParseInt would accept a leading "+", which would give one library two
	// spellings.
	if s == "" || s[0] == '+' || s[0] == '-' {
		return 0, ErrInvalidTopic
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalidTopic
	}
	return id, nil
}

func tokenChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

func truncate(s string) string {
	if len(s) > 48 {
		return s[:48] + "..."
	}
	return s
}
