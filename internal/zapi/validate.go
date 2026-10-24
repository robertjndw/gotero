package zapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zotero/schema"
)

// writeError is a per-object failure, reported in the "failed" member of a
// write response.
type writeError struct {
	Code int
	Msg  string
	// Data goes out as the failure's "data" object.
	Data json.RawMessage
}

func (e *writeError) Error() string { return e.Msg }

func badRequest(format string, args ...any) *writeError {
	return &writeError{Code: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) *writeError {
	return &writeError{Code: http.StatusConflict, Msg: fmt.Sprintf(format, args...)}
}

// missingRef is the 409 for a reference to an object the server does not
// have. Zotero reads data.collection or data.parentItem, marks that object
// for upload and retries, so a dependency that got lost repairs itself. The
// messages match zotero.org's.
func missingRef(field, key, format string) *writeError {
	data, _ := json.Marshal(map[string]string{field: key})
	return &writeError{Code: http.StatusConflict, Msg: fmt.Sprintf(format, key), Data: data}
}

var childItemTypes = map[string]bool{"note": true, "attachment": true, "annotation": true}

var (
	attachmentExtraFields = []string{"linkMode", "contentType", "charset", "filename", "md5", "mtime", "path", "lastRead", "note"}
	annotationExtraFields = []string{
		"annotationType", "annotationAuthorName", "annotationText", "annotationComment",
		"annotationColor", "annotationPageLabel", "annotationSortIndex", "annotationPosition",
	}
	linkModes = map[string]bool{
		"imported_file": true, "imported_url": true, "linked_file": true, "linked_url": true, "embedded_image": true,
	}
	// Fields the server understands on every item type.
	commonItemFields = map[string]bool{
		"key": true, "version": true, "itemType": true, "creators": true, "tags": true, "collections": true,
		"relations": true, "parentItem": true, "deleted": true, "inPublications": true,
		"dateAdded": true, "dateModified": true,
	}
)

func isFalsy(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "false", "0", "null", `""`:
		return true
	}
	return false
}

func isScalar(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '{', '[', 't', 'f':
		return false
	}
	return true
}

func parseTime(s string) bool {
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// ownField reports whether name belongs to the item type itself: a schema
// field, a field every type has, or an extra of attachments, notes and
// annotations. Base-field aliases do not count; see fieldAllowed.
func ownField(s *schema.Schema, itemType, name string) bool {
	if s.ValidField(itemType, name) || commonItemFields[name] {
		return true
	}
	switch itemType {
	case "attachment":
		return slices.Contains(attachmentExtraFields, name)
	case "note":
		return name == "note"
	case "annotation":
		return slices.Contains(annotationExtraFields, name)
	}
	return false
}

// fieldAllowed reports whether name is a field of the item type, either
// directly or through a base field mapping (for example "publicationTitle"
// on a type that calls it "websiteTitle").
func fieldAllowed(s *schema.Schema, itemType, name string) bool {
	if ownField(s, itemType, name) {
		return true
	}
	for _, f := range s.Fields(itemType) {
		if s.BaseField(itemType, f) == name {
			return true
		}
	}
	return false
}

// validateItemPatch checks the fields a client sent for an item. itemType is
// the type the item will have after the write.
func validateItemPatch(s *schema.Schema, itemType string, patch zotero.OrderedMap) *writeError {
	if !s.HasItemType(itemType) {
		return badRequest("'%s' is not a valid itemType", itemType)
	}
	var werr *writeError
	patch.Each(func(name string, raw json.RawMessage) bool {
		werr = validateItemField(s, itemType, name, raw)
		return werr == nil
	})
	return werr
}

func validateItemField(s *schema.Schema, itemType, name string, raw json.RawMessage) *writeError {
	if !fieldAllowed(s, itemType, name) {
		return badRequest("Invalid field '%s' for item type '%s'", name, itemType)
	}
	switch name {
	case "key", "version", "itemType", "parentItem", "deleted":
		return nil // validated by the caller, which also normalizes them
	case "creators":
		var cs []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &cs); err != nil {
			return badRequest("'creators' must be an array of objects")
		}
		for _, c := range cs {
			var ct string
			if json.Unmarshal(c["creatorType"], &ct) != nil || ct == "" {
				return badRequest("Each creator needs a creatorType")
			}
		}
	case "tags":
		return validateTags(raw)
	case "collections":
		var keys []string
		if err := json.Unmarshal(raw, &keys); err != nil {
			return badRequest("'collections' must be an array of collection keys")
		}
		for _, k := range keys {
			if !zotero.ValidKey(k) {
				return badRequest("'%s' is not a valid collection key", k)
			}
		}
	case "relations":
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return badRequest("'relations' must be an object")
		}
	case "inPublications":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return badRequest("'inPublications' must be a boolean")
		}
	case "dateAdded", "dateModified":
		var d string
		if err := json.Unmarshal(raw, &d); err != nil || !parseTime(d) {
			return badRequest("'%s' must be in ISO 8601 UTC format (2006-01-02T15:04:05Z)", name)
		}
	case "linkMode":
		var m string
		if err := json.Unmarshal(raw, &m); err != nil || !linkModes[m] {
			return badRequest("'%s' is not a valid linkMode", string(raw))
		}
	case "md5":
		var m *string
		if err := json.Unmarshal(raw, &m); err != nil {
			return badRequest("'md5' must be a string or null")
		}
	case "mtime":
		// "" is what some clients send for "no mtime"; it is ignored later.
		var m *int64
		if err := json.Unmarshal(raw, &m); err != nil && string(bytes.TrimSpace(raw)) != `""` {
			return badRequest("'mtime' must be an integer or null")
		}
	case "lastRead":
		if isClearedLastRead(raw) {
			return nil // null, "" and false clear it
		}
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
			return badRequest("'lastRead' must be an integer")
		}
	case "path":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return badRequest("'path' must be a string")
		}
	default:
		if !isScalar(raw) {
			return badRequest("Invalid value for field '%s'", name)
		}
	}
	return nil
}

func validateTags(raw json.RawMessage) *writeError {
	var tags []struct {
		Tag  *string `json:"tag"`
		Type *int    `json:"type"`
	}
	if err := json.Unmarshal(raw, &tags); err != nil {
		return badRequest("'tags' must be an array of {tag, type} objects")
	}
	for _, t := range tags {
		if t.Tag == nil {
			return badRequest("Tag cannot be empty")
		}
		if t.Type != nil && *t.Type != 0 && *t.Type != 1 {
			return badRequest("Tag type must be 0 or 1")
		}
	}
	return nil
}

func validateCollectionPatch(key string, patch zotero.OrderedMap) *writeError {
	var werr *writeError
	patch.Each(func(name string, raw json.RawMessage) bool {
		switch name {
		case "key", "version", "deleted": // deleted is normalized beforehand
		case "name":
			var n string
			if json.Unmarshal(raw, &n) != nil || n == "" {
				werr = badRequest("Collection name cannot be empty")
			}
		case "parentCollection":
			if string(bytes.TrimSpace(raw)) == "false" {
				break
			}
			var p string
			if json.Unmarshal(raw, &p) != nil || !zotero.ValidKey(p) {
				werr = badRequest("'parentCollection' must be false or a collection key")
			} else if p == key {
				werr = conflict("Collection cannot be a child of itself")
			}
		case "relations":
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) != nil {
				werr = badRequest("'relations' must be an object")
			}
		default:
			werr = badRequest("Invalid property '%s' for collection", name)
		}
		return werr == nil
	})
	return werr
}

func validateSearchPatch(patch zotero.OrderedMap) *writeError {
	var werr *writeError
	patch.Each(func(name string, raw json.RawMessage) bool {
		switch name {
		case "key", "version", "deleted": // deleted is normalized beforehand
		case "name":
			var n string
			if json.Unmarshal(raw, &n) != nil || n == "" {
				werr = badRequest("Search name cannot be empty")
			}
		case "conditions":
			var cs []map[string]json.RawMessage
			if json.Unmarshal(raw, &cs) != nil {
				werr = badRequest("'conditions' must be an array of objects")
				break
			}
			for _, c := range cs {
				var cond string
				if json.Unmarshal(c["condition"], &cond) != nil || cond == "" {
					werr = badRequest("Each search condition needs a 'condition'")
					break
				}
			}
		default:
			werr = badRequest("Invalid property '%s' for search", name)
		}
		return werr == nil
	})
	return werr
}

// dropEmptyTags removes tags whose name is empty or only whitespace. Zotero
// can produce them and zotero.org skips them instead of failing the item.
// A tag object without a "tag" member is still left for validateTags.
func dropEmptyTags(patch *zotero.OrderedMap) {
	raw, ok := patch.Get("tags")
	if !ok {
		return
	}
	// Almost no write has an empty tag, so look at the names first and only
	// rebuild the array when one turns up. Untouched bytes also keep
	// unchanged detection exact.
	var names []struct {
		Tag *string `json:"tag"`
	}
	if json.Unmarshal(raw, &names) != nil {
		return
	}
	drop := make([]bool, len(names))
	found := false
	for i, n := range names {
		drop[i] = n.Tag != nil && strings.TrimSpace(*n.Tag) == ""
		found = found || drop[i]
	}
	if !found {
		return
	}
	var tags []json.RawMessage
	if json.Unmarshal(raw, &tags) != nil {
		return
	}
	kept := make([]json.RawMessage, 0, len(tags))
	for i, t := range tags {
		if !drop[i] {
			kept = append(kept, t)
		}
	}
	_ = patch.SetValue("tags", kept)
}

// checkAttachmentExtras covers the attachment fields whose validity depends
// on more than their own value: where the library lives and the link mode.
func checkAttachmentExtras(group bool, p prepared) *writeError {
	// Clearing is harmless anywhere; only a real value is user-library only.
	if raw, ok := p.patch.Get("lastRead"); ok && group && !isClearedLastRead(raw) {
		return badRequest("lastRead can only be set in user libraries")
	}
	if !p.patch.Has("path") {
		return nil
	}
	mode := stringField(p.patch, "linkMode")
	if mode == "" {
		mode = stringField(p.base, "linkMode")
	}
	if mode != "linked_file" {
		return badRequest("'path' can only be set on linked_file attachments")
	}
	if group {
		return badRequest("Linked files can only be added to user libraries")
	}
	return nil
}

// normalizeAttachmentExtras settles the values clients send to mean "none".
// lastRead disappears when cleared, like parentItem. md5 and mtime are
// server-managed once a file was uploaded, so a null or "" from a client that
// doesn't know about the file must not wipe them; when nothing is stored yet,
// "" becomes null so the field never holds an empty string.
func normalizeAttachmentExtras(p *prepared) {
	if raw, ok := p.patch.Get("lastRead"); ok && isClearedLastRead(raw) {
		p.patch.Delete("lastRead")
		p.removed = p.base.Delete("lastRead") || p.removed
	}
	for _, k := range []string{"md5", "mtime"} {
		raw, ok := p.patch.Get(k)
		if !ok {
			continue
		}
		sent := string(bytes.TrimSpace(raw))
		if sent != "null" && sent != `""` {
			continue
		}
		if cur, has := p.base.Get(k); has && string(bytes.TrimSpace(cur)) != "null" {
			p.patch.Delete(k)
		} else if sent == `""` {
			p.patch.Set(k, json.RawMessage("null"))
		}
	}
}

// isClearedLastRead reports the values that clear lastRead. 0 is excluded:
// it is a real (if odd) timestamp.
func isClearedLastRead(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "null", `""`, "false":
		return true
	}
	return false
}
