package stream

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestTopics(t *testing.T) {
	if UserTopic(12) != "/users/12" || GroupTopic(3) != "/groups/3" || LoginSessionTopic("abc") != "login-session:abc" {
		t.Error("topic constructors")
	}
	tests := []struct {
		in   string
		want Topic
		ok   bool
	}{
		{"/users/1", Topic{Kind: TopicUser, ID: 1}, true},
		{"/groups/99", Topic{Kind: TopicGroup, ID: 99}, true},
		{"login-session:abc-DEF_123", Topic{Kind: TopicLoginSession, Token: "abc-DEF_123"}, true},
		{"styles", Topic{Kind: TopicPublic, Token: "styles"}, true},
		{"translators", Topic{Kind: TopicPublic, Token: "translators"}, true},
		{"/users/0", Topic{}, false},
		{"/users/-1", Topic{}, false},
		{"/users/+1", Topic{}, false},
		{"/users/", Topic{}, false},
		{"/users/1/x", Topic{}, false},
		{"/users/99999999999999999999", Topic{}, false},
		{"/libraries/1", Topic{}, false},
		{"login-session:", Topic{}, false},
		{"login-session:a b", Topic{}, false},
		{"login-session:" + strings.Repeat("a", 200), Topic{}, false},
		{"", Topic{}, false},
		{"Styles", Topic{}, false},
	}
	for _, tc := range tests {
		got, err := ParseTopic(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseTopic(%q) = %+v, %v", tc.in, got, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidTopic) {
			t.Errorf("error type: %v", err)
		}
		if err == nil && got.String() != tc.in {
			t.Errorf("String() = %q, want %q", got.String(), tc.in)
		}
	}
}

func TestCloseCodes(t *testing.T) {
	for code, want := range map[int]bool{
		1000: false, 4399: false, 4400: true, 4403: true, 4413: true, 4429: false, 4499: true, 4500: false,
	} {
		if NoReconnect(code) != want {
			t.Errorf("NoReconnect(%d) != %v", code, want)
		}
	}
	if CloseBadRequest != 4400 || CloseForbidden != 4403 || CloseTooManySubs != 4413 {
		t.Error("close code values")
	}
}

func TestParseClientMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kind string
		subs []Subscription
		err  bool
	}{
		{"create with apiKey", `{"action":"createSubscriptions","subscriptions":[{"apiKey":"K"}]}`, ActionCreateSubscriptions, []Subscription{{APIKey: "K"}}, false},
		{"create with topics", `{"action":"createSubscriptions","subscriptions":[{"apiKey":"K","topics":["/users/1","/groups/2"]}]}`, ActionCreateSubscriptions, []Subscription{{APIKey: "K", Topics: []string{"/users/1", "/groups/2"}}}, false},
		{"short form", `{"apiKey":"K","topics":["/users/1"]}`, ActionCreateSubscriptions, []Subscription{{APIKey: "K", Topics: []string{"/users/1"}}}, false},
		{"public topics", `{"topics":["styles","translators"]}`, ActionCreateSubscriptions, []Subscription{{Topics: []string{"styles", "translators"}}}, false},
		{"login session", `{"action":"createSubscriptions","subscriptions":[{"topics":["login-session:tok"]}]}`, ActionCreateSubscriptions, []Subscription{{Topics: []string{"login-session:tok"}}}, false},
		{"delete", `{"action":"deleteSubscriptions","subscriptions":[{"apiKey":"K","topic":"/users/1"}]}`, ActionDeleteSubscriptions, []Subscription{{APIKey: "K", Topic: "/users/1"}}, false},
		{"delete whole key", `{"action":"deleteSubscriptions","subscriptions":[{"apiKey":"K"}]}`, ActionDeleteSubscriptions, []Subscription{{APIKey: "K"}}, false},
		{"unknown fields ignored", `{"action":"createSubscriptions","extra":1,"subscriptions":[{"apiKey":"K"}]}`, ActionCreateSubscriptions, []Subscription{{APIKey: "K"}}, false},

		{"not json", `nope`, "", nil, true},
		{"not object", `[]`, "", nil, true},
		{"empty object", `{}`, "", nil, true},
		{"trailing", `{"apiKey":"K"} {}`, "", nil, true},
		{"unknown action", `{"action":"explode","subscriptions":[{"apiKey":"K"}]}`, "", nil, true},
		{"both forms", `{"apiKey":"K","subscriptions":[{"apiKey":"K"}]}`, "", nil, true},
		{"empty subscription", `{"action":"createSubscriptions","subscriptions":[{}]}`, "", nil, true},
		{"delete without key", `{"action":"deleteSubscriptions","subscriptions":[{"topic":"/users/1"}]}`, "", nil, true},
		{"bad topic", `{"apiKey":"K","topics":["/nope/1"]}`, "", nil, true},
		{"bad delete topic", `{"action":"deleteSubscriptions","subscriptions":[{"apiKey":"K","topic":"x"}]}`, "", nil, true},
		{"huge key", `{"apiKey":"` + strings.Repeat("k", 500) + `"}`, "", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseClientMessage([]byte(tc.in))
			if (err != nil) != tc.err {
				t.Fatalf("err = %v", err)
			}
			if tc.err {
				return
			}
			if m.Kind() != tc.kind {
				t.Errorf("kind %s", m.Kind())
			}
			subs, err := m.Normalize()
			if err != nil || !reflect.DeepEqual(subs, tc.subs) {
				t.Errorf("subs = %+v, %v", subs, err)
			}
		})
	}
}

func TestTooManySubscriptions(t *testing.T) {
	var subs []string
	for range MaxSubscriptions + 1 {
		subs = append(subs, `{"apiKey":"K"}`)
	}
	_, err := ParseClientMessage([]byte(`{"action":"createSubscriptions","subscriptions":[` + strings.Join(subs, ",") + `]}`))
	if !errors.Is(err, ErrTooManySubscriptions) {
		t.Errorf("got %v", err)
	}
	_, err = ParseClientMessage([]byte(`{"action":"createSubscriptions","subscriptions":[` + strings.Join(subs[:MaxSubscriptions], ",") + `]}`))
	if err != nil {
		t.Errorf("exactly the max: %v", err)
	}
	topics := strings.TrimSuffix(strings.Repeat(`"styles",`, MaxSubscriptions+1), ",")
	if _, err := ParseClientMessage([]byte(`{"topics":[` + topics + `]}`)); !errors.Is(err, ErrTooManySubscriptions) {
		t.Errorf("topics: %v", err)
	}
}

func TestEventsJSON(t *testing.T) {
	tests := []struct {
		ev   Event
		want string
	}{
		{Connected{Retry: 10000}, `{"event":"connected","retry":10000}`},
		{SubscriptionsCreated{}, `{"event":"subscriptionsCreated","subscriptions":[],"errors":[]}`},
		{SubscriptionsCreated{
			Subscriptions: []Subscription{{APIKey: "K", Topics: []string{"/users/1"}}},
			Errors:        []SubscriptionError{{APIKey: "BAD", Error: "Invalid API key"}},
		}, `{"event":"subscriptionsCreated","subscriptions":[{"apiKey":"K","topics":["/users/1"]}],"errors":[{"apiKey":"BAD","error":"Invalid API key"}]}`},
		{SubscriptionsDeleted{}, `{"event":"subscriptionsDeleted"}`},
		{TopicUpdated{Topic: "/users/1", Version: 5}, `{"event":"topicUpdated","topic":"/users/1","version":5}`},
		{TopicAdded{APIKey: "K", Topic: "/groups/2"}, `{"event":"topicAdded","apiKey":"K","topic":"/groups/2"}`},
		{TopicRemoved{APIKey: "K", Topic: "/groups/2"}, `{"event":"topicRemoved","apiKey":"K","topic":"/groups/2"}`},
		{LoginComplete{Topic: "login-session:t"}, `{"event":"loginComplete","topic":"login-session:t"}`},
	}
	for _, tc := range tests {
		b, err := json.Marshal(tc.ev)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Errorf("got  %s\nwant %s", b, tc.want)
		}
		var probe struct{ Event string }
		if err := json.Unmarshal(b, &probe); err != nil || probe.Event != tc.ev.EventName() {
			t.Errorf("event name %q vs %q", probe.Event, tc.ev.EventName())
		}
	}
	// Pointers marshal the same.
	if b, _ := json.Marshal(&Connected{Retry: 1}); string(b) != `{"event":"connected","retry":1}` {
		t.Errorf("pointer: %s", b)
	}
}

func FuzzStreamClientMessage(f *testing.F) {
	for _, s := range []string{
		`{"action":"createSubscriptions","subscriptions":[{"apiKey":"K"}]}`,
		`{"apiKey":"K","topics":["/users/1"]}`,
		`{"topics":["styles","translators"]}`,
		`{"action":"deleteSubscriptions","subscriptions":[{"apiKey":"K","topic":"/users/1"}]}`,
		`{}`, `[]`, `null`, `{"subscriptions":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseClientMessage(data)
		if err != nil {
			return
		}
		subs, err := m.Normalize()
		if err != nil {
			t.Fatalf("parsed message failed Normalize: %v", err)
		}
		if len(subs) == 0 || len(subs) > MaxSubscriptions {
			t.Fatalf("%d subscriptions", len(subs))
		}
		for _, s := range subs {
			for _, topic := range s.Topics {
				p, err := ParseTopic(topic)
				if err != nil || p.String() != topic {
					t.Fatalf("topic %q does not round trip: %v", topic, err)
				}
			}
		}
	})
}
