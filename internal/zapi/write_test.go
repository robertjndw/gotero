package zapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zotero/schema"
)

// fakeLib is an in-memory getter, enough for the pure planning logic.
type fakeLib struct {
	objs map[repo.ObjectType]map[string]repo.Object
}

func newFakeLib() *fakeLib {
	return &fakeLib{objs: map[repo.ObjectType]map[string]repo.Object{}}
}

func (f *fakeLib) put(t repo.ObjectType, key string, version int64, data string) {
	if f.objs[t] == nil {
		f.objs[t] = map[string]repo.Object{}
	}
	f.objs[t][key] = repo.Object{Key: key, Version: version, Data: json.RawMessage(data)}
}

func (f *fakeLib) Get(_ context.Context, t repo.ObjectType, keys []string) (map[string]repo.Object, error) {
	out := map[string]repo.Object{}
	for _, k := range keys {
		if o, ok := f.objs[t][k]; ok {
			out[k] = o
		}
	}
	return out, nil
}

var fixedNow = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

func opts(kind repo.ObjectType, replace bool) planOpts {
	n := 0
	return planOpts{
		Kind: kind, Schema: schema.Default(), Now: fixedNow, Replace: replace,
		NewKey: func() string { n++; return "GEN" + string(rune('A'+n)) + "AAAA" + "4" },
	}
}

func raws(t *testing.T, objs ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(objs))
	for i, o := range objs {
		out[i] = json.RawMessage(o)
	}
	return out
}

func plan(t *testing.T, f *fakeLib, o planOpts, objs ...string) []outcome {
	t.Helper()
	res, err := planWrite(context.Background(), f, o, raws(t, objs...))
	require.NoError(t, err)
	require.Len(t, res, len(objs))
	return res
}

func dataOf(t *testing.T, o outcome) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(o.Obj.Data, &m))
	return m
}

const storedBook = `{"itemType":"book","title":"old","abstractNote":"keep","tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-02T00:00:00Z"}`

func TestPlanItemCreate(t *testing.T) {
	res := plan(t, newFakeLib(), opts(repo.TypeItem, false),
		`{"itemType":"book","title":"hello","tags":[{"tag":"a"},{"tag":"b","type":1}]}`)
	o := res[0]
	require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
	assert.Regexp(t, `^[2-9A-Z]{8}$|^GEN`, o.Key)
	d := dataOf(t, o)
	assert.Equal(t, "2025-03-04T05:06:07Z", d["dateAdded"])
	assert.Equal(t, "2025-03-04T05:06:07Z", d["dateModified"])
	assert.Equal(t, []any{}, d["collections"])
	assert.Equal(t, map[string]any{}, d["relations"])
	assert.NotContains(t, d, "key")
	assert.NotContains(t, d, "version")
	assert.Equal(t, "book", o.Obj.ItemType)
	assert.Equal(t, []repo.ItemTag{{Tag: "a"}, {Tag: "b", Type: 1}}, o.Obj.Tags)
	assert.NotEmpty(t, o.Obj.ContentHash)
}

func TestPlanKeepsClientDates(t *testing.T) {
	res := plan(t, newFakeLib(), opts(repo.TypeItem, false),
		`{"itemType":"book","dateAdded":"2019-01-01T00:00:00Z","dateModified":"2019-02-02T00:00:00Z"}`)
	d := dataOf(t, res[0])
	assert.Equal(t, "2019-01-01T00:00:00Z", d["dateAdded"])
	assert.Equal(t, "2019-02-02T00:00:00Z", d["dateModified"])
}

func TestPlanAttachmentDefaults(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantNull bool
	}{
		{"imported file gets null file fields", `{"itemType":"attachment","linkMode":"imported_file","filename":"a.pdf"}`, true},
		{"linked url has none", `{"itemType":"attachment","linkMode":"linked_url","url":"http://x"}`, false},
		{"webdav client values are kept", `{"itemType":"attachment","linkMode":"imported_file","md5":"abc","mtime":5}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := dataOf(t, plan(t, newFakeLib(), opts(repo.TypeItem, false), c.body)[0])
			if c.wantNull {
				v, ok := d["md5"]
				assert.True(t, ok)
				assert.Nil(t, v)
				v, ok = d["mtime"]
				assert.True(t, ok)
				assert.Nil(t, v)
			}
			if c.name == "webdav client values are kept" {
				assert.Equal(t, "abc", d["md5"])
				assert.EqualValues(t, 5, d["mtime"])
			}
		})
	}
}

func TestPlanVersionRules(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "AAAAAAA2", 7, storedBook)
	cases := []struct {
		name   string
		body   string
		status outcomeStatus
		code   int
	}{
		{"missing object with version is 404", `{"key":"BBBBBBB2","version":3,"itemType":"book"}`, outFailed, 404},
		{"stale version is 412", `{"key":"AAAAAAA2","version":6,"title":"x"}`, outFailed, 412},
		{"client ahead of stored version is accepted", `{"key":"AAAAAAA2","version":8,"title":"x"}`, outUpdated, 0},
		{"explicit version zero on existing object is 412", `{"key":"AAAAAAA2","version":0,"title":"x"}`, outFailed, 412},
		{"version zero retry of identical content is unchanged", `{"key":"AAAAAAA2","version":0,"title":"old","itemType":"book"}`, outUnchanged, 0},
		{"version zero with a bad field still 412", `{"key":"AAAAAAA2","version":0,"bogus":"x"}`, outFailed, 412},
		{"stale identical content is still 412", `{"key":"AAAAAAA2","version":6,"title":"old","itemType":"book"}`, outFailed, 412},
		{"current version updates", `{"key":"AAAAAAA2","version":7,"title":"x"}`, outUpdated, 0},
		{"no version overwrites", `{"key":"AAAAAAA2","title":"x"}`, outUpdated, 0},
		{"version zero creates", `{"key":"BBBBBBB2","version":0,"itemType":"book"}`, outCreated, 0},
		{"negative version is 400", `{"key":"BBBBBBB2","version":-1,"itemType":"book"}`, outFailed, 400},
		{"fractional version is 400", `{"key":"BBBBBBB2","version":1.5,"itemType":"book"}`, outFailed, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := plan(t, f, opts(repo.TypeItem, false), c.body)[0]
			assert.Equal(t, c.status, o.Status, "%+v", o.Fail)
			assert.Equal(t, c.code, o.Fail.Code)
			if c.status == outFailed {
				assert.NotEmpty(t, o.Fail.Message)
			}
		})
	}
}

func TestPlanUnchangedAndMerge(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "AAAAAAA2", 7, storedBook)

	t.Run("identical resend is unchanged", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","version":7,`+storedBook[1:])[0]
		assert.Equal(t, outUnchanged, o.Status)
		assert.Equal(t, "AAAAAAA2", o.Key)
	})
	t.Run("reordered and respaced resend is unchanged", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{ "title" : "old", "key":"AAAAAAA2", "itemType":"book" }`)[0]
		assert.Equal(t, outUnchanged, o.Status)
	})
	t.Run("patch keeps other fields and bumps dateModified", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","title":"new"}`)[0]
		require.Equal(t, outUpdated, o.Status)
		d := dataOf(t, o)
		assert.Equal(t, "new", d["title"])
		assert.Equal(t, "keep", d["abstractNote"])
		assert.Equal(t, "2020-01-01T00:00:00Z", d["dateAdded"])
		assert.Equal(t, "2025-03-04T05:06:07Z", d["dateModified"])
	})
	t.Run("client dateModified wins", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","title":"new","dateModified":"2021-05-05T00:00:00Z"}`)[0]
		assert.Equal(t, "2021-05-05T00:00:00Z", dataOf(t, o)["dateModified"])
	})
	t.Run("dateAdded is immutable", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","title":"new","dateAdded":"2001-01-01T00:00:00Z"}`)[0]
		assert.Equal(t, "2020-01-01T00:00:00Z", dataOf(t, o)["dateAdded"])
		o = plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","dateAdded":"2001-01-01T00:00:00Z"}`)[0]
		assert.Equal(t, outUnchanged, o.Status)
	})
}

func TestPlanReplace(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "AAAAAAA2", 7, storedBook)
	t.Run("drops omitted fields, keeps dateAdded", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, true), `{"key":"AAAAAAA2","itemType":"book","title":"replaced"}`)[0]
		require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
		d := dataOf(t, o)
		assert.NotContains(t, d, "abstractNote")
		assert.Equal(t, "2020-01-01T00:00:00Z", d["dateAdded"])
		assert.Equal(t, "2025-03-04T05:06:07Z", d["dateModified"])
	})
	t.Run("itemType is required", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, true), `{"key":"AAAAAAA2","title":"x"}`)[0]
		assert.Equal(t, outFailed, o.Status)
		assert.Equal(t, 400, o.Fail.Code)
	})
	t.Run("same content is unchanged", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, true), `{"key":"AAAAAAA2",`+storedBook[1:])[0]
		assert.Equal(t, outUnchanged, o.Status)
	})
	t.Run("keeps file metadata of attachments", func(t *testing.T) {
		f := newFakeLib()
		f.put(repo.TypeItem, "AAAAAAA2", 1, `{"itemType":"attachment","linkMode":"imported_file","md5":"abc","mtime":9,"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`)
		o := plan(t, f, opts(repo.TypeItem, true), `{"key":"AAAAAAA2","itemType":"attachment","linkMode":"imported_file","title":"t"}`)[0]
		require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
		d := dataOf(t, o)
		assert.Equal(t, "abc", d["md5"])
		assert.EqualValues(t, 9, d["mtime"])
	})
}

func TestPlanItemValidation(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "PARENTA2", 1, storedBook)
	f.put(repo.TypeCollection, "CCCCAAA2", 1, `{"name":"c"}`)
	cases := []struct {
		name string
		body string
		code int
	}{
		{"not an object", `[1]`, 400},
		{"bad key", `{"key":"bad!","itemType":"book"}`, 400},
		{"numeric key", `{"key":5,"itemType":"book"}`, 400},
		{"missing itemType", `{"title":"x"}`, 400},
		{"unknown itemType", `{"itemType":"spaceship"}`, 400},
		{"unknown field", `{"itemType":"book","bogus":"x"}`, 400},
		{"field of another type", `{"itemType":"book","websiteTitle":"x"}`, 400},
		{"object as field value", `{"itemType":"book","title":{"a":1}}`, 400},
		{"creators not array", `{"itemType":"book","creators":"me"}`, 400},
		{"creator without type", `{"itemType":"book","creators":[{"name":"x"}]}`, 400},
		{"tags not objects", `{"itemType":"book","tags":["a"]}`, 400},
		{"tag without name", `{"itemType":"book","tags":[{"type":1}]}`, 400},
		{"bad tag type", `{"itemType":"book","tags":[{"tag":"a","type":2}]}`, 400},
		{"collections not keys", `{"itemType":"book","collections":["no"]}`, 400},
		{"unknown collection", `{"itemType":"book","collections":["ZZZZZZZ2"]}`, 409},
		{"relations array", `{"itemType":"book","relations":[1]}`, 400},
		{"lastRead on a book", `{"itemType":"book","lastRead":5}`, 400},
		{"lastRead not an integer", `{"itemType":"attachment","linkMode":"imported_file","lastRead":"5"}`, 400},
		{"lastRead fractional", `{"itemType":"attachment","linkMode":"imported_file","lastRead":1.5}`, 400},
		{"path on imported file", `{"itemType":"attachment","linkMode":"imported_file","path":"/a.pdf"}`, 400},
		{"path not a string", `{"itemType":"attachment","linkMode":"linked_file","path":5}`, 400},
		{"mtime not an integer", `{"itemType":"attachment","linkMode":"imported_file","mtime":"5"}`, 400},
		{"bad dateAdded", `{"itemType":"book","dateAdded":"yesterday"}`, 400},
		{"bad linkMode", `{"itemType":"attachment","linkMode":"teleport"}`, 400},
		{"bad deleted", `{"itemType":"book","deleted":"maybe"}`, 400},
		{"book as child", `{"itemType":"book","parentItem":"PARENTA2"}`, 400},
		{"parent is not a key", `{"itemType":"note","parentItem":"nope"}`, 400},
		{"parent missing", `{"itemType":"note","parentItem":"ZZZZZZZ2"}`, 409},
		{"self parent", `{"key":"ABCDEFG2","itemType":"note","parentItem":"ABCDEFG2"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := plan(t, f, opts(repo.TypeItem, false), c.body)[0]
			assert.Equal(t, outFailed, o.Status)
			assert.Equal(t, c.code, o.Fail.Code, o.Fail.Message)
		})
	}

	t.Run("valid child note and attachment", func(t *testing.T) {
		for _, b := range []string{
			`{"itemType":"note","note":"<p>x</p>","parentItem":"PARENTA2"}`,
			`{"itemType":"attachment","linkMode":"linked_url","parentItem":"PARENTA2"}`,
		} {
			o := plan(t, f, opts(repo.TypeItem, false), b)[0]
			require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
			assert.Equal(t, "PARENTA2", o.Obj.ParentKey)
		}
	})
	t.Run("parent created in the same request", func(t *testing.T) {
		res := plan(t, newFakeLib(), opts(repo.TypeItem, false),
			`{"key":"CHILDAA2","itemType":"note","parentItem":"PARENTB2"}`,
			`{"key":"PARENTB2","itemType":"book"}`)
		assert.Equal(t, outCreated, res[0].Status, "%+v", res[0].Fail)
		assert.Equal(t, outCreated, res[1].Status)
	})
	t.Run("existing collection accepted", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"itemType":"book","collections":["CCCCAAA2"]}`)[0]
		assert.Equal(t, outCreated, o.Status, "%+v", o.Fail)
	})
	t.Run("base field names are accepted", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"itemType":"webpage","publicationTitle":"x","title":"y"}`)[0]
		assert.NotEqual(t, outFailed, o.Status, "%+v", o.Fail)
	})
}

func TestPlanDeletedAndParentFlags(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "PARENTA2", 1, storedBook)
	f.put(repo.TypeItem, "CHILDAA2", 2, `{"itemType":"note","note":"x","parentItem":"PARENTA2","deleted":1,"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`)

	o := plan(t, newFakeLib(), opts(repo.TypeItem, false), `{"itemType":"book","deleted":true}`)[0]
	assert.True(t, o.Obj.InTrash)
	assert.EqualValues(t, 1, dataOf(t, o)["deleted"])

	o = plan(t, f, opts(repo.TypeItem, false), `{"key":"CHILDAA2","deleted":0}`)[0]
	require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
	assert.False(t, o.Obj.InTrash)
	assert.NotContains(t, dataOf(t, o), "deleted")

	o = plan(t, f, opts(repo.TypeItem, false), `{"key":"CHILDAA2","parentItem":false}`)[0]
	require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
	assert.Empty(t, o.Obj.ParentKey)
	assert.NotContains(t, dataOf(t, o), "parentItem")

	// parentItem false on a top-level item is a no-op, not a change.
	o = plan(t, f, opts(repo.TypeItem, false), `{"key":"PARENTA2","parentItem":false,"deleted":false}`)[0]
	assert.Equal(t, outUnchanged, o.Status)
}

func TestPlanCollections(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeCollection, "CCCCAAA2", 3, `{"name":"c","parentCollection":false,"relations":{}}`)
	cases := []struct {
		name   string
		body   string
		status outcomeStatus
		code   int
	}{
		{"create", `{"name":"new"}`, outCreated, 0},
		{"name required", `{"parentCollection":false}`, outFailed, 400},
		{"empty name", `{"name":""}`, outFailed, 400},
		{"unknown property", `{"name":"x","color":"red"}`, outFailed, 400},
		{"parent must be key or false", `{"name":"x","parentCollection":"no"}`, outFailed, 400},
		{"self parent is 409", `{"key":"SELFAAA2","name":"x","parentCollection":"SELFAAA2"}`, outFailed, 409},
		{"missing parent is 409", `{"name":"x","parentCollection":"ZZZZZZZ2"}`, outFailed, 409},
		{"existing parent", `{"name":"x","parentCollection":"CCCCAAA2"}`, outCreated, 0},
		{"rename existing", `{"key":"CCCCAAA2","version":3,"name":"renamed"}`, outUpdated, 0},
		{"same content", `{"key":"CCCCAAA2","version":3,"name":"c"}`, outUnchanged, 0},
		{"stale", `{"key":"CCCCAAA2","version":2,"name":"c"}`, outFailed, 412},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := plan(t, f, opts(repo.TypeCollection, false), c.body)[0]
			assert.Equal(t, c.status, o.Status, "%+v", o.Fail)
			assert.Equal(t, c.code, o.Fail.Code)
		})
	}
	t.Run("defaults", func(t *testing.T) {
		d := dataOf(t, plan(t, f, opts(repo.TypeCollection, false), `{"name":"new"}`)[0])
		assert.Equal(t, false, d["parentCollection"])
		assert.Equal(t, map[string]any{}, d["relations"])
	})
	t.Run("parent in same request", func(t *testing.T) {
		res := plan(t, newFakeLib(), opts(repo.TypeCollection, false),
			`{"key":"CHILDAA2","name":"child","parentCollection":"PARENTB2"}`, `{"key":"PARENTB2","name":"parent"}`)
		assert.Equal(t, outCreated, res[0].Status, "%+v", res[0].Fail)
	})
}

func TestPlanSearches(t *testing.T) {
	f := newFakeLib()
	cond := `[{"condition":"title","operator":"contains","value":"go"}]`
	o := plan(t, f, opts(repo.TypeSearch, false), `{"name":"s","conditions":`+cond+`}`)[0]
	assert.Equal(t, outCreated, o.Status, "%+v", o.Fail)
	for _, bad := range []string{`{"conditions":[]}`, `{"name":"s","conditions":"x"}`, `{"name":"s","conditions":[{"operator":"is"}]}`, `{"name":"s","extra":1}`} {
		o := plan(t, f, opts(repo.TypeSearch, false), bad)[0]
		assert.Equal(t, outFailed, o.Status, bad)
		assert.Equal(t, 400, o.Fail.Code, bad)
	}
}

func TestPlanBatchBookkeeping(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "AAAAAAA2", 7, storedBook)
	res := plan(t, f, opts(repo.TypeItem, false),
		`{"itemType":"book","title":"new"}`,
		`{"key":"AAAAAAA2","version":7,`+storedBook[1:],
		`{"key":"AAAAAAA2","title":"dup"}`,
		`{"itemType":"nope"}`,
		`{"key":"BBBBBBB2","version":9,"itemType":"book"}`,
	)
	got := []outcomeStatus{res[0].Status, res[1].Status, res[2].Status, res[3].Status, res[4].Status}
	assert.Equal(t, []outcomeStatus{outCreated, outUnchanged, outFailed, outFailed, outFailed}, got)
	assert.Equal(t, 409, res[2].Fail.Code)
	for i, r := range res {
		assert.Equal(t, i, r.Index)
		assert.NotEmpty(t, r.Key)
	}
	assert.Equal(t, "AAAAAAA2", res[2].Fail.Key)
	assert.Equal(t, 404, res[4].Fail.Code)
}

func TestObjectFromDataHashIgnoresKeyAndVersion(t *testing.T) {
	var a, b zotero.OrderedMap
	require.NoError(t, json.Unmarshal([]byte(`{"key":"AAAAAAA2","version":3,"itemType":"book","title":"t"}`), &a))
	require.NoError(t, json.Unmarshal([]byte(`{"title":"t","itemType":"book"}`), &b))
	oa, err := ObjectFromData(repo.TypeItem, "AAAAAAA2", a)
	require.NoError(t, err)
	ob, err := ObjectFromData(repo.TypeItem, "AAAAAAA2", b)
	require.NoError(t, err)
	assert.Equal(t, oa.ContentHash, ob.ContentHash)
	assert.NotContains(t, string(oa.Data), `"key"`)
}

func TestCreatorSummary(t *testing.T) {
	s := schema.Default()
	cases := []struct{ creators, want string }{
		{`[{"creatorType":"author","lastName":"Smith"}]`, "Smith"},
		{`[{"creatorType":"author","lastName":"Smith"},{"creatorType":"author","lastName":"Jones"}]`, "Smith and Jones"},
		{`[{"creatorType":"author","lastName":"A"},{"creatorType":"author","lastName":"B"},{"creatorType":"author","lastName":"C"}]`, "A et al."},
		{`[{"creatorType":"editor","lastName":"Ed"},{"creatorType":"author","name":"Corp"}]`, "Corp"},
		{`[{"creatorType":"editor","lastName":"Ed"}]`, "Ed"},
		{`[]`, ""},
		{`"junk"`, ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, creatorSummary(json.RawMessage(c.creators), "book", s), c.creators)
	}
}

const storedArticle = `{"itemType":"journalArticle","title":"t","publicationTitle":"Nature","volume":"3","creators":[{"creatorType":"reviewedAuthor","firstName":"A","lastName":"B"},{"creatorType":"author","name":"C"}],"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-02T00:00:00Z"}`

func TestPlanItemTypeChange(t *testing.T) {
	f := newFakeLib()
	f.put(repo.TypeItem, "AAAAAAA2", 7, storedArticle)

	t.Run("fields the new type lacks are dropped", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","version":7,"itemType":"book"}`)[0]
		require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
		d := dataOf(t, o)
		assert.Equal(t, "book", d["itemType"])
		assert.NotContains(t, d, "publicationTitle")
		assert.Equal(t, "t", d["title"])
		assert.Equal(t, "3", d["volume"]) // volume exists on books too
		cs := d["creators"].([]any)
		assert.Equal(t, "author", cs[0].(map[string]any)["creatorType"], "reviewedAuthor is not a book role")
		assert.Equal(t, "author", cs[1].(map[string]any)["creatorType"])
	})
	t.Run("base-mapped fields move", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","version":7,"itemType":"webpage"}`)[0]
		require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
		d := dataOf(t, o)
		assert.NotContains(t, d, "publicationTitle")
		assert.Equal(t, "Nature", d["websiteTitle"])
		assert.NotContains(t, d, "volume")
	})
	t.Run("a target that is already set wins", func(t *testing.T) {
		g := newFakeLib()
		g.put(repo.TypeItem, "AAAAAAA2", 7,
			`{"itemType":"bookSection","bookTitle":"Book","publicationTitle":"Other","tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-02T00:00:00Z"}`)
		d := dataOf(t, plan(t, g, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","version":7,"itemType":"journalArticle"}`)[0])
		assert.Equal(t, "Other", d["publicationTitle"])
		assert.NotContains(t, d, "bookTitle")
	})
	t.Run("PUT converts the kept server fields too", func(t *testing.T) {
		g := newFakeLib()
		g.put(repo.TypeItem, "AAAAAAA2", 7,
			`{"itemType":"attachment","linkMode":"imported_file","filename":"a.pdf","md5":"abc","mtime":5,"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-02T00:00:00Z"}`)
		d := dataOf(t, plan(t, g, opts(repo.TypeItem, true), `{"key":"AAAAAAA2","version":7,"itemType":"book","title":"now a book"}`)[0])
		assert.NotContains(t, d, "md5")
		assert.NotContains(t, d, "mtime")
	})
	t.Run("same type changes nothing", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","version":7,"itemType":"journalArticle"}`)[0]
		assert.Equal(t, outUnchanged, o.Status)
	})
}

func TestPlanNullCollectionsAreNormalized(t *testing.T) {
	d := dataOf(t, plan(t, newFakeLib(), opts(repo.TypeItem, false),
		`{"itemType":"book","tags":null,"collections":null,"creators":null,"relations":null}`)[0])
	assert.Equal(t, []any{}, d["tags"])
	assert.Equal(t, []any{}, d["collections"])
	assert.Equal(t, []any{}, d["creators"])
	assert.Equal(t, map[string]any{}, d["relations"])

	f := newFakeLib()
	f.put(repo.TypeItem, "AAAAAAA2", 7, storedBook)
	o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","version":7,"tags":null}`)[0]
	assert.Equal(t, outUnchanged, o.Status, "null tags on an untagged item is already the empty state")

	for _, body := range []string{
		`{"itemType":"book","tags":"x"}`, `{"itemType":"book","collections":{}}`,
		`{"itemType":"book","creators":{}}`, `{"itemType":"book","relations":"x"}`,
		`{"itemType":"book","tags":5}`, `{"itemType":"book","creators":[1]}`,
	} {
		o := plan(t, newFakeLib(), opts(repo.TypeItem, false), body)[0]
		assert.Equal(t, outFailed, o.Status, body)
		assert.Equal(t, 400, o.Fail.Code, body)
	}

	c := dataOf(t, plan(t, newFakeLib(), opts(repo.TypeCollection, false), `{"name":"c","relations":null}`)[0])
	assert.Equal(t, map[string]any{}, c["relations"])
}

func TestPlanAttachmentExtras(t *testing.T) {
	f := newFakeLib()
	const stored = `{"itemType":"attachment","linkMode":"imported_file","md5":"abc","mtime":9,"lastRead":100,"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`
	f.put(repo.TypeItem, "AAAAAAA2", 3, stored)
	f.put(repo.TypeItem, "LINKEDA2", 3, `{"itemType":"attachment","linkMode":"linked_file","path":"/x","tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`)

	t.Run("lastRead is stored", func(t *testing.T) {
		o := plan(t, newFakeLib(), opts(repo.TypeItem, false), `{"itemType":"attachment","linkMode":"imported_file","lastRead":1700000000}`)[0]
		require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
		assert.EqualValues(t, 1700000000, dataOf(t, o)["lastRead"])
	})
	for _, clear := range []string{"null", `""`, "false"} {
		t.Run("lastRead "+clear+" clears it", func(t *testing.T) {
			o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","lastRead":`+clear+`}`)[0]
			require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
			assert.NotContains(t, dataOf(t, o), "lastRead")
		})
	}
	t.Run("clearing a lastRead that was never set is unchanged", func(t *testing.T) {
		g := newFakeLib()
		g.put(repo.TypeItem, "BBBBBBB2", 1, `{"itemType":"attachment","linkMode":"imported_file","tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`)
		o := plan(t, g, opts(repo.TypeItem, false), `{"key":"BBBBBBB2","lastRead":null}`)[0]
		assert.Equal(t, outUnchanged, o.Status)
	})
	t.Run("lastRead is refused in group libraries", func(t *testing.T) {
		g := opts(repo.TypeItem, false)
		g.Group = true
		o := plan(t, newFakeLib(), g, `{"itemType":"attachment","linkMode":"imported_file","lastRead":5}`)[0]
		require.Equal(t, outFailed, o.Status)
		assert.Equal(t, 400, o.Fail.Code)
		assert.Equal(t, "lastRead can only be set in user libraries", o.Fail.Message)
	})
	t.Run("clearing lastRead is fine in group libraries", func(t *testing.T) {
		g := opts(repo.TypeItem, false)
		g.Group = true
		o := plan(t, newFakeLib(), g, `{"itemType":"attachment","linkMode":"imported_file","lastRead":null}`)[0]
		require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
		assert.NotContains(t, dataOf(t, o), "lastRead")
	})
	t.Run("path on linked_file", func(t *testing.T) {
		o := plan(t, newFakeLib(), opts(repo.TypeItem, false), `{"itemType":"attachment","linkMode":"linked_file","path":"attachments:a.pdf"}`)[0]
		require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
		assert.Equal(t, "attachments:a.pdf", dataOf(t, o)["path"])
	})
	t.Run("path update uses the stored linkMode", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"LINKEDA2","path":"/y"}`)[0]
		require.Equal(t, outUpdated, o.Status, "%+v", o.Fail)
		o = plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","path":"/y"}`)[0]
		assert.Equal(t, outFailed, o.Status)
	})
	t.Run("path is refused in group libraries", func(t *testing.T) {
		g := opts(repo.TypeItem, false)
		g.Group = true
		o := plan(t, newFakeLib(), g, `{"itemType":"attachment","linkMode":"linked_file","path":"/a"}`)[0]
		require.Equal(t, outFailed, o.Status)
		assert.Equal(t, 400, o.Fail.Code)
		assert.Equal(t, "Linked files can only be added to user libraries", o.Fail.Message)
	})
	for _, v := range []string{"null", `""`} {
		t.Run("md5 and mtime "+v+" keep stored values", func(t *testing.T) {
			body := `{"key":"AAAAAAA2","md5":` + v + `,"mtime":` + v + `}`
			assert.Equal(t, outUnchanged, plan(t, f, opts(repo.TypeItem, false), body)[0].Status)

			put := `{"key":"AAAAAAA2","itemType":"attachment","linkMode":"imported_file","lastRead":100,"md5":` + v + `,"mtime":` + v + `}`
			o := plan(t, f, opts(repo.TypeItem, true), put)[0]
			assert.Equal(t, outUnchanged, o.Status, "%+v", o.Fail)
		})
	}
	t.Run("empty md5 and mtime stay null when never set", func(t *testing.T) {
		for _, replace := range []bool{false, true} {
			o := plan(t, newFakeLib(), opts(repo.TypeItem, replace), `{"itemType":"attachment","linkMode":"imported_file","md5":"","mtime":""}`)[0]
			require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
			d := dataOf(t, o)
			assert.Nil(t, d["md5"])
			assert.Nil(t, d["mtime"])
		}
		g := newFakeLib()
		g.put(repo.TypeItem, "BBBBBBB2", 1, `{"itemType":"attachment","linkMode":"imported_file","md5":null,"mtime":null,"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`)
		assert.Equal(t, outUnchanged, plan(t, g, opts(repo.TypeItem, false), `{"key":"BBBBBBB2","md5":"","mtime":""}`)[0].Status)
	})
	t.Run("real md5 and mtime still replace", func(t *testing.T) {
		d := dataOf(t, plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","md5":"def","mtime":10}`)[0])
		assert.Equal(t, "def", d["md5"])
		assert.EqualValues(t, 10, d["mtime"])
	})
}

func TestPlanEmptyTags(t *testing.T) {
	f := newFakeLib()
	const stored = `{"itemType":"book","title":"old","tags":[{"tag":"keep"}],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-02T00:00:00Z"}`
	f.put(repo.TypeItem, "AAAAAAA2", 2, stored)

	t.Run("empty and blank tags are dropped on create", func(t *testing.T) {
		o := plan(t, newFakeLib(), opts(repo.TypeItem, false), `{"itemType":"book","tags":[{"tag":""},{"tag":"  \t"},{"tag":"0"},{"tag":"a","type":1}]}`)[0]
		require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
		assert.Equal(t, []repo.ItemTag{{Tag: "0"}, {Tag: "a", Type: 1}}, o.Obj.Tags)
	})
	t.Run("an item whose only change is an empty tag is unchanged", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","tags":[{"tag":"keep"},{"tag":""}]}`)[0]
		assert.Equal(t, outUnchanged, o.Status, "%+v", o.Fail)
		o = plan(t, f, opts(repo.TypeItem, true), `{"key":"AAAAAAA2",`+stored[1:len(stored)-1]+`,"tags":[{"tag":"keep"},{"tag":" "}]}`)[0]
		assert.Equal(t, outUnchanged, o.Status, "%+v", o.Fail)
	})
	t.Run("bad tag type still fails next to an empty tag", func(t *testing.T) {
		o := plan(t, f, opts(repo.TypeItem, false), `{"key":"AAAAAAA2","tags":[{"tag":""},{"tag":"a","type":2}]}`)[0]
		assert.Equal(t, 400, o.Fail.Code)
	})
}

func TestPlanRelationsEmptyArray(t *testing.T) {
	o := plan(t, newFakeLib(), opts(repo.TypeItem, false), `{"itemType":"book","relations":[]}`)[0]
	require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
	assert.Equal(t, map[string]any{}, dataOf(t, o)["relations"])
	o = plan(t, newFakeLib(), opts(repo.TypeCollection, false), `{"name":"c","relations":[]}`)[0]
	require.Equal(t, outCreated, o.Status, "%+v", o.Fail)
	assert.Equal(t, map[string]any{}, dataOf(t, o)["relations"])
}
