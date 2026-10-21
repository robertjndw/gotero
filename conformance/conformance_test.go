package conformance

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recorder lets tests observe helper failures without failing themselves.
type recorder struct {
	testing.TB
	msg string
}

func (r *recorder) Helper() {}
func (r *recorder) Fatalf(f string, a ...any) {
	r.msg = fmt.Sprintf(f, a...)
	panic(r)
}
func (r *recorder) Errorf(f string, a ...any) { r.msg = fmt.Sprintf(f, a...) }

func fails(fn func(tb testing.TB)) (msg string) {
	r := &recorder{}
	defer func() {
		if p := recover(); p != nil && p != r {
			panic(p)
		}
		msg = r.msg
	}()
	fn(r)
	return ""
}

func TestNewKeyFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		k := newKey()
		if !keyRe.MatchString(k) {
			t.Fatalf("bad key %q", k)
		}
		seen[k] = true
	}
	if len(seen) < 495 {
		t.Errorf("keys collide too often: %d unique of 500", len(seen))
	}
}

func TestSuiteTableSane(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range allSuites() {
		if len(s.tests) == 0 {
			t.Errorf("suite %s is empty", s.name)
		}
		for _, tc := range s.tests {
			id := s.name + "/" + tc.name
			if seen[id] {
				t.Errorf("duplicate test %s", id)
			}
			seen[id] = true
		}
	}
	if TestCount() != len(seen) {
		t.Error("TestCount mismatch")
	}
}

func TestRunSkipsWithoutConfig(t *testing.T) {
	t.Run("no url", func(t *testing.T) { Run(t, Config{}) })
}

func fakeEnv(t *testing.T, h http.HandlerFunc) *env {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e, err := newEnv(Config{BaseURL: srv.URL + "/api", Provisioner: NewStaticProvisioner(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDoSendsHeadersAndGzip(t *testing.T) {
	var gotVer, gotKey, gotEnc, gotBody, gotPath string
	e := fakeEnv(t, func(w http.ResponseWriter, r *http.Request) {
		gotVer, gotKey, gotEnc, gotPath = r.Header.Get("Zotero-API-Version"), r.Header.Get("Zotero-API-Key"), r.Header.Get("Content-Encoding"), r.URL.Path
		zr, err := gzip.NewReader(r.Body)
		if err == nil {
			b, _ := io.ReadAll(zr)
			gotBody = string(b)
		}
		w.Header().Set("Last-Modified-Version", "42")
		w.WriteHeader(http.StatusOK)
	})
	r := e.do(t, "POST", "users/1/items", withKey("K"), withJSON(obj{"a": 1}), withGzip())
	if gotVer != "3" || gotKey != "K" || gotEnc != "gzip" || gotBody != `{"a":1}` || gotPath != "/api/users/1/items" {
		t.Errorf("got ver=%q key=%q enc=%q body=%q path=%q", gotVer, gotKey, gotEnc, gotBody, gotPath)
	}
	if versionHeader(t, r) != 42 {
		t.Error("version header not parsed")
	}
}

func TestDoDoesNotFollowRedirects(t *testing.T) {
	e := fakeEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	r := e.do(t, "GET", "x")
	if r.Status != 302 || r.Header.Get("Location") != "/elsewhere" {
		t.Errorf("got %s %v", r, r.Header)
	}
}

func TestHelpersReportFailures(t *testing.T) {
	r := &response{Status: 500, Header: http.Header{}}
	if m := fails(func(tb testing.TB) { mustStatus(tb, r, 200) }); !strings.Contains(m, "want 200") {
		t.Errorf("mustStatus message %q", m)
	}
	if m := fails(func(tb testing.TB) { versionHeader(tb, r) }); !strings.Contains(m, "Last-Modified-Version") {
		t.Errorf("versionHeader message %q", m)
	}
	r.Header.Set("Last-Modified-Version", "abc")
	if m := fails(func(tb testing.TB) { versionHeader(tb, r) }); m == "" {
		t.Error("non-integer version accepted")
	}
	if m := fails(func(tb testing.TB) { mustHeader(tb, r, "X-Nope") }); m == "" {
		t.Error("missing header accepted")
	}
}

func TestRetryAfterMustBeInteger(t *testing.T) {
	h := http.Header{"Retry-After": {"1.5"}}
	if m := fails(func(tb testing.TB) { checkUniversal(tb, "GET", "u", &response{Header: h}) }); m == "" {
		t.Error("fractional Retry-After accepted")
	}
	h = http.Header{"Retry-After": {"3"}, "Backoff": {"10"}}
	if m := fails(func(tb testing.TB) { checkUniversal(tb, "GET", "u", &response{Header: h}) }); m != "" {
		t.Errorf("valid headers rejected: %s", m)
	}
}

func TestAtHelper(t *testing.T) {
	o := obj{"a": obj{"b": 3.0, "c": "x"}}
	if got := atInt(t, o, "a", "b"); got != 3 {
		t.Errorf("got %d", got)
	}
	if m := fails(func(tb testing.TB) { at[string](tb, o, "a", "b") }); m == "" {
		t.Error("type mismatch accepted")
	}
	if m := fails(func(tb testing.TB) { at[string](tb, o, "a", "zzz") }); m == "" {
		t.Error("missing path accepted")
	}
}

func TestAssertOrder(t *testing.T) {
	good := []byte(`{"key":"A","data":{"key":"A","itemType":"attachment","linkMode":"imported_file","filename":"f","md5":null}}`)
	bad := []byte(`{"key":"A","data":{"key":"A","filename":"f","linkMode":"imported_file"}}`)
	if m := fails(func(tb testing.TB) { assertOrder(tb, good, attachmentOrder) }); m != "" {
		t.Errorf("good order rejected: %s", m)
	}
	if m := fails(func(tb testing.TB) { assertOrder(tb, bad, attachmentOrder) }); m == "" {
		t.Error("bad order accepted")
	}
}

func TestStaticProvisioner(t *testing.T) {
	users, err := ParseStaticUsers("1:alice:pw:KEYA, 2:bob:pw2:KEYB\n")
	if err != nil || len(users) != 2 || users[1].APIKey != "KEYB" {
		t.Fatalf("users=%v err=%v", users, err)
	}
	for _, bad := range []string{"1:a:b", "x:a:b:c", "1:a:b:"} {
		if _, err := ParseStaticUsers(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	p := NewStaticProvisioner(users)
	for range 2 {
		if _, err := p.NewUser(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.NewUser(context.Background()); !errors.Is(err, ErrNotEnoughUsers) {
		t.Errorf("err = %v, want ErrNotEnoughUsers", err)
	}
	if _, err := p.NewGroup(context.Background(), User{}, nil, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestNewEnvValidates(t *testing.T) {
	if _, err := newEnv(Config{BaseURL: "localhost:8080/api"}); err == nil {
		t.Error("relative BaseURL accepted")
	}
	e, err := newEnv(Config{BaseURL: "http://h/api"})
	if err != nil || e.base != "http://h/api/" {
		t.Errorf("base = %q err=%v", e.base, err)
	}
}

func TestPatternReader(t *testing.T) {
	b, err := io.ReadAll(&patternReader{block: []byte("abc"), left: 8})
	if err != nil || string(b) != "abcabcab" {
		t.Errorf("got %q %v", b, err)
	}
}
