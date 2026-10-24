package httpx

import (
	"errors"
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
//
// The deadline is cleared once the body is fully read. After that net/http
// keeps a background read going to notice clients hanging up, and if that
// read hits our deadline it cancels the request context, killing long
// downloads and listings. Requests without a body get no deadline at all;
// ReadHeaderTimeout covers them.
func BodyDeadlines(whole, idle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/stream":
			case r.Body == nil || r.Body == http.NoBody:
			case strings.HasPrefix(r.URL.Path, "/files/upload/"):
				rc := http.NewResponseController(w)
				_ = rc.SetReadDeadline(time.Now().Add(idle))
				r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc, idle: idle}
			default:
				rc := http.NewResponseController(w)
				_ = rc.SetReadDeadline(time.Now().Add(whole))
				r.Body = &deadlineBody{ReadCloser: r.Body, rc: rc}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// deadlineBody clears the read deadline at EOF. With idle set it also keeps
// pushing the deadline out while the body makes progress.
type deadlineBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	last time.Time
	done bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		if !b.done {
			b.done = true
			_ = b.rc.SetReadDeadline(time.Time{})
		}
		return n, err
	}
	// Extending on every read would cost a syscall per chunk; once a second
	// is plenty to keep a steadily progressing upload alive.
	if now := time.Now(); b.idle > 0 && n > 0 && now.Sub(b.last) > time.Second {
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
	// Extend before the write. If we just spent a minute waiting on a slow
	// query, the old deadline is already gone and the write would fail even
	// though the client is fine. Same throttle as deadlineBody: one syscall per
	// second, not per chunk.
	if now := time.Now(); now.Sub(d.last) > time.Second {
		d.last = now
		_ = d.rc.SetWriteDeadline(now.Add(d.idle))
	}
	return d.w.Write(p)
}

// Release clears the deadline.
func (d *DeadlineWriter) Release() { _ = d.rc.SetWriteDeadline(time.Time{}) }
