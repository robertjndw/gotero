package zotero

// ObjectType names a kind of library object.
type ObjectType string

// The object types that live in a library.
const (
	TypeCollection ObjectType = "collection"
	TypeSearch     ObjectType = "search"
	TypeItem       ObjectType = "item"
	TypeSetting    ObjectType = "setting"
	TypeTag        ObjectType = "tag"
)

// Valid reports whether t is one of the known object types.
func (t ObjectType) Valid() bool {
	switch t {
	case TypeCollection, TypeSearch, TypeItem, TypeSetting, TypeTag:
		return true
	}
	return false
}

// Plural returns the URL path segment for the type, e.g. "collections".
func (t ObjectType) Plural() string {
	if t == TypeSearch {
		return "searches"
	}
	return string(t) + "s"
}

// KeyParam returns the query parameter that carries a comma separated key
// list for the type, e.g. "collectionKey". Tags are addressed by name rather
// than key, so they use "tag".
func (t ObjectType) KeyParam() string {
	if t == TypeTag {
		return "tag"
	}
	return string(t) + "Key"
}

// ParseObjectType maps a plural path segment such as "items" back to its
// type.
func ParseObjectType(plural string) (ObjectType, bool) {
	for _, t := range []ObjectType{TypeCollection, TypeSearch, TypeItem, TypeSetting, TypeTag} {
		if t.Plural() == plural {
			return t, true
		}
	}
	return "", false
}
