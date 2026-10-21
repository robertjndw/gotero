package importer

import (
	"encoding/json"
	"strings"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zotero"
)

// convertObject turns a zotero.org object into the row the API would have
// stored for it. The data stays as zotero.org sent it apart from a few
// spellings the write path normalizes too. ok is false for objects that
// cannot be stored.
func convertObject(t repo.ObjectType, o zotero.Object) (repo.Object, bool) {
	var m zotero.OrderedMap
	if err := json.Unmarshal(o.Data, &m); err != nil {
		return repo.Object{}, false
	}
	key := o.Key
	if key == "" {
		key, _ = m.GetString("key")
	}
	version := o.Version
	if version == 0 {
		version, _ = m.GetInt("version")
	}
	if !zotero.ValidKey(key) || version < 0 {
		return repo.Object{}, false
	}
	if raw, ok := m.Get("relations"); ok && strings.TrimSpace(string(raw)) == "[]" {
		m.Set("relations", json.RawMessage("{}"))
	}
	switch t {
	case repo.TypeItem:
		if it, _ := m.GetString("itemType"); it == "" {
			return repo.Object{}, false
		}
		if raw, ok := m.Get("deleted"); ok {
			switch strings.TrimSpace(string(raw)) {
			case "true", "1":
				m.Set("deleted", json.RawMessage("1"))
			default:
				m.Delete("deleted")
			}
		}
		if raw, ok := m.Get("parentItem"); ok && strings.TrimSpace(string(raw)) == "false" {
			m.Delete("parentItem")
		}
		m.SetDefault("tags", json.RawMessage("[]"))
		m.SetDefault("collections", json.RawMessage("[]"))
		m.SetDefault("relations", json.RawMessage("{}"))
	case repo.TypeCollection:
		m.SetDefault("parentCollection", json.RawMessage("false"))
		m.SetDefault("relations", json.RawMessage("{}"))
	case repo.TypeSearch:
		m.SetDefault("conditions", json.RawMessage("[]"))
	}
	obj, err := zapi.ObjectFromData(t, key, m)
	if err != nil {
		return repo.Object{}, false
	}
	obj.Version = version
	return obj, true
}
