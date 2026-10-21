// Package httpx holds HTTP middleware shared by the Zotero API, the admin
// API and the web UI.
package httpx

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type ctxKey int

const (
	publicURLKey ctxKey = iota
	clientIPKey
)

// ClientIP returns the caller's address as resolved by ResolvePublicURL:
// the direct peer, or the first untrusted hop of X-Forwarded-For when the
// peer is a trusted proxy. Rate limits key on this; behind a proxy the raw
// peer address would put every user into one bucket.
func ClientIP(ctx context.Context) string {
	s, _ := ctx.Value(clientIPKey).(string)
	return s
}

// RetryAfter formats a wait for the Retry-After header: whole seconds,
// rounded up, at least 1, because clients treat 0 as "retry immediately".
func RetryAfter(d time.Duration) string {
	return strconv.FormatInt(max(1, int64((d+time.Second-1)/time.Second)), 10)
}

// BaseURL returns the externally visible base URL (scheme://host, no
// trailing slash) resolved by the ResolvePublicURL middleware.
func BaseURL(ctx context.Context) string {
	s, _ := ctx.Value(publicURLKey).(string)
	return s
}

// statusWriter records the status and size for access logs. It forwards
// Flush and Unwrap so streaming responses and http.ResponseController keep
// working.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack is needed for the streaming websocket. gin type-asserts
// http.Hijacker directly instead of using http.ResponseController, so
// Unwrap alone is not enough.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("httpx: connection does not support hijacking")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return h.Hijack()
}

// Log assigns a request ID, recovers panics and writes one access log line
// per request. Only the path is logged, never the query string (which may
// carry ?key=), and path segments that are credentials are redacted.
func Log(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := newRequestID()
			ctx := r.Context()
			sw := &statusWriter{ResponseWriter: w}
			sw.Header().Set("X-Request-Id", id)
			defer func() {
				if v := recover(); v != nil {
					if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
						panic(v)
					}
					log.ErrorContext(ctx, "panic", "request_id", id, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
					if sw.status == 0 {
						http.Error(sw, "Internal server error", http.StatusInternalServerError)
					}
				}
				level := slog.LevelInfo
				switch {
				case sw.status >= 500:
					level = slog.LevelError
				case r.URL.Path == "/healthz":
					level = slog.LevelDebug
				}
				log.Log(ctx, level, "request",
					"request_id", id,
					"method", r.Method,
					"path", RedactPath(r.URL.Path),
					"status", sw.status,
					"bytes", sw.bytes,
					"duration_ms", time.Since(start).Milliseconds(),
					"remote", r.RemoteAddr,
				)
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// secretAfter maps path prefixes to the credential segment that follows.
// Keys and tokens in these positions grant access on their own (a login
// session token yields a full API key), so they must not reach log storage.
var secretAfter = []struct{ prefix, keep string }{
	{"/api/keys/sessions/", ""},
	{"/api/keys/", "current"},
	{"/files/download/", ""},
	{"/files/upload/", ""},
	{"/admin/api/login-sessions/", ""},
	{"/admin/api/invites/", ""},
}

// RedactPath replaces credential path segments with a placeholder.
func RedactPath(p string) string {
	for _, s := range secretAfter {
		rest, ok := strings.CutPrefix(p, s.prefix)
		if !ok || rest == "" {
			continue
		}
		seg, tail, _ := strings.Cut(rest, "/")
		if seg == s.keep || (s.prefix == "/api/keys/" && seg == "sessions") {
			continue
		}
		if tail != "" {
			tail = "/" + tail
		}
		return s.prefix + "REDACTED" + tail
	}
	return p
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ResolvePublicURL resolves the externally visible base URL for each request.
// A configured URL always wins. Otherwise X-Forwarded-Proto/-Host are only
// honoured when the direct peer is a trusted proxy, because these values end
// up in absolute URLs we hand to clients (upload targets, download redirects,
// login pages) and must not be attacker controlled.
func ResolvePublicURL(configured string, trusted []netip.Prefix) func(http.Handler) http.Handler {
	configured = strings.TrimRight(configured, "/")
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			base := configured
			if base == "" {
				base = derivePublicURL(r, trusted)
			}
			ctx := context.WithValue(r.Context(), publicURLKey, base)
			ctx = context.WithValue(ctx, clientIPKey, clientIP(r, trusted))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func derivePublicURL(r *http.Request, trusted []netip.Prefix) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if isTrusted(r.RemoteAddr, trusted) {
		if p := firstValue(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
			scheme = p
		}
		if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" && validHost(h) {
			host = h
		}
	}
	return scheme + "://" + host
}

func clientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !isTrusted(r.RemoteAddr, trusted) {
		return peer
	}
	// Walk from the right: the rightmost entries were appended by our own
	// proxies, everything left of the first untrusted hop is client-supplied
	// and can be forged.
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		a, err := netip.ParseAddr(h)
		if err != nil {
			break
		}
		if !isTrusted(h, trusted) {
			return a.Unmap().String()
		}
	}
	return peer
}

func firstValue(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// validHost rejects anything that could smuggle a path or userinfo into a
// URL we build from it.
func validHost(h string) bool {
	return h != "" && !strings.ContainsAny(h, "/\\@?# \t\r\n")
}

func isTrusted(remoteAddr string, trusted []netip.Prefix) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// ParsePrefixes turns config entries (IPs or CIDRs) into prefixes.
func ParsePrefixes(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q", e)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// ErrBodyTooLarge is returned by bodies wrapped with LimitBody once the
// limit is exceeded.
var ErrBodyTooLarge = errors.New("request body too large")

// DecodeBody wraps r.Body so handlers always read plain bytes: gzip bodies
// (Zotero compresses anything over 1000 bytes) are decompressed, and both
// the compressed and the decompressed size are capped to defuse gzip bombs.
// Handlers that stream files bypass this by not being wrapped.
func DecodeBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			raw := http.MaxBytesReader(w, r.Body, maxBytes)
			switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
			case "", "identity":
				r.Body = raw
			case "gzip":
				zr, err := gzip.NewReader(raw)
				if err != nil {
					http.Error(w, "Invalid gzip request body", http.StatusBadRequest)
					return
				}
				r.Body = &limitedBody{r: zr, closer: raw, remaining: maxBytes}
				r.Header.Del("Content-Encoding")
				r.ContentLength = -1
			default:
				http.Error(w, "Unsupported Content-Encoding", http.StatusUnsupportedMediaType)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type limitedBody struct {
	r         io.Reader
	closer    io.Closer
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// Probe one byte so a body of exactly the limit is still accepted.
		var one [1]byte
		if n, _ := b.r.Read(one[:]); n > 0 {
			return 0, ErrBodyTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.closer.Close() }

// IsTooLarge reports whether err came from a size limit, so handlers can
// answer 413 instead of 400.
func IsTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.Is(err, ErrBodyTooLarge) || errors.As(err, &mbe)
}

// Chain applies middleware so that the first one listed runs outermost.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
