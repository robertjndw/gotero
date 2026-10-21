package conformance

import (
	"fmt"
	"strings"
	"testing"
)

var writeTests = []testCase{
	{"CreateItemsResponseShape", testWriteShape},
	{"OneVersionBumpPerRequest", testWriteOneBump},
	{"ResendIdenticalIsUnchanged", testWriteUnchanged},
	{"PartialPatchKeepsOtherFields", testWritePartial},
	{"StaleObjectVersionFails412", testWriteStaleObject},
	{"MissingObjectWithVersionFails404", testWriteMissing404},
	{"StaleLibraryVersionHeader412", testWriteStaleHeader},
	{"CurrentLibraryVersionHeaderAccepted", testWriteCurrentHeader},
	{"TooManyObjects413", testWriteTooMany},
	{"InvalidKeyFormatFails400", testWriteBadKey},
	{"CollectionsCreateUpdate", testWriteCollections},
	{"SearchesCreateUpdate", testWriteSearches},
	{"ItemInCollection", testWriteItemCollection},
	{"PutReplacesItem", testWritePut},
	{"PatchMergesItem", testWritePatch},
	{"DeleteItems", testWriteDeleteItems},
	{"DeleteMissingKeyIsNotAnError", testWriteDeleteMissing},
	{"DeleteRequiresPrecondition428", testWriteDelete428},
	{"DeleteStaleVersion412", testWriteDeleteStale},
	{"DeleteCollectionsAndSearches", testWriteDeleteCollSearch},
	{"WriteTokenReplay412", testWriteToken},
	{"GzipRequestBody", testWriteGzip},
	{"TrashCollectionsAndSearches", testWriteTrashCollSearch},
}

func testWriteShape(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.post(t, "items", []obj{bookItem("a"), bookItem("b"), bookItem("c")})
	mustStatus(t, r, 200)
	v := versionHeader(t, r)
	o := parseObj(t, r)
	for _, k := range []string{"successful", "success", "unchanged", "failed"} {
		at[obj](t, o, k)
	}
	for i := 0; i < 3; i++ {
		idx := fmt.Sprint(i)
		key := at[string](t, o, "successful", idx, "key")
		if !keyRe.MatchString(key) {
			t.Errorf("generated key %q is malformed", key)
		}
		if at[string](t, o, "success", idx) != key {
			t.Errorf("success[%s] != successful[%s].key", idx, idx)
		}
		if atInt(t, o, "successful", idx, "version") != v {
			t.Errorf("object %s version != Last-Modified-Version %d", idx, v)
		}
		at[obj](t, o, "successful", idx, "data")
		at[obj](t, o, "successful", idx, "library")
	}
}

func testWriteOneBump(t *testing.T, e *env) {
	l := e.userLib(t)
	before := l.version(t)
	var batch []obj
	for i := 0; i < 5; i++ {
		batch = append(batch, bookItem(fmt.Sprint("b", i)))
	}
	r := l.post(t, "items", batch)
	mustStatus(t, r, 200)
	after := versionHeader(t, r)
	if after != before+1 {
		t.Errorf("library version %d -> %d, want exactly one bump", before, after)
	}
	o := parseObj(t, r)
	for i := range batch {
		if v := atInt(t, o, "successful", fmt.Sprint(i), "version"); v != after {
			t.Errorf("object %d version = %d, want %d", i, v, after)
		}
	}
	if got := l.version(t); got != after {
		t.Errorf("version after read = %d, want %d", got, after)
	}
}

func testWriteUnchanged(t *testing.T, e *env) {
	l := e.userLib(t)
	res := l.write(t, "items", []obj{bookItem("same")})
	data := at[obj](t, res, "successful", "0", "data")
	before := l.version(t)
	r := l.post(t, "items", []obj{data})
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	if at[string](t, o, "unchanged", "0") != data["key"] {
		t.Errorf("expected unchanged[0], got %s", r)
	}
	if got := versionHeader(t, r); got != before {
		t.Errorf("version moved %d -> %d on a no-op write", before, got)
	}
	if len(at[obj](t, o, "successful")) != 0 {
		t.Error("no-op write reported successful objects")
	}
}

func testWritePartial(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "items", obj{"itemType": "book", "title": "old", "abstractNote": "keep me"})
	res := l.write(t, "items", []obj{{"key": key, "version": v, "title": "new"}})
	at[string](t, res, "success", "0")
	d := at[obj](t, l.item(t, key), "data")
	if d["title"] != "new" || d["abstractNote"] != "keep me" {
		t.Errorf("data after patch = %v", d)
	}
}

func testWriteStaleObject(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v1 := l.create(t, "items", bookItem("t"))
	l.write(t, "items", []obj{{"key": key, "version": v1, "title": "t2"}})
	r := l.post(t, "items", []obj{{"key": key, "version": v1, "title": "t3"}})
	mustStatus(t, r, 200)
	f := at[obj](t, parseObj(t, r), "failed", "0")
	if atInt(t, f, "code") != 412 || f["key"] != key {
		t.Errorf("failed[0] = %v, want 412 for %s", f, key)
	}
	at[string](t, f, "message")
}

func testWriteMissing404(t *testing.T, e *env) {
	l := e.userLib(t)
	key := newKey()
	r := l.post(t, "items", []obj{{"key": key, "version": 5, "itemType": "book", "title": "ghost"}})
	mustStatus(t, r, 200)
	f := at[obj](t, parseObj(t, r), "failed", "0")
	if atInt(t, f, "code") != 404 {
		t.Errorf("failed[0] = %v, want 404", f)
	}
}

func testWriteStaleHeader(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", bookItem("first"))
	cur := l.version(t)
	r := l.post(t, "items", []obj{bookItem("late")}, withIfUnmodified(cur-1))
	mustStatus(t, r, 412)
	if got := versionHeader(t, r); got != cur {
		t.Errorf("412 Last-Modified-Version = %d, want %d", got, cur)
	}
	if l.version(t) != cur {
		t.Error("rejected write changed the library")
	}
}

func testWriteCurrentHeader(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", bookItem("first"))
	r := l.post(t, "items", []obj{bookItem("second")}, withIfUnmodified(l.version(t)))
	mustStatus(t, r, 200)
}

func testWriteTooMany(t *testing.T, e *env) {
	l := e.userLib(t)
	var batch []obj
	for i := 0; i < 51; i++ {
		batch = append(batch, bookItem("x"))
	}
	r := l.post(t, "items", batch)
	mustStatus(t, r, 413)
}

func testWriteBadKey(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.post(t, "items", []obj{{"key": "bad!", "itemType": "book", "title": "x"}})
	mustStatus(t, r, 200)
	f := at[obj](t, parseObj(t, r), "failed", "0")
	if atInt(t, f, "code") != 400 {
		t.Errorf("failed[0] = %v, want 400", f)
	}
}

func testWriteCollections(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "collections", obj{"name": "Reading"})
	res := l.write(t, "collections", []obj{{"key": key, "version": v, "name": "Renamed"}})
	at[string](t, res, "success", "0")
	r := l.get(t, "collections/"+key)
	mustStatus(t, r, 200)
	if n := at[string](t, parseObj(t, r), "data", "name"); n != "Renamed" {
		t.Errorf("name = %q", n)
	}
	// stale
	r = l.post(t, "collections", []obj{{"key": key, "version": v, "name": "Stale"}})
	mustStatus(t, r, 200)
	if c := atInt(t, parseObj(t, r), "failed", "0", "code"); c != 412 {
		t.Errorf("stale collection code = %d", c)
	}
}

func testWriteSearches(t *testing.T, e *env) {
	l := e.userLib(t)
	cond := []obj{{"condition": "title", "operator": "contains", "value": "go"}}
	key, v := l.create(t, "searches", obj{"name": "Go", "conditions": cond})
	res := l.write(t, "searches", []obj{{"key": key, "version": v, "name": "Go2"}})
	at[string](t, res, "success", "0")
	r := l.get(t, "searches/"+key)
	mustStatus(t, r, 200)
	d := at[obj](t, parseObj(t, r), "data")
	if d["name"] != "Go2" || len(at[[]any](t, d, "conditions")) != 1 {
		t.Errorf("search data = %v", d)
	}
}

func testWriteItemCollection(t *testing.T, e *env) {
	l := e.userLib(t)
	ck, _ := l.create(t, "collections", obj{"name": "C"})
	ik, _ := l.create(t, "items", obj{"itemType": "book", "title": "in c", "collections": []string{ck}})
	cols := at[[]any](t, l.item(t, ik), "data", "collections")
	if len(cols) != 1 || cols[0] != ck {
		t.Errorf("collections = %v", cols)
	}
	r := l.get(t, "collections/"+ck+"/items?format=keys")
	mustStatus(t, r, 200)
	if strings.TrimSpace(string(r.Body)) != ik {
		t.Errorf("collection items = %q", r.Body)
	}
}

func testWritePut(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "items", obj{"itemType": "book", "title": "old", "abstractNote": "gone"})
	r := l.do(t, "PUT", "items/"+key, withJSON(obj{"itemType": "book", "title": "replaced"}), withIfUnmodified(v))
	mustStatus(t, r, 204)
	if nv := versionHeader(t, r); nv <= v {
		t.Errorf("version %d -> %d, want bump", v, nv)
	}
	d := at[obj](t, l.item(t, key), "data")
	if d["title"] != "replaced" {
		t.Errorf("title = %v", d["title"])
	}
	if a, ok := d["abstractNote"]; ok && a != "" {
		t.Errorf("PUT kept omitted field abstractNote = %v", a)
	}
}

func testWritePatch(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "items", obj{"itemType": "book", "title": "old", "abstractNote": "kept"})
	r := l.do(t, "PATCH", "items/"+key, withJSON(obj{"title": "patched"}), withIfUnmodified(v))
	mustStatus(t, r, 204)
	d := at[obj](t, l.item(t, key), "data")
	if d["title"] != "patched" || d["abstractNote"] != "kept" {
		t.Errorf("data = %v", d)
	}
	// Stale precondition.
	r = l.do(t, "PATCH", "items/"+key, withJSON(obj{"title": "x"}), withIfUnmodified(v))
	mustStatus(t, r, 412)
	versionHeader(t, r)
}

func testWriteDeleteItems(t *testing.T, e *env) {
	l := e.userLib(t)
	keys := l.createN(t, 3, func(i int) obj { return bookItem(fmt.Sprint("d", i)) })
	before := l.version(t)
	r := l.do(t, "DELETE", "items?itemKey="+joinKeys(keys[:2]), withIfUnmodified(before))
	mustStatus(t, r, 204)
	if v := versionHeader(t, r); v <= before {
		t.Errorf("delete did not bump version: %d -> %d", before, v)
	}
	mustStatus(t, l.get(t, "items/"+keys[0]), 404)
	mustStatus(t, l.get(t, "items/"+keys[2]), 200)
}

func testWriteDeleteMissing(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.do(t, "DELETE", "items?itemKey="+newKey(), withIfUnmodified(l.version(t)))
	mustStatus(t, r, 204)
	versionHeader(t, r)
}

func testWriteDelete428(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.create(t, "items", bookItem("x"))
	r := l.do(t, "DELETE", "items?itemKey="+key)
	mustStatus(t, r, 428)
	mustStatus(t, l.get(t, "items/"+key), 200)
}

func testWriteDeleteStale(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "items", bookItem("x"))
	r := l.do(t, "DELETE", "items?itemKey="+key, withIfUnmodified(v-1))
	mustStatus(t, r, 412)
	versionHeader(t, r)
	mustStatus(t, l.get(t, "items/"+key), 200)
}

func testWriteDeleteCollSearch(t *testing.T, e *env) {
	l := e.userLib(t)
	ck, _ := l.create(t, "collections", obj{"name": "c"})
	sk, _ := l.create(t, "searches", obj{"name": "s", "conditions": []obj{{"condition": "title", "operator": "is", "value": "z"}}})
	mustStatus(t, l.do(t, "DELETE", "collections?collectionKey="+ck, withIfUnmodified(l.version(t))), 204)
	mustStatus(t, l.do(t, "DELETE", "searches?searchKey="+sk, withIfUnmodified(l.version(t))), 204)
	mustStatus(t, l.get(t, "collections/"+ck), 404)
	mustStatus(t, l.get(t, "searches/"+sk), 404)
}

func testWriteToken(t *testing.T, e *env) {
	l := e.userLib(t)
	tok := randHex(16) // 32 characters
	r := l.post(t, "items", []obj{bookItem("tok")}, withHeader("Zotero-Write-Token", tok))
	mustStatus(t, r, 200)
	before := l.version(t)
	r = l.post(t, "items", []obj{bookItem("tok again")}, withHeader("Zotero-Write-Token", tok))
	mustStatus(t, r, 412)
	if l.version(t) != before {
		t.Error("replayed token still wrote")
	}
}

func testWriteGzip(t *testing.T, e *env) {
	l := e.userLib(t)
	r := l.post(t, "items", []obj{bookItem("zipped")}, withGzip())
	mustStatus(t, r, 200)
	at[string](t, parseObj(t, r), "success", "0")
}

// Since Zotero 7 collections and searches can sit in the trash, which the
// client syncs as deleted: true on the object. Rejecting the property makes
// trashing a collection fail on the desktop.
func testWriteTrashCollSearch(t *testing.T, e *env) {
	l := e.userLib(t)
	for _, c := range []struct {
		typ string
		o   obj
	}{
		{"collections", obj{"name": "Old"}},
		{"searches", obj{"name": "Old", "conditions": []obj{{"condition": "title", "operator": "contains", "value": "x"}}}},
	} {
		key, v := l.create(t, c.typ, c.o)
		res := l.write(t, c.typ, []obj{{"key": key, "version": v, "deleted": true}})
		at[string](t, res, "success", "0")
		data := at[obj](t, parseObj(t, l.get(t, c.typ+"/"+key)), "data")
		if d, ok := data["deleted"]; !ok || (d != true && d != float64(1)) {
			t.Errorf("%s: deleted = %v after trashing", c.typ, d)
		}
		v = int64(at[float64](t, data, "version"))
		res = l.write(t, c.typ, []obj{{"key": key, "version": v, "deleted": false}})
		at[string](t, res, "success", "0")
		data = at[obj](t, parseObj(t, l.get(t, c.typ+"/"+key)), "data")
		if d, ok := data["deleted"]; ok && d != false && d != float64(0) {
			t.Errorf("%s: deleted = %v after restoring", c.typ, d)
		}
	}
}
