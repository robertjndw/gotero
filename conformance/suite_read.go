package conformance

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var readTests = []testCase{
	{"VersionsUnpaginated", testReadVersionsUnpaginated},
	{"VersionsSince", testReadVersionsSince},
	{"IncludeTrashed", testReadTrashed},
	{"TopExcludesChildren", testReadTop},
	{"ItemKeyHundredKeys", testReadItemKeys},
	{"LastModifiedVersionOnEveryList", testReadVersionHeaders},
	{"IfModifiedSinceVersion304", testRead304},
	{"SingleItemEnvelope", testReadEnvelope},
	{"SingleMissingItem404", testReadMissing},
	{"FormatKeys", testReadFormatKeys},
	{"CollectionsVersions", testReadCollectionVersions},
	{"PaginationHeaders", testReadPagination},
	{"ItemChildren", testReadChildren},
}

func versionsMap(t *testing.T, r *response) map[string]int64 {
	o := parseObj(t, r)
	out := map[string]int64{}
	for k, v := range o {
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("versions[%s] = %v, want number", k, v)
		}
		out[k] = int64(f)
	}
	return out
}

func testReadVersionsUnpaginated(t *testing.T, e *env) {
	l := e.userLib(t)
	keys := l.createN(t, 120, func(i int) obj { return bookItem(fmt.Sprint("n", i)) })
	for _, p := range []string{"items", "items/top"} {
		r := l.get(t, p+"?format=versions&includeTrashed=1")
		mustStatus(t, r, 200)
		versionHeader(t, r)
		if got := len(versionsMap(t, r)); got != len(keys) {
			t.Errorf("%s: %d versions, want %d", p, got, len(keys))
		}
		// The client has no pagination loop for version listings.
		if lk := r.Header.Get("Link"); strings.Contains(lk, `rel="next"`) {
			t.Errorf("%s: paginated: Link=%q", p, lk)
		}
	}
}

func testReadVersionsSince(t *testing.T, e *env) {
	l := e.userLib(t)
	l.createN(t, 3, func(i int) obj { return bookItem("old") })
	since := l.version(t)
	newKeys := l.createN(t, 2, func(i int) obj { return bookItem("new") })
	r := l.get(t, "items?format=versions&since="+strconv.FormatInt(since, 10))
	mustStatus(t, r, 200)
	m := versionsMap(t, r)
	if len(m) != 2 {
		t.Fatalf("since=%d returned %v", since, m)
	}
	for _, k := range newKeys {
		if m[k] <= since {
			t.Errorf("%s version %d <= since", k, m[k])
		}
	}
}

func testReadTrashed(t *testing.T, e *env) {
	l := e.userLib(t)
	live, _ := l.create(t, "items", bookItem("live"))
	trashed, _ := l.create(t, "items", obj{"itemType": "book", "title": "gone", "deleted": 1})
	r := l.get(t, "items?format=versions")
	mustStatus(t, r, 200)
	m := versionsMap(t, r)
	if _, ok := m[trashed]; ok {
		t.Error("trashed item listed without includeTrashed")
	}
	if _, ok := m[live]; !ok {
		t.Error("live item missing")
	}
	r = l.get(t, "items?format=versions&includeTrashed=1")
	if _, ok := versionsMap(t, r)[trashed]; !ok {
		t.Error("trashed item missing with includeTrashed=1")
	}
	r = l.get(t, "items/trash?format=keys")
	mustStatus(t, r, 200)
	if strings.TrimSpace(string(r.Body)) != trashed {
		t.Errorf("trash = %q", r.Body)
	}
	if d := at[obj](t, l.item(t, trashed), "data"); atInt(t, d, "deleted") != 1 {
		t.Errorf("deleted flag = %v", d["deleted"])
	}
}

func testReadTop(t *testing.T, e *env) {
	l := e.userLib(t)
	parent, _ := l.create(t, "items", bookItem("p"))
	note, _ := l.create(t, "items", obj{"itemType": "note", "note": "<p>c</p>", "parentItem": parent})
	att := attachmentItem("a.pdf")
	att["parentItem"] = parent
	attKey, _ := l.create(t, "items", att)
	r := l.get(t, "items/top?format=versions")
	mustStatus(t, r, 200)
	m := versionsMap(t, r)
	if len(m) != 1 || m[parent] == 0 {
		t.Errorf("items/top = %v, want only %s", m, parent)
	}
	all := versionsMap(t, l.get(t, "items?format=versions"))
	for _, k := range []string{parent, note, attKey} {
		if _, ok := all[k]; !ok {
			t.Errorf("items lacks %s", k)
		}
	}
}

// Zotero desktop requests up to 100 keys without a limit parameter and
// never follows Link, so the server must return every requested object
// instead of the default page of 25.
func testReadItemKeys(t *testing.T, e *env) {
	l := e.userLib(t)
	real := l.createN(t, 60, func(i int) obj { return bookItem("k") })
	req := append(append([]string{}, real...), newKeys(40)...)
	r := l.get(t, "items?itemKey="+joinKeys(req)+"&includeTrashed=1")
	mustStatus(t, r, 200)
	arr := parseArr(t, r)
	if len(arr) != 60 {
		t.Errorf("got %d items, want 60", len(arr))
	}
}

func testReadVersionHeaders(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", bookItem("x"))
	for _, p := range []string{"items", "items/top", "collections", "searches", "tags", "settings", "deleted?since=0", "items?format=versions", "fulltext?since=0"} {
		r := l.get(t, p)
		mustStatus(t, r, 200)
		versionHeader(t, r)
	}
}

func testRead304(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", bookItem("x"))
	cur := l.version(t)
	cv := strconv.FormatInt(cur, 10)
	for _, p := range []string{"settings", "items", "collections?format=versions", "items?format=versions&since=0"} {
		r := l.get(t, p, withHeader("If-Modified-Since-Version", cv))
		mustStatus(t, r, 304)
		if got := versionHeader(t, r); got != cur {
			t.Errorf("%s: 304 version = %d, want %d", p, got, cur)
		}
		r = l.get(t, p, withHeader("If-Modified-Since-Version", strconv.FormatInt(cur-1, 10)))
		mustStatus(t, r, 200)
	}
}

func testReadEnvelope(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "items", bookItem("env"))
	r := l.get(t, "items/"+key)
	mustStatus(t, r, 200)
	if got := versionHeader(t, r); got != v {
		t.Errorf("header version %d, want %d", got, v)
	}
	o := parseObj(t, r)
	if at[string](t, o, "key") != key || atInt(t, o, "version") != v {
		t.Errorf("envelope key/version wrong: %v", o)
	}
	if at[string](t, o, "library", "type") != "user" || atInt(t, o, "library", "id") != l.u.ID {
		t.Errorf("library = %v", o["library"])
	}
	if at[string](t, o, "data", "key") != key || atInt(t, o, "data", "version") != v {
		t.Errorf("data key/version wrong: %v", o["data"])
	}
	at[string](t, o, "data", "itemType")
}

func testReadMissing(t *testing.T, e *env) {
	l := e.userLib(t)
	mustStatus(t, l.get(t, "items/"+newKey()), 404)
}

func testReadFormatKeys(t *testing.T, e *env) {
	l := e.userLib(t)
	keys := l.createN(t, 4, func(i int) obj { return bookItem("k") })
	r := l.get(t, "items?format=keys")
	mustStatus(t, r, 200)
	got := strings.Fields(string(r.Body))
	sort.Strings(got)
	want := append([]string{}, keys...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func testReadCollectionVersions(t *testing.T, e *env) {
	l := e.userLib(t)
	ck, v := l.create(t, "collections", obj{"name": "c"})
	r := l.get(t, "collections?format=versions&since=0")
	mustStatus(t, r, 200)
	if versionsMap(t, r)[ck] != v {
		t.Errorf("collection version mismatch: %s", r)
	}
}

func testReadPagination(t *testing.T, e *env) {
	l := e.userLib(t)
	l.createN(t, 5, func(i int) obj { return bookItem("p") })
	r := l.get(t, "items?limit=2&start=0")
	mustStatus(t, r, 200)
	if n, err := strconv.Atoi(mustHeader(t, r, "Total-Results")); err != nil || n != 5 {
		t.Errorf("Total-Results = %q", r.Header.Get("Total-Results"))
	}
	if !strings.Contains(mustHeader(t, r, "Link"), `rel="next"`) {
		t.Errorf("Link = %q, want rel=next", r.Header.Get("Link"))
	}
	if n := len(parseArr(t, r)); n != 2 {
		t.Errorf("got %d items", n)
	}
}

func testReadChildren(t *testing.T, e *env) {
	l := e.userLib(t)
	parent, _ := l.create(t, "items", bookItem("p"))
	child, _ := l.create(t, "items", obj{"itemType": "note", "note": "<p>c</p>", "parentItem": parent})
	r := l.get(t, "items/"+parent+"/children?format=keys")
	mustStatus(t, r, 200)
	if strings.TrimSpace(string(r.Body)) != child {
		t.Errorf("children = %q", r.Body)
	}
}
