package importer

import (
	"context"
	"slices"
	"sync"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

// fulltextBatch bounds how much indexed text is held and written at once.
const fulltextBatch = 20

func (r *run) syncSettings(ctx context.Context, l *lib) error {
	remote, lmv, err := l.src.Settings(ctx, 0)
	if err != nil {
		return err
	}
	l.observe(r, lmv)
	rd, err := r.w.Store.Library(ctx, l.id)
	if err != nil {
		return err
	}
	have, err := rd.Settings(ctx, 0)
	if err != nil {
		return err
	}
	cur := make(map[string]repo.Setting, len(have))
	for _, s := range have {
		cur[s.Key] = s
	}
	var put []repo.Setting
	var keys []string
	var top int64
	for k, s := range remote {
		top = max(top, s.Version)
		c, ok := cur[k]
		if l.fresh && ok && c.Version == s.Version && zotero.EqualJSON(c.Value, s.Value) {
			continue
		}
		// Settings carry no import record, so outside a fresh import an
		// existing one may be a gotero edit and stays as it is.
		if !l.fresh && ok {
			continue
		}
		put = append(put, repo.Setting{Key: k, Value: s.Value, Version: s.Version})
		keys = append(keys, k)
	}
	if len(put) == 0 {
		return nil
	}
	_, err = r.w.Store.InLibrary(ctx, l.id, func(tx repo.LibraryTx) error {
		if err := tx.ClearDeleted(ctx, repo.TypeSetting, keys); err != nil {
			return err
		}
		if !l.fresh {
			v, err := tx.NextVersion(ctx)
			if err != nil {
				return err
			}
			for i := range put {
				put[i].Version = v
			}
		}
		if err := tx.PutSettings(ctx, put); err != nil {
			return err
		}
		if l.fresh {
			return tx.RaiseVersion(ctx, top)
		}
		return nil
	})
	return err
}

func (r *run) syncFulltext(ctx context.Context, l *lib) error {
	remote, lmv, err := l.src.FulltextVersions(ctx, 0)
	if zoteroclient.IsForbidden(err) || zoteroclient.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	l.observe(r, lmv)
	rd, err := r.w.Store.Library(ctx, l.id)
	if err != nil {
		return err
	}
	have, err := rd.FulltextVersions(ctx, 0)
	if err != nil {
		return err
	}
	var need []string
	for k, v := range remote {
		// Outside a fresh import versions are not comparable; existing full
		// text is left as it is.
		held, ok := have[k]
		if (l.fresh && held != v) || (!l.fresh && !ok) {
			need = append(need, k)
		}
	}
	slices.Sort(need)

	for chunk := range slices.Chunk(need, fulltextBatch) {
		// Full text belongs to an item; one that failed to import or was
		// deleted meanwhile has nowhere to attach.
		items, err := rd.Get(ctx, repo.TypeItem, chunk)
		if err != nil {
			return err
		}
		var (
			mu  sync.Mutex
			out []repo.Fulltext
			top int64
		)
		err = parallel(ctx, fileWorkers, len(chunk), func(ctx context.Context, i int) error {
			key := chunk[i]
			if _, ok := items[key]; !ok {
				return nil
			}
			c, _, err := l.src.Fulltext(ctx, key)
			if zoteroclient.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
			f := repo.Fulltext{ItemKey: key, Content: c.Content, Version: remote[key]}
			if c.UsesPages() {
				f.IndexedPages, f.TotalPages = &c.IndexedPages, &c.TotalPages
			} else {
				f.IndexedChars, f.TotalChars = &c.IndexedChars, &c.TotalChars
			}
			mu.Lock()
			out = append(out, f)
			top = max(top, f.Version)
			mu.Unlock()
			return nil
		})
		if err != nil {
			return err
		}
		if len(out) > 0 {
			_, err = r.w.Store.InLibrary(ctx, l.id, func(tx repo.LibraryTx) error {
				if !l.fresh {
					v, err := tx.NextVersion(ctx)
					if err != nil {
						return err
					}
					for i := range out {
						out[i].Version = v
					}
				}
				if err := tx.PutFulltext(ctx, out); err != nil {
					return err
				}
				if l.fresh {
					return tx.RaiseVersion(ctx, top)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		if err := r.checkpoint(ctx, false); err != nil {
			return err
		}
	}
	return nil
}
