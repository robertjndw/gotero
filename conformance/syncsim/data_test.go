package syncsim

import (
	"encoding/json"
	"testing"
)

func o(t *testing.T, s string) obj {
	t.Helper()
	var m obj
	if err := decode([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNormalizeTreatsEmptyAsMissing(t *testing.T) {
	a := o(t, `{"key":"AAAAAAAA","version":3,"title":"x","extra":"","tags":[],"collections":[],"relations":{},
		"parentItem":false,"deleted":0,"md5":null}`)
	if got := canon(normalize(a)); got != `{"title":"x"}` {
		t.Errorf("normalize = %s", got)
	}
	b := o(t, `{"tags":[{"tag":"b"},{"tag":"a","type":0},{"tag":"a"}],"collections":["Z","A","Z"]}`)
	if got := canon(normalize(b)); got != `{"collections":["A","Z"],"tags":[{"tag":"a"},{"tag":"b"}]}` {
		t.Errorf("normalize sets = %s", got)
	}
}

func TestMerge3(t *testing.T) {
	cache := o(t, `{"title":"t","extra":"e","tags":[{"tag":"a"},{"tag":"b"}],"collections":["C1"],"dateModified":"2020-01-01T00:00:00Z"}`)
	local := o(t, `{"title":"local","extra":"e","tags":[{"tag":"a"},{"tag":"x"}],"collections":["C1","C2"],"dateModified":"2020-01-03T00:00:00Z"}`)
	remote := o(t, `{"title":"t","extra":"remote","tags":[{"tag":"b"},{"tag":"y"}],"collections":[],"dateModified":"2020-01-02T00:00:00Z"}`)
	merged, n := merge3(cache, local, remote, PreferRemote)
	if n != 0 {
		t.Errorf("conflicts = %d, want 0", n)
	}
	// Each side's field change is kept, set removals on either side win,
	// additions on either side are kept, and the later dateModified wins.
	want := `{"collections":["C2"],"dateModified":"2020-01-03T00:00:00Z","extra":"remote","tags":[{"tag":"x"},{"tag":"y"}],"title":"local"}`
	if got := canon(merged); got != want {
		t.Errorf("merged = %s\nwant     %s", got, want)
	}

	both := o(t, `{"title":"remote"}`)
	for _, tc := range []struct {
		p    Policy
		want string
	}{{PreferRemote, "remote"}, {PreferLocal, "local"}} {
		m, n := merge3(o(t, `{"title":"t"}`), o(t, `{"title":"local"}`), both, tc.p)
		if n != 1 || m["title"] != tc.want {
			t.Errorf("%v: merged %v with %d conflicts", tc.p, m, n)
		}
	}
	if m, n := merge3(o(t, `{"title":"t"}`), o(t, `{"title":"same"}`), o(t, `{"title":"same"}`), PreferLocal); n != 0 || m["title"] != "same" {
		t.Errorf("identical changes are no conflict: %v %d", m, n)
	}
}

func TestPatchFor(t *testing.T) {
	cache := o(t, `{"title":"t","extra":"e","parentItem":"PPPPPPPP","deleted":1,"tags":[{"tag":"a"}]}`)
	data := o(t, `{"title":"new","tags":[{"tag":"a"}]}`)
	p := patchFor(cache, data)
	// Cleared fields go up as explicit empties, since POST leaves omitted
	// fields alone.
	want := map[string]any{"title": "new", "extra": "", "parentItem": false, "deleted": false}
	b, _ := json.Marshal(want)
	if canon(p) != string(b) {
		t.Errorf("patch = %s, want %s", canon(p), b)
	}
}
