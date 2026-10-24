package syncsim

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/robertjndw/gotero/conformance"
)

func (c *client) newKey() string {
	b := make([]byte, 8)
	for i := range b {
		b[i] = conformance.KeyAlphabet[c.rng.IntN(len(conformance.KeyAlphabet))]
	}
	return string(b)
}

// A small shared pool of tags and words makes clients collide on the same
// values, which is where merge bugs hide.
var (
	tagPool  = []string{"alpha", "beta", "gamma", "delta", "to-read", "0", "Ünïcødé", "with space"}
	wordPool = []string{"sync", "zotero", "library", "conflict", "merge", "version", "stream", "file"}
	colors   = []string{"#FF0000", "#00FF00", "#0000FF", "#FF8C19"}
)

func (c *client) pick(list []string) string { return list[c.rng.IntN(len(list))] }

func (c *client) text() string {
	return fmt.Sprintf("%s %s %d", c.pick(wordPool), c.pick(wordPool), c.rng.IntN(1000))
}

// now is a client clock for dateModified. Each client runs a little apart
// from the others, as real machines do.
func (c *client) now() string {
	return time.Now().Add(c.skew).UTC().Format(time.RFC3339)
}

type op struct {
	name   string
	weight int
	files  bool // only with Config.Files
	run    func(c *client, l *library) bool
}

var ops = []op{
	{"createCollection", 6, false, (*client).opCreateCollection},
	{"renameCollection", 4, false, (*client).opRenameCollection},
	{"moveCollection", 3, false, (*client).opMoveCollection},
	{"deleteCollection", 2, false, (*client).opDeleteCollection},
	{"createSearch", 2, false, (*client).opCreateSearch},
	{"renameSearch", 2, false, (*client).opRenameSearch},
	{"deleteSearch", 1, false, (*client).opDeleteSearch},
	{"createItem", 12, false, (*client).opCreateItem},
	{"editItem", 14, false, (*client).opEditItem},
	{"toggleTag", 8, false, (*client).opToggleTag},
	{"toggleCollection", 6, false, (*client).opToggleCollection},
	{"trashItem", 4, false, (*client).opTrashItem},
	{"createNote", 6, false, (*client).opCreateNote},
	{"deleteItem", 3, false, (*client).opDeleteItem},
	{"deleteTag", 1, false, (*client).opDeleteTag},
	{"setTagColors", 2, false, (*client).opSetTagColors},
	{"clearTagColors", 1, false, (*client).opClearTagColors},
	{"createAttachment", 4, true, (*client).opCreateAttachment},
	{"modifyFile", 5, true, (*client).opModifyFile},
}

// randomOp applies one random user action to a random library. Actions
// whose target does not exist yet are skipped and another one is drawn.
func (c *client) randomOp() string {
	total := 0
	for _, o := range ops {
		if !o.files || c.sim.cfg.Files {
			total += o.weight
		}
	}
	for range 20 {
		l := c.libs[c.rng.IntN(len(c.libs))]
		n := c.rng.IntN(total)
		for _, o := range ops {
			if o.files && !c.sim.cfg.Files {
				continue
			}
			if n -= o.weight; n < 0 {
				if o.run(c, l) {
					c.sim.stats.countOp(o.name)
					return o.name
				}
				break
			}
		}
	}
	return ""
}

// pickObj returns a random local object of kind k matching ok.
func (c *client) pickObj(l *library, k kind, ok func(*object) bool) *object {
	var cands []*object
	for _, key := range l.sortedKeys(k) {
		if o := l.objs[k][key]; ok == nil || ok(o) {
			cands = append(cands, o)
		}
	}
	if len(cands) == 0 {
		return nil
	}
	return cands[c.rng.IntN(len(cands))]
}

func (c *client) add(l *library, k kind, data obj) *object {
	o := &object{kind: k, key: c.newKey(), data: normalize(data)}
	l.objs[k][o.key] = o
	c.trace.add("op: new %s %s in %s %s", k, o.key, l.path, canon(o.data))
	return o
}

// edited marks a local change for upload, stamping dateModified on items as
// Zotero does.
func (c *client) edited(o *object) {
	c.trace.add("op: edit %s %s", o.kind, o.key)
	o.synced = false
	if o.kind == kItem {
		o.data["dateModified"] = c.now()
	}
}

func isRegular(o *object) bool {
	t := str(o.data, "itemType")
	return t != "note" && t != "attachment" && str(o.data, "parentItem") == ""
}

func (c *client) opCreateCollection(l *library) bool {
	d := obj{"name": c.text()}
	if c.rng.IntN(3) == 0 {
		if p := c.pickObj(l, kCollection, nil); p != nil {
			d["parentCollection"] = p.key
		}
	}
	c.add(l, kCollection, d)
	return true
}

func (c *client) opRenameCollection(l *library) bool {
	o := c.pickObj(l, kCollection, nil)
	if o == nil {
		return false
	}
	o.data["name"] = c.text()
	c.edited(o)
	return true
}

// opMoveCollection moves a collection to the top level or under another one
// that is not inside it. Two clients doing this at once can each produce a
// valid tree that is a cycle when combined.
func (c *client) opMoveCollection(l *library) bool {
	o := c.pickObj(l, kCollection, nil)
	if o == nil {
		return false
	}
	target := c.pickObj(l, kCollection, func(t *object) bool { return !l.isAncestor(o.key, t.key) })
	if target == nil || c.rng.IntN(4) == 0 {
		delete(o.data, "parentCollection")
	} else {
		o.data["parentCollection"] = target.key
	}
	c.edited(o)
	return true
}

func (c *client) opDeleteCollection(l *library) bool {
	o := c.pickObj(l, kCollection, nil)
	if o == nil {
		return false
	}
	for _, key := range l.removeLocal(kCollection, o.key) {
		c.trace.add("op: delete collection %s", key)
		l.pendingDel[kCollection][key] = true
	}
	return true
}

func (c *client) opCreateSearch(l *library) bool {
	c.add(l, kSearch, obj{"name": c.text(), "conditions": []any{
		map[string]any{"condition": "title", "operator": "contains", "value": c.pick(wordPool)},
	}})
	return true
}

func (c *client) opRenameSearch(l *library) bool {
	o := c.pickObj(l, kSearch, nil)
	if o == nil {
		return false
	}
	o.data["name"] = c.text()
	c.edited(o)
	return true
}

func (c *client) opDeleteSearch(l *library) bool {
	o := c.pickObj(l, kSearch, nil)
	if o == nil {
		return false
	}
	l.removeLocal(kSearch, o.key)
	l.pendingDel[kSearch][o.key] = true
	return true
}

func (c *client) randomTags() []any {
	var out []any
	for range c.rng.IntN(3) {
		t := map[string]any{"tag": c.pick(tagPool)}
		if c.rng.IntN(4) == 0 {
			t["type"] = json.Number("1")
		}
		out = append(out, t)
	}
	return out
}

func (c *client) opCreateItem(l *library) bool {
	now := c.now()
	d := obj{
		"itemType": "book", "title": c.text(), "tags": c.randomTags(),
		"dateAdded": now, "dateModified": now,
	}
	if c.rng.IntN(2) == 0 {
		d["extra"] = c.text()
	}
	if coll := c.pickObj(l, kCollection, nil); coll != nil && c.rng.IntN(2) == 0 {
		d["collections"] = []any{coll.key}
	}
	c.add(l, kItem, d)
	return true
}

func (c *client) opEditItem(l *library) bool {
	o := c.pickObj(l, kItem, nil)
	if o == nil {
		return false
	}
	switch str(o.data, "itemType") {
	case "note":
		o.data["note"] = "<p>" + c.text() + "</p>"
	case "attachment":
		o.data["title"] = c.text()
	default:
		field := c.pick([]string{"title", "title", "extra"})
		if c.rng.IntN(6) == 0 {
			delete(o.data, field)
		} else {
			o.data[field] = c.text()
		}
	}
	c.edited(o)
	return true
}

func (c *client) opToggleTag(l *library) bool {
	o := c.pickObj(l, kItem, nil)
	if o == nil {
		return false
	}
	tag := c.pick(tagPool)
	tags := asList(o.data["tags"])
	kept := slices.DeleteFunc(slices.Clone(tags), func(t any) bool {
		m, _ := t.(map[string]any)
		return m != nil && m["tag"] == tag
	})
	if len(kept) == len(tags) {
		kept = append(kept, map[string]any{"tag": tag})
	}
	o.data["tags"] = kept
	o.data = normalize(o.data)
	c.edited(o)
	return true
}

func (c *client) opToggleCollection(l *library) bool {
	o := c.pickObj(l, kItem, isRegular)
	coll := c.pickObj(l, kCollection, nil)
	if o == nil || coll == nil {
		return false
	}
	cols := strList(o.data, "collections")
	if i := slices.Index(cols, coll.key); i >= 0 {
		cols = slices.Delete(cols, i, i+1)
	} else {
		cols = append(cols, coll.key)
	}
	setStrList(o.data, "collections", cols)
	o.data = normalize(o.data)
	c.edited(o)
	return true
}

func (c *client) opTrashItem(l *library) bool {
	o := c.pickObj(l, kItem, nil)
	if o == nil {
		return false
	}
	if _, ok := o.data["deleted"]; ok {
		delete(o.data, "deleted")
	} else {
		o.data["deleted"] = json.Number("1")
	}
	c.edited(o)
	return true
}

func (c *client) opCreateNote(l *library) bool {
	p := c.pickObj(l, kItem, isRegular)
	if p == nil {
		return false
	}
	now := c.now()
	c.add(l, kItem, obj{
		"itemType": "note", "parentItem": p.key, "note": "<p>" + c.text() + "</p>",
		"tags": c.randomTags(), "dateAdded": now, "dateModified": now,
	})
	return true
}

func (c *client) opDeleteItem(l *library) bool {
	o := c.pickObj(l, kItem, nil)
	if o == nil {
		return false
	}
	for _, key := range l.removeLocal(kItem, o.key) {
		c.trace.add("op: delete item %s", key)
		l.pendingDel[kItem][key] = true
	}
	return true
}

func (c *client) opDeleteTag(l *library) bool {
	tag := c.pick(tagPool)
	if !l.removeTag(tag) {
		return false
	}
	l.pendingTagDel[tag] = true
	return true
}

func (c *client) opSetTagColors(l *library) bool {
	var v []any
	for range 1 + c.rng.IntN(3) {
		v = append(v, map[string]any{"name": c.pick(tagPool), "color": c.pick(colors)})
	}
	l.tagColors.value = v
	l.pendingSetDel = false
	return true
}

func (c *client) opClearTagColors(l *library) bool {
	if l.tagColors.value == nil {
		return false
	}
	l.tagColors.value = nil
	if l.tagColors.version > 0 {
		l.pendingSetDel = true
	}
	return true
}

func (c *client) opCreateAttachment(l *library) bool {
	now := c.now()
	d := obj{
		"itemType": "attachment", "linkMode": "imported_file", "title": c.text(),
		"contentType": "text/plain", "charset": "utf-8", "filename": "file.txt",
		"dateAdded": now, "dateModified": now,
	}
	if p := c.pickObj(l, kItem, isRegular); p != nil && c.rng.IntN(3) > 0 {
		d["parentItem"] = p.key
	}
	o := c.add(l, kItem, d)
	l.files[o.key] = newFile([]byte(c.text() + "\n"))
	return true
}

func (c *client) opModifyFile(l *library) bool {
	o := c.pickObj(l, kItem, func(o *object) bool { return l.files[o.key] != nil && l.files[o.key].md5 != "" })
	if o == nil {
		return false
	}
	f := newFile([]byte(c.text() + "\n" + c.text() + "\n"))
	f.synced = l.files[o.key].synced
	l.files[o.key] = f
	return true
}
