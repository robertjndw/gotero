package conformance

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

var quirksTests = []testCase{
	{"AttachmentFieldOrderOnItem", testQuirkOrderSingle},
	{"AttachmentFieldOrderInList", testQuirkOrderList},
	{"MD5AndMtimeNullBeforeUpload", testQuirkNullFile},
	{"MtimeIsJSONNumber", testQuirkMtimeNumber},
	{"RelationURIsPreserved", testQuirkRelations},
	{"DefaultItemFieldsPresent", testQuirkDefaults},
	{"VersionHeaderOnMissingObject", testQuirkMissingHeader},
	{"BackoffAndRetryAfterAreIntegers", testQuirkRetryAfter},
	{"NullArrayFieldsBecomeEmpty", testQuirkNullArrays},
	{"WrongTypedArrayFieldsRejected", testQuirkWrongTypes},
	{"ItemTypeChangeDropsInvalidFields", testQuirkItemTypeChange},
	{"AttachmentLastRead", testQuirkLastRead},
	{"LastReadHiddenFromOldSchemas", testQuirkLastReadSchema},
	{"LastReadRefusedInGroups", testQuirkLastReadGroup},
	{"LinkedFilePath", testQuirkPath},
	{"EmptyTagsAreDropped", testQuirkEmptyTags},
	{"NullMD5AndMtimeKeepStoredValues", testQuirkKeepFileFields},
	{"EmptyRelationsArrayAccepted", testQuirkEmptyRelations},
	{"UnknownFieldStillFails", testQuirkUnknownField},
}

// Serialization order the client depends on (protocol-notes, Object JSON).
var attachmentOrder = []string{
	"key", "version", "parentItem", "itemType", "linkMode", "title", "accessDate", "url", "note",
	"contentType", "charset", "filename", "md5", "mtime", "path", "lastRead", "tags", "collections", "relations",
	"dateAdded", "dateModified",
}

// assertOrder checks that keys present in the raw "data" object appear in
// the canonical order.
func assertOrder(t testing.TB, raw []byte, order []string) {
	t.Helper()
	i := bytes.Index(raw, []byte(`"data"`))
	if i < 0 {
		t.Fatalf("no data object in %s", raw)
	}
	data := raw[i:]
	last, lastName := -1, ""
	for _, k := range order {
		idx := bytes.Index(data, []byte(`"`+k+`":`))
		if idx < 0 {
			continue
		}
		if idx < last {
			t.Errorf("%q appears before %q in data; the client requires the opposite", k, lastName)
		}
		last, lastName = idx, k
	}
	if !bytes.Contains(data, []byte(`"linkMode"`)) || !bytes.Contains(data, []byte(`"filename"`)) {
		t.Errorf("attachment data lacks linkMode or filename: %s", data)
	}
}

func testQuirkOrderSingle(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.create(t, "items", attachmentItem("o.pdf"))
	r := l.get(t, "items/"+key)
	mustStatus(t, r, 200)
	assertOrder(t, r.Body, attachmentOrder)
}

func testQuirkOrderList(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.create(t, "items", attachmentItem("o.pdf"))
	r := l.get(t, "items?itemKey="+key)
	mustStatus(t, r, 200)
	assertOrder(t, r.Body, attachmentOrder)
	r = l.get(t, "items/top?limit=5")
	mustStatus(t, r, 200)
	assertOrder(t, r.Body, attachmentOrder)
}

func testQuirkNullFile(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.create(t, "items", attachmentItem("n.pdf"))
	d := at[obj](t, l.item(t, key), "data")
	for _, k := range []string{"md5", "mtime"} {
		v, ok := d[k]
		if !ok || v != nil {
			t.Errorf("%s = %v (present=%v), want null", k, v, ok)
		}
	}
}

var mtimeNumber = regexp.MustCompile(`"mtime":\s*\d+[,}\s]`)

func testQuirkMtimeNumber(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "m.pdf")
	l.uploadFile(t, key, newFileSpec("m.pdf", []byte("mtime "+randHex(8))))
	r := l.get(t, "items/"+key)
	mustStatus(t, r, 200)
	if !mtimeNumber.Match(r.Body) {
		t.Errorf("mtime is not a bare JSON number in %s", r.Body)
	}
}

func testQuirkRelations(t *testing.T, e *env) {
	l := e.userLib(t)
	rel := "http://zotero.org/users/12345/items/ABCD2345"
	key, _ := l.create(t, "items", obj{"itemType": "book", "title": "r", "relations": obj{"dc:relation": rel}})
	got := at[string](t, l.item(t, key), "data", "relations", "dc:relation")
	if got != rel {
		t.Errorf("relation = %q, want it stored exactly as sent", got)
	}
}

func testQuirkDefaults(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.create(t, "items", bookItem("d"))
	d := at[obj](t, l.item(t, key), "data")
	at[[]any](t, d, "tags")
	at[[]any](t, d, "collections")
	at[obj](t, d, "relations")
	at[string](t, d, "dateAdded")
	at[string](t, d, "dateModified")
}

func testQuirkMissingHeader(t *testing.T, e *env) {
	l := e.userLib(t)
	l.create(t, "items", bookItem("x"))
	r := l.get(t, "items/"+newKey())
	mustStatus(t, r, 404)
	versionHeader(t, r)
}

// checkUniversal already validates Retry-After and Backoff on every response;
// this test just exercises a spread of error paths so those checks see them.
func testQuirkRetryAfter(t *testing.T, e *env) {
	l := e.userLib(t)
	l.get(t, "items/"+newKey())
	l.post(t, "items", []obj{bookItem("x")}, withIfUnmodified(0))
	e.do(t, "GET", "keys/current", withKey("ABCDEFGHIJKLMNOPQRSTUVWX"))
	var big []obj
	for i := 0; i < 51; i++ {
		big = append(big, bookItem(strings.Repeat("x", 3)))
	}
	l.post(t, "items", big)
}

// The desktop client breaks on null where it expects an array or object, so
// the server must never store or serve one.
func testQuirkNullArrays(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.create(t, "items", obj{
		"itemType": "book", "title": "n", "tags": nil, "collections": nil, "creators": nil, "relations": nil,
	})
	d := at[obj](t, l.item(t, k), "data")
	for _, f := range []string{"tags", "collections", "creators"} {
		v, present := d[f]
		if arr, ok := v.([]any); (present && !ok) || len(arr) != 0 || (!present && f != "creators") {
			t.Errorf("%s = %#v, want []", f, v)
		}
	}
	if v, ok := d["relations"].(obj); !ok || len(v) != 0 {
		t.Errorf("relations = %#v, want {}", d["relations"])
	}

	// Patching null over real values clears them.
	k2, _ := l.create(t, "items", obj{"itemType": "book", "title": "m", "tags": []obj{{"tag": "x"}}})
	mustStatus(t, l.do(t, "PATCH", "items/"+k2, withJSON(obj{"tags": nil})), 204)
	d = at[obj](t, l.item(t, k2), "data")
	if v, ok := d["tags"].([]any); !ok || len(v) != 0 {
		t.Errorf("tags = %#v after patching null, want []", d["tags"])
	}
}

func testQuirkWrongTypes(t *testing.T, e *env) {
	l := e.userLib(t)
	for _, bad := range []obj{
		{"itemType": "book", "tags": "x"},
		{"itemType": "book", "collections": obj{}},
		{"itemType": "book", "creators": obj{}},
		{"itemType": "book", "relations": "x"},
	} {
		res := l.write(t, "items", []obj{bad})
		if len(at[obj](t, res, "failed")) != 1 {
			t.Errorf("%v: want one failure, got %v", bad, res)
			continue
		}
		if c := atInt(t, res, "failed", "0", "code"); c != 400 {
			t.Errorf("%v: code = %d, want 400", bad, c)
		}
	}
}

// Changing the item type must not leave fields behind that the new type does
// not have, or the client rejects the item.
func testQuirkItemTypeChange(t *testing.T, e *env) {
	l := e.userLib(t)
	k, v := l.create(t, "items", obj{
		"itemType": "journalArticle", "title": "t", "publicationTitle": "Nature",
		"creators": []obj{{"creatorType": "reviewedAuthor", "firstName": "A", "lastName": "B"}},
	})
	mustStatus(t, l.do(t, "PATCH", "items/"+k, withJSON(obj{"itemType": "book"}), withIfUnmodified(v)), 204)
	d := at[obj](t, l.item(t, k), "data")
	if has(d, "publicationTitle") {
		t.Errorf("publicationTitle survived the change to book: %v", d)
	}
	if at[string](t, d, "title") != "t" {
		t.Errorf("title = %v", d["title"])
	}
	cs := at[[]any](t, d, "creators")
	if len(cs) != 1 || at[string](t, cs[0].(obj), "creatorType") != "author" {
		t.Errorf("creators = %v, want the creator moved to the primary type", cs)
	}
}

func failCode(t *testing.T, res obj) int64 {
	t.Helper()
	if len(at[obj](t, res, "failed")) != 1 {
		t.Fatalf("want one failure, got %v", res)
	}
	return atInt(t, res, "failed", "0", "code")
}

func testQuirkLastRead(t *testing.T, e *env) {
	l := e.userLib(t)
	a := attachmentItem("lr.pdf")
	a["lastRead"] = 1700000000
	key, _ := l.create(t, "items", a)
	if got := atInt(t, l.item(t, key), "data", "lastRead"); got != 1700000000 {
		t.Errorf("lastRead = %d", got)
	}
	for _, clear := range []any{nil, "", false} {
		mustStatus(t, l.do(t, "PATCH", "items/"+key, withJSON(obj{"lastRead": 5})), 204)
		v := l.version(t)
		mustStatus(t, l.do(t, "PATCH", "items/"+key, withJSON(obj{"lastRead": clear}), withIfUnmodified(v)), 204)
		if d := at[obj](t, l.item(t, key), "data"); has(d, "lastRead") {
			t.Errorf("lastRead %#v did not clear it: %v", clear, d["lastRead"])
		}
	}
	res := l.write(t, "items", []obj{{"key": key, "lastRead": "yesterday"}})
	if c := failCode(t, res); c != 400 {
		t.Errorf("non-integer lastRead: code %d, want 400", c)
	}
}

// Clients older than schema 42 reject fields they don't know.
func testQuirkLastReadSchema(t *testing.T, e *env) {
	l := e.userLib(t)
	a := attachmentItem("old.pdf")
	a["lastRead"] = 1700000000
	key, _ := l.create(t, "items", a)
	r := l.get(t, "items/"+key, withHeader("Zotero-Schema-Version", "41"))
	mustStatus(t, r, 200)
	if has(at[obj](t, parseObj(t, r), "data"), "lastRead") {
		t.Errorf("schema 41 client got lastRead: %s", r.Body)
	}
	r = l.get(t, "items/"+key, withHeader("Zotero-Schema-Version", "42"))
	mustStatus(t, r, 200)
	if !has(at[obj](t, parseObj(t, r), "data"), "lastRead") {
		t.Errorf("schema 42 client lacks lastRead: %s", r.Body)
	}
	assertOrder(t, r.Body, attachmentOrder)
}

func testQuirkLastReadGroup(t *testing.T, e *env) {
	f := e.groupFixture(t)
	l := e.groupLib(f.g, f.admin)
	a := attachmentItem("g.pdf")
	a["lastRead"] = 5
	res := l.write(t, "items", []obj{a})
	if c := failCode(t, res); c != 400 {
		t.Errorf("code = %d, want 400", c)
	}
	if m := at[string](t, res, "failed", "0", "message"); !strings.Contains(m, "user libraries") {
		t.Errorf("message = %q", m)
	}
}

func testQuirkPath(t *testing.T, e *env) {
	l := e.userLib(t)
	linked := obj{"itemType": "attachment", "linkMode": "linked_file", "title": "p", "path": "attachments:p.pdf"}
	key, _ := l.create(t, "items", linked)
	if got := at[string](t, l.item(t, key), "data", "path"); got != "attachments:p.pdf" {
		t.Errorf("path = %q", got)
	}
	bad := attachmentItem("p.pdf")
	bad["path"] = "/x/p.pdf"
	if c := failCode(t, l.write(t, "items", []obj{bad})); c != 400 {
		t.Errorf("path on imported_file: code %d, want 400", c)
	}

	f := e.groupFixture(t)
	g := e.groupLib(f.g, f.admin)
	res := g.write(t, "items", []obj{linked})
	if c := failCode(t, res); c != 400 {
		t.Errorf("path in a group: code %d, want 400", c)
	}
}

// Zotero can produce empty tags; they are skipped, not failed.
func testQuirkEmptyTags(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v := l.create(t, "items", obj{"itemType": "book", "title": "t", "tags": []obj{{"tag": ""}, {"tag": "  "}, {"tag": "0"}, {"tag": "a"}}})
	tags := at[[]any](t, l.item(t, key), "data", "tags")
	if len(tags) != 2 {
		t.Errorf("tags = %v, want 0 and a", tags)
	}
	res := l.write(t, "items", []obj{{"key": key, "version": v, "tags": []obj{{"tag": "0"}, {"tag": "a"}, {"tag": ""}}}})
	if got := at[string](t, res, "unchanged", "0"); got != key {
		t.Errorf("unchanged = %v, want %s", res["unchanged"], key)
	}
	res = l.write(t, "items", []obj{{"itemType": "book", "tags": []obj{{"tag": "a", "type": 2}}}})
	if c := failCode(t, res); c != 400 {
		t.Errorf("bad tag type: code %d, want 400", c)
	}
}

// A client that doesn't know about the uploaded file sends null or "" for
// md5 and mtime; that must not erase what the file upload recorded.
func testQuirkKeepFileFields(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.create(t, "items", attachmentItem("keep.pdf"))
	l.uploadFile(t, key, newFileSpec("keep.pdf", []byte("keep "+randHex(8))))
	before := at[obj](t, l.item(t, key), "data")
	md5, mtime := before["md5"], before["mtime"]
	if md5 == nil || mtime == nil {
		t.Fatalf("upload did not set md5/mtime: %v", before)
	}
	for _, sent := range []any{nil, ""} {
		v := l.version(t)
		mustStatus(t, l.do(t, "PATCH", "items/"+key, withJSON(obj{"md5": sent, "mtime": sent}), withIfUnmodified(v)), 204)
		d := at[obj](t, l.item(t, key), "data")
		if d["md5"] != md5 || d["mtime"] != mtime {
			t.Errorf("sent %#v: md5/mtime = %v/%v, want %v/%v", sent, d["md5"], d["mtime"], md5, mtime)
		}
	}
	fresh, _ := l.create(t, "items", obj{"itemType": "attachment", "linkMode": "imported_file", "title": "n", "md5": "", "mtime": ""})
	d := at[obj](t, l.item(t, fresh), "data")
	if v, ok := d["md5"]; !ok || v != nil {
		t.Errorf("md5 = %#v, want null", v)
	}
	if v, ok := d["mtime"]; !ok || v != nil {
		t.Errorf("mtime = %#v, want null", v)
	}
}

func testQuirkEmptyRelations(t *testing.T, e *env) {
	l := e.userLib(t)
	for typ, o := range map[string]obj{
		"items":       {"itemType": "book", "title": "r", "relations": []any{}},
		"collections": {"name": "r", "relations": []any{}},
	} {
		key, _ := l.create(t, typ, o)
		r := l.get(t, typ+"/"+key)
		mustStatus(t, r, 200)
		if rel, ok := at[obj](t, parseObj(t, r), "data")["relations"].(obj); !ok || len(rel) != 0 {
			t.Errorf("%s relations = %s, want {}", typ, r.Body)
		}
	}
}

func testQuirkUnknownField(t *testing.T, e *env) {
	l := e.userLib(t)
	res := l.write(t, "items", []obj{{"itemType": "book", "bogus": "x"}})
	if c := failCode(t, res); c != 400 {
		t.Errorf("code = %d, want 400", c)
	}
}
