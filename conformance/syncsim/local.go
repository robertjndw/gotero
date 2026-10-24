package syncsim

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"

	"github.com/robertjndw/gotero/conformance"
)

// object is one collection, search or item in a client's local database.
type object struct {
	kind    kind
	key     string
	version int64
	data    obj
	// cache is the server's copy as of the last sync, which Zotero keeps for
	// three-way merges and upload patches. nil means never synced.
	cache obj
	// synced is Zotero's flag, not a comparison of data and cache: some
	// local cascades (a deleted collection leaving its items) change the
	// data without making the object something to upload.
	synced bool
}

func (o *object) dirty() bool { return !o.synced }

// file is an attachment's local file.
type file struct {
	content []byte
	md5     string
	// dirty means the local file changed and has to be uploaded.
	dirty bool
	// synced is the md5 this client last uploaded or downloaded, Zotero's
	// attachmentSyncedHash. Uploads send it as If-Match, so a file someone
	// else replaced in the meantime is a 412 instead of being overwritten.
	synced string
}

func newFile(content []byte) *file {
	return &file{content: content, md5: conformance.MD5Hex(content), dirty: true}
}

// newRand is a seeded generator. The simulation has to replay from its
// seed, so cryptographic randomness would defeat the point.
func newRand(seed, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, stream)) //nolint:gosec // reproducible on purpose
}

// setting is the tagColors setting.
type setting struct {
	value   any // nil when unset
	version int64
	cache   any
	synced  bool // cache reflects the server
}

func (s *setting) dirty() bool { return !s.synced || !eqValue(s.value, s.cache) }

// library is a client's local copy of one library.
type library struct {
	path    string // "users/1" or "groups/2"
	version int64
	objs    map[kind]map[string]*object
	// pendingDel are local deletions not uploaded yet.
	pendingDel    map[kind]map[string]bool
	pendingTagDel map[string]bool
	tagColors     setting
	pendingSetDel bool
	files         map[string]*file
	// needFull is set after a per-object 412 or 404, which makes Zotero
	// reset the library with a full sync.
	needFull bool
	// fetch holds keys to download on the next sync regardless of version.
	fetch map[kind]map[string]bool
	// restored are locally deleted collections brought back by a remote
	// change, whose items need their membership repaired.
	restored []string
	// repairOnFetch are collections in fetch whose items lost their
	// membership locally and need it repaired once they are back.
	repairOnFetch map[string]bool
	// dropped[coll][item] are memberships the server still had when this
	// client dropped them because it is deleting coll.
	dropped map[string]map[string]bool
}

func newLibrary(path string) *library {
	l := &library{
		path:          path,
		objs:          map[kind]map[string]*object{},
		pendingDel:    map[kind]map[string]bool{},
		fetch:         map[kind]map[string]bool{},
		repairOnFetch: map[string]bool{},
		dropped:       map[string]map[string]bool{},
		pendingTagDel: map[string]bool{},
		files:         map[string]*file{},
		tagColors:     setting{synced: true},
	}
	for _, k := range kinds {
		l.objs[k] = map[string]*object{}
		l.pendingDel[k] = map[string]bool{}
		l.fetch[k] = map[string]bool{}
	}
	return l
}

func (l *library) sortedKeys(k kind) []string { return slices.Sorted(maps.Keys(l.objs[k])) }

// unsynced lists what is waiting to be uploaded, data first, then files.
// The data part decides whether a sync loops again; files upload after it.
func (l *library) unsynced() (data, files []string) {
	for _, k := range kinds {
		for _, key := range slices.Sorted(maps.Keys(l.pendingDel[k])) {
			data = append(data, "delete "+string(k)+"/"+key)
		}
		for _, key := range l.sortedKeys(k) {
			if o := l.objs[k][key]; o.dirty() {
				data = append(data, fmt.Sprintf("%s/%s@%d %s", k, key, o.version, canon(patchFor(o.cache, o.data))))
			}
		}
	}
	for _, t := range slices.Sorted(maps.Keys(l.pendingTagDel)) {
		data = append(data, "delete tag "+t)
	}
	if l.pendingSetDel {
		data = append(data, "delete tagColors")
	} else if l.tagColors.dirty() {
		data = append(data, "tagColors "+canon(l.tagColors.value))
	}
	for _, key := range slices.Sorted(maps.Keys(l.files)) {
		if l.files[key].dirty && l.objs[kItem][key] != nil {
			files = append(files, "file of "+key)
		}
	}
	return data, files
}

func (l *library) hasDataChanges() bool {
	d, _ := l.unsynced()
	return len(d) > 0
}

func (l *library) children(parent string) []string {
	var out []string
	for _, key := range l.sortedKeys(kItem) {
		if str(l.objs[kItem][key].data, "parentItem") == parent {
			out = append(out, key)
		}
	}
	return out
}

func (l *library) subcollections(parent string) []string {
	var out []string
	for _, key := range l.sortedKeys(kCollection) {
		if str(l.objs[kCollection][key].data, "parentCollection") == parent {
			out = append(out, key)
		}
	}
	return out
}

// removeLocal drops an object and what Zotero erases with it: children of
// an item, sub-collections of a collection. Items leave a deleted collection
// without being marked for upload; the server makes the same change. It
// returns every key removed.
func (l *library) removeLocal(k kind, key string) []string {
	if l.objs[k][key] == nil {
		return nil
	}
	var removed []string
	switch k {
	case kItem:
		for _, c := range l.children(key) {
			removed = append(removed, l.removeLocal(kItem, c)...)
		}
		delete(l.files, key)
	case kCollection:
		for _, c := range l.subcollections(key) {
			removed = append(removed, l.removeLocal(kCollection, c)...)
		}
		for _, it := range l.objs[kItem] {
			if cols := strList(it.data, "collections"); slices.Contains(cols, key) {
				setStrList(it.data, "collections", slices.DeleteFunc(cols, func(s string) bool { return s == key }))
			}
		}
	}
	delete(l.objs[k], key)
	return append(removed, key)
}

// withoutDeletedCollections drops membership in collections this client
// has deleted but not uploaded the deletion of yet.
func (l *library) withoutDeletedCollections(key string, d obj) obj {
	cols := strList(d, "collections")
	kept := slices.DeleteFunc(slices.Clone(cols), func(c string) bool { return l.pendingDel[kCollection][c] })
	if len(kept) == len(cols) {
		return d
	}
	for _, c := range cols {
		if l.pendingDel[kCollection][c] {
			if l.dropped[c] == nil {
				l.dropped[c] = map[string]bool{}
			}
			l.dropped[c][key] = true
		}
	}
	d = clone(d)
	setStrList(d, "collections", kept)
	return normalize(d)
}

// removeTag is Zotero's "delete tag": every item carrying it loses it and is
// marked for upload. The items go up before the tag deletion does.
func (l *library) removeTag(tag string) bool {
	found := false
	for _, it := range l.objs[kItem] {
		list := asList(it.data["tags"])
		kept := slices.DeleteFunc(slices.Clone(list), func(t any) bool {
			m, _ := t.(map[string]any)
			return m != nil && m["tag"] == tag
		})
		if len(kept) != len(list) {
			it.data["tags"] = kept
			it.synced = false
			found = true
		}
	}
	return found
}

// isAncestor reports whether anc is coll or one of its parents.
func (l *library) isAncestor(anc, coll string) bool {
	seen := map[string]bool{}
	for c := coll; c != "" && !seen[c]; {
		if c == anc {
			return true
		}
		seen[c] = true
		o := l.objs[kCollection][c]
		if o == nil {
			return false
		}
		c = str(o.data, "parentCollection")
	}
	return false
}

// fixCollectionCycles undoes local parent changes that, together with a
// remote move, would put a collection under its own descendant. Zotero
// refuses such a tree, so the remote parent wins.
func (l *library) fixCollectionCycles() int {
	fixed := 0
	for _, key := range l.sortedKeys(kCollection) {
		o := l.objs[kCollection][key]
		p := str(o.data, "parentCollection")
		if p == "" || !l.isAncestor(key, p) {
			continue
		}
		if cp := str(o.cache, "parentCollection"); cp != "" {
			o.data["parentCollection"] = cp
		} else {
			delete(o.data, "parentCollection")
		}
		if o.cache != nil && equal(o.data, o.cache) {
			o.synced = true
		}
		fixed++
	}
	return fixed
}
