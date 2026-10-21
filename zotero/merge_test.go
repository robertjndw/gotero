package zotero

import (
	"encoding/json"
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
