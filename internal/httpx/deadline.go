package httpx

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// BodyDeadlines protects against clients that trickle request bodies to
// tie up connections. http.Server.ReadTimeout cannot be used because it
// would also cut off legitimate multi-hour uploads, so the policy is per
// route:
//   - ordinary requests must deliver their whole body within whole;
//   - file uploads may take as long as they need, but every read must make
//     progress within idle;
//   - the streaming websocket is left alone, because a deadline set before
//     the hijack would carry over into the long-lived connection.
func BodyDeadlines(whole, idle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/stream":
			case strings.HasPrefix(r.URL.Path, "/files/upload/"):
				rc := http.NewResponseController(w)
				_ = rc.SetReadDeadline(time.Now().Add(idle))
				r.Body = &idleBody{ReadCloser: r.Body, rc: rc, idle: idle}
			default:
				_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(whole))
			}
			next.ServeHTTP(w, r)
		})
	}
}

type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	// Extending on every read would cost a syscall per chunk; once a second
	// is plenty to keep a steadily progressing upload alive.
	if now := time.Now(); n > 0 && now.Sub(b.last) > time.Second {
		b.last = now
		_ = b.rc.SetReadDeadline(now.Add(b.idle))
	}
	return n, err
}

// DeadlineWriter keeps a rolling write deadline on a response while bytes
// keep flowing. The server has no WriteTimeout (downloads and the websocket
// are long-lived), so a client that stops reading would otherwise pin the
// handler, and any database cursor it holds, for as long as the socket stays
// open. Once a write stalls for idle, it fails and the handler can bail out.
type DeadlineWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
}

// NewDeadlineWriter arms the first deadline right away. Call Release when the
// handler is done writing: the connection is reused for the next request on
// a keep-alive connection and must not inherit a deadline that is about to
// expire.
func NewDeadlineWriter(rw http.ResponseWriter, idle time.Duration) *DeadlineWriter {
	d := &DeadlineWriter{w: rw, rc: http.NewResponseController(rw), idle: idle, last: time.Now()}
	_ = d.rc.SetWriteDeadline(d.last.Add(idle))
	return d
}

func (d *DeadlineWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	// Same throttle as idleBody: one syscall per second, not per chunk.
	if now := time.Now(); n > 0 && now.Sub(d.last) > time.Second {
		d.last = now
		_ = d.rc.SetWriteDeadline(now.Add(d.idle))
	}
	return n, err
}

// Release clears the deadline.
func (d *DeadlineWriter) Release() { _ = d.rc.SetWriteDeadline(time.Time{}) }
