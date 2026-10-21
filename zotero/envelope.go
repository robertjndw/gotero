package zotero

import "encoding/json"

// Link is an entry of a "links" object.
type Link struct {
	Href string `json:"href"`
	Type string `json:"type,omitempty"`
}

// Links maps relation names such as "self" and "alternate" to links.
type Links map[string]Link

// Library identifies the library an object belongs to.
type Library struct {
	// Type is "user" or "group".
	Type  string `json:"type"`
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Links Links  `json:"links"`
}

// MarshalJSON writes missing links as {}.
func (l Library) MarshalJSON() ([]byte, error) {
	if l.Links == nil {
		l.Links = Links{}
	}
	type plain Library
	return json.Marshal(plain(l))
}

// Meta is the "meta" object of an API response entry. Which members appear
// depends on the object type: items carry creatorSummary, parsedDate and
// numChildren, collections carry numCollections and numItems.
type Meta struct {
	CreatorSummary string `json:"creatorSummary,omitempty"`
	ParsedDate     string `json:"parsedDate,omitempty"`
	// NumChildren is a pointer because 0 is meaningful for items that can have children.
	NumChildren    *int `json:"numChildren,omitempty"`
	NumCollections *int `json:"numCollections,omitempty"`
	NumItems       *int `json:"numItems,omitempty"`
}

// Object is the envelope around a collection, search or item in API
// responses. Data is the object's own JSON as produced by MarshalItemData or
// the stored form for collections and searches.
type Object struct {
	Key     string          `json:"key"`
	Version int64           `json:"version"`
	Library Library         `json:"library"`
	Links   Links           `json:"links"`
	Meta    Meta            `json:"meta"`
	Data    json.RawMessage `json:"data"`
}

// MarshalJSON writes missing links as {}.
func (o Object) MarshalJSON() ([]byte, error) {
	if o.Links == nil {
		o.Links = Links{}
	}
	type plain Object
	return json.Marshal(plain(o))
}

// UserAccess is the user library part of an API key's permissions. Members
// that are false are left out, as zotero.org does.
type UserAccess struct {
	Library bool `json:"library,omitempty"`
	Files   bool `json:"files,omitempty"`
	Notes   bool `json:"notes,omitempty"`
	Write   bool `json:"write,omitempty"`
}

// GroupAccess is the permission an API key has on a group library.
type GroupAccess struct {
	Library bool `json:"library,omitempty"`
	Write   bool `json:"write,omitempty"`
}

// AllGroups is the key in Access.Groups that applies to every group.
const AllGroups = "all"

// Access describes what an API key may do. Groups is keyed by AllGroups or by
// a decimal group ID.
type Access struct {
	User   UserAccess             `json:"user"`
	Groups map[string]GroupAccess `json:"groups"`
}

// MarshalJSON writes nil Groups as {}.
func (a Access) MarshalJSON() ([]byte, error) {
	if a.Groups == nil {
		a.Groups = map[string]GroupAccess{}
	}
	type plain Access
	return json.Marshal(plain(a))
}

// KeyInfo is the body of GET keys/current.
type KeyInfo struct {
	Key         string `json:"key"`
	UserID      int64  `json:"userID"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Access      Access `json:"access"`
}

// GroupData is the "data" member of a group.
type GroupData struct {
	ID             int64   `json:"id"`
	Version        int64   `json:"version"`
	Name           string  `json:"name"`
	Owner          int64   `json:"owner"`
	Type           string  `json:"type"`
	Description    string  `json:"description"`
	URL            string  `json:"url"`
	LibraryEditing string  `json:"libraryEditing"`
	LibraryReading string  `json:"libraryReading"`
	FileEditing    string  `json:"fileEditing"`
	Admins         []int64 `json:"admins"`
	Members        []int64 `json:"members"`
}

// MarshalJSON writes nil Admins and Members as []. A null there makes the
// client treat the library as read-only.
func (g GroupData) MarshalJSON() ([]byte, error) {
	if g.Admins == nil {
		g.Admins = []int64{}
	}
	if g.Members == nil {
		g.Members = []int64{}
	}
	type plain GroupData
	return json.Marshal(plain(g))
}

// GroupMeta is the "meta" object of a group.
type GroupMeta struct {
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	NumItems     int    `json:"numItems"`
}

// Group is the body of GET groups/{id}.
type Group struct {
	ID      int64     `json:"id"`
	Version int64     `json:"version"`
	Links   Links     `json:"links"`
	Meta    GroupMeta `json:"meta"`
	Data    GroupData `json:"data"`
}

// MarshalJSON writes missing links as {}.
func (g Group) MarshalJSON() ([]byte, error) {
	if g.Links == nil {
		g.Links = Links{}
	}
	type plain Group
	return json.Marshal(plain(g))
}

// FulltextContent is a full-text record. A text document counts characters,
// a PDF counts pages; the unit that doesn't apply is left out of the output.
type FulltextContent struct {
	// Key is set in POST fulltext bodies and omitted elsewhere.
	Key          string `json:"key,omitempty"`
	Content      string `json:"content"`
	IndexedChars int    `json:"indexedChars"`
	TotalChars   int    `json:"totalChars"`
	IndexedPages int    `json:"indexedPages"`
	TotalPages   int    `json:"totalPages"`
}

// UsesPages reports whether the record is measured in pages.
func (f FulltextContent) UsesPages() bool { return f.IndexedPages != 0 || f.TotalPages != 0 }

// MarshalJSON emits either the page counters or the character counters.
func (f FulltextContent) MarshalJSON() ([]byte, error) {
	type chars struct {
		Key          string `json:"key,omitempty"`
		Content      string `json:"content"`
		IndexedChars int    `json:"indexedChars"`
		TotalChars   int    `json:"totalChars"`
	}
	type pages struct {
		Key          string `json:"key,omitempty"`
		Content      string `json:"content"`
		IndexedPages int    `json:"indexedPages"`
		TotalPages   int    `json:"totalPages"`
	}
	if f.UsesPages() {
		return json.Marshal(pages{f.Key, f.Content, f.IndexedPages, f.TotalPages})
	}
	return json.Marshal(chars{f.Key, f.Content, f.IndexedChars, f.TotalChars})
}
