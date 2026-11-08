package webdav_test

import (
	"os"
	"testing"

	"github.com/robertjndw/gotero/storage/webdav"
)

func TestStageLocallyCommitRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	b := newBackend(t, func(c *webdav.Config) { c.StageLocally = true; c.TempDir = dir })
	// The second commit hits the 412 dedup branch.
	for _, key := range []string{"ab/cd/first", "ab/cd/first"} {
		u, err := b.Create(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := u.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
		if err := u.Commit(t.Context(), key); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("temp dir not empty after Commit: %d entries", len(entries))
		}
	}
}
