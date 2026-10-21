package pg

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/robertjndw/gotero/internal/repo"
)

// objectTable maps an object type to its table. Table names are only ever
// taken from this whitelist, never from caller input.
func objectTable(t repo.ObjectType) (string, error) {
	switch t {
	case repo.TypeItem:
		return "items", nil
	case repo.TypeCollection:
		return "collections", nil
	case repo.TypeSearch:
		return "searches", nil
	}
	return "", fmt.Errorf("repo: %q is not a stored object type", t)
}

var sortExprs = map[string]string{
	"":                 "o.version",
	"version":          "o.version",
	"dateAdded":        "o.data->>'dateAdded'",
	"dateModified":     "o.data->>'dateModified'",
	"title":            "lower(o.data->>'title')",
	"name":             "lower(o.data->>'name')",
	"itemType":         "o.data->>'itemType'",
	"date":             "o.data->>'date'",
	"publisher":        "lower(o.data->>'publisher')",
	"publicationTitle": "lower(o.data->>'publicationTitle')",
}

// builder accumulates a WHERE clause with positional parameters so user
// values never touch the SQL text.
type builder struct {
	args  []any
	where []string
}

func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

func (b *builder) and(format string, params ...any) {
	ph := make([]any, len(params))
	for i, p := range params {
		ph[i] = b.arg(p)
	}
	b.where = append(b.where, fmt.Sprintf(format, ph...))
}

func (b *builder) whereSQL() string { return strings.Join(b.where, " AND ") }

// filters adds the conditions shared by Versions, Objects, Count and Tags. The
// table is aliased as o. An empty non-nil Keys slice matches nothing, while a
// nil one means "no key filter".
func (b *builder) filters(libraryID int64, q repo.ListQuery, items bool) {
	b.and("o.library_id = %s", libraryID)
	if q.Since > 0 {
		b.and("o.version > %s", q.Since)
	}
	if q.Keys != nil {
		b.and("o.key = ANY(%s)", q.Keys)
	}
	if !items {
		if q.Type == repo.TypeCollection {
			// Collections keep their parent in the JSON document. A top-level
			// collection has either no parentCollection or the value false.
			if q.TopOnly {
				b.where = append(b.where, "(o.data->'parentCollection' IS NULL OR o.data->'parentCollection' = 'false'::jsonb)")
			}
			if q.ParentKey != "" {
				b.and("o.data->>'parentCollection' = %s", q.ParentKey)
			}
		}
		return
	}
	switch {
	case q.OnlyTrashed:
		b.where = append(b.where, "o.in_trash")
	case !q.IncludeTrashed:
		b.where = append(b.where, "NOT o.in_trash")
	}
	if q.ExcludeNotes {
		b.where = append(b.where, "o.item_type <> 'note'")
	}
	if q.TopOnly {
		b.where = append(b.where, "o.parent_key IS NULL")
	}
	if q.ParentKey != "" {
		b.and("o.parent_key = %s", q.ParentKey)
	}
	for _, tag := range q.Tag {
		b.and("EXISTS (SELECT 1 FROM item_tags t WHERE t.library_id = o.library_id AND t.item_key = o.key AND t.tag = %s)", tag)
	}
	if q.Collection != "" {
		b.and("o.data->'collections' ? %s", q.Collection)
	}
}

func versionsSQL(libraryID int64, q repo.VersionsQuery) (string, []any, error) {
	table, err := objectTable(q.Type)
	if err != nil {
		return "", nil, err
	}
	b := &builder{}
	b.filters(libraryID, repo.ListQuery{VersionsQuery: q}, q.Type == repo.TypeItem)
	// Ordering by version alone matches the (library_id, version) index, so
	// big libraries stream without a sort.
	return "SELECT o.key, o.version FROM " + table + " o WHERE " + b.whereSQL() + " ORDER BY o.version", b.args, nil
}

func objectsSQL(libraryID int64, q repo.ListQuery) (string, []any, error) {
	table, err := objectTable(q.Type)
	if err != nil {
		return "", nil, err
	}
	expr, ok := sortExprs[q.Sort]
	if !ok {
		return "", nil, fmt.Errorf("%w %q", repo.ErrUnsupportedSort, q.Sort)
	}
	items := q.Type == repo.TypeItem
	b := &builder{}
	b.filters(libraryID, q, items)

	cols := "o.key, o.version, o.data, o.content_hash, '', '', false"
	if items {
		cols = "o.key, o.version, o.data, o.content_hash, o.item_type, COALESCE(o.parent_key, ''), o.in_trash"
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	// Date columns have second resolution, so version (then key) breaks ties:
	// paging stays stable and "newest first" really is newest first.
	sql := "SELECT " + cols + " FROM " + table + " o WHERE " + b.whereSQL() +
		" ORDER BY " + expr + " " + dir + ", o.version " + dir + ", o.key " + dir
	if q.Limit > 0 {
		sql += " LIMIT " + b.arg(q.Limit)
	}
	if q.Start > 0 {
		sql += " OFFSET " + b.arg(q.Start)
	}
	return sql, b.args, nil
}

func countSQL(libraryID int64, q repo.ListQuery) (string, []any, error) {
	table, err := objectTable(q.Type)
	if err != nil {
		return "", nil, err
	}
	b := &builder{}
	b.filters(libraryID, q, q.Type == repo.TypeItem)
	return "SELECT count(*) FROM " + table + " o WHERE " + b.whereSQL(), b.args, nil
}

func tagsSQL(libraryID int64, q repo.ListQuery) (string, []any) {
	b := &builder{}
	b.filters(libraryID, q, true)
	return `SELECT t.tag, t.type, count(*) FROM item_tags t
		JOIN items o ON o.library_id = t.library_id AND o.key = t.item_key
		WHERE ` + b.whereSQL() + ` GROUP BY t.tag, t.type ORDER BY t.tag, t.type`, b.args
}
