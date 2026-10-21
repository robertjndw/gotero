package schema

import (
	"slices"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	s := Default()
	if s != Default() {
		t.Error("Default should return one shared instance")
	}
	if s.Version() < 40 {
		t.Errorf("version %d looks wrong", s.Version())
	}
	types := s.ItemTypes()
	for _, want := range []string{"book", "journalArticle", "attachment", "note", "annotation", "webpage"} {
		if !slices.Contains(types, want) || !s.HasItemType(want) {
			t.Errorf("missing item type %s", want)
		}
	}
	if s.HasItemType("hologram") {
		t.Error("hologram should not exist")
	}
	// ItemTypes returns a copy.
	types[0] = "mutated"
	if s.ItemTypes()[0] == "mutated" {
		t.Error("ItemTypes leaks internal slice")
	}
}

func TestFields(t *testing.T) {
	s := Default()
	f := s.Fields("book")
	if len(f) == 0 || f[0] != "title" {
		t.Fatalf("book fields %v", f)
	}
	if !s.ValidField("book", "ISBN") || s.ValidField("book", "publicationTitle") || s.ValidField("nope", "title") {
		t.Error("ValidField")
	}
	if s.Fields("nope") != nil || s.CreatorTypes("nope") != nil {
		t.Error("unknown type should give nil")
	}
	if got := s.BaseField("book", "format"); got != "medium" {
		t.Errorf("BaseField(book, format) = %q", got)
	}
	if s.BaseField("book", "title") != "" || s.BaseField("nope", "x") != "" {
		t.Error("no base field expected")
	}
	if ct := s.CreatorTypes("book"); len(ct) == 0 || ct[0] != "author" {
		t.Errorf("creator types %v", ct)
	}
	if len(s.Fields("note")) != 0 || len(s.Fields("annotation")) != 0 {
		t.Error("note and annotation have no schema fields")
	}
}

func TestFieldIntroduced(t *testing.T) {
	s := Default()
	if got := s.FieldIntroduced("book", "title"); got < 1 || got > s.Version() {
		t.Errorf("title introduced %d", got)
	}
	if got := s.FieldIntroduced("book", "citationKey"); got <= s.FieldIntroduced("book", "title") {
		t.Errorf("citationKey (%d) should be newer than title", got)
	}
	if s.FieldIntroduced("book", "nope") != 0 || s.FieldIntroduced("nope", "title") != 0 {
		t.Error("unknown should be 0")
	}
	for _, typ := range s.ItemTypes() {
		for _, f := range s.Fields(typ) {
			if v := s.FieldIntroduced(typ, f); v < 1 || v > s.Version() {
				t.Errorf("%s.%s introduced %d outside 1..%d", typ, f, v, s.Version())
			}
		}
	}
}

const sample = `{
  "version": 7,
  "itemTypes": [
    {"itemType":"thing","fields":[{"field":"title"},{"field":"format","baseField":"medium"}],"creatorTypes":[{"creatorType":"author","primary":true},{"creatorType":"editor"}]},
    {"itemType":"note","fields":[],"creatorTypes":[]}
  ],
  "meta": {"fields":{}},
  "locales": {"en-US": {"itemTypes": {"thing":"Thing"}}}
}`

func TestLoad(t *testing.T) {
	s, err := Load(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version() != 7 || !slices.Equal(s.ItemTypes(), []string{"thing", "note"}) {
		t.Errorf("version %d types %v", s.Version(), s.ItemTypes())
	}
	if !slices.Equal(s.Fields("thing"), []string{"title", "format"}) {
		t.Errorf("fields %v", s.Fields("thing"))
	}
	if s.BaseField("thing", "format") != "medium" {
		t.Error("base field")
	}
	if !slices.Equal(s.CreatorTypes("thing"), []string{"author", "editor"}) {
		t.Errorf("creators %v", s.CreatorTypes("thing"))
	}
	if s.FieldIntroduced("thing", "title") != 0 {
		t.Error("loaded schemas have unknown introduction versions")
	}
}

func TestLoadErrors(t *testing.T) {
	for name, in := range map[string]string{
		"empty":      ``,
		"not json":   `x`,
		"no version": `{"itemTypes":[{"itemType":"a"}]}`,
		"no types":   `{"version":1,"itemTypes":[]}`,
		"no name":    `{"version":1,"itemTypes":[{"fields":[]}]}`,
		"duplicate":  `{"version":1,"itemTypes":[{"itemType":"a"},{"itemType":"a"}]}`,
	} {
		if _, err := Load(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
