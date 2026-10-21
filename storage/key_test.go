package storage_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/robertjndw/gotero/storage"
)

func TestValidateKey(t *testing.T) {
	good := []string{"a", "ab/abcdef0123", "a/b/c", "x.y/z..w", "with space/ok", "ü/日"}
	bad := []string{"", "/a", "a/", "a//b", "../a", "a/..", "a/./b", ".", "..", "a\\b", ".staging", ".staging/x", ".stagingfoo", "a\x00b"}
	for _, k := range good {
		if err := storage.ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v", k, err)
		}
	}
	for _, k := range bad {
		if err := storage.ValidateKey(k); !errors.Is(err, storage.ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", k, err)
		}
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, p := range []string{"", "ab", "ab/", "ab/abc"} {
		if err := storage.ValidatePrefix(p); err != nil {
			t.Errorf("ValidatePrefix(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"/", "../x", ".staging/", "a/../b", "/abs"} {
		if err := storage.ValidatePrefix(p); err == nil {
			t.Errorf("ValidatePrefix(%q) accepted", p)
		}
	}
}

func FuzzValidateKey(f *testing.F) {
	for _, s := range []string{"", "a/b", "../x", "a//b", ".staging/x", "a\\b", "/abs", "ab/abcdef"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		err := storage.ValidateKey(key)
		if err != nil {
			if !errors.Is(err, storage.ErrInvalidKey) {
				t.Fatalf("error not ErrInvalidKey: %v", err)
			}
			return
		}
		// Anything accepted must be safe to turn into a relative path.
		if strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.HasPrefix(key, storage.StagingDir) {
			t.Fatalf("accepted dangerous key %q", key)
		}
		for _, seg := range strings.Split(key, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("accepted key %q with segment %q", key, seg)
			}
		}
	})
}
