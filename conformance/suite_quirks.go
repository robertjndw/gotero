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
}

// Serialization order the client depends on (protocol-notes, Object JSON).
var attachmentOrder = []string{
	"key", "version", "parentItem", "itemType", "linkMode", "title", "accessDate", "url", "note",
	"contentType", "charset", "filename", "md5", "mtime", "tags", "collections", "relations",
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
