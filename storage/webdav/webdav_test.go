package webdav_test

import (
	"flag"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/internal/containertest"
	"github.com/robertjndw/gotero/storage/storagetest"
	"github.com/robertjndw/gotero/storage/webdav"
)

var (
	serverAddr string
	skipReason string
)

func TestMain(m *testing.M) {
	flag.Parse()
	var srv *containertest.Server
	if !testing.Short() {
		var err error
		srv, err = containertest.StartRclone("8080",
			"serve", "webdav", "/data", "--addr", ":8080", "--user", "test", "--pass", "test")
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
		t.Skipf("no WebDAV test server (is Docker available?): %s", skipReason)
	}
	return serverAddr
}

// newBackend uses a fresh sub-collection per call so tests stay isolated.
func newBackend(t *testing.T, mod func(*webdav.Config)) *webdav.Backend {
	t.Helper()
	addr := server(t)
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	cfg := webdav.Config{URL: "http://" + addr + "/" + name + "/", Username: "test", Password: "test"}
	if mod != nil {
		mod(&cfg)
	}
	// rclone's WebDAV server creates the collection on demand via MKCOL.
	req, _ := http.NewRequestWithContext(t.Context(), "MKCOL", cfg.URL, nil)
	req.SetBasicAuth("test", "test")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	b, err := webdav.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestConformanceStreaming(t *testing.T) {
	// rclone only exposes a PUT once it has finished, so an abandoned
	// upload is invisible to the sweeper here; TestSweepOrphan covers it.
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend { return newBackend(t, nil) },
		storagetest.Options{SweepMayMissAbandoned: true})
}

func TestConformanceStageLocally(t *testing.T) {
	storagetest.RunWithOptions(t, func(t *testing.T) storage.Backend {
		return newBackend(t, func(c *webdav.Config) { c.StageLocally = true; c.TempDir = t.TempDir() })
	}, storagetest.Options{LargeSize: 8 << 20, SweepMayMissAbandoned: true})
}

func TestBadCredentials(t *testing.T) {
	b := newBackend(t, func(c *webdav.Config) { c.Password = "wrong" })
	_, err := b.Stat(t.Context(), "x/y")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Stat with bad credentials: %v", err)
	}
	if _, err := b.Create(t.Context()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("Create with bad credentials: %v", err)
	}
}

func TestMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test skipped in -short mode")
	}
	b := newBackend(t, nil)
	storagetest.HeapGrowth(t, b, 128<<20, 64<<20)
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "ftp://x/", "http://", "::bad"} {
		if _, err := webdav.New(webdav.Config{URL: u}); err == nil {
			t.Errorf("URL %q accepted", u)
		}
	}
}

func TestSweepOrphan(t *testing.T) {
	b := newBackend(t, nil)
	if err := storagetest.Put(t.Context(), b, "keep/me", []byte("committed")); err != nil {
		t.Fatal(err)
	}
	// What a crashed process leaves behind: a finished staging file whose
	// MOVE never happened. Written with plain HTTP, bypassing the backend.
	up, err := b.Create(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(t.Context()); err != nil { // make sure .staging/ exists
		t.Fatal(err)
	}
	addr := server(t)
	url := "http://" + addr + "/" + strings.ReplaceAll(t.Name(), "/", "_") + "/.staging/orphan"
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, url, strings.NewReader("junk"))
	req.SetBasicAuth("test", "test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT orphan: %s", resp.Status)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, err := b.SweepStaging(t.Context(), time.Hour); err != nil || n != 0 {
		t.Fatalf("fresh orphan swept: %d, %v", n, err)
	}
	if n, err := b.SweepStaging(t.Context(), 500*time.Millisecond); err != nil || n != 1 {
		t.Fatalf("SweepStaging = %d, %v; want 1", n, err)
	}
	if _, err := b.Stat(t.Context(), "keep/me"); err != nil {
		t.Fatal(err)
	}
}
