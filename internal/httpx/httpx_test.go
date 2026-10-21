package httpx

import (
	"bytes"
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(b)
	_ = w.Close()
	return buf.Bytes()
}

func readAll(t *testing.T, limit int64, body []byte, encoding string) (string, error) {
	t.Helper()
	var got string
	var gotErr error
	h := DecodeBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		got, gotErr = string(b), err
	}))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		return "", &statusErr{rec.Code}
	}
	return got, gotErr
}

type statusErr struct{ code int }

func (e *statusErr) Error() string { return http.StatusText(e.code) }

func TestDecodeBodyPlainAndGzip(t *testing.T) {
	payload := []byte(strings.Repeat(`{"a":1}`, 100))
	for _, enc := range []string{"", "gzip"} {
		body := payload
		if enc == "gzip" {
			body = gz(t, payload)
		}
		got, err := readAll(t, int64(len(payload)), body, enc)
		if err != nil || got != string(payload) {
			t.Fatalf("%q: err=%v len=%d", enc, err, len(got))
		}
	}
}

func TestDecodeBodyGzipBomb(t *testing.T) {
	// 10 MiB of zeros compresses to ~10 KiB, well under the raw limit.
	bomb := gz(t, make([]byte, 10<<20))
	_, err := readAll(t, 1<<20, bomb, "gzip")
	if !IsTooLarge(err) {
		t.Fatalf("expected too large, got %v", err)
	}
}

func TestDecodeBodyRawLimit(t *testing.T) {
	_, err := readAll(t, 10, []byte(strings.Repeat("x", 11)), "")
	if !IsTooLarge(err) {
		t.Fatalf("expected too large, got %v", err)
	}
}

func TestDecodeBodyRejects(t *testing.T) {
	if _, err := readAll(t, 100, []byte("not gzip"), "gzip"); err == nil {
		t.Fatal("expected error for invalid gzip")
	}
	if _, err := readAll(t, 100, []byte("x"), "br"); err == nil {
		t.Fatal("expected error for unsupported encoding")
	}
}

func TestResolvePublicURL(t *testing.T) {
	trusted, err := ParsePrefixes([]string{"10.0.0.0/8", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, configured, remote, proto, host, want string
	}{
		{"configured wins", "https://z.example/", "10.0.0.1:1", "http", "evil", "https://z.example"},
		{"untrusted ignores forwarded", "", "192.0.2.1:1", "https", "evil.example", "http://gotero:8080"},
		{"trusted proxy", "", "10.1.2.3:1", "https", "z.example", "https://z.example"},
		{"trusted v4-mapped", "", "[::ffff:127.0.0.1]:1", "https", "z.example", "https://z.example"},
		{"bad forwarded host", "", "10.1.2.3:1", "https", "z.example/evil", "https://gotero:8080"},
		{"bad proto", "", "10.1.2.3:1", "javascript", "", "http://gotero:8080"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := ResolvePublicURL(c.configured, trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = BaseURL(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "http://gotero:8080/", nil)
			req.RemoteAddr = c.remote
			if c.proto != "" {
				req.Header.Set("X-Forwarded-Proto", c.proto)
			}
			if c.host != "" {
				req.Header.Set("X-Forwarded-Host", c.host)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestParsePrefixesInvalid(t *testing.T) {
	if _, err := ParsePrefixes([]string{"nope"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestLogRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	h := Log(slog.New(slog.NewJSONHandler(&logs, nil)))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x?key=secret", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing request id")
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatal("query string leaked into logs")
	}
}

func FuzzDerivePublicURL(f *testing.F) {
	trusted := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
	f.Add("https", "z.example")
	f.Add("http, https", "a.example, b.example")
	f.Fuzz(func(t *testing.T, proto, host string) {
		req := httptest.NewRequest(http.MethodGet, "http://gotero/", nil)
		req.RemoteAddr = "10.0.0.1:1"
		req.Header.Set("X-Forwarded-Proto", proto)
		req.Header.Set("X-Forwarded-Host", host)
		got := derivePublicURL(req, trusted)
		if !strings.HasPrefix(got, "http://") && !strings.HasPrefix(got, "https://") {
			t.Fatalf("bad scheme: %q", got)
		}
		rest := got[strings.Index(got, "://")+3:]
		if strings.ContainsAny(rest, "/\\@?# \t\r\n") {
			t.Fatalf("unsafe host: %q", got)
		}
	})
}

func TestGinAdapter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	e.POST("/ok", Gin(DecodeBody(100)), func(c *gin.Context) {
		b, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.String(http.StatusOK, string(b))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ok", bytes.NewReader(gz(t, []byte("hello"))))
	req.Header.Set("Content-Encoding", "gzip")
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	// Middleware that answers itself must stop the chain.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/ok", strings.NewReader("x"))
	req.Header.Set("Content-Encoding", "br")
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestClientIP(t *testing.T) {
	trusted, _ := ParsePrefixes([]string{"10.0.0.0/8"})
	cases := []struct{ remote, xff, want string }{
		{"192.0.2.7:1", "203.0.113.9", "192.0.2.7"},                     // untrusted peer: header ignored
		{"10.0.0.2:1", "203.0.113.9", "203.0.113.9"},                    // behind our proxy
		{"10.0.0.2:1", "1.1.1.1, 203.0.113.9, 10.0.0.5", "203.0.113.9"}, // forged left part ignored
		{"10.0.0.2:1", "", "10.0.0.2"},
		{"10.0.0.2:1", "garbage", "10.0.0.2"},
	}
	for _, c := range cases {
		var got string
		h := ResolvePublicURL("", trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = ClientIP(r.Context())
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got != c.want {
			t.Errorf("%s %q: got %q want %q", c.remote, c.xff, got, c.want)
		}
	}
}

func TestRedactPath(t *testing.T) {
	for in, want := range map[string]string{
		"/api/keys/current":                     "/api/keys/current",
		"/api/keys/abcDEF123":                   "/api/keys/REDACTED",
		"/api/keys/sessions":                    "/api/keys/sessions",
		"/api/keys/sessions/tok":                "/api/keys/sessions/REDACTED",
		"/files/download/tok.sig":               "/files/download/REDACTED",
		"/files/upload/0123":                    "/files/upload/REDACTED",
		"/admin/api/login-sessions/tok/approve": "/admin/api/login-sessions/REDACTED/approve",
		"/admin/api/invites/tok":                "/admin/api/invites/REDACTED",
		"/api/users/1/items/ABCD2345":           "/api/users/1/items/ABCD2345",
	} {
		if got := RedactPath(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}
