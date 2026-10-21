package local_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/storage/local"
	"github.com/robertjndw/gotero/storage/storagetest"
)

func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		b, err := local.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test skipped in -short mode")
	}
	b, err := local.New(t.TempDir(), local.WithNoSync())
	if err != nil {
		t.Fatal(err)
	}
	storagetest.HeapGrowth(t, b, 256<<20, 64<<20)
}

func TestPermissionsAndLayout(t *testing.T) {
	root := t.TempDir()
	b, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := storagetest.Put(t.Context(), b, "ab/abcdef", []byte("x")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "ab", "abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o640 {
		t.Errorf("file mode %o, want 640", got)
	}
	st, err = os.Stat(filepath.Join(root, "ab"))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o750 {
		t.Errorf("dir mode %o, want 750", got)
	}
}

func TestWalkIgnoresStaging(t *testing.T) {
	b, err := local.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up, err := b.Create(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Abort(t.Context()) }()
	_, _ = up.Write([]byte("pending"))
	err = b.Walk(t.Context(), "", func(i storage.Info) error {
		t.Errorf("unexpected object %q", i.Key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
