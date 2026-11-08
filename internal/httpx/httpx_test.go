package httpx

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"testing/iotest"
	"time"

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
		// Our proxy would never append garbage, so whatever sits left of it
		// was not vouched for by anyone we trust.
		{"10.0.0.2:1", "1.2.3.4, garbage", "10.0.0.2"},
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

// The probe read at the limit must not reject a body that ends exactly there.
func TestDecodeBodyGzipExactlyAtLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	for _, limit := range []int64{int64(len(payload)), int64(len(payload)) - 1} {
		var got []byte
		var gotErr error
		var contentLength int64
		var encoding string
		h := DecodeBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			contentLength, encoding = r.ContentLength, r.Header.Get("Content-Encoding")
			// One byte at a time, so the limit is reached by a read that
			// returns no error and the probe has to decide.
			got, gotErr = io.ReadAll(iotest.OneByteReader(r.Body))
		}))
		body := gz(t, payload)
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Content-Encoding", "gzip")
		h.ServeHTTP(httptest.NewRecorder(), req)

		// Handlers size buffers from ContentLength, and the compressed
		// length the request arrived with says nothing about the plain body.
		if contentLength != -1 || encoding != "" {
			t.Fatalf("limit %d: ContentLength=%d Content-Encoding=%q after gunzip", limit, contentLength, encoding)
		}
		if limit == int64(len(payload)) {
			if gotErr != nil || !bytes.Equal(got, payload) {
				t.Fatalf("body of exactly the limit: err=%v len=%d", gotErr, len(got))
			}
		} else if !IsTooLarge(gotErr) {
			t.Fatalf("body one byte over the limit: err=%v len=%d", gotErr, len(got))
		}
	}
}

func TestLogRecoversErrorPanic(t *testing.T) {
	h := Log(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(errors.New("boom"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
}

// ErrAbortHandler is net/http's way of dropping a connection on purpose; it
// has to reach the server rather than turn into a 500.
func TestLogRepanicsAbortHandler(t *testing.T) {
	h := Log(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		if err, _ := recover().(error); !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", err)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
}

func TestRetryAfter(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:                   "1",
		0:                              "1",
		time.Nanosecond:                "1",
		time.Second:                    "1",
		time.Second + time.Millisecond: "2",
		2 * time.Second:                "2",
		90 * time.Second:               "90",
	} {
		if got := RetryAfter(d); got != want {
			t.Errorf("RetryAfter(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("outer"), mw("middle"), mw("inner"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := strings.Join(order, ","); got != "outer,middle,inner,handler" {
		t.Fatalf("order %s", got)
	}
}
