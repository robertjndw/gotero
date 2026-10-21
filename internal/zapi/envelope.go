package zapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zotero/schema"
)

// apiRoot returns the externally visible API root, including whatever prefix
// the server mounted the API under, without a trailing slash.
func apiRoot(c *gin.Context) string {
	base := httpx.BaseURL(c.Request.Context())
	if base == "" {
		scheme := "http"
		if c.Request.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + c.Request.Host
	}
	p := c.Request.URL.Path
	cut := len(p)
	for _, marker := range []string{"/users/", "/groups/", "/keys"} {
		if i := strings.Index(p, marker); i >= 0 && i < cut {
			cut = i
		}
	}
	return base + p[:cut]
}

// clientSchemaVersion reads Zotero-Schema-Version. Missing or garbage means
// "don't filter", the safe choice for non-Zotero API users.
func clientSchemaVersion(c *gin.Context) int {
	n, err := strconv.Atoi(c.GetHeader("Zotero-Schema-Version"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// builder renders stored objects as API envelopes for one request.
type builder struct {
	schema        *schema.Schema
	schemaVersion int
	root          string // API root
	libPath       string // e.g. /users/1
	libJSON       []byte
}

func (a *API) newBuilder(c *gin.Context, l *LibraryContext) *builder {
	root := apiRoot(c)
	libPath := l.Topic()
	lj, _ := json.Marshal(zotero.Library{
		Type: l.Type, ID: l.ID, Name: l.Name,
		Links: zotero.Links{"alternate": {Href: root + libPath, Type: "text/html"}},
	})
	return &builder{schema: a.schema, schemaVersion: clientSchemaVersion(c), root: root, libPath: libPath, libJSON: lj}
}

var (
	collectionOrder = []string{"name", "parentCollection", "relations"}
	searchOrder     = []string{"name", "conditions"}
)

// dataJSON renders an object's data with key and version first. Stored data
// has neither, since both live in their own columns.
func (b *builder) dataJSON(t repo.ObjectType, o repo.Object) (zotero.OrderedMap, []byte, error) {
	var m zotero.OrderedMap
	if err := json.Unmarshal(o.Data, &m); err != nil {
		return m, nil, fmt.Errorf("decode %s %s: %w", t, o.Key, err)
	}
	if t == repo.TypeItem {
		m.Set("key", quote(o.Key))
		m.Set("version", json.RawMessage(strconv.FormatInt(o.Version, 10)))
		out, err := zotero.MarshalItemData(m, b.schema, b.schemaVersion)
		return m, out, err
	}
	order := collectionOrder
	if t == repo.TypeSearch {
		order = searchOrder
	}
	var ordered zotero.OrderedMap
	ordered.Set("key", quote(o.Key))
	ordered.Set("version", json.RawMessage(strconv.FormatInt(o.Version, 10)))
	for _, k := range order {
		if v, ok := m.Get(k); ok {
			ordered.Set(k, v)
		}
	}
	m.Each(func(k string, v json.RawMessage) bool {
		if !ordered.Has(k) {
			ordered.Set(k, v)
		}
		return true
	})
	return m, ordered.AppendJSON(nil), nil
}

func quote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// appendObject appends the full envelope for o. The envelope is assembled by
// hand so the data member is copied verbatim instead of being re-escaped.
func (b *builder) appendObject(dst []byte, t repo.ObjectType, o repo.Object, meta zotero.Meta) ([]byte, error) {
	m, data, err := b.dataJSON(t, o)
	if err != nil {
		return dst, err
	}
	if t == repo.TypeItem {
		b.fillItemMeta(&meta, m)
	}
	path := fmt.Sprintf("%s/%s/%s", b.libPath, pluralOf(t), o.Key)
	links := zotero.Links{
		"self":      {Href: b.root + path, Type: "application/json"},
		"alternate": {Href: b.root + path, Type: "text/html"},
	}
	if t == repo.TypeItem && o.ParentKey != "" {
		links["up"] = zotero.Link{Href: fmt.Sprintf("%s%s/items/%s", b.root, b.libPath, o.ParentKey), Type: "application/json"}
	}
	lj, _ := json.Marshal(links)
	mj, _ := json.Marshal(meta)

	dst = append(dst, `{"key":`...)
	dst = strconv.AppendQuote(dst, o.Key)
	dst = append(dst, `,"version":`...)
	dst = strconv.AppendInt(dst, o.Version, 10)
	dst = append(dst, `,"library":`...)
	dst = append(dst, b.libJSON...)
	dst = append(dst, `,"links":`...)
	dst = append(dst, lj...)
	dst = append(dst, `,"meta":`...)
	dst = append(dst, mj...)
	dst = append(dst, `,"data":`...)
	dst = append(dst, data...)
	return append(dst, '}'), nil
}

var parsedDateRe = regexp.MustCompile(`^(\d{4})(?:-(\d{1,2})(?:-(\d{1,2}))?)?$`)

func (b *builder) fillItemMeta(meta *zotero.Meta, m zotero.OrderedMap) {
	if d, ok := m.GetString("date"); ok {
		if g := parsedDateRe.FindStringSubmatch(strings.TrimSpace(d)); g != nil {
			s := g[1]
			if g[2] != "" {
				s += "-" + pad2(g[2])
				if g[3] != "" {
					s += "-" + pad2(g[3])
				}
			}
			meta.ParsedDate = s
		}
	}
	if raw, ok := m.Get("creators"); ok {
		itemType, _ := m.GetString("itemType")
		meta.CreatorSummary = creatorSummary(raw, itemType, b.schema)
	}
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

// creatorSummary follows the usual convention: "Smith", "Smith and Jones",
// "Smith et al.", using authors first and falling back to editors, then
// contributors.
func creatorSummary(raw json.RawMessage, itemType string, s *schema.Schema) string {
	var creators []struct {
		Type     string `json:"creatorType"`
		LastName string `json:"lastName"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(raw, &creators) != nil || len(creators) == 0 {
		return ""
	}
	primary := "author"
	if types := s.CreatorTypes(itemType); len(types) > 0 {
		primary = types[0]
	}
	for _, want := range []string{primary, "editor", "contributor"} {
		var names []string
		for _, c := range creators {
			if c.Type != want {
				continue
			}
			n := c.LastName
			if n == "" {
				n = c.Name
			}
			if n != "" {
				names = append(names, n)
			}
		}
		switch len(names) {
		case 0:
			continue
		case 1:
			return names[0]
		case 2:
			return names[0] + " and " + names[1]
		default:
			return names[0] + " et al."
		}
	}
	return ""
}
