package conformance

import (
	"strconv"
	"testing"
)

var fulltextTests = []testCase{
	{"PostThenRead", testFulltextPost},
	{"SinceMap", testFulltextSince},
	{"PutSingleItem", testFulltextPut},
	{"MissingContent404", testFulltextMissing},
	{"IdenticalWritesDoNotBumpVersion", testFulltextIdentical},
}

func (l *lib) attachment(t *testing.T) (string, int64) {
	return l.create(t, "items", attachmentItem("f.pdf"))
}

func testFulltextPost(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.attachment(t)
	// POST /fulltext is undocumented upstream; protocol-notes says it needs
	// If-Unmodified-Since-Version and answers in the write-response format.
	r := l.post(t, "fulltext", []obj{{"key": k, "content": "hello world", "indexedPages": 2, "totalPages": 2}}, withIfUnmodified(l.version(t)))
	mustStatus(t, r, 200)
	versionHeader(t, r)
	o := parseObj(t, r)
	if f, ok := o["failed"].(obj); ok && len(f) > 0 {
		t.Fatalf("fulltext write failed: %v", f)
	}
	r = l.get(t, "items/"+k+"/fulltext")
	mustStatus(t, r, 200)
	versionHeader(t, r)
	c := parseObj(t, r)
	if at[string](t, c, "content") != "hello world" || atInt(t, c, "indexedPages") != 2 || atInt(t, c, "totalPages") != 2 {
		t.Errorf("fulltext = %v", c)
	}
}

func testFulltextSince(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.attachment(t)
	mustStatus(t, l.post(t, "fulltext", []obj{{"key": k, "content": "x", "indexedChars": 1, "totalChars": 1}}, withIfUnmodified(l.version(t))), 200)
	r := l.get(t, "fulltext?since=0")
	mustStatus(t, r, 200)
	v := atInt(t, parseObj(t, r), k)
	if v <= 0 {
		t.Errorf("fulltext version for %s = %d", k, v)
	}
	r = l.get(t, "fulltext?since="+strconv.FormatInt(l.version(t), 10))
	mustStatus(t, r, 200)
	if o := parseObj(t, r); len(o) != 0 {
		t.Errorf("since=current returned %v", o)
	}
}

func testFulltextPut(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.attachment(t)
	r := l.do(t, "PUT", "items/"+k+"/fulltext", withJSON(obj{"content": "put text", "indexedChars": 8, "totalChars": 8}))
	mustStatus(t, r, 204)
	c := parseObj(t, l.get(t, "items/"+k+"/fulltext"))
	if at[string](t, c, "content") != "put text" || atInt(t, c, "totalChars") != 8 {
		t.Errorf("fulltext = %v", c)
	}
	mustStatus(t, l.do(t, "PUT", "items/"+newKey()+"/fulltext", withJSON(obj{"content": "x"})), 404)
}

func testFulltextMissing(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.attachment(t)
	mustStatus(t, l.get(t, "items/"+k+"/fulltext"), 404)
	mustStatus(t, l.get(t, "items/"+newKey()+"/fulltext"), 404)
}

// Re-uploading the same full text is what a client does after any local
// reindex; it must not move the library version and wake every other client.
func testFulltextIdentical(t *testing.T, e *env) {
	l := e.userLib(t)
	k, _ := l.attachment(t)
	body := obj{"content": "same text", "indexedChars": 9, "totalChars": 9}
	mustStatus(t, l.do(t, "PUT", "items/"+k+"/fulltext", withJSON(body)), 204)
	v := l.version(t)
	fv := atInt(t, parseObj(t, l.get(t, "fulltext?since=0")), k)

	r := l.do(t, "PUT", "items/"+k+"/fulltext", withJSON(body))
	mustStatus(t, r, 204)
	if got := versionHeader(t, r); got != v {
		t.Errorf("identical PUT moved the library version %d -> %d", v, got)
	}

	r = l.post(t, "fulltext", []obj{{"key": k, "content": "same text", "indexedChars": 9, "totalChars": 9}}, withIfUnmodified(v))
	mustStatus(t, r, 200)
	if got := versionHeader(t, r); got != v {
		t.Errorf("identical POST moved the library version %d -> %d", v, got)
	}
	// The client reads .key from every unchanged entry, so bare strings
	// would break it.
	if got := at[string](t, parseObj(t, r), "unchanged", "0", "key"); got != k {
		t.Errorf("unchanged[0].key = %q, want %q", got, k)
	}
	if got := atInt(t, parseObj(t, l.get(t, "fulltext?since=0")), k); got != fv {
		t.Errorf("fulltext version %d -> %d, want unchanged", fv, got)
	}

	// A real change still bumps.
	body["content"] = "other text"
	r = l.do(t, "PUT", "items/"+k+"/fulltext", withJSON(body))
	mustStatus(t, r, 204)
	if got := versionHeader(t, r); got <= v {
		t.Errorf("changed PUT did not bump the version (%d -> %d)", v, got)
	}
}
