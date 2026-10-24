package conformance

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"testing"
)

// KeyAlphabet is the character set of Zotero object keys.
const KeyAlphabet = "23456789ABCDEFGHIJKLMNPQRSTUVWXYZ"

var keyRe = regexp.MustCompile(`^[23456789ABCDEFGHIJKLMNPQRSTUVWXYZ]{8}$`)

// newKey returns a random object key in the Zotero alphabet.
func newKey() string {
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(KeyAlphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = KeyAlphabet[n.Int64()]
	}
	return string(b)
}

func newKeys(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = newKey()
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

type obj = map[string]any

func parseObj(t testing.TB, r *response) obj {
	t.Helper()
	var o obj
	if err := json.Unmarshal(r.Body, &o); err != nil {
		t.Fatalf("response is not a JSON object (%v): %s", err, r)
	}
	return o
}

func parseArr(t testing.TB, r *response) []any {
	t.Helper()
	var a []any
	if err := json.Unmarshal(r.Body, &a); err != nil {
		t.Fatalf("response is not a JSON array (%v): %s", err, r)
	}
	return a
}

// at walks nested objects and asserts the leaf has type T.
func at[T any](t testing.TB, o obj, path ...string) T {
	t.Helper()
	var cur any = o
	for _, p := range path {
		m, ok := cur.(obj)
		if !ok {
			t.Fatalf("path %v: %q parent is not an object", path, p)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("path %v: missing %q in %v", path, p, m)
		}
	}
	v, ok := cur.(T)
	if !ok {
		var zero T
		t.Fatalf("path %v: got %T (%v), want %T", path, cur, cur, zero)
	}
	return v
}

func atInt(t testing.TB, o obj, path ...string) int64 {
	t.Helper()
	return int64(at[float64](t, o, path...))
}

func has(o obj, k string) bool { _, ok := o[k]; return ok }

func bookItem(title string) obj {
	return obj{"itemType": "book", "title": title}
}

func attachmentItem(filename string) obj {
	return obj{
		"itemType":    "attachment",
		"linkMode":    "imported_file",
		"title":       filename,
		"contentType": "application/pdf",
		"filename":    filename,
	}
}
