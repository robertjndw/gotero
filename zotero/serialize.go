package zotero

import (
	"errors"
	"sync"

	"github.com/robertjndw/gotero/zotero/schema"
)

// ErrNoItemType is returned by MarshalItemData when the data has no usable
// "itemType", since the output order can't be chosen without it.
var ErrNoItemType = errors.New("zotero: item data has no itemType")

var (
	itemHead = []string{"key", "version", "parentItem", "itemType"}
	itemTail = []string{"tags", "collections", "relations", "deleted", "inPublications", "dateAdded", "dateModified"}

	// The attachment order is load-bearing: the desktop client breaks when
	// "filename" shows up before "linkMode".
	attachmentFields = []string{"linkMode", "title", "accessDate", "url", "note", "contentType", "charset", "filename", "md5", "mtime"}
	annotationFields = []string{
		"annotationType", "annotationAuthorName", "annotationText", "annotationComment",
		"annotationColor", "annotationPageLabel", "annotationSortIndex", "annotationPosition",
	}
)

// plan is the precomputed output order for one item type.
type plan struct {
	order []string
	// introduced is aligned with order: the schema version in which the field
	// appeared, or 0 for fields that are never filtered.
	introduced []int
}

type planKey struct {
	s        *schema.Schema
	itemType string
}

var plans sync.Map // planKey -> *plan

func planFor(s *schema.Schema, itemType string) *plan {
	k := planKey{s, itemType}
	if p, ok := plans.Load(k); ok {
		return p.(*plan)
	}
	var body []string
	switch itemType {
	case "attachment":
		body = attachmentFields
	case "note":
		body = []string{"note"}
	case "annotation":
		body = annotationFields
	default:
		fields := s.Fields(itemType)
		body = make([]string, 0, len(fields)+1)
		placed := false
		for _, f := range fields {
			body = append(body, f)
			if f == "title" {
				body = append(body, "creators")
				placed = true
			}
		}
		if !placed {
			body = append([]string{"creators"}, body...)
		}
	}
	p := &plan{order: make([]string, 0, len(itemHead)+len(body)+len(itemTail))}
	p.order = append(p.order, itemHead...)
	p.order = append(p.order, body...)
	p.order = append(p.order, itemTail...)
	p.introduced = make([]int, len(p.order))
	for i, f := range p.order {
		p.introduced[i] = s.FieldIntroduced(itemType, f)
	}
	// Item types come from client input; caching arbitrary strings would let
	// a client grow this map without bound.
	if !s.HasItemType(itemType) {
		return p
	}
	actual, _ := plans.LoadOrStore(k, p)
	return actual.(*plan)
}

// MarshalItemData serializes an item's data object in the field order the
// Zotero client expects: key, version, parentItem, itemType, then the type's
// fields in schema order (creators right after title), then tags,
// collections, relations, deleted, inPublications, dateAdded and
// dateModified. Attachments, notes and annotations use fixed layouts instead
// of the schema order. Keys the layout doesn't know are appended in their
// stored order so no data is lost.
//
// When clientSchemaVersion is positive, schema fields introduced after that
// version are left out, because older clients reject fields they don't know.
// A nil schema means schema.Default().
func MarshalItemData(data OrderedMap, s *schema.Schema, clientSchemaVersion int) ([]byte, error) {
	return AppendItemData(make([]byte, 0, data.encodedSize()), data, s, clientSchemaVersion)
}

// AppendItemData is MarshalItemData appending to dst.
func AppendItemData(dst []byte, data OrderedMap, s *schema.Schema, clientSchemaVersion int) ([]byte, error) {
	if s == nil {
		s = schema.Default()
	}
	itemType, ok := data.GetString("itemType")
	if !ok || itemType == "" {
		return dst, ErrNoItemType
	}
	p := planFor(s, itemType)

	// seen records which entries the plan already consumed. A fixed array
	// covers typical items without allocating.
	var small [4]uint64
	seen := small[:]
	if need := (data.Len() + 63) / 64; need > len(small) {
		seen = make([]uint64, need)
	}

	dst = append(dst, '{')
	first := true
	emit := func(i int) {
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = appendString(dst, data.keys[i])
		dst = append(dst, ':')
		if v := data.vals[i]; len(v) > 0 {
			dst = append(dst, v...)
		} else {
			dst = append(dst, "null"...)
		}
	}
	for n, key := range p.order {
		i := data.find(key)
		if i < 0 {
			continue
		}
		// Filtered fields still count as consumed, otherwise the trailing
		// pass would put them right back.
		seen[i/64] |= 1 << (i % 64)
		if clientSchemaVersion > 0 && p.introduced[n] > clientSchemaVersion {
			continue
		}
		emit(i)
	}
	for i := range data.keys {
		if seen[i/64]&(1<<(i%64)) == 0 {
			emit(i)
		}
	}
	return append(dst, '}'), nil
}
