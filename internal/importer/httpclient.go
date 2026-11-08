package importer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// No overall request timeout here: a large attachment can take many minutes
// to download. Each wait on zotero.org is bounded instead.
const (
	dialTimeout           = 30 * time.Second
	tlsHandshakeTimeout   = 30 * time.Second
	responseHeaderTimeout = 2 * time.Minute
	// bodyIdleTimeout is how long a single read of a response body may wait
	// for the next bytes.
	bodyIdleTimeout = 2 * time.Minute
)

// newHTTPClient builds the client for zotero.org and its file hosts. With
// http.DefaultClient a connection that stops answering blocks the worker,
// which handles one job at a time, forever.
func newHTTPClient(idle time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = tlsHandshakeTimeout
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: &idleTransport{next: tr, idle: idle}}
}

// idleTransport aborts a response whose body stops delivering data. It covers
// JSON listings and file downloads alike, which a whole-request timeout
// could not do without also limiting how big a file may be.
type idleTransport struct {
	next http.RoundTripper
	idle time.Duration
}

func (t *idleTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	resp, err := t.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel(nil)
		return nil, err
	}
	b := &idleBody{body: resp.Body, cancel: cancel, idle: t.idle}
	b.timer = time.AfterFunc(time.Hour, func() {
		b.fired.Store(true)
		cancel(errBodyStalled)
	})
	b.timer.Stop()
	resp.Body = b
	return resp, nil
}

var errBodyStalled = errors.New("importer: zotero.org stopped sending data")

// idleBody arms its timer only while a Read is in progress. Time the caller
// spends between reads, for example writing a chunk to slow storage, does
// not count against zotero.org.
type idleBody struct {
	body   io.ReadCloser
	cancel context.CancelCauseFunc
	idle   time.Duration
	timer  *time.Timer
	fired  atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	n, err := b.body.Read(p)
	b.timer.Stop()
	if err != nil && b.fired.Load() {
		err = fmt.Errorf("%w for %s", errBodyStalled, b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.body.Close()
	b.cancel(nil)
	return err
}
