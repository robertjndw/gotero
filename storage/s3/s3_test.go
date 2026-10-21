package s3_test

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/internal/containertest"
	"github.com/robertjndw/gotero/storage/s3"
	"github.com/robertjndw/gotero/storage/storagetest"
)

// serverAddr is set by TestMain when Docker is usable.
var (
	serverAddr string
	skipReason string
)

func TestMain(m *testing.M) {
	flag.Parse()
	var srv *containertest.Server
	if !testing.Short() {
		var err error
		srv, err = containertest.StartVersityGW("test", "testsecret")
		if err != nil {
			skipReason = err.Error()
		} else {
			serverAddr = srv.Addr
		}
	}
	code := m.Run()
	srv.Close() // os.Exit skips defers
	os.Exit(code)
}

func server(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("container tests skipped in -short mode")
	}
	if serverAddr == "" {
		t.Skipf("no S3 test server (is Docker available?): %s", skipReason)
	}
	return serverAddr
}

func newBackend(t *testing.T, mod func(*s3.Config)) *s3.Backend {
	t.Helper()
	addr := server(t)
	var id [6]byte
	_, _ = rand.Read(id[:])
	cfg := s3.Config{
		Endpoint:     addr,
		Bucket:       "t-" + hex.EncodeToString(id[:]),
		AccessKey:    "test",
		SecretKey:    "testsecret",
		PartSize:     s3.MinPartSize,
		CreateBucket: true,
	}
	if mod != nil {
		mod(&cfg)
	}
	b, err := s3.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformance(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		return newBackend(t, nil)
	}, storagetest.Options{SweepNeedsBytes: s3.MinPartSize + 1<<20})
}

func TestConformanceWithPrefix(t *testing.T) {
	// One shared bucket, isolated by prefix, as a real deployment would be.
	bucket := ""
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		return newBackend(t, func(c *s3.Config) {
			if bucket == "" {
				bucket = c.Bucket
			}
			c.Bucket = bucket
			c.Prefix = "/some/prefix/" + strings.ReplaceAll(t.Name(), "/", "_")
		})
	}, storagetest.Options{SweepNeedsBytes: s3.MinPartSize + 1<<20, LargeSize: 12 << 20})
}

func TestMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test skipped in -short mode")
	}
	b := newBackend(t, func(c *s3.Config) { c.PartSize = s3.DefaultPartSize })
	storagetest.HeapGrowth(t, b, 256<<20, 64<<20)
}

func TestPartSizeValidation(t *testing.T) {
	if _, err := s3.New(t.Context(), s3.Config{Endpoint: "localhost:1", Bucket: "b", PartSize: 1024}); err == nil {
		t.Fatal("tiny part size accepted")
	}
	if _, err := s3.New(t.Context(), s3.Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
}

func TestSweepRemovesOrphanedStagingObject(t *testing.T) {
	b := newBackend(t, nil)
	if err := storagetest.Put(t.Context(), b, "keep/me", []byte("committed")); err != nil {
		t.Fatal(err)
	}
	// What a crashed process leaves behind: a finished staging object that
	// was never copied into place.
	if err := s3.PutOrphan(t.Context(), b, "orphan", []byte("junk")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := b.SweepStaging(t.Context(), time.Hour); err != nil || n != 0 {
		t.Fatalf("fresh orphan swept: %d, %v", n, err)
	}
	n, err := b.SweepStaging(t.Context(), 500*time.Millisecond)
	if err != nil || n != 1 {
		t.Fatalf("SweepStaging = %d, %v; want 1", n, err)
	}
	if _, err := b.Stat(t.Context(), "keep/me"); err != nil {
		t.Fatalf("committed object lost: %v", err)
	}
}
