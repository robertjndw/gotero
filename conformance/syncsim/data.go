package syncsim

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"sort"
)

// obj is an object's data as Zotero sends it, decoded with UseNumber so
// integers like mtime survive a round trip exactly.
type obj = map[string]any

type kind string

const (
	kCollection kind = "collections"
	kSearch     kind = "searches"
	kItem       kind = "items"
)

// kinds is the order Zotero uploads and downloads in: collections before the
// items that reference them.
var kinds = []kind{kCollection, kSearch, kItem}

func (k kind) keyParam() string {
	switch k {
	case kCollection:
		return "collectionKey"
	case kSearch:
		return "searchKey"
	}
	return "itemKey"
}

// parentField is the data field naming an object's parent, empty for kinds
// that cannot nest.
func (k kind) parentField() string {
	switch k {
	case kItem:
		return "parentItem"
	case kCollection:
		return "parentCollection"
	}
	return ""
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// normalize returns the comparable form of an object's data. Zotero treats an
// empty value and a missing one alike (an empty title is no title, parentItem
// false is no parent), and the server is free to fill in empty defaults, so
// all of those are dropped. Tag and collection lists are sets.
func normalize(d obj) obj {
	out := obj{}
	for k, v := range d {
		if k == "key" || k == "version" || isEmpty(v) {
			continue
		}
		switch k {
		case "tags":
			out[k] = normTags(v)
		case "collections":
			out[k] = normStrings(v)
		default:
			out[k] = v
		}
		if isEmpty(out[k]) {
			delete(out, k)
		}
	}
	return out
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case json.Number:
		return x == "0"
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func normTags(v any) []any {
	list, _ := v.([]any)
	seen := map[string]bool{}
	var out []any
	for _, t := range list {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["tag"].(string)
		if name == "" {
			continue
		}
		nt := map[string]any{"tag": name}
		if typ, ok := m["type"].(json.Number); ok && typ != "0" {
			nt["type"] = typ
		}
		c := canon(nt)
		if !seen[c] {
			seen[c] = true
			out = append(out, nt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return canon(out[i]) < canon(out[j]) })
	return out
}

func normStrings(v any) []any {
	list, _ := v.([]any)
	var s []string
	for _, x := range list {
		if str, ok := x.(string); ok && str != "" {
			s = append(s, str)
		}
	}
	slices.Sort(s)
	s = slices.Compact(s)
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}

// canon is a stable encoding for comparisons: encoding/json sorts map keys.
func canon(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func equal(a, b obj) bool { return canon(normalize(a)) == canon(normalize(b)) }

func eqValue(a, b any) bool { return canon(a) == canon(b) }

func clone(d obj) obj {
	if d == nil {
		return nil
	}
	var out obj
	if err := decode([]byte(canon(d)), &out); err != nil {
		panic(err)
	}
	return out
}

func str(d obj, k string) string {
	s, _ := d[k].(string)
	return s
}

func strList(d obj, k string) []string {
	var out []string
	list, _ := d[k].([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func setStrList(d obj, k string, s []string) {
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	d[k] = out
}

// Policy decides true conflicts, where both sides changed the same field to
// different values. Zotero asks the user; the simulator answers for them.
type Policy int

const (
	PreferRemote Policy = iota
	PreferLocal
)

func (p Policy) String() string {
	if p == PreferLocal {
		return "local"
	}
	return "remote"
}

// merge3 reconciles local changes with remote ones against the last synced
// copy, the way Zotero's sync does: changes on one side are taken, tag and
// collection lists merge as sets, and true conflicts follow the policy.
// dateModified never conflicts; the later one wins.
func merge3(cache, local, remote obj, p Policy) (merged obj, conflicts int) {
	c, l, r := normalize(cache), normalize(local), normalize(remote)
	merged = obj{}
	keys := map[string]bool{}
	for _, m := range []obj{c, l, r} {
		for k := range m {
			keys[k] = true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		cv, lv, rv := c[k], l[k], r[k]
		var v any
		switch k {
		case "tags", "collections":
			v = mergeSet(cv, lv, rv)
		case "dateModified":
			ls, _ := lv.(string)
			rs, _ := rv.(string)
			v = max(ls, rs)
		default:
			lc, rc := !eqValue(lv, cv), !eqValue(rv, cv)
			switch {
			case !rc:
				v = lv
			case !lc || eqValue(lv, rv):
				v = rv
			default:
				conflicts++
				if p == PreferLocal {
					v = lv
				} else {
					v = rv
				}
			}
		}
		if !isEmpty(v) {
			merged[k] = v
		}
	}
	return merged, conflicts
}

// mergeSet keeps what both sides kept and what either side added: a removal
// on one side wins over the other side leaving the element alone.
func mergeSet(c, l, r any) []any {
	base, lset, rset := setOf(c), setOf(l), setOf(r)
	var out []any
	seen := map[string]bool{}
	for _, list := range [][]any{asList(l), asList(r)} {
		for _, x := range list {
			k := canon(x)
			if seen[k] {
				continue
			}
			if !base[k] || (lset[k] && rset[k]) {
				seen[k] = true
				out = append(out, x)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return canon(out[i]) < canon(out[j]) })
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func setOf(v any) map[string]bool {
	out := map[string]bool{}
	for _, x := range asList(v) {
		out[canon(x)] = true
	}
	return out
}

// patchFor builds what Zotero uploads for an object it has synced before:
// the changed fields, plus an explicit empty value for each field that was
// cleared, since a POST leaves omitted fields alone.
func patchFor(cache, data obj) obj {
	c, d := normalize(cache), normalize(data)
	out := obj{}
	for k, v := range d {
		if !eqValue(v, c[k]) {
			out[k] = v
		}
	}
	for k, v := range c {
		if _, ok := d[k]; !ok {
			out[k] = emptyLike(k, v)
		}
	}
	return out
}

func emptyLike(field string, v any) any {
	switch field {
	case "deleted", "parentItem", "parentCollection":
		return false
	}
	switch v.(type) {
	case []any:
		return []any{}
	case map[string]any:
		return map[string]any{}
	case bool, json.Number:
		return false
	}
	return ""
}
