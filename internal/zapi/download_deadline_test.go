package zapi_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
)

// closeSignalBackend reports when a download lets go of its reader, and can
// hide io.Seeker to exercise the plain copy path.
type closeSignalBackend struct {
	storage.Backend
	seekable bool
	closed   chan struct{}
}

type signalReader struct {
	io.Reader
	closed chan struct{}
}

func (r signalReader) Close() error { close(r.closed); return nil }

type signalReadSeeker struct {
	io.ReadSeeker
	closed chan struct{}
}

func (r signalReadSeeker) Close() error { close(r.closed); return nil }

func (b *closeSignalBackend) Open(ctx context.Context, key string) (storage.Reader, storage.Info, error) {
	rd, info, err := b.Backend.Open(ctx, key)
	if err != nil {
		return nil, info, err
	}
	rs, ok := rd.(io.ReadSeeker)
	if b.seekable && ok {
		return signalReadSeeker{ReadSeeker: rs, closed: b.closed}, info, nil
	}
	return signalReader{Reader: rd, closed: b.closed}, info, nil
}

// A client that stops reading a download must not pin the handler and the
// backend reader forever.
func TestDownloadCutsOffStalledReader(t *testing.T) {
	for name, seekable := range map[string]bool{"serve content": true, "plain copy": false} {
		t.Run(name, func(t *testing.T) {
			be, err := local.New(t.TempDir())
			require.NoError(t, err)
			sb := &closeSignalBackend{Backend: be, seekable: seekable, closed: make(chan struct{})}
			e := newEnv(t, withDeps(func(d *zapi.Deps) {
				d.Files = files.NewService(sb, d.Store.Files(), "0123456789abcdef0123456789abcdef")
				d.WriteIdleTimeout = 200 * time.Millisecond
			}))
			u := e.newUser(t, "alice", fullAccess)
			l := e.userLib(t, u)
			key := e.attachment(t, l, "big.pdf")
			// Far more than the socket buffers on either side can absorb.
			p := newPayload("big.pdf", bytes.Repeat([]byte("0123456789abcdef"), 4<<20))
			e.store(t, l, key, p)

			r := l.get("items/" + key + "/file")
			require.Equal(t, 302, r.Status)
			loc, err := url.Parse(r.Header.Get("Location"))
			require.NoError(t, err)

			conn, err := net.Dial("tcp", loc.Host)
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			_, err = io.WriteString(conn, "GET "+loc.RequestURI()+" HTTP/1.1\r\nHost: "+loc.Host+"\r\n\r\n")
			require.NoError(t, err)

			select {
			case <-sb.closed:
			case <-time.After(10 * time.Second):
				t.Fatal("download to a client that stopped reading never timed out")
			}
		})
	}
}
