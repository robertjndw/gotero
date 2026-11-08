package zapi_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zotero"
)

func TestPatchDeletedZero(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	trashed := l.create("items", m{"itemType": "book", "title": "t", "deleted": 1})
	live := l.create("items", m{"itemType": "book", "title": "l"})
	require.Contains(t, string(l.get("items/trash?format=keys").Body), trashed)

	for _, k := range []string{trashed, live} {
		r := l.send("PATCH", "items/"+k, m{"deleted": 0})
		require.Equal(t, 204, r.Status, string(r.Body))
		assert.NotContains(t, l.dataOf("items", k), "deleted", k)
	}
	trash := string(l.get("items/trash?format=keys").Body)
	assert.NotContains(t, trash, trashed, "deleted: 0 restores the item")
	assert.NotContains(t, trash, live, "deleted: 0 must not trash a live item")
	assert.Contains(t, string(l.get("items/top?format=keys").Body), trashed)
}

// The write path and the importer both normalize "deleted" before this, so
// only 1 should ever reach it. Anything else must not put an item in the
// trash, in case a future caller forgets.
func TestObjectFromDataTrashFlag(t *testing.T) {
	for raw, want := range map[string]bool{`1`: true, `0`: false, `2`: false, `null`: false} {
		var data zotero.OrderedMap
		require.NoError(t, data.UnmarshalJSON([]byte(`{"itemType":"book","deleted":`+raw+`}`)))
		o, err := zapi.ObjectFromData(repo.TypeItem, "AAAAAAA2", data)
		require.NoError(t, err)
		assert.Equal(t, want, o.InTrash, "deleted: %s", raw)
	}
}

func TestStaleLibraryVersionOnFulltextAndSettings(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	item := e.attachment(t, l, "a.pdf")
	l.create("items", m{"itemType": "book", "title": "moves the library on"})
	stale := "1"
	require.Greater(t, l.version(), int64(1))

	r := l.send("PUT", "items/"+item+"/fulltext", m{"content": "x", "indexedChars": 1, "totalChars": 1}, "If-Unmodified-Since-Version", stale)
	assert.Equal(t, 412, r.Status, string(r.Body))
	assert.Equal(t, 404, l.get("items/"+item+"/fulltext").Status, "nothing may have been stored")

	r = l.post("settings", m{"tagColors": m{"value": []m{{"name": "a", "color": "#fff"}}}}, "If-Unmodified-Since-Version", stale)
	assert.Equal(t, 412, r.Status, string(r.Body))
	r = l.send("PUT", "settings/tagColors", m{"value": []m{{"name": "a", "color": "#fff"}}}, "If-Unmodified-Since-Version", stale)
	assert.Equal(t, 412, r.Status, string(r.Body))
	assert.Equal(t, 404, l.get("settings/tagColors").Status)
}

// A client that claims a newer library version than the server has must be
// let through, see checkLibraryVersion.
func TestClientAheadOfServerIsNotRefused(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	ahead := formatInt(l.version() + 100)
	r := l.post("items", []m{{"itemType": "book", "title": "b"}}, "If-Unmodified-Since-Version", ahead)
	require.Equal(t, 200, r.Status, string(r.Body))
	assert.Contains(t, r.json(t)["success"], "0")
	r = l.post("settings", m{"s": m{"value": 1}}, "If-Unmodified-Since-Version", formatInt(l.version()+100))
	assert.Equal(t, 204, r.Status, string(r.Body))
}

func TestBatchKeepsGoingAfterBadEntry(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	for name, bad := range map[string]any{
		"not an object": 5,
		"invalid key":   m{"key": "nope", "itemType": "book"},
	} {
		t.Run(name, func(t *testing.T) {
			r := l.post("items", []any{bad, m{"itemType": "book", "title": "fine"}})
			require.Equal(t, 200, r.Status, string(r.Body))
			j := r.json(t)
			assert.Contains(t, j["failed"], "0")
			assert.Contains(t, j["success"], "1", "the valid object after a bad one must still be written")
		})
	}
}

// failOrphans has to look at every entry, also those after an unrelated or
// a failed one.
func TestBatchFailsChildOfFailedParent(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	const parent = "PARENTA2"
	r := l.post("items", []m{
		{"itemType": "book", "title": "unrelated"},
		{"key": parent, "itemType": "notARealType"},
		{"itemType": "note", "note": "child", "parentItem": parent},
	})
	require.Equal(t, 200, r.Status, string(r.Body))
	j := r.json(t)
	assert.Contains(t, j["success"], "0")
	failed := j["failed"].(m)
	require.Contains(t, failed, "1")
	require.Contains(t, failed, "2", "the child of a parent that failed must not be saved: %s", r.Body)
	assert.EqualValues(t, 409, failed["2"].(m)["code"])
}

func TestFileFieldDefaults(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	note := l.create("items", m{"itemType": "note", "note": "host"})
	cases := []struct {
		name  string
		item  m
		files bool
	}{
		{"book", m{"itemType": "book", "title": "b"}, false},
		{"note", m{"itemType": "note", "note": "n"}, false},
		{"linked_url", m{"itemType": "attachment", "linkMode": "linked_url", "title": "l", "url": "https://example.org"}, false},
		{"imported_file", m{"itemType": "attachment", "linkMode": "imported_file", "title": "f", "filename": "f.pdf"}, true},
		{"imported_url", m{"itemType": "attachment", "linkMode": "imported_url", "title": "u", "url": "https://example.org", "filename": "u.html"}, true},
		{"embedded_image", m{"itemType": "attachment", "linkMode": "embedded_image", "parentItem": note}, true},
	}
	for _, tc := range cases {
		d := l.dataOf("items", l.create("items", tc.item))
		for _, f := range []string{"md5", "mtime"} {
			v, ok := d[f]
			if tc.files {
				assert.True(t, ok, "%s must have %s", tc.name, f)
				assert.Nil(t, v, "%s %s", tc.name, f)
			} else {
				assert.False(t, ok, "%s must not have %s", tc.name, f)
			}
		}
	}
}

func TestPutCollectionName(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := l.create("collections", m{"name": "old"})

	r := l.send("PUT", "collections/"+key, m{"parentCollection": false})
	assert.Equal(t, 400, r.Status, "PUT replaces the whole object, so the name is required: %s", r.Body)
	assert.Equal(t, "old", l.dataOf("collections", key)["name"])

	r = l.send("PATCH", "collections/"+key, m{"parentCollection": false})
	assert.Equal(t, 204, r.Status, "PATCH keeps the stored name: %s", r.Body)

	r = l.send("PUT", "collections/"+key, m{"name": "new"})
	require.Equal(t, 204, r.Status, string(r.Body))
	assert.Equal(t, "new", l.dataOf("collections", key)["name"])
}

// zotero.org counts only the children a key may see, so a key without notes
// access must not learn about child notes from numChildren in lists either.
func TestNumChildrenInListsSkipsHiddenNotes(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t, "noter", fullAccess)
	full := e.userLib(t, owner)
	parent := full.create("items", m{"itemType": "book", "title": "p"})
	full.create("items", m{"itemType": "note", "note": "hidden", "parentItem": parent})
	full.create("items", m{"itemType": "attachment", "linkMode": "linked_url", "title": "a", "url": "https://example.org", "parentItem": parent})

	l := &lib{t: t, e: e, key: e.newKey(t, owner, repo.KeyAccess{Library: true, Write: true, Groups: "all"}), path: full.path}
	numChildren := func(l *lib) any {
		list := l.get("items/top").array(t)
		require.Len(t, list, 1)
		return list[0].(m)["meta"].(m)["numChildren"]
	}
	assert.EqualValues(t, 1, numChildren(l))
	assert.EqualValues(t, 2, numChildren(full))
}
