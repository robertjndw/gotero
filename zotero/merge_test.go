package zotero

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEqualJSON(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{`1`, `1`, true},
		{`1`, `1.0`, true},
		{`1`, `1.00e0`, true},
		{`10`, `1e1`, true},
		{`0.1`, `1e-1`, true},
		{`100`, `1E2`, true},
		{`-0`, `0`, true},
		{`0.0`, `0`, true},
		{`1`, `2`, false},
		{`1`, `-1`, false},
		{`12345678901234567890123`, `12345678901234567890124`, false},
		{`12345678901234567890123`, `1.2345678901234567890123e22`, true},
		{`"a"`, `"a "`, false},
		{`"a"`, `"a"`, true},
		{`{"a":1,"b":2}`, `{ "b" : 2, "a" : 1 }`, true},
		{`{"a":1}`, `{"a":1,"b":null}`, false},
		{`[1,2]`, `[2,1]`, false},
		{`[{"x":1,"y":2}]`, `[{"y":2,"x":1.0}]`, true},
		{`null`, `0`, false},
		{`true`, `1`, false},
		{`""`, `null`, false},
		{`{}`, `[]`, false},
		{`1e999999999999`, `1e999999999999`, true},
		{`1e999999999999`, `1e999999999998`, false},
		{`not json`, `not json`, true},
		{`not json`, `not  json`, false},
		{`1 2`, `1 2`, true},
	}
	for _, tc := range tests {
		if got := EqualJSON(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
			t.Errorf("EqualJSON(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := EqualJSON(json.RawMessage(tc.b), json.RawMessage(tc.a)); got != tc.want {
			t.Errorf("EqualJSON(%s, %s) = %v, want %v (swapped)", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name        string
		stored      string
		patch       string
		want        string
		wantChanged bool
	}{
		{
			name:   "identical",
			stored: `{"key":"ABCD2345","version":5,"title":"T","n":1}`,
			patch:  `{"key":"ABCD2345","version":5,"title":"T","n":1}`,
			want:   `{"key":"ABCD2345","version":5,"title":"T","n":1}`,
		},
		{
			name:   "key and version only differ",
			stored: `{"key":"ABCD2345","version":5,"title":"T"}`,
			patch:  `{"key":"EFGH6789","version":9}`,
			want:   `{"key":"ABCD2345","version":5,"title":"T"}`,
		},
		{
			name:        "replace one field",
			stored:      `{"key":"ABCD2345","version":5,"title":"T","extra":"e"}`,
			patch:       `{"version":5,"title":"New"}`,
			want:        `{"key":"ABCD2345","version":5,"title":"New","extra":"e"}`,
			wantChanged: true,
		},
		{
			name:        "new field appended",
			stored:      `{"a":1}`,
			patch:       `{"b":2}`,
			want:        `{"a":1,"b":2}`,
			wantChanged: true,
		},
		{
			name:   "absent fields kept",
			stored: `{"a":1,"b":2,"c":3}`,
			patch:  `{"b":2}`,
			want:   `{"a":1,"b":2,"c":3}`,
		},
		{
			name:   "nested whitespace and key order",
			stored: `{"relations":{"x":["u1"],"y":"v"},"n":1}`,
			patch:  `{"relations":{ "y":"v", "x":[ "u1" ] },"n":1.0}`,
			want:   `{"relations":{"x":["u1"],"y":"v"},"n":1}`,
		},
		{
			name:        "nested value differs",
			stored:      `{"relations":{"x":["u1"]}}`,
			patch:       `{"relations":{"x":["u2"]}}`,
			want:        `{"relations":{"x":["u2"]}}`,
			wantChanged: true,
		},
		{
			name:        "set to null",
			stored:      `{"a":"x"}`,
			patch:       `{"a":null}`,
			want:        `{"a":null}`,
			wantChanged: true,
		},
		{
			name:        "empty stored",
			stored:      `{}`,
			patch:       `{"key":"ABCD2345","itemType":"note"}`,
			want:        `{"itemType":"note"}`,
			wantChanged: true,
		},
		{
			name:   "empty patch",
			stored: `{"a":1}`,
			patch:  `{}`,
			want:   `{"a":1}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stored, patch := mustParse(t, tc.stored), mustParse(t, tc.patch)
			before := stored.String()
			got, changed := Merge(stored, patch)
			if got.String() != tc.want {
				t.Errorf("merged %s, want %s", got, tc.want)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if stored.String() != before {
				t.Errorf("stored mutated: %s", stored)
			}
		})
	}
}

func TestCanonicalHash(t *testing.T) {
	a := mustParse(t, `{"key":"AAAAAAAA","version":1,"title":"T","rel":{"x":1,"y":[1.0,2]},"dateModified":"d"}`)
	b := mustParse(t, `{"dateModified":"d","rel":{ "y":[1,2], "x":1 },"title":"T","key":"BBBBBBBB","version":99}`)
	if CanonicalHash(a) != CanonicalHash(b) {
		t.Error("equivalent objects hash differently")
	}
	c := mustParse(t, `{"key":"AAAAAAAA","version":1,"title":"T","rel":{"x":1,"y":[1.0,2]},"dateModified":"e"}`)
	if CanonicalHash(a) == CanonicalHash(c) {
		t.Error("dateModified must be part of the hash")
	}
	d := mustParse(t, `{"title":"T","rel":{"x":1,"y":[1.0,2]},"dateModified":"d","extra":null}`)
	if CanonicalHash(a) == CanonicalHash(d) {
		t.Error("extra field must change the hash")
	}
	// A key named like another's value must not be confusable.
	e1 := mustParse(t, `{"a":"b"}`)
	e2 := mustParse(t, `{"a":"b","c":"d"}`)
	if CanonicalHash(e1) == CanonicalHash(e2) {
		t.Error("collision")
	}
	var empty OrderedMap
	_ = CanonicalHash(empty)
}

func TestMergeAgreesWithHash(t *testing.T) {
	stored := mustParse(t, `{"key":"ABCD2345","version":3,"title":"T","n":1}`)
	patch := mustParse(t, `{"version":3,"n":1.0}`)
	merged, changed := Merge(stored, patch)
	if changed || CanonicalHash(merged) != CanonicalHash(stored) {
		t.Error("unchanged merge must keep the hash")
	}
}

func FuzzMerge(f *testing.F) {
	f.Add([]byte(`{"a":1,"b":{"x":1}}`), []byte(`{"b":{"x":1.0},"c":2}`))
	f.Add([]byte(`{}`), []byte(`{"key":"x","version":1}`))
	f.Add([]byte(`{"a":[1,2]}`), []byte(`{"a":[2,1]}`))
	f.Fuzz(func(t *testing.T, s, p []byte) {
		var stored, patch OrderedMap
		if json.Unmarshal(s, &stored) != nil || json.Unmarshal(p, &patch) != nil {
			return
		}
		merged, changed := Merge(stored, patch)

		// Merging the same patch again must be a no-op.
		again, changed2 := Merge(merged, patch)
		if changed2 {
			t.Fatalf("second merge reported a change: %s + %s", merged, patch)
		}
		if CanonicalHash(again) != CanonicalHash(merged) {
			t.Fatal("hash moved on a no-op merge")
		}
		if !changed && CanonicalHash(merged) != CanonicalHash(stored) {
			t.Fatalf("unchanged but hash differs: %s vs %s", merged, stored)
		}
		// Stored keys keep their positions.
		sk, mk := stored.Keys(), merged.Keys()
		for i, k := range sk {
			if mk[i] != k {
				t.Fatalf("key order changed: %v -> %v", sk, mk)
			}
		}
		// Merging an object with itself never changes anything.
		if _, ch := Merge(stored, stored); ch {
			t.Fatal("self-merge reported a change")
		}
	})
}

// An unchanged field must not stop Merge from looking at the ones after it.
func TestMergeAfterUnchangedField(t *testing.T) {
	stored := mustMap(t, `{"title":"same","date":"old"}`)
	patch := mustMap(t, `{"key":"AAAAAAA2","version":3,"title":"same","date":"new"}`)
	merged, changed := Merge(stored, patch)
	if !changed {
		t.Error("changed = false")
	}
	if got, _ := merged.GetString("date"); got != "new" {
		t.Errorf("date = %q, want new (merged %s)", got, merged)
	}
}

func TestEqualJSONNumberScale(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"10", "0.1", false},
		{"100", "1", false},
		{"1000", "0.001", false},
		{"10", "1e1", true},
		{"100", "1.00e2", true},
		{"0.10", "1e-1", true},
	} {
		if got := EqualJSON(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
			t.Errorf("EqualJSON(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Exponents beyond +-2^40 are compared as written instead of being
// normalized, and the sign must survive that fallback.
func TestEqualJSONAbsurdExponent(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1e1099511627776", "1.0e1099511627776", true},
		{"1e-1099511627776", "1.0e-1099511627776", true},
		{"1e1099511627777", "1.0e1099511627777", false},
		{"1e-1099511627777", "1.0e-1099511627777", false},
		{"-1e1099511627777", "1e1099511627777", false},
		{"-1e99999999999999999999", "1e99999999999999999999", false},
		{" -1e1099511627777", "-1e1099511627777", true},
	} {
		if got := EqualJSON(json.RawMessage(tc.a), json.RawMessage(tc.b)); got != tc.want {
			t.Errorf("EqualJSON(%s, %s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// Nesting past maxCanonDepth makes canonicalization fail, so two spellings
// of the same deep value no longer compare equal. The extra space keeps the
// bytes.Equal shortcut out of the way.
func TestEqualJSONDepthLimit(t *testing.T) {
	arrays := func(n int) (string, string) {
		s := strings.Repeat("[", n) + strings.Repeat("]", n)
		return s, s[:n] + " " + s[n:]
	}
	objects := func(n int) (string, string) {
		s := strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n)
		return s, strings.Replace(s, "1", " 1", 1)
	}
	for name, mk := range map[string]func(int) (string, string){"arrays": arrays, "objects": objects} {
		a, b := mk(maxCanonDepth)
		if !EqualJSON(json.RawMessage(a), json.RawMessage(b)) {
			t.Errorf("%s at the limit should compare equal", name)
		}
		a, b = mk(maxCanonDepth + 2)
		if EqualJSON(json.RawMessage(a), json.RawMessage(b)) {
			t.Errorf("%s past the limit should not be canonicalized", name)
		}
	}
}

func mustMap(t *testing.T, s string) OrderedMap {
	t.Helper()
	var m OrderedMap
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
