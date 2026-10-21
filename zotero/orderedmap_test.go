package zotero

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

func mustParse(t testing.TB, s string) OrderedMap {
	t.Helper()
	var m OrderedMap
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return m
}

func TestOrderedMapRoundTrip(t *testing.T) {
	tests := []string{
		`{}`,
		`{"b":1,"a":2,"c":3}`,
		`{"z":{"y":1,"x":[1,2,{"w":null}]},"a":"s"}`,
		`{"n":12345678901234567890123,"f":1.0,"e":1E+2,"neg":-0.0}`,
		`{"s":"tab\there é \"q\" \\ /","u":" "}`,
		`{"":1}`,
		`{"a":true,"b":false,"c":null}`,
	}
	for _, in := range tests {
		m := mustParse(t, in)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		back := mustParse(t, string(out))
		if !reflect.DeepEqual(back.Keys(), m.Keys()) {
			t.Errorf("key order changed %s -> %s", in, out)
		}
		var a, b any
		if err := json.Unmarshal([]byte(in), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &b); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("semantics changed %s -> %s", in, out)
		}
	}
}

func TestOrderedMapKeepsNumbersExact(t *testing.T) {
	m := mustParse(t, `{"big":12345678901234567890123,"f":1.50}`)
	if got := m.AppendJSON(nil); string(got) != `{"big":12345678901234567890123,"f":1.50}` {
		t.Errorf("got %s", got)
	}
}

func TestOrderedMapOrderAndOps(t *testing.T) {
	m := mustParse(t, `{"c":1,"a":2,"b":3}`)
	if got := m.Keys(); !reflect.DeepEqual(got, []string{"c", "a", "b"}) {
		t.Errorf("keys %v", got)
	}
	m.Set("a", json.RawMessage("20"))
	m.Set("d", json.RawMessage("4"))
	if got := m.String(); got != `{"c":1,"a":20,"b":3,"d":4}` {
		t.Errorf("after set: %s", got)
	}
	if !m.Delete("c") || m.Delete("c") {
		t.Error("delete semantics")
	}
	if got := m.String(); got != `{"a":20,"b":3,"d":4}` {
		t.Errorf("after delete: %s", got)
	}
	if v, ok := m.Get("b"); !ok || string(v) != "3" {
		t.Errorf("get b: %s %v", v, ok)
	}
	if _, ok := m.Get("zz"); ok || m.Has("zz") {
		t.Error("zz should be absent")
	}
	if m.Len() != 3 {
		t.Errorf("len %d", m.Len())
	}
}

func TestOrderedMapDuplicateKeys(t *testing.T) {
	m := mustParse(t, `{"a":1,"b":2,"a":3}`)
	if got := m.String(); got != `{"a":3,"b":2}` {
		t.Errorf("got %s", got)
	}
}

func TestOrderedMapNullAndErrors(t *testing.T) {
	var m OrderedMap
	if err := json.Unmarshal([]byte(`null`), &m); err != nil || m.Len() != 0 {
		t.Errorf("null: %v %d", err, m.Len())
	}
	for _, bad := range []string{`[]`, `"x"`, `1`, `{"a":}`, `{"a":1`, `{`, ``} {
		var m OrderedMap
		if err := m.UnmarshalJSON([]byte(bad)); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if err := m.UnmarshalJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Error("trailing data should fail")
	}
}

func TestOrderedMapCompactsNested(t *testing.T) {
	m := mustParse(t, "{\"a\": [ 1,\n 2 ], \"b\": { \"x\" : 1 }}")
	if got := m.String(); got != `{"a":[1,2],"b":{"x":1}}` {
		t.Errorf("got %s", got)
	}
}

func TestOrderedMapNilValueMarshalsNull(t *testing.T) {
	var m OrderedMap
	m.Set("a", nil)
	if m.String() != `{"a":null}` {
		t.Errorf("got %s", m.String())
	}
}

func TestOrderedMapLargeUsesIndex(t *testing.T) {
	var m OrderedMap
	for i := range 100 {
		m.Set(fmt.Sprintf("k%d", i), json.RawMessage(fmt.Sprint(i)))
	}
	if m.idx == nil {
		t.Fatal("expected index for large map")
	}
	for i := range 100 {
		if v, ok := m.Get(fmt.Sprintf("k%d", i)); !ok || string(v) != fmt.Sprint(i) {
			t.Fatalf("k%d = %s", i, v)
		}
	}
	m.Delete("k50")
	if m.Has("k50") || !m.Has("k51") || m.Len() != 99 {
		t.Error("index stale after delete")
	}
	v, _ := m.Get("k99")
	if string(v) != "99" {
		t.Errorf("k99 = %s", v)
	}
	for i := 0; i < 90; i++ {
		m.Delete(fmt.Sprintf("k%d", i))
	}
	if m.idx != nil {
		t.Error("index should be dropped when small again")
	}
}

func TestOrderedMapClone(t *testing.T) {
	m := mustParse(t, `{"a":1}`)
	c := m.Clone()
	c.Set("a", json.RawMessage("2"))
	c.Set("b", json.RawMessage("3"))
	if m.String() != `{"a":1}` {
		t.Errorf("original modified: %s", m)
	}
	if c.String() != `{"a":2,"b":3}` {
		t.Errorf("clone: %s", c)
	}
}

func TestOrderedMapAppendJSONEscapes(t *testing.T) {
	var m OrderedMap
	m.Set("a\"b\n\x01é <>&", json.RawMessage("1"))
	out := m.AppendJSON(nil)
	if !json.Valid(out) {
		t.Fatalf("invalid: %s", out)
	}
	var back map[string]int
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["a\"b\n\x01é <>&"]; !ok {
		t.Errorf("key lost: %s", out)
	}
	// Invalid UTF-8 must not produce invalid JSON.
	var bad OrderedMap
	bad.Set("\xff", json.RawMessage("1"))
	if !json.Valid(bad.AppendJSON(nil)) {
		t.Error("invalid UTF-8 key produced invalid JSON")
	}
}

func TestOrderedMapInStruct(t *testing.T) {
	type wrap struct {
		D OrderedMap `json:"d"`
	}
	var w wrap
	if err := json.Unmarshal([]byte(`{"d":{"z":1,"a":2}}`), &w); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(w)
	if string(out) != `{"d":{"z":1,"a":2}}` {
		t.Errorf("got %s", out)
	}
}

func TestOrderedMapGetters(t *testing.T) {
	m := mustParse(t, `{"s":"hi\n","n":42,"x":1.5,"o":{}}`)
	if s, ok := m.GetString("s"); !ok || s != "hi\n" {
		t.Errorf("GetString: %q %v", s, ok)
	}
	if _, ok := m.GetString("n"); ok {
		t.Error("number is not a string")
	}
	if n, ok := m.GetInt("n"); !ok || n != 42 {
		t.Errorf("GetInt: %d %v", n, ok)
	}
	if _, ok := m.GetInt("x"); ok {
		t.Error("1.5 is not an int")
	}
	if err := m.SetValue("v", []int{1, 2}); err != nil || m.String() != `{"s":"hi\n","n":42,"x":1.5,"o":{},"v":[1,2]}` {
		t.Errorf("SetValue: %v %s", err, m)
	}
	n := 0
	m.Each(func(string, json.RawMessage) bool { n++; return n < 2 })
	if n != 2 {
		t.Errorf("Each visited %d", n)
	}
}

func FuzzOrderedMapRoundTrip(f *testing.F) {
	seeds := []string{`{}`, `{"b":1,"a":[1,2,{"c":null}]}`, `{"a":1,"a":2}`, `{"n":1e400,"s":"\ud800"}`, `{"a" : { }}`}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	if b, err := os.ReadFile("testdata/book.in.json"); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var m OrderedMap
		if err := json.Unmarshal(data, &m); err != nil {
			return
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m2 OrderedMap
		if err := json.Unmarshal(out, &m2); err != nil {
			t.Fatalf("reparse %s: %v", out, err)
		}
		if !reflect.DeepEqual(m.Keys(), m2.Keys()) {
			t.Fatalf("key order changed: %v -> %v", m.Keys(), m2.Keys())
		}
		for _, k := range m.Keys() {
			a, _ := m.Get(k)
			b, _ := m2.Get(k)
			if !EqualJSON(a, b) && !bytes.Equal(a, b) {
				t.Fatalf("value of %q changed: %s -> %s", k, a, b)
			}
		}
		// AppendJSON must agree with MarshalJSON modulo HTML escaping.
		if !json.Valid(m.AppendJSON(nil)) {
			t.Fatal("AppendJSON produced invalid JSON")
		}
	})
}

func benchDoc(b *testing.B) []byte {
	b.Helper()
	in, err := os.ReadFile("testdata/book.in.json")
	if err != nil {
		b.Fatal(err)
	}
	return in
}

func BenchmarkOrderedMapUnmarshal(b *testing.B) {
	in := benchDoc(b)
	b.ReportAllocs()
	b.SetBytes(int64(len(in)))
	for range b.N {
		var m OrderedMap
		if err := json.Unmarshal(in, &m); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOrderedMapMarshal(b *testing.B) {
	m := mustParse(b, string(benchDoc(b)))
	buf := make([]byte, 0, 2048)
	b.ReportAllocs()
	for range b.N {
		buf = m.AppendJSON(buf[:0])
	}
}
