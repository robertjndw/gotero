package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// maxSchemaSize bounds what Load will read. The official file is a few MiB
// because of the locale tables, so this leaves plenty of headroom.
const maxSchemaSize = 64 << 20

type genField struct {
	Name       string
	Base       string
	Introduced int
}

type genType struct {
	Name     string
	Fields   []genField
	Creators []string
}

type fieldInfo struct {
	base       string
	introduced int
}

type itemType struct {
	fields   []string
	info     map[string]fieldInfo
	creators []string
}

// Schema is an immutable description of the Zotero item model: which item
// types exist, which fields each one has (in schema order) and which creator
// types are allowed. It is safe for concurrent use.
type Schema struct {
	version int
	names   []string
	types   map[string]*itemType
}

func newSchema(version int, order []string) *Schema {
	return &Schema{version: version, names: order, types: make(map[string]*itemType, len(order))}
}

var (
	defaultOnce   sync.Once
	defaultSchema *Schema
)

// Default returns the schema compiled into this package from the generated
// tables. Every call returns the same instance.
func Default() *Schema {
	defaultOnce.Do(func() {
		names := make([]string, 0, len(genTypes))
		for _, t := range genTypes {
			names = append(names, t.Name)
		}
		s := newSchema(genVersion, names)
		for _, t := range genTypes {
			it := &itemType{
				fields:   make([]string, 0, len(t.Fields)),
				info:     make(map[string]fieldInfo, len(t.Fields)),
				creators: t.Creators,
			}
			for _, f := range t.Fields {
				it.fields = append(it.fields, f.Name)
				it.info[f.Name] = fieldInfo{base: f.Base, introduced: f.Introduced}
			}
			s.types[t.Name] = it
		}
		defaultSchema = s
	})
	return defaultSchema
}

// rawSchema is the slice of schema.json we care about. Everything else (CSL
// mappings, locale strings) is deliberately skipped.
type rawSchema struct {
	Version   int `json:"version"`
	ItemTypes []struct {
		ItemType string `json:"itemType"`
		Fields   []struct {
			Field     string `json:"field"`
			BaseField string `json:"baseField"`
		} `json:"fields"`
		CreatorTypes []struct {
			CreatorType string `json:"creatorType"`
		} `json:"creatorTypes"`
	} `json:"itemTypes"`
}

// Load parses an official schema.json. Field introduction versions are not
// recorded in that file, so FieldIntroduced reports 0 for every field of a
// loaded schema, which means "never filter".
func Load(r io.Reader) (*Schema, error) {
	var raw rawSchema
	if err := json.NewDecoder(io.LimitReader(r, maxSchemaSize)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("schema: decode: %w", err)
	}
	if raw.Version <= 0 {
		return nil, errors.New("schema: missing or invalid version")
	}
	if len(raw.ItemTypes) == 0 {
		return nil, errors.New("schema: no item types")
	}
	names := make([]string, 0, len(raw.ItemTypes))
	s := newSchema(raw.Version, names)
	for _, t := range raw.ItemTypes {
		if t.ItemType == "" {
			return nil, errors.New("schema: item type without a name")
		}
		if _, dup := s.types[t.ItemType]; dup {
			return nil, fmt.Errorf("schema: duplicate item type %q", t.ItemType)
		}
		it := &itemType{
			fields:   make([]string, 0, len(t.Fields)),
			info:     make(map[string]fieldInfo, len(t.Fields)),
			creators: make([]string, 0, len(t.CreatorTypes)),
		}
		for _, f := range t.Fields {
			it.fields = append(it.fields, f.Field)
			it.info[f.Field] = fieldInfo{base: f.BaseField}
		}
		for _, c := range t.CreatorTypes {
			it.creators = append(it.creators, c.CreatorType)
		}
		s.names = append(s.names, t.ItemType)
		s.types[t.ItemType] = it
	}
	return s, nil
}

// Version returns the schema version number.
func (s *Schema) Version() int { return s.version }

// ItemTypes returns the item type names in schema order. The caller owns the
// returned slice.
func (s *Schema) ItemTypes() []string { return append([]string(nil), s.names...) }

// HasItemType reports whether the schema defines the item type.
func (s *Schema) HasItemType(itemType string) bool {
	_, ok := s.types[itemType]
	return ok
}

// Fields returns the item type's fields in schema order, or nil for an unknown
// type. The slice is shared with the schema and must not be modified.
func (s *Schema) Fields(itemType string) []string {
	if t := s.types[itemType]; t != nil {
		return t.fields
	}
	return nil
}

// ValidField reports whether field belongs to the item type.
func (s *Schema) ValidField(itemType, field string) bool {
	t := s.types[itemType]
	if t == nil {
		return false
	}
	_, ok := t.info[field]
	return ok
}

// BaseField returns the base field a type-specific field maps to (for
// example "medium" for a book's "format"), or "" when there is no mapping.
func (s *Schema) BaseField(itemType, field string) string {
	if t := s.types[itemType]; t != nil {
		return t.info[field].base
	}
	return ""
}

// CreatorTypes returns the allowed creator types in schema order, where the
// first one is the primary type. The slice is shared and must not be
// modified.
func (s *Schema) CreatorTypes(itemType string) []string {
	if t := s.types[itemType]; t != nil {
		return t.creators
	}
	return nil
}

// FieldIntroduced returns the first schema version that contained the field
// for this item type, or 0 when unknown or not a schema field.
func (s *Schema) FieldIntroduced(itemType, field string) int {
	if t := s.types[itemType]; t != nil {
		return t.info[field].introduced
	}
	return 0
}

// SortedItemTypes is like ItemTypes but alphabetical, handy for stable output.
func (s *Schema) SortedItemTypes() []string {
	out := s.ItemTypes()
	sort.Strings(out)
	return out
}
