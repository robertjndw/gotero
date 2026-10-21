package conformance

import (
	"slices"
	"strings"
	"testing"
)

var tagsTests = []testCase{
	{"GetTagsShape", testTagsGet},
	{"ItemsFilteredByTag", testTagsFilter},
	{"DeleteMultipleTags", testTagsDeleteOr},
	{"BareDeleteTags204", testTagsBareDelete},
	{"ReAddedTagLeavesDeleteLog", testTagsReAdded},
	{"DeletingUnusedTagIsNoOp", testTagsDeleteMissing},
}

func taggedItem(title string, tags ...string) obj {
	var ts []obj
	for _, tg := range tags {
		ts = append(ts, obj{"tag": tg})
	}
	return obj{"itemType": "book", "title": title, "tags": ts}
}

func tagNames(t *testing.T, r *response) []string {
	mustStatus(t, r, 200)
	var out []string
	for _, v := range parseArr(t, r) {
		o, _ := v.(obj)
		out = append(out, at[string](t, o, "tag"))
	}
	return out
}

func testTagsGet(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", taggedItem("t", "alpha"))
	r := l.get(t, "tags")
	mustStatus(t, r, 200)
	versionHeader(t, r)
	arr := parseArr(t, r)
	if len(arr) != 1 {
		t.Fatalf("tags = %s", r)
	}
	o := arr[0].(obj)
	if at[string](t, o, "tag") != "alpha" {
		t.Errorf("tag = %v", o["tag"])
	}
	at[obj](t, o, "meta")
}

func testTagsFilter(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.create(t, "items", taggedItem("t", "alpha"))
	l.create(t, "items", taggedItem("u", "beta"))
	r := l.get(t, "items?tag=alpha&format=keys")
	mustStatus(t, r, 200)
	if strings.TrimSpace(string(r.Body)) != k {
		t.Errorf("items by tag = %q", r.Body)
	}
}

func testTagsDeleteOr(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.create(t, "items", taggedItem("t", "a", "b", "c"))
	before := l.version(t)
	r := l.do(t, "DELETE", "tags?tag="+esc("a || b"), withIfUnmodified(before))
	mustStatus(t, r, 204)
	if v := versionHeader(t, r); v <= before {
		t.Errorf("version %d -> %d, want bump", before, v)
	}
	names := tagNames(t, l.get(t, "tags"))
	if len(names) != 1 || names[0] != "c" {
		t.Errorf("remaining tags = %v", names)
	}
	d := at[obj](t, l.item(t, k), "data")
	if ts := at[[]any](t, d, "tags"); len(ts) != 1 {
		t.Errorf("item tags = %v", ts)
	}
}

func testTagsBareDelete(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.do(t, "DELETE", "tags", withIfUnmodified(l.version(t)))
	mustStatus(t, r, 204)
	versionHeader(t, r)
}

func deletedTags(t *testing.T, l *lib) []string {
	t.Helper()
	r := l.get(t, "deleted?since=0")
	mustStatus(t, r, 200)
	return strs(at[[]any](t, parseObj(t, r), "tags"))
}

// A tag that comes back on an item must leave deleted.tags, otherwise other
// clients strip it again on their next sync.
func testTagsReAdded(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", taggedItem("first", "foo"))
	mustStatus(t, l.do(t, "DELETE", "tags?tag=foo", withIfUnmodified(l.version(t))), 204)
	if got := deletedTags(t, l); !slices.Contains(got, "foo") {
		t.Fatalf("deleted.tags = %v, want it to contain foo", got)
	}

	// Via update of an existing item.
	k, _ := l.create(t, "items", bookItem("second"))
	mustStatus(t, l.do(t, "PATCH", "items/"+k, withJSON(obj{"tags": []obj{{"tag": "foo"}}})), 204)
	if got := deletedTags(t, l); slices.Contains(got, "foo") {
		t.Errorf("deleted.tags = %v after tagging an existing item, want foo gone", got)
	}

	// Via creation of a new item.
	mustStatus(t, l.do(t, "DELETE", "tags?tag=foo", withIfUnmodified(l.version(t))), 204)
	if got := deletedTags(t, l); !slices.Contains(got, "foo") {
		t.Fatalf("deleted.tags = %v, want it to contain foo", got)
	}
	l.create(t, "items", taggedItem("third", "foo"))
	if got := deletedTags(t, l); slices.Contains(got, "foo") {
		t.Errorf("deleted.tags = %v after creating a tagged item, want foo gone", got)
	}
}

// Deleting a tag nobody carries changes nothing, so the library version must
// stay put and the name must not show up in the delete log.
func testTagsDeleteMissing(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", taggedItem("t", "present"))
	before := l.version(t)
	r := l.do(t, "DELETE", "tags?tag=nonexistent", withIfUnmodified(before))
	mustStatus(t, r, 204)
	if v := versionHeader(t, r); v != before {
		t.Errorf("version %d -> %d, want unchanged", before, v)
	}
	if got := deletedTags(t, l); slices.Contains(got, "nonexistent") {
		t.Errorf("deleted.tags = %v, want no entry for a tag that never existed", got)
	}
	if now := l.version(t); now != before {
		t.Errorf("library version moved %d -> %d", before, now)
	}
}
