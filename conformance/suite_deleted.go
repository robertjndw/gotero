package conformance

import (
	"slices"
	"strconv"
	"testing"
)

var deletedTests = []testCase{
	{"ArraysAlwaysPresent", testDeletedEmpty},
	{"AllObjectKindsLogged", testDeletedKinds},
	{"SinceFilters", testDeletedSince},
}

var deletedKinds = []string{"collections", "searches", "items", "tags", "settings"}

func testDeletedEmpty(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.get(t, "deleted?since=0")
	mustStatus(t, r, 200)
	versionHeader(t, r)
	o := parseObj(t, r)
	for _, k := range deletedKinds {
		if got := at[[]any](t, o, k); len(got) != 0 {
			t.Errorf("%s = %v on an empty library", k, got)
		}
	}
}

func strs(a []any) []string {
	out := make([]string, 0, len(a))
	for _, v := range a {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func testDeletedKinds(t *testing.T, e *env) {
	l := e.userLib(t)
	ck, _ := l.create(t, "collections", obj{"name": "c"})
	sk, _ := l.create(t, "searches", obj{"name": "s", "conditions": []obj{{"condition": "title", "operator": "is", "value": "z"}}})
	ik, _ := l.create(t, "items", obj{"itemType": "book", "title": "i", "tags": []obj{{"tag": "doomed"}}})
	mustStatus(t, l.post(t, "settings", obj{"tagColors": obj{"value": []obj{{"name": "doomed", "color": "#FF0000"}}, "version": 0}}), 204)

	mustStatus(t, l.do(t, "DELETE", "collections?collectionKey="+ck, withIfUnmodified(l.version(t))), 204)
	mustStatus(t, l.do(t, "DELETE", "searches?searchKey="+sk, withIfUnmodified(l.version(t))), 204)
	// The tag has to be deleted while an item still carries it: removing a
	// tag nobody uses is a no-op and is not logged.
	mustStatus(t, l.do(t, "DELETE", "tags?tag=doomed", withIfUnmodified(l.version(t))), 204)
	mustStatus(t, l.do(t, "DELETE", "items?itemKey="+ik, withIfUnmodified(l.version(t))), 204)
	mustStatus(t, l.do(t, "DELETE", "settings?settingKey=tagColors", withIfUnmodified(l.version(t))), 204)

	r := l.get(t, "deleted?since=0")
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	want := map[string]string{"collections": ck, "searches": sk, "items": ik, "settings": "tagColors", "tags": "doomed"}
	for kind, k := range want {
		if got := strs(at[[]any](t, o, kind)); !slices.Contains(got, k) {
			t.Errorf("deleted.%s = %v, want %q", kind, got, k)
		}
	}
}

func testDeletedSince(t *testing.T, e *env) {
	l := e.userLib(t)
	keys := l.createN(t, 2, func(i int) obj { return bookItem("d") })
	r := l.do(t, "DELETE", "items?itemKey="+keys[0], withIfUnmodified(l.version(t)))
	mustStatus(t, r, 204)
	v1 := versionHeader(t, r)
	mustStatus(t, l.do(t, "DELETE", "items?itemKey="+keys[1], withIfUnmodified(v1)), 204)

	r = l.get(t, "deleted?since="+strconv.FormatInt(v1, 10))
	mustStatus(t, r, 200)
	got := strs(at[[]any](t, parseObj(t, r), "items"))
	if len(got) != 1 || got[0] != keys[1] {
		t.Errorf("since=%d items = %v, want only %s", v1, got, keys[1])
	}
	r = l.get(t, "deleted?since=0")
	if got := strs(at[[]any](t, parseObj(t, r), "items")); len(got) != 2 {
		t.Errorf("since=0 items = %v", got)
	}
}
