package zapi

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func storedItem(itemType, extra string) string {
	return fmt.Sprintf(`{"itemType":%q%s,"tags":[],"collections":[],"relations":{},"dateAdded":"2020-01-01T00:00:00Z","dateModified":"2020-01-01T00:00:00Z"}`, itemType, extra)
}

func storedColl(parent string) string {
	p := "false"
	if parent != "" {
		p = fmt.Sprintf("%q", parent)
	}
	return fmt.Sprintf(`{"name":"c","parentCollection":%s,"relations":{}}`, p)
}

// treeLib is a small library: a book with a child note and a PDF that has
// an annotation, a standalone note with an embedded image, and a link.
func treeLib() *fakeLib {
	f := newFakeLib()
	f.put(repo.TypeItem, "BUUKAAA2", 1, storedItem("book", ""))
	f.put(repo.TypeItem, "BUUKBBB2", 1, storedItem("book", ""))
	f.put(repo.TypeItem, "NUTEAAA2", 1, storedItem("note", `,"note":"x","parentItem":"BUUKAAA2"`))
	f.put(repo.TypeItem, "NUTEBBB2", 1, storedItem("note", `,"note":"y"`))
	f.put(repo.TypeItem, "PDFAAAA2", 1, storedItem("attachment", `,"linkMode":"imported_file","parentItem":"BUUKAAA2"`))
	f.put(repo.TypeItem, "PDFBBBB2", 1, storedItem("attachment", `,"linkMode":"imported_file"`))
	f.put(repo.TypeItem, "ANNUAAA2", 1, storedItem("annotation", `,"annotationType":"highlight","parentItem":"PDFBBBB2"`))
	f.put(repo.TypeItem, "LINKAAA2", 1, storedItem("attachment", `,"linkMode":"linked_url"`))
	f.put(repo.TypeItem, "IMAGEAA2", 1, storedItem("attachment", `,"linkMode":"embedded_image","parentItem":"NUTEBBB2"`))
	return f
}

func TestPlanItemParentRules(t *testing.T) {
	cases := []struct {
		name string
		objs []string
		// want is the expected HTTP-ish code per object: 0 for saved.
		want []int
	}{
		{"note under note", []string{`{"itemType":"note","note":"x","parentItem":"NUTEBBB2"}`}, []int{400}},
		{"attachment under attachment", []string{`{"itemType":"attachment","linkMode":"linked_url","parentItem":"PDFBBBB2"}`}, []int{400}},
		{"note under annotation", []string{`{"itemType":"note","note":"x","parentItem":"ANNUAAA2"}`}, []int{400}},
		{"annotation under book", []string{`{"itemType":"annotation","annotationType":"highlight","parentItem":"BUUKAAA2"}`}, []int{400}},
		{"annotation under note", []string{`{"itemType":"annotation","annotationType":"highlight","parentItem":"NUTEBBB2"}`}, []int{400}},
		{"annotation under link", []string{`{"itemType":"annotation","annotationType":"highlight","parentItem":"LINKAAA2"}`}, []int{400}},
		{"annotation under pdf", []string{`{"itemType":"annotation","annotationType":"highlight","parentItem":"PDFBBBB2"}`}, []int{0}},
		{"embedded image under note", []string{`{"itemType":"attachment","linkMode":"embedded_image","parentItem":"NUTEBBB2"}`}, []int{0}},
		{"embedded image under child note", []string{`{"itemType":"attachment","linkMode":"embedded_image","parentItem":"NUTEAAA2"}`}, []int{0}},
		{"child note patched to book", []string{`{"key":"NUTEAAA2","itemType":"book"}`}, []int{400}},
		{"child note put as book", []string{`{"key":"NUTEAAA2","itemType":"book","parentItem":"BUUKAAA2"}`}, []int{400}},
		{"child note patched to annotation", []string{`{"key":"NUTEAAA2","itemType":"annotation","annotationType":"highlight"}`}, []int{400}},
		{"book with children becomes a note", []string{`{"key":"BUUKAAA2","itemType":"note","note":"x"}`}, []int{400}},
		{"pdf with annotation becomes a link", []string{`{"key":"PDFBBBB2","linkMode":"linked_url"}`}, []int{400}},
		{"pdf with annotation becomes a note", []string{`{"key":"PDFBBBB2","itemType":"note","note":"x"}`}, []int{400}},
		{"pdf with annotation moves under a book", []string{`{"key":"PDFBBBB2","parentItem":"BUUKBBB2"}`}, []int{0}},
		{"pdf with annotation moves under a note", []string{`{"key":"PDFBBBB2","parentItem":"NUTEBBB2"}`}, []int{400}},
		{"book with children changes regular type", []string{`{"key":"BUUKAAA2","itemType":"thesis"}`}, []int{0}},
		{"note moves to another parent", []string{`{"key":"NUTEAAA2","parentItem":"BUUKBBB2"}`}, []int{0}},
		{"child note becomes standalone", []string{`{"key":"NUTEAAA2","parentItem":false}`}, []int{0}},
		{"child note trashed", []string{`{"key":"NUTEAAA2","deleted":1}`}, []int{0}},
		{"parent after child", []string{
			`{"key":"NEWNUTE2","itemType":"note","note":"x","parentItem":"NEWBUUK2"}`,
			`{"key":"NEWBUUK2","itemType":"book"}`,
		}, []int{0, 0}},
		{"parent before child", []string{
			`{"key":"NEWBUUK2","itemType":"book"}`,
			`{"key":"NEWPDFA2","itemType":"attachment","linkMode":"imported_file","parentItem":"NEWBUUK2"}`,
			`{"key":"NEWANNU2","itemType":"annotation","annotationType":"highlight","parentItem":"NEWPDFA2"}`,
		}, []int{0, 0, 0}},
		{"batch parent is a note", []string{
			`{"key":"NEWNUTE2","itemType":"note","note":"x","parentItem":"NEWNUTF2"}`,
			`{"key":"NEWNUTF2","itemType":"note","note":"y"}`,
		}, []int{400, 0}},
		{"note cycle in one batch", []string{
			`{"key":"NEWNUTE2","itemType":"note","note":"x","parentItem":"NEWNUTF2"}`,
			`{"key":"NEWNUTF2","itemType":"note","note":"y","parentItem":"NEWNUTE2"}`,
		}, []int{400, 409}}, // the second is orphaned by the first failing
		{"batch parent converted to note", []string{
			`{"key":"BUUKBBB2","itemType":"note","note":"x"}`,
			`{"key":"NEWNUTE2","itemType":"note","note":"y","parentItem":"BUUKBBB2"}`,
		}, []int{0, 400}},
		{"stored child cannot follow its parent becoming a note", []string{
			`{"key":"BUUKAAA2","itemType":"note","note":"x"}`,
			`{"key":"NUTEAAA2","note":"still here"}`,
		}, []int{400, 0}},
		{"children moved away first", []string{
			`{"key":"NUTEAAA2","parentItem":"BUUKBBB2"}`,
			`{"key":"PDFAAAA2","parentItem":false}`,
			`{"key":"BUUKAAA2","itemType":"note","note":"x"}`,
		}, []int{0, 0, 0}},
		{"failed parent conversion leaves the stored parent", []string{
			`{"key":"PDFBBBB2","itemType":"attachment","linkMode":"bogus"}`,
			`{"key":"NEWANNU2","itemType":"annotation","annotationType":"highlight","parentItem":"PDFBBBB2"}`,
		}, []int{400, 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := plan(t, treeLib(), opts(repo.TypeItem, false), c.objs...)
			for i, want := range c.want {
				if want == 0 {
					assert.NotEqual(t, outFailed, res[i].Status, "object %d: %+v", i, res[i].Fail)
				} else {
					require.Equal(t, outFailed, res[i].Status, "object %d", i)
					assert.Equal(t, want, res[i].Fail.Code, "object %d: %s", i, res[i].Fail.Message)
				}
			}
		})
	}
}

func TestPlanCollectionCycles(t *testing.T) {
	// A > B > C is stored, D is a separate root.
	lib := func() *fakeLib {
		f := newFakeLib()
		f.put(repo.TypeCollection, "CULLAAA2", 1, storedColl(""))
		f.put(repo.TypeCollection, "CULLBBB2", 1, storedColl("CULLAAA2"))
		f.put(repo.TypeCollection, "CULLCCC2", 1, storedColl("CULLBBB2"))
		f.put(repo.TypeCollection, "CULLDDD2", 1, storedColl(""))
		// X and Y already point at each other, as left behind by older
		// versions that let cycles through.
		f.put(repo.TypeCollection, "CULLXXX2", 1, storedColl("CULLYYY2"))
		f.put(repo.TypeCollection, "CULLYYY2", 1, storedColl("CULLXXX2"))
		return f
	}
	cases := []struct {
		name string
		objs []string
		want []int
	}{
		{"cycle of two", []string{`{"key":"CULLAAA2","parentCollection":"CULLBBB2"}`}, []int{409}},
		{"cycle of three", []string{`{"key":"CULLAAA2","parentCollection":"CULLCCC2"}`}, []int{409}},
		{"move under a sibling tree", []string{`{"key":"CULLAAA2","parentCollection":"CULLDDD2"}`}, []int{0}},
		{"move a subtree up", []string{`{"key":"CULLCCC2","parentCollection":"CULLAAA2"}`}, []int{0}},
		{"cycle within one batch", []string{
			`{"key":"CULLDDD2","parentCollection":"CULLEEE2"}`,
			`{"key":"CULLEEE2","name":"e","parentCollection":"CULLDDD2"}`,
		}, []int{409, 0}},
		{"new collections in one batch, child first", []string{
			`{"key":"CULLFFF2","name":"f","parentCollection":"CULLEEE2"}`,
			`{"key":"CULLEEE2","name":"e","parentCollection":"CULLAAA2"}`,
		}, []int{0, 0}},
		{"swap parent and child in one batch", []string{
			`{"key":"CULLBBB2","parentCollection":false}`,
			`{"key":"CULLAAA2","parentCollection":"CULLBBB2"}`,
		}, []int{0, 0}},
		{"stored cycle can be broken", []string{`{"key":"CULLXXX2","parentCollection":false}`}, []int{0}},
		{"walk into a stored cycle ends", []string{`{"key":"CULLDDD2","parentCollection":"CULLXXX2"}`}, []int{0}},
		{"renaming inside a stored cycle", []string{`{"key":"CULLXXX2","parentCollection":"CULLYYY2","name":"renamed"}`}, []int{0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := plan(t, lib(), opts(repo.TypeCollection, false), c.objs...)
			for i, want := range c.want {
				if want == 0 {
					assert.NotEqual(t, outFailed, res[i].Status, "object %d: %+v", i, res[i].Fail)
				} else {
					require.Equal(t, outFailed, res[i].Status, "object %d", i)
					assert.Equal(t, want, res[i].Fail.Code, "object %d: %s", i, res[i].Fail.Message)
				}
			}
		})
	}
}
