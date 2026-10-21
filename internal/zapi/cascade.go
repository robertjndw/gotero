package zapi

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// rebuildItem re-derives an item's columns and hash after its document
// changed, stamping the given version.
func rebuildItem(key string, m zotero.OrderedMap, version int64) (repo.Object, error) {
	o, err := ObjectFromData(repo.TypeItem, key, m)
	o.Version = version
	return o, err
}

// descendants returns keys plus everything below them: child items for items,
// sub-collections for collections. It walks level by level, so the query
// count depends on depth rather than library size.
func descendants(ctx context.Context, r repo.LibraryReader, t repo.ObjectType, keys []string) ([]string, error) {
	seen := map[string]bool{}
	var all []string
	frontier := keys
	for len(frontier) > 0 {
		var next []string
		for _, k := range frontier {
			if seen[k] {
				continue
			}
			seen[k] = true
			all = append(all, k)
			seq, err := r.Versions(ctx, repo.VersionsQuery{Type: t, ParentKey: k, IncludeTrashed: true})
			if err != nil {
				return nil, err
			}
			for kv, err := range seq {
				if err != nil {
					return nil, err
				}
				next = append(next, kv.Key)
			}
		}
		frontier = next
	}
	return all, nil
}

// deleteKeys removes objects of one type, with the cascades zotero.org
// performs: children go with their parent item, sub-collections go with their
// parent collection, and deleted collections disappear from items. It bumps
// the library version only when something actually exists to delete.
func deleteKeys(ctx context.Context, tx repo.LibraryTx, t repo.ObjectType, keys []string) error {
	existing, err := tx.Get(ctx, t, keys)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}
	present := make([]string, 0, len(existing))
	for _, k := range keys {
		if _, ok := existing[k]; ok {
			present = append(present, k)
		}
	}
	ver, err := tx.NextVersion(ctx)
	if err != nil {
		return err
	}
	switch t {
	case repo.TypeItem:
		all, err := descendants(ctx, tx, repo.TypeItem, present)
		if err != nil {
			return err
		}
		// The attachment_files rows go with the items through the foreign
		// key, which cannot adjust the library's storage counter.
		freed, err := tx.AttachmentFilesSize(ctx, all)
		if err != nil {
			return err
		}
		if _, err = tx.DeleteObjects(ctx, t, all, ver); err != nil {
			return err
		}
		if freed > 0 {
			return tx.AddStorageUsed(ctx, -freed)
		}
		return nil
	case repo.TypeCollection:
		all, err := descendants(ctx, tx, repo.TypeCollection, present)
		if err != nil {
			return err
		}
		if err := detachItems(ctx, tx, all, ver); err != nil {
			return err
		}
		_, err = tx.DeleteObjects(ctx, t, all, ver)
		return err
	default:
		_, err = tx.DeleteObjects(ctx, t, present, ver)
		return err
	}
}

// detachItems removes the collections from every item that is in one of
// them. Those items change, so they get the new version and clients see it.
func detachItems(ctx context.Context, tx repo.LibraryTx, collections []string, ver int64) error {
	gone := map[string]bool{}
	for _, k := range collections {
		gone[k] = true
	}
	affected := map[string]repo.Object{}
	for _, ck := range collections {
		seq, err := tx.Objects(ctx, repo.ListQuery{
			VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, IncludeTrashed: true}, Collection: ck,
		})
		if err != nil {
			return err
		}
		for o, err := range seq {
			if err != nil {
				return err
			}
			affected[o.Key] = o
		}
	}
	objs := make([]repo.Object, 0, len(affected))
	for _, o := range affected {
		var m zotero.OrderedMap
		if err := json.Unmarshal(o.Data, &m); err != nil {
			return err
		}
		cols := slices.DeleteFunc(stringList(m, "collections"), func(k string) bool { return gone[k] })
		if err := m.SetValue("collections", nonNilStrings(cols)); err != nil {
			return err
		}
		no, err := rebuildItem(o.Key, m, ver)
		if err != nil {
			return err
		}
		objs = append(objs, no)
	}
	return tx.PutObjects(ctx, repo.TypeItem, objs)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
