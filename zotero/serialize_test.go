package zotero

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertjndw/gotero/zotero/schema"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestMarshalItemDataGolden(t *testing.T) {
	cases := []struct {
		name    string
		version int
	}{
		{"book", 0},
		{"journal", 0},
		{"attachment", 0},
		{"note", 0},
		{"annotation", 0},
		{"unknown", 0},
		{"versioned", 0},
		{"versioned", 32},
	}
	for _, tc := range cases {
		name := tc.name
		if tc.version > 0 {
			name += ".v" + itoa(tc.version)
		}
		t.Run(name, func(t *testing.T) {
			in, err := os.ReadFile(filepath.Join("testdata", tc.name+".in.json"))
			if err != nil {
				t.Fatal(err)
			}
			var m OrderedMap
			if err := json.Unmarshal(in, &m); err != nil {
				t.Fatal(err)
			}
			got, err := MarshalItemData(m, schema.Default(), tc.version)
			if err != nil {
				t.Fatal(err)
			}
			if !json.Valid(got) {
				t.Fatalf("invalid JSON: %s", got)
			}
			var pretty bytes.Buffer
			// One key per line keeps golden diffs reviewable and makes the
			// order the thing under test.
			pretty.WriteString("{\n")
			var out OrderedMap
			if err := json.Unmarshal(got, &out); err != nil {
				t.Fatal(err)
			}
			for i, k := range out.Keys() {
				v, _ := out.Get(k)
				pretty.WriteString("  " + string(appendString(nil, k)) + ": " + string(v))
				if i < out.Len()-1 {
					pretty.WriteString(",")
				}
				pretty.WriteString("\n")
			}
			pretty.WriteString("}\n")

			path := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(pretty.Bytes(), want) {
				t.Errorf("mismatch\n got:\n%s\nwant:\n%s", pretty.Bytes(), want)
			}
			// Nothing may be dropped, apart from schema-filtered fields.
			if tc.version == 0 && out.Len() != m.Len() {
				t.Errorf("key count %d, want %d", out.Len(), m.Len())
			}
		})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestMarshalItemDataErrors(t *testing.T) {
	var m OrderedMap
	if _, err := MarshalItemData(m, nil, 0); !errors.Is(err, ErrNoItemType) {
		t.Errorf("empty: %v", err)
	}
	m.Set("itemType", json.RawMessage("5"))
	if _, err := MarshalItemData(m, nil, 0); !errors.Is(err, ErrNoItemType) {
		t.Errorf("non-string: %v", err)
	}
}

func TestMarshalItemDataUnknownType(t *testing.T) {
	var m OrderedMap
	if err := json.Unmarshal([]byte(`{"zz":1,"itemType":"hologram","creators":[],"key":"ABCD2345","tags":[]}`), &m); err != nil {
		t.Fatal(err)
	}
	got, err := MarshalItemData(m, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"key":"ABCD2345","itemType":"hologram","creators":[],"tags":[],"zz":1}`
	if string(got) != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
}

func TestMarshalItemDataVersionFilterKeepsCore(t *testing.T) {
	// Even with an absurdly old client, nothing but schema fields is dropped.
	var m OrderedMap
	if err := json.Unmarshal([]byte(`{"key":"ABCD2345","version":1,"itemType":"book","title":"T","DOI":"x","tags":[],"dateAdded":"d","dateModified":"d"}`), &m); err != nil {
		t.Fatal(err)
	}
	got, err := MarshalItemData(m, schema.Default(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "DOI") {
		t.Errorf("DOI should be filtered: %s", got)
	}
	for _, k := range []string{`"key"`, `"version"`, `"itemType"`, `"title"`, `"tags"`, `"dateAdded"`, `"dateModified"`} {
		if !strings.Contains(string(got), k) {
			t.Errorf("missing %s in %s", k, got)
		}
	}
}

func TestMarshalItemDataManyKeys(t *testing.T) {
	// More than 256 unknown keys exercises the heap-allocated seen bitset.
	var m OrderedMap
	m.Set("itemType", json.RawMessage(`"note"`))
	for i := range 300 {
		m.Set("x"+itoa(i), json.RawMessage(itoa(i)))
	}
	got, err := MarshalItemData(m, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var back OrderedMap
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back.Len() != 301 {
		t.Errorf("len %d", back.Len())
	}
}

func benchItem(b *testing.B) OrderedMap {
	b.Helper()
	in, err := os.ReadFile("testdata/book.in.json")
	if err != nil {
		b.Fatal(err)
	}
	var m OrderedMap
	if err := json.Unmarshal(in, &m); err != nil {
		b.Fatal(err)
	}
	return m
}

func BenchmarkMarshalItemData(b *testing.B) {
	m := benchItem(b)
	s := schema.Default()
	buf := make([]byte, 0, 2048)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var err error
		buf, err = AppendItemData(buf[:0], m, s, 33)
		if err != nil {
			b.Fatal(err)
		}
	}
}
