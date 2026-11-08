package zapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zotero/schema"
)

// maxObjectsPerWrite is the most objects one POST may carry.
const maxObjectsPerWrite = 50

type outcomeStatus int

const (
	outCreated outcomeStatus = iota
	outUpdated
	outUnchanged
	outFailed
)

// outcome is the decision for one object of a write request. The planner
// never touches the database; the caller persists created and updated
// objects under a single new library version.
type outcome struct {
	Index  int
	Key    string
	Status outcomeStatus
	// Obj is the object to store (without a version) for created and updated.
	Obj  repo.Object
	Fail zotero.WriteFailure
}

// getter is the part of repo.LibraryReader the planner needs.
type getter interface {
	Get(ctx context.Context, t repo.ObjectType, keys []string) (map[string]repo.Object, error)
	Versions(ctx context.Context, q repo.VersionsQuery) (iter.Seq2[repo.KeyVersion, error], error)
}

type planOpts struct {
	Kind    repo.ObjectType
	Schema  *schema.Schema
	Now     time.Time
	Replace bool // PUT semantics: the body replaces the stored object
	// DenyNotes refuses to create, change or convert note items, for keys
	// without the notes permission.
	DenyNotes bool
	// NewKey generates a key for objects that came without one.
	NewKey func() string
	// Group is set for group libraries, which refuse user-only fields.
	Group bool
}

type entry struct {
	idx     int
	key     string
	version int64
	// hasVersion tells an explicit "version": 0 from an absent version. On an
	// existing object the first is a stale write, the second a blind overwrite.
	hasVersion bool
	patch      zotero.OrderedMap
	fail       *writeError
}

func failedOutcome(e entry, werr *writeError) outcome {
	return outcome{Index: e.idx, Key: e.key, Status: outFailed, Fail: zotero.WriteFailure{
		Key: e.key, Code: werr.Code, Message: werr.Msg, Data: werr.Data,
	}}
}

var errNotesDenied = &writeError{Code: http.StatusForbidden, Msg: "Notes cannot be written with this API key"}

func kindLabel(t repo.ObjectType) string {
	switch t {
	case repo.TypeCollection:
		return "Collection"
	case repo.TypeSearch:
		return "Search"
	}
	return "Item"
}

// parseEntries decodes the request objects and settles their keys.
func parseEntries(raws []json.RawMessage, opts planOpts) []entry {
	out := make([]entry, len(raws))
	seen := map[string]bool{}
	for i, raw := range raws {
		e := entry{idx: i}
		out[i] = e
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			out[i].fail = badRequest("Uploaded data must be an array of objects")
			continue
		}
		if err := json.Unmarshal(trimmed, &e.patch); err != nil {
			out[i].fail = badRequest("Invalid JSON object")
			continue
		}
		if raw, ok := e.patch.Get("key"); ok {
			var k string
			if json.Unmarshal(raw, &k) != nil || !zotero.ValidKey(k) {
				// Echo a string key so the client can match the failure.
				if json.Unmarshal(raw, &k) == nil {
					e.key = k
				}
				e.fail = badRequest("Invalid key")
				out[i] = e
				continue
			}
			e.key = k
		} else {
			e.key = opts.NewKey()
		}
		if raw, ok := e.patch.Get("version"); ok {
			v, vok := e.patch.GetInt("version")
			if !vok || v < 0 {
				e.fail = badRequest("Invalid version '%s'", string(raw))
				out[i] = e
				continue
			}
			e.version, e.hasVersion = v, true
		}
		if seen[e.key] {
			e.fail = conflict("Object appears more than once in the request")
			out[i] = e
			continue
		}
		seen[e.key] = true
		out[i] = e
	}
	return out
}

// planWrite decides what happens to each object. It returns one outcome per
// input, in input order.
func planWrite(ctx context.Context, g getter, opts planOpts, raws []json.RawMessage) ([]outcome, error) {
	entries := parseEntries(raws, opts)

	var keys []string
	batchKeys := map[string]bool{}
	for _, e := range entries {
		if e.fail == nil {
			keys = append(keys, e.key)
			batchKeys[e.key] = true
		}
	}
	existing, err := g.Get(ctx, opts.Kind, keys)
	if err != nil {
		return nil, err
	}
	refs, err := loadRefs(ctx, g, opts.Kind, entries, batchKeys, existing)
	if err != nil {
		return nil, err
	}

	out := make([]outcome, len(entries))
	for i, e := range entries {
		if e.fail != nil {
			out[i] = failedOutcome(e, e.fail)
			continue
		}
		var stored *repo.Object
		if o, ok := existing[e.key]; ok {
			stored = &o
		}
		out[i] = planOne(opts, e, stored, refs)
	}
	// The tree checks look at the request as a whole, and failing an object
	// puts its stored state back, which can strand or invalidate others.
	// Failures only ever grow, so this settles.
	for {
		failOrphans(opts.Kind, entries, out, existing, batchKeys)
		n, err := checkTree(ctx, g, opts.Kind, out, existing, refs)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return out, nil
		}
	}
}

// failOrphans fails objects whose parent is another object of the same
// request that was not saved and does not exist either. planOne accepts any
// key of the request as a parent, so without this a child would be stored
// pointing at nothing when its parent fails, for example with a 404 for a
// collection deleted elsewhere. Zotero sends parents and children in one
// batch, and a dangling parent breaks the library for every client. The 409
// is the one zotero.org sends for a missing parent, so Zotero uploads the
// parent and retries. Failures can chain, hence the loop.
//
// inRequest holds only the entries that parsed. One that did not (a bad
// version, say) was never looked up in existing, so its key would look
// missing even when it is stored; planOne already checked those against
// storage.
func failOrphans(kind repo.ObjectType, entries []entry, out []outcome, existing map[string]repo.Object, inRequest map[string]bool) {
	field, msg, data := "parentItem", "Parent item %s not found", "parentItem"
	if kind == repo.TypeCollection {
		field, msg, data = "parentCollection", "Parent collection %s not found", "collection"
	} else if kind != repo.TypeItem {
		return
	}
	saved := map[string]bool{}
	for _, o := range out {
		if o.Status != outFailed {
			saved[o.Key] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for i, e := range entries {
			if out[i].Status == outFailed {
				continue
			}
			p := stringField(e.patch, field)
			if p == "" || saved[p] {
				continue
			}
			if _, ok := existing[p]; ok {
				continue
			}
			if !inRequest[p] {
				continue // a stored parent, checked by planOne
			}
			out[i] = failedOutcome(e, missingRef(data, p, msg))
			delete(saved, e.key)
			changed = true
		}
	}
}

// refSets are keys of objects that references in this request may point at.
type refSets struct {
	items       map[string]bool
	collections map[string]bool
	// parents holds the stored parents (of the request's own kind) that
	// were loaded, so the tree checks know their type without another query.
	parents map[string]repo.Object
}

func loadRefs(ctx context.Context, g getter, kind repo.ObjectType, entries []entry, batch map[string]bool, existing map[string]repo.Object) (refSets, error) {
	refs := refSets{items: map[string]bool{}, collections: map[string]bool{}, parents: map[string]repo.Object{}}
	var parentKeys, collKeys []string
	// The current parents of stored items matter too: a PATCH that only
	// changes the type of a child must still fit under its parent.
	if kind == repo.TypeItem {
		for _, o := range existing {
			if p := nodeOf(kind, o.Data).parent; p != "" {
				parentKeys = append(parentKeys, p)
			}
		}
	}
	for _, e := range entries {
		if e.fail != nil {
			continue
		}
		switch kind {
		case repo.TypeItem:
			if p := stringField(e.patch, "parentItem"); p != "" {
				parentKeys = append(parentKeys, p)
			}
			collKeys = append(collKeys, stringList(e.patch, "collections")...)
		case repo.TypeCollection:
			if p := stringField(e.patch, "parentCollection"); p != "" {
				collKeys = append(collKeys, p)
			}
		}
	}
	if kind == repo.TypeItem {
		for k := range batch {
			refs.items[k] = true
		}
	}
	if kind == repo.TypeCollection {
		for k := range batch {
			refs.collections[k] = true
		}
	}
	if len(parentKeys) > 0 {
		found, err := g.Get(ctx, repo.TypeItem, dedupe(parentKeys))
		if err != nil {
			return refs, err
		}
		for k, o := range found {
			refs.items[k] = true
			refs.parents[k] = o
		}
	}
	if len(collKeys) > 0 {
		found, err := g.Get(ctx, repo.TypeCollection, dedupe(collKeys))
		if err != nil {
			return refs, err
		}
		for k, o := range found {
			refs.collections[k] = true
			if kind == repo.TypeCollection {
				refs.parents[k] = o
			}
		}
	}
	return refs, nil
}

func dedupe(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

func stringField(m zotero.OrderedMap, name string) string {
	s, _ := m.GetString(name)
	return s
}

func stringList(m zotero.OrderedMap, name string) []string {
	raw, ok := m.Get(name)
	if !ok {
		return nil
	}
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

// versionConflict is the per-object precondition shared by objects and
// settings. Only a stored version newer than the client's counts as a
// conflict; a client that is ahead of us (say after a restore) is let
// through, the same way library versions are treated. A "version": 0 is
// most often a create retried after its response was lost, and the client
// turns a 412 into a full library resync, so when the content already
// matches it is reported as unchanged instead.
func versionConflict(stored, sent int64, unchanged bool) bool {
	return stored > sent && (sent != 0 || !unchanged)
}

func planOne(opts planOpts, e entry, stored *repo.Object, refs refSets) outcome {
	label := kindLabel(opts.Kind)
	if opts.DenyNotes && stored != nil && stored.ItemType == "note" {
		// Checked before the version precondition so a hidden note does not
		// even reveal its version.
		return failedOutcome(e, errNotesDenied)
	}
	if stored == nil && e.version > 0 {
		return failedOutcome(e, &writeError{
			Code: http.StatusNotFound,
			Msg:  label + " doesn't exist (it may have been deleted)",
		})
	}
	var (
		data    zotero.OrderedMap
		changed bool
		werr    *writeError
	)
	switch opts.Kind {
	case repo.TypeItem:
		data, changed, werr = buildItem(opts, e, stored, refs)
	case repo.TypeCollection:
		data, changed, werr = buildCollection(opts, e, stored, refs)
	case repo.TypeSearch:
		data, changed, werr = buildSearch(opts, e, stored)
	default:
		werr = badRequest("Unsupported object type")
	}
	switch {
	case stored != nil && e.hasVersion && versionConflict(stored.Version, e.version, werr == nil && !changed):
		return failedOutcome(e, &writeError{
			Code: http.StatusPreconditionFailed,
			Msg: fmt.Sprintf("%s has been modified since specified version (expected %d, found %d)",
				label, e.version, stored.Version),
		})
	case werr != nil:
		return failedOutcome(e, werr)
	case !changed:
		return outcome{Index: e.idx, Key: e.key, Status: outUnchanged}
	}
	obj, err := ObjectFromData(opts.Kind, e.key, data)
	if err != nil {
		return failedOutcome(e, badRequest("Invalid object: %v", err))
	}
	st := outCreated
	if stored != nil {
		st = outUpdated
	}
	return outcome{Index: e.idx, Key: e.key, Status: st, Obj: obj}
}

// ObjectFromData derives everything the repository stores next to the JSON.
// Key and version are bookkeeping and never part of the stored document.
// The importer calls it too, so imported rows get the same denormalized
// columns as API writes.
func ObjectFromData(kind repo.ObjectType, key string, data zotero.OrderedMap) (repo.Object, error) {
	data.Delete("key")
	data.Delete("version")
	raw, err := json.Marshal(data)
	if err != nil {
		return repo.Object{}, err
	}
	h := zotero.CanonicalHash(data)
	o := repo.Object{Key: key, Data: raw, ContentHash: h[:]}
	if kind == repo.TypeItem {
		o.ItemType = stringField(data, "itemType")
		o.ParentKey = stringField(data, "parentItem")
		if v, ok := data.GetInt("deleted"); ok && v == 1 {
			o.InTrash = true
		}
		o.Tags = tagsOf(data)
	}
	return o, nil
}

func tagsOf(data zotero.OrderedMap) []repo.ItemTag {
	raw, ok := data.Get("tags")
	if !ok {
		return nil
	}
	var in []struct {
		Tag  string `json:"tag"`
		Type int    `json:"type"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	out := make([]repo.ItemTag, 0, len(in))
	for _, t := range in {
		out = append(out, repo.ItemTag{Tag: t.Tag, Type: t.Type})
	}
	return out
}

func nowString(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// prepared holds a normalized patch plus the stored document it applies to.
type prepared struct {
	patch zotero.OrderedMap
	base  zotero.OrderedMap
	// removed is set when the patch cleared a field (parentItem false,
	// deleted 0) that the stored object had, which counts as a change.
	removed bool
}

// mergeOrReplace combines the stored document and the patch according to the
// request method. It reports whether the result differs from what is stored.
func mergeOrReplace(opts planOpts, p prepared, stored *repo.Object, defaults func(*zotero.OrderedMap), keep []string) (zotero.OrderedMap, bool) {
	if stored == nil {
		data := p.patch.Clone()
		defaults(&data)
		return data, true
	}
	if !opts.Replace {
		merged, changed := zotero.Merge(p.base, p.patch)
		return merged, changed || p.removed
	}
	// PUT: the body is the new object, but server-managed values survive.
	data := p.patch.Clone()
	for _, k := range keep {
		if v, ok := p.base.Get(k); ok && !data.Has(k) {
			data.Set(k, v)
		}
	}
	defaults(&data)
	changed := zotero.CanonicalHash(data) != zotero.CanonicalHash(p.base)
	return data, changed
}

func loadBase(stored *repo.Object) (zotero.OrderedMap, *writeError) {
	var base zotero.OrderedMap
	if stored == nil {
		return base, nil
	}
	if err := json.Unmarshal(stored.Data, &base); err != nil {
		return base, &writeError{Code: http.StatusInternalServerError, Msg: "Stored object is corrupt"}
	}
	return base, nil
}

func buildItem(opts planOpts, e entry, stored *repo.Object, refs refSets) (zotero.OrderedMap, bool, *writeError) {
	base, werr := loadBase(stored)
	if werr != nil {
		return base, false, werr
	}
	p := prepared{patch: e.patch.Clone(), base: base.Clone()}
	p.patch.Delete("key")
	p.patch.Delete("version")

	// parentItem and deleted are "absent means off" fields: false/0 removes
	// them instead of being stored.
	if raw, ok := p.patch.Get("parentItem"); ok {
		if isFalsy(raw) {
			p.patch.Delete("parentItem")
			p.removed = p.base.Delete("parentItem") || p.removed
		} else if k, kok := p.patch.GetString("parentItem"); !kok || !zotero.ValidKey(k) {
			return base, false, badRequest("'parentItem' must be false or an item key")
		} else if k == e.key {
			return base, false, badRequest("Item cannot be a child of itself")
		}
	}
	if werr := normalizeDeleted(&p); werr != nil {
		return base, false, werr
	}
	emptyArrayToObject(p.patch, "relations")
	nullToEmpty(p.patch, map[string]string{"tags": "[]", "collections": "[]", "creators": "[]", "relations": "{}"})
	dropEmptyTags(&p.patch)
	// dateAdded never changes after creation, whatever the client sends.
	if stored != nil {
		p.patch.Delete("dateAdded")
	}

	itemType := stringField(p.patch, "itemType")
	if itemType == "" && !opts.Replace {
		itemType = stringField(p.base, "itemType")
	}
	if itemType == "" {
		return base, false, badRequest("'itemType' property not provided")
	}
	if werr := validateItemPatch(opts.Schema, itemType, p.patch); werr != nil {
		return base, false, werr
	}
	if itemType == "attachment" {
		if werr := checkAttachmentExtras(opts.Group, p); werr != nil {
			return base, false, werr
		}
		normalizeAttachmentExtras(&p)
	}
	if opts.DenyNotes && (itemType == "note" || stringField(p.base, "itemType") == "note") {
		return base, false, errNotesDenied
	}
	if parent := stringField(p.patch, "parentItem"); parent != "" && !refs.items[parent] {
		return base, false, missingRef("parentItem", parent, "Parent item %s not found")
	}
	known := map[string]bool{}
	for _, k := range stringList(p.base, "collections") {
		known[k] = true
	}
	for _, k := range stringList(p.patch, "collections") {
		if !known[k] && !refs.collections[k] {
			return base, false, missingRef("collection", k, "Collection %s not found")
		}
	}

	isImportedAttachment := func(m zotero.OrderedMap) bool {
		mode := stringField(m, "linkMode")
		return itemType == "attachment" && (mode == "imported_file" || mode == "imported_url" || mode == "embedded_image")
	}
	defaults := func(m *zotero.OrderedMap) {
		m.SetDefault("tags", json.RawMessage("[]"))
		m.SetDefault("collections", json.RawMessage("[]"))
		m.SetDefault("relations", json.RawMessage("{}"))
		now := quote(nowString(opts.Now))
		if !m.Has("dateAdded") {
			m.Set("dateAdded", now)
		}
		if !m.Has("dateModified") {
			m.Set("dateModified", now)
		}
		// Clients that don't use WebDAV leave file metadata to the server, so
		// make the fields exist (as null) before any upload happens.
		if isImportedAttachment(*m) {
			m.SetDefault("md5", json.RawMessage("null"))
			m.SetDefault("mtime", json.RawMessage("null"))
		}
	}
	keep := []string{"dateAdded", "dateModified", "md5", "mtime"}
	merged, changed := mergeOrReplace(opts, p, stored, defaults, keep)
	if stored != nil {
		if old := stringField(p.base, "itemType"); old != "" && old != itemType {
			convertItemType(opts.Schema, old, itemType, &merged)
			changed = true
		}
	}
	// Checked on the merged result, since a PATCH can turn a child note into
	// a book without mentioning parentItem. A stored item that already
	// breaks the rule is only refused when this write touches it, so legacy
	// data does not lock the item against every other edit.
	if parent := stringField(merged, "parentItem"); parent != "" && !childItemTypes[itemType] &&
		(stored == nil || parent != stringField(p.base, "parentItem") || itemType != stringField(p.base, "itemType")) {
		return base, false, badRequest("Only notes, attachments and annotations can be child items")
	}
	if changed && stored != nil && !p.patch.Has("dateModified") {
		merged.Set("dateModified", quote(nowString(opts.Now)))
	}
	if stored == nil {
		return merged, true, nil
	}
	return merged, changed, nil
}

// nullToEmpty replaces an explicit null in the listed array and object
// fields with the empty value. zotero.org is lenient here, and storing the
// null would be served back as null, which breaks desktop sync of the
// library. Any other wrong type is left for validation to reject.
func nullToEmpty(m zotero.OrderedMap, empties map[string]string) {
	for name, empty := range empties {
		if raw, ok := m.Get(name); ok && string(bytes.TrimSpace(raw)) == "null" {
			m.Set(name, json.RawMessage(empty))
		}
	}
}

// emptyArrayToObject turns an empty JSON array into {}. PHP clients encode an
// empty map as [], and zotero.org takes it for an empty object.
func emptyArrayToObject(m zotero.OrderedMap, name string) {
	if raw, ok := m.Get(name); ok && string(bytes.TrimSpace(raw)) == "[]" {
		m.Set(name, json.RawMessage("{}"))
	}
}

// normalizeDeleted handles the trash flag, which items, collections and
// searches all carry since Zotero 7. Like zotero.org, it is stored as 1 when
// set and absent otherwise.
func normalizeDeleted(p *prepared) *writeError {
	raw, ok := p.patch.Get("deleted")
	if !ok {
		return nil
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true", "1":
		p.patch.Set("deleted", json.RawMessage("1"))
	case "false", "0", "null":
		p.patch.Delete("deleted")
		p.removed = p.base.Delete("deleted") || p.removed
	default:
		return badRequest("'deleted' must be 1 or true")
	}
	return nil
}

func buildCollection(opts planOpts, e entry, stored *repo.Object, refs refSets) (zotero.OrderedMap, bool, *writeError) {
	base, werr := loadBase(stored)
	if werr != nil {
		return base, false, werr
	}
	p := prepared{patch: e.patch.Clone(), base: base.Clone()}
	p.patch.Delete("key")
	p.patch.Delete("version")
	if raw, ok := p.patch.Get("parentCollection"); ok && isFalsy(raw) {
		p.patch.Set("parentCollection", json.RawMessage("false"))
	}
	if werr := normalizeDeleted(&p); werr != nil {
		return base, false, werr
	}
	nullToEmpty(p.patch, map[string]string{"relations": "{}"})
	emptyArrayToObject(p.patch, "relations")
	if werr := validateCollectionPatch(e.key, p.patch); werr != nil {
		return base, false, werr
	}
	if stored == nil && stringField(p.patch, "name") == "" {
		return base, false, badRequest("Collection name cannot be empty")
	}
	if opts.Replace && stringField(p.patch, "name") == "" {
		return base, false, badRequest("Collection name cannot be empty")
	}
	if parent := stringField(p.patch, "parentCollection"); parent != "" && !refs.collections[parent] {
		return base, false, missingRef("collection", parent, "Parent collection %s not found")
	}
	defaults := func(m *zotero.OrderedMap) {
		m.SetDefault("parentCollection", json.RawMessage("false"))
		m.SetDefault("relations", json.RawMessage("{}"))
	}
	merged, changed := mergeOrReplace(opts, p, stored, defaults, nil)
	return merged, changed, nil
}

func buildSearch(opts planOpts, e entry, stored *repo.Object) (zotero.OrderedMap, bool, *writeError) {
	base, werr := loadBase(stored)
	if werr != nil {
		return base, false, werr
	}
	p := prepared{patch: e.patch.Clone(), base: base.Clone()}
	p.patch.Delete("key")
	p.patch.Delete("version")
	if werr := normalizeDeleted(&p); werr != nil {
		return base, false, werr
	}
	nullToEmpty(p.patch, map[string]string{"conditions": "[]"})
	if werr := validateSearchPatch(p.patch); werr != nil {
		return base, false, werr
	}
	if (stored == nil || opts.Replace) && stringField(p.patch, "name") == "" {
		return base, false, badRequest("Search name cannot be empty")
	}
	defaults := func(m *zotero.OrderedMap) { m.SetDefault("conditions", json.RawMessage("[]")) }
	merged, changed := mergeOrReplace(opts, p, stored, defaults, nil)
	return merged, changed, nil
}

// treeNode is what the parent rules need to know about an item or a
// collection.
type treeNode struct {
	itemType string
	linkMode string
	parent   string
}

func nodeOf(kind repo.ObjectType, data json.RawMessage) treeNode {
	var m zotero.OrderedMap
	if json.Unmarshal(data, &m) != nil {
		return treeNode{}
	}
	if kind == repo.TypeCollection {
		return treeNode{parent: stringField(m, "parentCollection")}
	}
	return treeNode{
		itemType: stringField(m, "itemType"),
		linkMode: stringField(m, "linkMode"),
		parent:   stringField(m, "parentItem"),
	}
}

// itemClass groups item types by where they may sit in the tree. Regular
// types are interchangeable, so turning a book into a thesis never strands
// a child.
func itemClass(itemType string) string {
	if childItemTypes[itemType] {
		return itemType
	}
	return "regular"
}

// parentRule is zotero.org's check of a child against its parent: notes and
// attachments hang off regular items, annotations off file attachments, and
// the embedded images of a note off that note. That makes the tree at most
// three levels deep, so enforcing it on every edge also rules out cycles.
func parentRule(child treeNode, parentKey string, parent treeNode) *writeError {
	if parent.itemType == "" {
		return nil // stored data we cannot judge
	}
	switch child.itemType {
	case "annotation":
		if parent.itemType != "attachment" || parent.linkMode == "linked_url" {
			return badRequest("Parent item %s of annotation must be a file attachment", parentKey)
		}
	case "note", "attachment":
		embedded := child.itemType == "attachment" && child.linkMode == "embedded_image"
		if childItemTypes[parent.itemType] && (!embedded || parent.itemType != "note") {
			return badRequest("Parent item %s cannot be a note or attachment", parentKey)
		}
	}
	return nil
}

// planTree is the request's view of the object tree: what each object will
// look like if the request is saved as planned, with storage underneath.
type planTree struct {
	g      getter
	kind   repo.ObjectType
	saved  map[string]treeNode
	stored map[string]treeNode
}

func (t *planTree) node(ctx context.Context, key string) (treeNode, bool, error) {
	if n, ok := t.saved[key]; ok {
		return n, true, nil
	}
	if n, ok := t.stored[key]; ok {
		return n, true, nil
	}
	found, err := t.g.Get(ctx, t.kind, []string{key})
	if err != nil {
		return treeNode{}, false, err
	}
	o, ok := found[key]
	if !ok {
		return treeNode{}, false, nil
	}
	n := nodeOf(t.kind, o.Data)
	t.stored[key] = n
	return n, true, nil
}

// checkTree fails objects whose planned parent would break the item or
// collection tree and returns how many it failed. Only edges the request
// changes are judged, so a library that is already inconsistent can still
// be edited, and repaired.
func checkTree(ctx context.Context, g getter, kind repo.ObjectType, out []outcome, existing map[string]repo.Object, refs refSets) (int, error) {
	if kind != repo.TypeItem && kind != repo.TypeCollection {
		return 0, nil
	}
	t := &planTree{g: g, kind: kind, saved: map[string]treeNode{}, stored: map[string]treeNode{}}
	for k, o := range refs.parents {
		t.stored[k] = nodeOf(kind, o.Data)
	}
	for k, o := range existing {
		t.stored[k] = nodeOf(kind, o.Data)
	}
	for _, o := range out {
		if o.Status == outCreated || o.Status == outUpdated {
			t.saved[o.Key] = nodeOf(kind, o.Obj.Data)
		}
	}
	failed := 0
	for i, o := range out {
		n, ok := t.saved[o.Key]
		if !ok {
			continue
		}
		var before *treeNode
		if _, ok := existing[o.Key]; ok {
			b := t.stored[o.Key]
			before = &b
		}
		var (
			werr *writeError
			err  error
		)
		if kind == repo.TypeCollection {
			if n.parent != "" && (before == nil || n.parent != before.parent) {
				werr, err = t.collectionCycle(ctx, o.Key, n.parent)
			}
		} else {
			werr, err = t.itemEdges(ctx, o.Key, n, before)
		}
		if err != nil {
			return 0, err
		}
		if werr != nil {
			out[i] = outcome{Index: o.Index, Key: o.Key, Status: outFailed, Fail: zotero.WriteFailure{
				Key: o.Key, Code: werr.Code, Message: werr.Msg, Data: werr.Data,
			}}
			delete(t.saved, o.Key)
			failed++
		}
	}
	return failed, nil
}

// collectionCycle walks up from the proposed parent. The visited set bounds
// the walk even when stored data already holds a cycle; one that does not
// pass through key was not made by this request and is left alone.
func (t *planTree) collectionCycle(ctx context.Context, key, parent string) (*writeError, error) {
	seen := map[string]bool{}
	for cur := parent; cur != "" && !seen[cur]; {
		if cur == key {
			return conflict("Cannot move collection %s into its own subcollection %s", key, parent), nil
		}
		seen[cur] = true
		n, ok, err := t.node(ctx, cur)
		if err != nil || !ok {
			return nil, err
		}
		cur = n.parent
	}
	return nil, nil
}

// itemEdges checks the item against its parent, and the item's stored
// children against it when its type changes. before is nil for new items.
func (t *planTree) itemEdges(ctx context.Context, key string, n treeNode, before *treeNode) (*writeError, error) {
	if n.parent != "" {
		_, parentSaved := t.saved[n.parent]
		touched := before == nil || n.parent != before.parent || n.itemType != before.itemType ||
			n.linkMode != before.linkMode || parentSaved
		if touched {
			p, ok, err := t.node(ctx, n.parent)
			if err != nil {
				return nil, err
			}
			if ok {
				if werr := parentRule(n, n.parent, p); werr != nil {
					return werr, nil
				}
			}
		}
	}
	if before == nil || (itemClass(n.itemType) == itemClass(before.itemType) && n.linkMode == before.linkMode) {
		return nil, nil
	}
	// Children saved in this request are judged against the new type on
	// their own. The rest stay under this item, trashed ones included, and
	// must still fit there.
	seq, err := t.g.Versions(ctx, repo.VersionsQuery{Type: repo.TypeItem, ParentKey: key, IncludeTrashed: true})
	if err != nil {
		return nil, err
	}
	var children []string
	for kv, err := range seq {
		if err != nil {
			return nil, err
		}
		if _, ok := t.saved[kv.Key]; !ok {
			children = append(children, kv.Key)
		}
	}
	if len(children) == 0 {
		return nil, nil
	}
	found, err := t.g.Get(ctx, repo.TypeItem, children)
	if err != nil {
		return nil, err
	}
	for _, c := range found {
		if parentRule(nodeOf(repo.TypeItem, c.Data), key, n) != nil {
			return badRequest("Item %s has child items that cannot be under a %s", key, n.itemType), nil
		}
	}
	return nil, nil
}
