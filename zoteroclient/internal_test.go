package zoteroclient

import (
	"testing"
	"time"
)

func TestParseLink(t *testing.T) {
	got := parseLink(`<https://a/x?start=25>; rel="next", <https://a/x?start=0>; rel=first, <https://a/y>; rel="last alternate"; title="a, b"`)
	want := map[string]string{
		"next": "https://a/x?start=25", "first": "https://a/x?start=0",
		"last": "https://a/y", "alternate": "https://a/y",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(parseLink("")) != 0 || len(parseLink("garbage, <unterminated")) != 0 {
		t.Error("malformed input should yield nothing")
	}
}

func FuzzParseLink(f *testing.F) {
	f.Add(`<https://a/x>; rel="next"`)
	f.Add(`<a>; rel=next, <b>; rel="prev last"; x="y\"z"`)
	f.Add(`<`)
	f.Add(`;;;,,,"""<>`)
	f.Fuzz(func(t *testing.T, s string) {
		for rel := range parseLink(s) {
			if rel == "" || rel != toLower(rel) {
				t.Fatalf("bad rel %q", rel)
			}
		}
	})
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3", 3 * time.Second, true},
		{" 0 ", 0, true},
		{"Thu, 01 Jan 2026 00:00:10 GMT", 10 * time.Second, true},
		{"Wed, 31 Dec 2025 00:00:10 GMT", 0, true},
		{"-1", 0, false},
		{"1.5", 0, false},
		{"", 0, false},
		{"soon", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in, now)
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func FuzzRetryAfter(f *testing.F) {
	for _, s := range []string{"1", "0", "99999999999999999999", "Thu, 01 Jan 2026 00:00:10 GMT", "-5", "", "1e3"} {
		f.Add(s)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Fuzz(func(t *testing.T, s string) {
		d, ok := parseRetryAfter(s, now)
		if d < 0 || d > maxPauseSeconds*time.Second {
			t.Fatalf("%q -> %v out of range", s, d)
		}
		if !ok && d != 0 {
			t.Fatalf("%q -> %v with ok=false", s, d)
		}
	})
}

func TestFallbackDelayBounds(t *testing.T) {
	for attempt := range 12 {
		for range 50 {
			d := fallbackDelay(attempt)
			if d < fallbackBase/2 || d > fallbackCap {
				t.Fatalf("attempt %d: %v", attempt, d)
			}
		}
	}
}
