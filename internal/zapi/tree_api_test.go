package zapi_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The planner's tree rules against PostgreSQL, where stored children are
// found through the real queries.
func TestTreeRulesOverHTTP(t *testing.T) {
	e := newEnv(t)
	l := e.userLib(t, e.newUser(t, "alice", fullAccess))

	patch := func(typ, key string, body m) resp {
		t.Helper()
		return l.send("PATCH", typ+"/"+key, body, "If-Unmodified-Since-Version", formatInt(l.version()))
	}

	t.Run("collection cycles across patches", func(t *testing.T) {
		a := l.create("collections", m{"name": "a"})
		b := l.create("collections", m{"name": "b", "parentCollection": a})
		c := l.create("collections", m{"name": "c", "parentCollection": b})

		r := patch("collections", a, m{"parentCollection": b})
		assert.Equal(t, 409, r.Status, string(r.Body))
		r = patch("collections", a, m{"parentCollection": c})
		assert.Equal(t, 409, r.Status, string(r.Body))
		assert.Equal(t, false, l.dataOf("collections", a)["parentCollection"])

		r = patch("collections", c, m{"parentCollection": a})
		assert.Equal(t, 204, r.Status, string(r.Body))
	})

	t.Run("collection cycle in one batch", func(t *testing.T) {
		d := l.create("collections", m{"name": "d"})
		r := l.post("collections", []m{
			{"key": d, "parentCollection": "CYCLEEE2"},
			{"key": "CYCLEEE2", "name": "e", "parentCollection": d},
		})
		require.Equal(t, 200, r.Status, string(r.Body))
		res := r.json(t)
		assert.EqualValues(t, 409, res["failed"].(m)["0"].(m)["code"])
		assert.Contains(t, res["success"], "1")
	})

	t.Run("items", func(t *testing.T) {
		book := l.create("items", m{"itemType": "book"})
		note := l.create("items", m{"itemType": "note", "note": "x", "parentItem": book})
		standalone := l.create("items", m{"itemType": "note", "note": "y"})

		r := patch("items", note, m{"itemType": "book"})
		assert.Equal(t, 400, r.Status, string(r.Body))
		r = patch("items", book, m{"itemType": "note", "note": "z"})
		assert.Equal(t, 400, r.Status, string(r.Body))
		r = l.post("items", []m{{"itemType": "note", "note": "n", "parentItem": standalone}})
		assert.EqualValues(t, 400, r.json(t)["failed"].(m)["0"].(m)["code"], string(r.Body))
		r = l.post("items", []m{{"itemType": "annotation", "annotationType": "highlight", "parentItem": book}})
		assert.EqualValues(t, 400, r.json(t)["failed"].(m)["0"].(m)["code"], string(r.Body))

		// The flows Zotero desktop relies on still work.
		other := l.create("items", m{"itemType": "book"})
		assert.Equal(t, 204, patch("items", note, m{"parentItem": other}).Status)
		assert.Equal(t, 204, patch("items", note, m{"parentItem": false}).Status)
		assert.Equal(t, 204, patch("items", note, m{"parentItem": book}).Status)
		assert.Equal(t, 204, patch("items", note, m{"deleted": 1}).Status)
		// The trashed note still counts as a child.
		r = patch("items", book, m{"itemType": "note", "note": "z"})
		assert.Equal(t, 400, r.Status, string(r.Body))
		r = l.post("items", []m{
			{"key": "ANNUTATE", "itemType": "annotation", "annotationType": "highlight", "parentItem": "PDFFILE2"},
			{"key": "PDFFILE2", "itemType": "attachment", "linkMode": "imported_file", "parentItem": "PARENTB2"},
			{"key": "PARENTB2", "itemType": "book"},
		})
		require.Equal(t, 200, r.Status, string(r.Body))
		assert.Len(t, r.json(t)["success"], 3, string(r.Body))
	})
}
