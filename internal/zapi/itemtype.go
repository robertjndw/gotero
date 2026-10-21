package zapi

import (
	"encoding/json"
	"slices"

	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zotero/schema"
)

// convertItemType makes a stored item valid for the type it is changing to.
// The desktop client refuses items that carry fields their type does not
// have (ZoteroInvalidDataError), and a merge keeps every old field, so the
// leftovers have to go here, the way zotero.org's own type change works:
//
//   - a field the new type lacks moves to the field that shares its base
//     field (bookTitle on a book section becomes publicationTitle on a
//     journal article), unless the target is already set;
//   - anything without a home is dropped;
//   - a creator whose role does not exist on the new type takes the type's
//     primary role.
func convertItemType(s *schema.Schema, oldType, newType string, m *zotero.OrderedMap) {
	var moves, drops []string
	m.Each(func(name string, _ json.RawMessage) bool {
		if ownField(s, newType, name) {
			return true
		}
		if to := sharedBaseField(s, oldType, newType, name); to != "" && !m.Has(to) {
			moves = append(moves, name, to)
		} else if !fieldAllowed(s, newType, name) {
			drops = append(drops, name)
		}
		return true
	})
	for i := 0; i < len(moves); i += 2 {
		if v, ok := m.Get(moves[i]); ok && !m.Has(moves[i+1]) {
			m.Set(moves[i+1], v)
		}
		m.Delete(moves[i])
	}
	for _, name := range drops {
		m.Delete(name)
	}
	convertCreators(s, newType, m)
}

// sharedBaseField finds the field of newType that maps to the same base
// field as name does on oldType, or "" if there is none.
func sharedBaseField(s *schema.Schema, oldType, newType, name string) string {
	base := s.BaseField(oldType, name)
	if base == "" {
		if !s.ValidField(oldType, name) {
			return ""
		}
		base = name // name already is a base field
	}
	if s.ValidField(newType, base) {
		return base
	}
	for _, f := range s.Fields(newType) {
		if s.BaseField(newType, f) == base {
			return f
		}
	}
	return ""
}

func convertCreators(s *schema.Schema, newType string, m *zotero.OrderedMap) {
	raw, ok := m.Get("creators")
	if !ok {
		return
	}
	var cs []map[string]json.RawMessage
	if json.Unmarshal(raw, &cs) != nil || len(cs) == 0 {
		return
	}
	allowed := s.CreatorTypes(newType)
	if len(allowed) == 0 {
		_ = m.SetValue("creators", []any{})
		return
	}
	changed := false
	for _, c := range cs {
		var ct string
		_ = json.Unmarshal(c["creatorType"], &ct)
		if !slices.Contains(allowed, ct) {
			c["creatorType"] = quote(allowed[0])
			changed = true
		}
	}
	if changed {
		_ = m.SetValue("creators", cs)
	}
}
