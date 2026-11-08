package zapi_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// newKey adds another API key for an existing user.
func (e *testEnv) newKey(t testing.TB, u user, access repo.KeyAccess) string {
	t.Helper()
	raw := zotero.NewAPIKey()
	_, err := e.Store.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: auth.HashToken(raw), Prefix: raw[:4], Name: "extra", Access: access})
	require.NoError(t, err)
	return raw
}

// The exists shortcut may only count blobs in libraries the key could read,
// even when they belong to the same user.
func TestExistsRespectsKeyScope(t *testing.T) {
	t.Run("groups none ignores group files", func(t *testing.T) {
		e := newEnv(t)
		alice := e.newUser(t, "alice", fullAccess)
		g, err := e.Store.Groups().Create(ctx(), repo.Group{OwnerID: alice.ID, Name: "g", LibraryReading: "members", LibraryEditing: "members", FileEditing: "members"})
		require.NoError(t, err)
		gl := e.groupLib(t, g.ID, alice)
		p := newPayload("group.pdf", []byte("only in the group"))
		e.store(t, gl, e.attachment(t, gl, "group.pdf"), p)

		noGroups := e.newKey(t, alice, repo.KeyAccess{Library: true, Write: true, Files: true, Groups: "none"})
		ul := &lib{t: t, e: e, key: noGroups, path: e.userLib(t, alice).path}
		key := e.attachment(t, ul, "group.pdf")
		r := e.form(t, ul, key, p.form(), "If-None-Match", "*")
		require.Equal(t, 200, r.Status, string(r.Body))
		assert.NotContains(t, r.json(t), "exists", "a key without group access must not learn the group has this file")
	})
	t.Run("no library access ignores personal files", func(t *testing.T) {
		e := newEnv(t)
		alice := e.newUser(t, "alice", fullAccess)
		ul := e.userLib(t, alice)
		p := newPayload("mine.pdf", []byte("only in my library"))
		e.store(t, ul, e.attachment(t, ul, "mine.pdf"), p)

		g, err := e.Store.Groups().Create(ctx(), repo.Group{OwnerID: alice.ID, Name: "g", LibraryReading: "members", LibraryEditing: "members", FileEditing: "members"})
		require.NoError(t, err)
		groupOnly := e.newKey(t, alice, repo.KeyAccess{Library: false, Write: true, Files: true, Groups: "all"})
		gl := &lib{t: t, e: e, key: groupOnly, path: e.groupLib(t, g.ID, alice).path}
		key := e.attachment(t, gl, "mine.pdf")
		r := e.form(t, gl, key, p.form(), "If-None-Match", "*")
		require.Equal(t, 200, r.Status, string(r.Body))
		assert.NotContains(t, r.json(t), "exists", "a group-only key must not learn what is in the personal library")
	})
}

// A key without notes access must get 403 for a stored note before any
// version check runs, so a stale version cannot be used to read the note's
// version out of a 412 message.
func TestHiddenNoteVersionNotRevealed(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t, "noter", fullAccess)
	full := e.userLib(t, owner)
	full.create("items", m{"itemType": "book", "title": "bump the version"})
	note := full.create("items", m{"itemType": "note", "note": "<p>secret</p>"})
	require.Greater(t, mustInt(t, ver(full.get("items/"+note).Header)), int64(1))

	l := &lib{t: t, e: e, key: e.newKey(t, owner, repo.KeyAccess{Library: true, Write: true, Groups: "all"}), path: full.path}

	r := l.post("items", []m{{"key": note, "version": 1, "note": "x"}})
	require.Equal(t, 200, r.Status, string(r.Body))
	f := r.json(t)["failed"].(m)["0"].(m)
	assert.EqualValues(t, 403, f["code"], "batch write: %v", f)

	r = l.send("PATCH", "items/"+note, m{"note": "x"}, "If-Unmodified-Since-Version", "1")
	assert.Equal(t, 403, r.Status, "PATCH: %s", r.Body)
	r = l.send("PUT", "items/"+note, m{"itemType": "note", "note": "x"}, "If-Unmodified-Since-Version", "1")
	assert.Equal(t, 403, r.Status, "PUT: %s", r.Body)
	r = l.send("DELETE", "items/"+note, nil, "If-Unmodified-Since-Version", "1")
	assert.Equal(t, 403, r.Status, "DELETE: %s", r.Body)
	assert.Equal(t, "<p>secret</p>", full.dataOf("items", note)["note"])

	// A regular item is still writable with the same key.
	book := full.create("items", m{"itemType": "book", "title": "b"})
	r = l.send("PATCH", "items/"+book, m{"title": "changed"})
	assert.Equal(t, 204, r.Status, string(r.Body))
}

// The note checks on delete only apply to keys without notes access.
func TestFullKeyDeletesNotes(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	one := l.create("items", m{"itemType": "note", "note": "one"})
	two := l.create("items", m{"itemType": "note", "note": "two"})
	three := l.create("items", m{"itemType": "note", "note": "three"})

	r := l.send("DELETE", "items/"+one, nil, "If-Unmodified-Since-Version", formatInt(l.version()))
	require.Equal(t, 204, r.Status, string(r.Body))
	r = l.send("DELETE", "items?itemKey="+two+","+three, nil, "If-Unmodified-Since-Version", formatInt(l.version()))
	require.Equal(t, 204, r.Status, string(r.Body))
	for _, k := range []string{one, two, three} {
		assert.Equal(t, 404, l.get("items/"+k).Status, k)
	}
}
