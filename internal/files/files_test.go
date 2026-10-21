package files

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // test data checksum
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
)

type fakeFiles struct {
	repo.FileRepo
	blobs map[string]repo.Blob
}

func (f *fakeFiles) PutBlob(_ context.Context, b repo.Blob) error {
	f.blobs[b.MD5] = b
	return nil
}

func newService(t *testing.T) (*Service, *fakeFiles, storage.Backend) {
	t.Helper()
	b, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ff := &fakeFiles{blobs: map[string]repo.Blob{}}
	return NewService(b, ff, strings.Repeat("s", 32)), ff, b
}

func sum(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // test
	return hex.EncodeToString(s[:])
}

func TestReceiveAndOpen(t *testing.T) {
	s, ff, _ := newService(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("zotero"), 100_000)
	if err := s.Receive(ctx, bytes.NewReader(data), sum(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if ff.blobs[sum(data)].Size != int64(len(data)) {
		t.Fatal("blob not recorded")
	}
	r, info, err := s.Open(ctx, sum(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, data) || info.Size != int64(len(data)) {
		t.Fatal("content mismatch")
	}
	// Re-uploading identical content is fine (dedup).
	if err := s.Receive(ctx, bytes.NewReader(data), sum(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

func TestReceiveRejectsMismatches(t *testing.T) {
	s, ff, b := newService(t)
	ctx := context.Background()
	data := []byte("hello world")
	cases := map[string]struct {
		body []byte
		md5  string
		size int64
	}{
		"wrong md5":   {data, sum([]byte("other")), int64(len(data))},
		"too short":   {data[:5], sum(data), int64(len(data))},
		"too long":    {append(data, '!'), sum(data), int64(len(data))},
		"invalid md5": {data, "XYZ", int64(len(data))},
	}
	for name, c := range cases {
		if err := s.Receive(ctx, bytes.NewReader(c.body), c.md5, c.size); !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if len(ff.blobs) != 0 {
		t.Fatal("mismatched upload recorded")
	}
	n := 0
	_ = b.Walk(ctx, "", func(storage.Info) error { n++; return nil })
	if n != 0 {
		t.Fatalf("mismatched upload left %d objects", n)
	}
	if swept, _ := b.SweepStaging(ctx, 0); swept != 0 {
		t.Fatalf("staging not cleaned up: %d", swept)
	}
}

func TestDownloadTokens(t *testing.T) {
	s, _, _ := newService(t)
	now := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return now }
	md5hex := sum([]byte("x"))
	tok := s.SignDownload(42, "ABCD2345", md5hex)
	c, err := s.VerifyDownload(tok)
	if err != nil || c.LibraryID != 42 || c.ItemKey != "ABCD2345" || c.MD5 != md5hex {
		t.Fatalf("verify: %+v %v", c, err)
	}

	// Tampering with any part invalidates it.
	enc, sig, _ := strings.Cut(tok, ".")
	for _, bad := range []string{"", "x", enc, enc + ".", enc + "." + sig + "x", "A" + enc[1:] + "." + sig} {
		if _, err := s.VerifyDownload(bad); !errors.Is(err, ErrBadToken) {
			t.Errorf("%q accepted", bad)
		}
	}
	// A different secret does not verify.
	other := NewService(nil, nil, strings.Repeat("t", 32))
	other.now = s.now
	if _, err := other.VerifyDownload(tok); !errors.Is(err, ErrBadToken) {
		t.Fatal("foreign secret accepted")
	}
	// Expiry.
	now = now.Add(DownloadTTL)
	if _, err := s.VerifyDownload(tok); !errors.Is(err, ErrBadToken) {
		t.Fatal("expired token accepted")
	}
}

func TestQuotaMB(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 1: 1, 1 << 20: 1, 1<<20 + 1: 2} {
		if got := QuotaMB(in); got != want {
			t.Errorf("QuotaMB(%d)=%d want %d", in, got, want)
		}
	}
}

func FuzzVerifyDownload(f *testing.F) {
	s := NewService(nil, nil, strings.Repeat("s", 32))
	f.Add(s.SignDownload(1, "ABCD2345", sum([]byte("a"))))
	f.Add("..")
	f.Fuzz(func(t *testing.T, tok string) {
		_, _ = s.VerifyDownload(tok) // must never panic
	})
}
