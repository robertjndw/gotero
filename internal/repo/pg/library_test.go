package pg_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
)

func TestInLibrarySerializesWrites(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")

	const n = 20
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.InLibrary(ctx(), u.LibraryID, func(tx repo.LibraryTx) error {
				v, err := tx.NextVersion(ctx())
				if err != nil {
					return err
				}
				mu.Lock()
				dup := seen[v]
				seen[v] = true
				mu.Unlock()
				if dup {
					return fmt.Errorf("duplicate version %d", v)
				}
				return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item(fmt.Sprintf("K%07d", i), v, nil)})
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	lib, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	assert.EqualValues(t, n, lib.Version())
	assert.Len(t, seen, n)
	cnt, err := lib.Count(ctx(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}})
	require.NoError(t, err)
	assert.EqualValues(t, n, cnt)
}

func TestNextVersionBumpsOnce(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	v := write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		a, err := tx.NextVersion(ctx())
		require.NoError(t, err)
		b, err := tx.NextVersion(ctx())
		require.NoError(t, err)
		assert.Equal(t, a, b)
		assert.Equal(t, a, tx.Version())
		return nil
	})
	assert.EqualValues(t, 1, v)
}

func TestInLibraryWithoutNextVersionKeepsVersion(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		_, err := tx.NextVersion(ctx())
		return err
	})
	v := write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		assert.EqualValues(t, 1, tx.Version())
		return nil
	})
	assert.EqualValues(t, 1, v)
}

func TestInLibraryRollsBackOnError(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	boom := errors.New("boom")
	_, err := s.InLibrary(ctx(), u.LibraryID, func(tx repo.LibraryTx) error {
		v, err := tx.NextVersion(ctx())
		require.NoError(t, err)
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", v, nil)}))
		return boom
	})
	require.ErrorIs(t, err, boom)

	lib, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, lib.Version())
	assert.Empty(t, collectVersions(t, lib, repo.VersionsQuery{Type: repo.TypeItem}))
}

func TestInLibraryUnknownLibrary(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.InLibrary(ctx(), 999, func(repo.LibraryTx) error { return nil })
	assert.ErrorIs(t, err, repo.ErrNotFound)
	_, err = s.Library(ctx(), 999)
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestInLibraryLockTimeoutIsBusy(t *testing.T) {
	s, _ := newStore(t, pg.WithLockTimeout(150*time.Millisecond))
	u := mustUser(t, s, "alice")

	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.InLibrary(ctx(), u.LibraryID, func(repo.LibraryTx) error {
			close(locked)
			<-release
			return nil
		})
		done <- err
	}()
	<-locked

	start := time.Now()
	_, err := s.InLibrary(ctx(), u.LibraryID, func(repo.LibraryTx) error {
		t.Error("callback must not run without the lock")
		return nil
	})
	require.ErrorIs(t, err, repo.ErrBusy)
	assert.Less(t, time.Since(start), 5*time.Second)

	close(release)
	require.NoError(t, <-done)

	// The lock is usable again once the holder is gone.
	_, err = s.InLibrary(ctx(), u.LibraryID, func(repo.LibraryTx) error { return nil })
	require.NoError(t, err)
}

func TestLockTimeoutDoesNotLeakIntoPool(t *testing.T) {
	// SET LOCAL must not stick to pooled connections.
	s, pool := newStore(t, pg.WithLockTimeout(7*time.Second))
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(repo.LibraryTx) error { return nil })
	var v string
	require.NoError(t, pool.QueryRow(ctx(), `SHOW lock_timeout`).Scan(&v))
	assert.Equal(t, "0", v)
}

func TestPutAndGetObjects(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")

	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		v, _ := tx.NextVersion(ctx())
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{
			item("AAAAAAAA", v, func(o *repo.Object) { o.Tags = []repo.ItemTag{{Tag: "go"}, {Tag: "db", Type: 1}} }),
			item("BBBBBBBB", v, func(o *repo.Object) { o.ParentKey = "AAAAAAAA"; o.ItemType = "note" }),
			item("CCCCCCCC", v, func(o *repo.Object) { o.InTrash = true }),
		}))
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeCollection, []repo.Object{
			{Key: "COLLCOLL", Version: v, Data: json.RawMessage(`{"name":"c"}`), ContentHash: []byte{1}},
		}))
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeSearch, []repo.Object{
			{Key: "SEARSEAR", Version: v, Data: json.RawMessage(`{"name":"s"}`)},
		}))
		return nil
	})

	r, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)

	got, err := r.Get(ctx(), repo.TypeItem, []string{"AAAAAAAA", "BBBBBBBB", "CCCCCCCC", "MISSING1"})
	require.NoError(t, err)
	require.Len(t, got, 3, "Get includes trashed items and ignores missing keys")
	assert.Equal(t, "note", got["BBBBBBBB"].ItemType)
	assert.Equal(t, "AAAAAAAA", got["BBBBBBBB"].ParentKey)
	assert.True(t, got["CCCCCCCC"].InTrash)
	assert.Empty(t, got["AAAAAAAA"].ParentKey)
	assert.JSONEq(t, `{"key":"AAAAAAAA","itemType":"book"}`, string(got["AAAAAAAA"].Data))
	assert.Equal(t, []byte("AAAAAAAA"), got["AAAAAAAA"].ContentHash)

	cols, err := r.Get(ctx(), repo.TypeCollection, []string{"COLLCOLL"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"c"}`, string(cols["COLLCOLL"].Data))
	srch, err := r.Get(ctx(), repo.TypeSearch, []string{"SEARSEAR"})
	require.NoError(t, err)
	assert.Len(t, srch, 1)

	empty, err := r.Get(ctx(), repo.TypeItem, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	_, err = r.Get(ctx(), repo.TypeTag, []string{"x"})
	assert.Error(t, err)
}

func TestPutObjectsUpsertsAndReplacesTags(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	put := func(o ...repo.Object) {
		write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
			return tx.PutObjects(ctx(), repo.TypeItem, o)
		})
	}
	put(item("AAAAAAAA", 1, func(o *repo.Object) { o.Tags = []repo.ItemTag{{Tag: "old"}, {Tag: "keep"}} }))
	put(item("AAAAAAAA", 2, func(o *repo.Object) {
		o.ItemType = "article"
		o.Tags = []repo.ItemTag{{Tag: "keep"}, {Tag: "new", Type: 1}, {Tag: "new", Type: 1}}
	}))

	r, _ := s.Library(ctx(), u.LibraryID)
	got, _ := r.Get(ctx(), repo.TypeItem, []string{"AAAAAAAA"})
	assert.EqualValues(t, 2, got["AAAAAAAA"].Version)
	assert.Equal(t, "article", got["AAAAAAAA"].ItemType)

	tags, err := r.Tags(ctx(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}})
	require.NoError(t, err)
	assert.Equal(t, []repo.ItemTag{{Tag: "keep", NumItems: 1}, {Tag: "new", Type: 1, NumItems: 1}}, tags)

	// Dropping all tags clears them.
	put(item("AAAAAAAA", 3, nil))
	tags, err = r.Tags(ctx(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}})
	require.NoError(t, err)
	assert.Empty(t, tags)
}

func TestPutObjectsDuplicateKeysLastWins(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{
			item("AAAAAAAA", 1, nil), item("AAAAAAAA", 2, nil),
		})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	got, _ := r.Get(ctx(), repo.TypeItem, []string{"AAAAAAAA"})
	assert.EqualValues(t, 2, got["AAAAAAAA"].Version)
}

func TestPutFiftyObjectsInOneCall(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	objs := make([]repo.Object, 50)
	for i := range objs {
		objs[i] = item(fmt.Sprintf("K%07d", i), 1, func(o *repo.Object) {
			o.Tags = []repo.ItemTag{{Tag: "a"}, {Tag: "b"}}
		})
	}
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeItem, objs)
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	n, err := r.Count(ctx(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}})
	require.NoError(t, err)
	assert.EqualValues(t, 50, n)
	n, err = r.Count(ctx(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}, Tag: []string{"a", "b"}})
	require.NoError(t, err)
	assert.EqualValues(t, 50, n)
}

func TestDeleteObjectsLogsAndCascades(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	require.NoError(t, s.Files().PutBlob(ctx(), repo.Blob{MD5: "0123456789abcdef0123456789abcdef", Size: 5}))

	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{
			item("AAAAAAAA", 1, func(o *repo.Object) { o.Tags = []repo.ItemTag{{Tag: "t"}} }),
			item("BBBBBBBB", 1, nil),
		}))
		require.NoError(t, tx.PutFulltext(ctx(), []repo.Fulltext{{ItemKey: "AAAAAAAA", Content: "hello", Version: 1}}))
		return tx.PutAttachmentFile(ctx(), repo.AttachmentFile{
			ItemKey: "AAAAAAAA", MD5: "0123456789abcdef0123456789abcdef", Filename: "a.pdf", Size: 5,
		})
	})

	var removed []string
	v := write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		v, _ := tx.NextVersion(ctx())
		var err error
		removed, err = tx.DeleteObjects(ctx(), repo.TypeItem, []string{"AAAAAAAA", "NOPENOPE"}, v)
		return err
	})
	assert.Equal(t, []string{"AAAAAAAA"}, removed)

	for _, table := range []string{"item_tags", "fulltext", "attachment_files"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx(), `SELECT count(*) FROM `+table).Scan(&n))
		assert.Zerof(t, n, "%s rows should cascade", table)
	}

	r, _ := s.Library(ctx(), u.LibraryID)
	del, err := r.Deleted(ctx(), 0)
	require.NoError(t, err)
	assert.Equal(t, map[repo.ObjectType][]string{repo.TypeItem: {"AAAAAAAA"}}, del)
	del, _ = r.Deleted(ctx(), v)
	assert.Empty(t, del, "since is exclusive")

	// Deleting again just moves the log entry to the newer version.
	v2 := write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		v, _ := tx.NextVersion(ctx())
		return tx.LogDeleted(ctx(), repo.TypeItem, []string{"AAAAAAAA"}, v)
	})
	r, _ = s.Library(ctx(), u.LibraryID)
	del, _ = r.Deleted(ctx(), v)
	assert.Equal(t, []string{"AAAAAAAA"}, del[repo.TypeItem])
	assert.Greater(t, v2, v)
}

func TestDeleteCollectionsAndSearches(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeCollection, []repo.Object{{Key: "COLLCOLL", Version: 1, Data: json.RawMessage(`{}`)}}))
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeSearch, []repo.Object{{Key: "SEARSEAR", Version: 1, Data: json.RawMessage(`{}`)}}))
		c, err := tx.DeleteObjects(ctx(), repo.TypeCollection, []string{"COLLCOLL"}, 2)
		require.NoError(t, err)
		assert.Equal(t, []string{"COLLCOLL"}, c)
		c, err = tx.DeleteObjects(ctx(), repo.TypeSearch, []string{"SEARSEAR"}, 2)
		require.NoError(t, err)
		assert.Equal(t, []string{"SEARSEAR"}, c)
		return nil
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	del, _ := r.Deleted(ctx(), 0)
	assert.Equal(t, []string{"COLLCOLL"}, del[repo.TypeCollection])
	assert.Equal(t, []string{"SEARSEAR"}, del[repo.TypeSearch])
}

func TestPutDoesNotClearDeletedLog(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.LogDeleted(ctx(), repo.TypeItem, []string{"AAAAAAAA", "BBBBBBBB"}, 3)
	})
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 4, nil)})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	del, _ := r.Deleted(ctx(), 0)
	assert.Equal(t, []string{"AAAAAAAA", "BBBBBBBB"}, del[repo.TypeItem])

	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.ClearDeleted(ctx(), repo.TypeItem, []string{"AAAAAAAA"})
	})
	r, _ = s.Library(ctx(), u.LibraryID)
	del, _ = r.Deleted(ctx(), 0)
	assert.Equal(t, []string{"BBBBBBBB"}, del[repo.TypeItem])
}

func TestDeletedGroupsByType(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		for _, typ := range []repo.ObjectType{repo.TypeItem, repo.TypeCollection, repo.TypeSearch, repo.TypeTag, repo.TypeSetting} {
			if err := tx.LogDeleted(ctx(), typ, []string{"b", "a"}, 5); err != nil {
				return err
			}
		}
		return nil
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	del, err := r.Deleted(ctx(), 4)
	require.NoError(t, err)
	assert.Len(t, del, 5)
	assert.Equal(t, []string{"a", "b"}, del[repo.TypeTag])
}

func seedQueryLibrary(t *testing.T, s *pg.Store, lib int64) {
	t.Helper()
	write(t, s, lib, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{
			item("AAAAAAA2", 1, func(o *repo.Object) {
				o.Data = itemData("AAAAAAA2", "2020-01-03", "2021-01-01", "COLLCOLA")
				o.Tags = []repo.ItemTag{{Tag: "x"}, {Tag: "y"}}
			}),
			item("BBBBBBB2", 2, func(o *repo.Object) {
				o.Data = itemData("BBBBBBB2", "2020-01-01", "2021-01-03", "COLLCOLA", "COLLCOLB")
				o.Tags = []repo.ItemTag{{Tag: "x"}}
			}),
			item("CCCCCCC2", 2, func(o *repo.Object) {
				o.Data = itemData("CCCCCCC2", "2020-01-02", "2021-01-02")
				o.ParentKey = "AAAAAAA2"
				o.Tags = []repo.ItemTag{{Tag: "y", Type: 1}}
			}),
			item("DDDDDDD2", 3, func(o *repo.Object) {
				o.Data = itemData("DDDDDDD2", "2020-01-04", "2021-01-04")
				o.InTrash = true
				o.Tags = []repo.ItemTag{{Tag: "trashed"}}
			}),
		}))
		return nil
	})
}

func TestVersionsFilters(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	seedQueryLibrary(t, s, u.LibraryID)
	r, _ := s.Library(ctx(), u.LibraryID)
	vq := func(f func(*repo.VersionsQuery)) []string {
		q := repo.VersionsQuery{Type: repo.TypeItem}
		if f != nil {
			f(&q)
		}
		return kvKeys(collectVersions(t, r, q))
	}
	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2", "CCCCCCC2"}, vq(nil))
	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2", "CCCCCCC2", "DDDDDDD2"}, vq(func(q *repo.VersionsQuery) { q.IncludeTrashed = true }))
	assert.Equal(t, []string{"BBBBBBB2", "CCCCCCC2"}, vq(func(q *repo.VersionsQuery) { q.Since = 1 }))
	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2"}, vq(func(q *repo.VersionsQuery) { q.TopOnly = true }))
	assert.Equal(t, []string{"CCCCCCC2"}, vq(func(q *repo.VersionsQuery) { q.ParentKey = "AAAAAAA2" }))
	assert.Equal(t, []string{"BBBBBBB2"}, vq(func(q *repo.VersionsQuery) { q.Keys = []string{"BBBBBBB2", "NOPE"} }))
	assert.Empty(t, vq(func(q *repo.VersionsQuery) { q.Keys = []string{} }))
	assert.Equal(t, []string{"DDDDDDD2"}, vq(func(q *repo.VersionsQuery) { q.IncludeTrashed = true; q.Keys = []string{"DDDDDDD2"} }))

	got := collectVersions(t, r, repo.VersionsQuery{Type: repo.TypeItem, Since: 2})
	assert.Empty(t, got)

	_, err := r.Versions(ctx(), repo.VersionsQuery{Type: repo.TypeTag})
	assert.Error(t, err)
	_, err = r.Versions(ctx(), repo.VersionsQuery{})
	assert.Error(t, err)
}

func TestVersionsOtherTypes(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeCollection, []repo.Object{
			{Key: "COLLCOL1", Version: 4, Data: json.RawMessage(`{}`)},
			{Key: "COLLCOL2", Version: 2, Data: json.RawMessage(`{}`)},
		})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	got := collectVersions(t, r, repo.VersionsQuery{Type: repo.TypeCollection})
	assert.Equal(t, []repo.KeyVersion{{Key: "COLLCOL2", Version: 2}, {Key: "COLLCOL1", Version: 4}}, got)
	got = collectVersions(t, r, repo.VersionsQuery{Type: repo.TypeCollection, Since: 2})
	assert.Equal(t, []repo.KeyVersion{{Key: "COLLCOL1", Version: 4}}, got)
}

func TestVersionsEarlyBreakReleasesConnection(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		objs := make([]repo.Object, 50)
		for i := range objs {
			objs[i] = item(fmt.Sprintf("K%07d", i), int64(i+1), nil)
		}
		return tx.PutObjects(ctx(), repo.TypeItem, objs)
	})
	r, _ := s.Library(ctx(), u.LibraryID)

	seq, err := r.Versions(ctx(), repo.VersionsQuery{Type: repo.TypeItem})
	require.NoError(t, err)
	assert.Zero(t, pool.Stat().AcquiredConns(), "building the iterator must not take a connection")
	for range seq {
		break
	}
	assert.Zero(t, pool.Stat().AcquiredConns())

	oseq, err := r.Objects(ctx(), repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}})
	require.NoError(t, err)
	n := 0
	for range oseq {
		if n++; n == 3 {
			break
		}
	}
	assert.Zero(t, pool.Stat().AcquiredConns())

	// An abandoned iterator that was never ranged over holds nothing either,
	// and the pool is still healthy.
	require.NoError(t, s.Ping(ctx()))
}

func TestVersionsHonoursContextCancel(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil)})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	c, cancel := contextWithCancel()
	seq, err := r.Versions(c, repo.VersionsQuery{Type: repo.TypeItem})
	require.NoError(t, err)
	cancel()
	var gotErr error
	for _, err := range seq {
		gotErr = err
	}
	assert.Error(t, gotErr)
	assert.Zero(t, pool.Stat().AcquiredConns())
}

func TestVersionsUsesIndexOnlyScan(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	_, err := pool.Exec(ctx(), `
		INSERT INTO items (library_id, key, version, item_type, data, content_hash)
		SELECT $1, 'K' || lpad(g::text, 7, '0'), g, 'book', '{}', '\x00' FROM generate_series(1, 20000) g`, u.LibraryID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx(), `VACUUM ANALYZE items`)
	require.NoError(t, err)

	rows, err := pool.Query(ctx(), `EXPLAIN SELECT o.key, o.version FROM items o
		WHERE o.library_id = $1 AND o.version > 0 AND NOT o.in_trash ORDER BY o.version`, u.LibraryID)
	require.NoError(t, err)
	var plan string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan += line + "\n"
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, plan, "Index Only Scan using items_version_idx")
}

func TestObjectsFiltersSortAndPaging(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	seedQueryLibrary(t, s, u.LibraryID)
	r, _ := s.Library(ctx(), u.LibraryID)
	base := repo.VersionsQuery{Type: repo.TypeItem}
	list := func(q repo.ListQuery) []string { return keysOf(collectObjects(t, r, q)) }

	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2", "CCCCCCC2"}, list(repo.ListQuery{VersionsQuery: base}))
	assert.Equal(t, []string{"CCCCCCC2", "BBBBBBB2", "AAAAAAA2"}, list(repo.ListQuery{VersionsQuery: base, Desc: true}))
	assert.Equal(t, []string{"BBBBBBB2", "CCCCCCC2", "AAAAAAA2"}, list(repo.ListQuery{VersionsQuery: base, Sort: "dateAdded"}))
	assert.Equal(t, []string{"BBBBBBB2", "CCCCCCC2", "AAAAAAA2"}, list(repo.ListQuery{VersionsQuery: base, Sort: "dateModified", Desc: true}))
	assert.Equal(t, []string{"CCCCCCC2", "BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: base, Sort: "dateAdded", Start: 1, Limit: 2, Desc: true}))
	assert.Equal(t, []string{"BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: base, Limit: 1, Start: 1}))
	assert.Equal(t, []string{"BBBBBBB2", "CCCCCCC2"}, list(repo.ListQuery{VersionsQuery: base, Start: 1}))

	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: base, Tag: []string{"x"}}))
	assert.Equal(t, []string{"AAAAAAA2"}, list(repo.ListQuery{VersionsQuery: base, Tag: []string{"x", "y"}}))
	assert.Empty(t, list(repo.ListQuery{VersionsQuery: base, Tag: []string{"x", "nope"}}))
	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: base, Collection: "COLLCOLA"}))
	assert.Equal(t, []string{"BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: base, Collection: "COLLCOLB"}))
	since := base
	since.Since = 1
	assert.Equal(t, []string{"BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: since, Collection: "COLLCOLA", Tag: []string{"x"}}))
	assert.Empty(t, list(repo.ListQuery{VersionsQuery: base, Collection: "ZZZZZZZZ"}))

	top := base
	top.TopOnly = true
	assert.Equal(t, []string{"AAAAAAA2", "BBBBBBB2"}, list(repo.ListQuery{VersionsQuery: top}))
	trashed := base
	trashed.IncludeTrashed = true
	assert.Len(t, list(repo.ListQuery{VersionsQuery: trashed}), 4)

	objs := collectObjects(t, r, repo.ListQuery{VersionsQuery: base, Limit: 1})
	assert.Equal(t, "book", objs[0].ItemType)

	_, err := r.Objects(ctx(), repo.ListQuery{VersionsQuery: base, Sort: "title; DROP TABLE items"})
	assert.Error(t, err)
}

func TestCountMatchesFilters(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	seedQueryLibrary(t, s, u.LibraryID)
	r, _ := s.Library(ctx(), u.LibraryID)
	base := repo.VersionsQuery{Type: repo.TypeItem}
	cases := []struct {
		q    repo.ListQuery
		want int64
	}{
		{repo.ListQuery{VersionsQuery: base}, 3},
		{repo.ListQuery{VersionsQuery: base, Limit: 1, Start: 2}, 3},
		{repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, IncludeTrashed: true}}, 4},
		{repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, TopOnly: true}}, 2},
		{repo.ListQuery{VersionsQuery: base, Tag: []string{"y"}}, 2},
		{repo.ListQuery{VersionsQuery: base, Collection: "COLLCOLA"}, 2},
		{repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, Since: 1}}, 2},
	}
	for i, c := range cases {
		n, err := r.Count(ctx(), c.q)
		require.NoError(t, err)
		assert.Equalf(t, c.want, n, "case %d", i)
	}
}

func TestTags(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	seedQueryLibrary(t, s, u.LibraryID)
	r, _ := s.Library(ctx(), u.LibraryID)
	tags := func(q repo.VersionsQuery) []repo.ItemTag {
		got, err := r.Tags(ctx(), repo.ListQuery{VersionsQuery: q})
		require.NoError(t, err)
		return got
	}
	assert.Equal(t, []repo.ItemTag{{Tag: "x", NumItems: 2}, {Tag: "y", NumItems: 1}, {Tag: "y", Type: 1, NumItems: 1}}, tags(repo.VersionsQuery{}))
	assert.Equal(t, []repo.ItemTag{{Tag: "x", NumItems: 2}, {Tag: "y", NumItems: 1}}, tags(repo.VersionsQuery{TopOnly: true}))
	assert.Equal(t, []repo.ItemTag{{Tag: "x", NumItems: 1}, {Tag: "y", Type: 1, NumItems: 1}}, tags(repo.VersionsQuery{Since: 1}))
	assert.Equal(t, []repo.ItemTag{{Tag: "x", NumItems: 1}}, tags(repo.VersionsQuery{Keys: []string{"BBBBBBB2"}}))
	assert.Len(t, tags(repo.VersionsQuery{IncludeTrashed: true}), 4)
}

func TestSettings(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutSettings(ctx(), []repo.Setting{
			{Key: "tagColors", Value: json.RawMessage(`[{"name":"a"}]`), Version: 1},
			{Key: "lastPageIndex_u_ABC", Value: json.RawMessage(`3`), Version: 2},
		})
	})
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutSettings(ctx(), []repo.Setting{{Key: "lastPageIndex_u_ABC", Value: json.RawMessage(`4`), Version: 3}})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	all, err := r.Settings(ctx(), 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "lastPageIndex_u_ABC", all[0].Key)
	assert.JSONEq(t, `4`, string(all[0].Value))
	assert.EqualValues(t, 3, all[0].Version)
	since, _ := r.Settings(ctx(), 1)
	assert.Len(t, since, 1)

	var removed []string
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		var err error
		removed, err = tx.DeleteSettings(ctx(), []string{"tagColors", "ghost"}, 9)
		return err
	})
	assert.Equal(t, []string{"tagColors"}, removed)
	r, _ = s.Library(ctx(), u.LibraryID)
	del, _ := r.Deleted(ctx(), 0)
	assert.Equal(t, []string{"tagColors"}, del[repo.TypeSetting])
	rest, _ := r.Settings(ctx(), 0)
	assert.Len(t, rest, 1)
}

func TestFulltext(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	chars, total, pages := 10, 20, 2
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil), item("BBBBBBBB", 1, nil)}))
		return tx.PutFulltext(ctx(), []repo.Fulltext{
			{ItemKey: "AAAAAAAA", Content: "one", IndexedChars: &chars, TotalChars: &total, Version: 2},
			{ItemKey: "BBBBBBBB", Content: "two", IndexedPages: &pages, TotalPages: &pages, Version: 3},
		})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	f, err := r.Fulltext(ctx(), "AAAAAAAA")
	require.NoError(t, err)
	assert.Equal(t, "one", f.Content)
	assert.Equal(t, &chars, f.IndexedChars)
	assert.Nil(t, f.IndexedPages)
	assert.EqualValues(t, 2, f.Version)

	_, err = r.Fulltext(ctx(), "NOPENOPE")
	assert.ErrorIs(t, err, repo.ErrNotFound)

	vs, err := r.FulltextVersions(ctx(), 2)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"BBBBBBBB": 3}, vs)

	// Replacing keeps one row per item.
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutFulltext(ctx(), []repo.Fulltext{{ItemKey: "AAAAAAAA", Content: "uno", Version: 4}})
	})
	r, _ = s.Library(ctx(), u.LibraryID)
	f, _ = r.Fulltext(ctx(), "AAAAAAAA")
	assert.Equal(t, "uno", f.Content)
	assert.Nil(t, f.IndexedChars)

	// Fulltext for an unknown item is rejected.
	_, err = s.InLibrary(ctx(), u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutFulltext(ctx(), []repo.Fulltext{{ItemKey: "GHOSTGHO", Content: "x", Version: 1}})
	})
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestAttachmentFilesAndStorage(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	const md5a, md5b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, s.Files().PutBlob(ctx(), repo.Blob{MD5: md5a, Size: 10}))
	require.NoError(t, s.Files().PutBlob(ctx(), repo.Blob{MD5: md5b, Size: 20}))

	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil), item("BBBBBBBB", 1, nil)}))
		require.NoError(t, tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "AAAAAAAA", MD5: md5a, Filename: "a.pdf", MtimeMS: 1700000000123, Size: 10}))
		require.NoError(t, tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "BBBBBBBB", MD5: md5b, Filename: "b.zip", Size: 20, Compressed: true}))
		require.NoError(t, tx.AddStorageUsed(ctx(), 30))
		assert.EqualValues(t, 30, tx.StorageUsed())
		return nil
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	assert.EqualValues(t, 30, r.StorageUsed())
	f, err := r.AttachmentFile(ctx(), "AAAAAAAA")
	require.NoError(t, err)
	assert.Equal(t, repo.AttachmentFile{ItemKey: "AAAAAAAA", MD5: md5a, Filename: "a.pdf", MtimeMS: 1700000000123, Size: 10}, f)
	_, err = r.AttachmentFile(ctx(), "CCCCCCCC")
	assert.ErrorIs(t, err, repo.ErrNotFound)

	// Re-pointing an attachment replaces its row.
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "AAAAAAAA", MD5: md5b, Filename: "c.pdf", Size: 20})
	})
	r, _ = s.Library(ctx(), u.LibraryID)
	f, _ = r.AttachmentFile(ctx(), "AAAAAAAA")
	assert.Equal(t, md5b, f.MD5)

	var removed []repo.AttachmentFile
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		var err error
		removed, err = tx.DeleteAttachmentFiles(ctx())
		require.NoError(t, err)
		return tx.AddStorageUsed(ctx(), -1000) // clamps at zero
	})
	assert.Len(t, removed, 2)
	r, _ = s.Library(ctx(), u.LibraryID)
	assert.Zero(t, r.StorageUsed())
	_, err = r.AttachmentFile(ctx(), "AAAAAAAA")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestClaimWriteToken(t *testing.T) {
	s, _ := newStore(t)
	a, b := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	claim := func(lib int64, tok string) (ok bool) {
		write(t, s, lib, func(tx repo.LibraryTx) error {
			var err error
			ok, err = tx.ClaimWriteToken(ctx(), tok)
			return err
		})
		return ok
	}
	assert.True(t, claim(a.LibraryID, "tok"))
	assert.False(t, claim(a.LibraryID, "tok"))
	assert.True(t, claim(b.LibraryID, "tok"), "tokens are per library")
}

func TestTxReadsSeeOwnWrites(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil)}))
		got, err := tx.Get(ctx(), repo.TypeItem, []string{"AAAAAAAA"})
		require.NoError(t, err)
		assert.Len(t, got, 1)
		// A concurrent unlocked reader does not see uncommitted rows.
		r, err := s.Library(ctx(), u.LibraryID)
		require.NoError(t, err)
		other, err := r.Get(ctx(), repo.TypeItem, []string{"AAAAAAAA"})
		require.NoError(t, err)
		assert.Empty(t, other)
		return nil
	})
}

func TestLockedLibraryReadersStillWork(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	locked, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.InLibrary(ctx(), u.LibraryID, func(repo.LibraryTx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	r, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err, "plain reads must not block on the write lock")
	assert.EqualValues(t, 0, r.Version())
	close(release)
	<-done
}

func TestPruneRaisesDeleteLogFloor(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.LogDeleted(ctx(), repo.TypeItem, []string{"OLDOLDO1"}, 3))
		return tx.LogDeleted(ctx(), repo.TypeItem, []string{"OLDOLDO2"}, 5)
	})
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.LogDeleted(ctx(), repo.TypeItem, []string{"NEWNEWN1"}, 8)
	})
	_, err := pool.Exec(ctx(), `UPDATE deleted_log SET created_at = now() - interval '40 days' WHERE version < 8`)
	require.NoError(t, err)

	n, err := s.Prune(ctx(), time.Now(), 30*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	r, _ := s.Library(ctx(), u.LibraryID)
	assert.EqualValues(t, 5, r.DeleteLogFloor())
	del, _ := r.Deleted(ctx(), 0)
	assert.Equal(t, []string{"NEWNEWN1"}, del[repo.TypeItem])

	// Nothing left to prune, floor stays.
	n, err = s.Prune(ctx(), time.Now(), 30*24*time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n)
	r, _ = s.Library(ctx(), u.LibraryID)
	assert.EqualValues(t, 5, r.DeleteLogFloor())
}

func TestPruneSkipsBusyLibrary(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.LogDeleted(ctx(), repo.TypeItem, []string{"OLDOLDO1"}, 3)
	})
	_, err := pool.Exec(ctx(), `UPDATE deleted_log SET created_at = now() - interval '40 days'`)
	require.NoError(t, err)

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.InLibrary(ctx(), u.LibraryID, func(repo.LibraryTx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	n, err := s.Prune(ctx(), time.Now(), 30*24*time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n, "a library being written is left for the next run")
	close(release)
	<-done

	n, err = s.Prune(ctx(), time.Now(), 30*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

func TestPruneExpiredRows(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	require.NoError(t, s.LoginSessions().Create(ctx(), repo.LoginSession{TokenHash: []byte("old"), ExpiresAt: past}))
	require.NoError(t, s.LoginSessions().Create(ctx(), repo.LoginSession{TokenHash: []byte("new"), ExpiresAt: future}))
	require.NoError(t, s.WebSessions().Create(ctx(), repo.WebSession{IDHash: []byte("old"), UserID: u.ID, CSRFToken: "c", ExpiresAt: past}))
	require.NoError(t, s.WebSessions().Create(ctx(), repo.WebSession{IDHash: []byte("new"), UserID: u.ID, CSRFToken: "c", ExpiresAt: future}))

	const md5 = "cccccccccccccccccccccccccccccccc"
	mk := func(key, state string, exp time.Time) {
		require.NoError(t, s.Files().CreateUploadToken(ctx(), repo.UploadToken{
			UploadKey: key, LibraryID: u.LibraryID, ItemKey: "AAAAAAAA", MD5: md5, Filename: "f", State: state, ExpiresAt: exp,
		}))
	}
	mk("expired-pending", "pending", past)
	mk("expired-registered", "registered", past)
	mk("live", "pending", future)

	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		_, err := tx.ClaimWriteToken(ctx(), "oldtoken")
		require.NoError(t, err)
		_, err = tx.ClaimWriteToken(ctx(), "newtoken")
		return err
	})
	_, err := pool.Exec(ctx(), `UPDATE write_tokens SET created_at = now() - interval '13 hours' WHERE token = 'oldtoken'`)
	require.NoError(t, err)

	mkInvite := func(h string, exp time.Time, used bool) {
		inv, err := s.Invites().Create(ctx(), repo.Invite{TokenHash: []byte(h), ExpiresAt: exp})
		require.NoError(t, err)
		if used {
			_, err := pool.Exec(ctx(), `UPDATE invites SET used_by = $2, used_at = now() WHERE id = $1`, inv.ID, u.ID)
			require.NoError(t, err)
		}
	}
	mkInvite("expired-long", now.Add(-31*24*time.Hour), false)
	mkInvite("expired-recent", now.Add(-24*time.Hour), false)
	mkInvite("expired-used", now.Add(-31*24*time.Hour), true)
	mkInvite("live", future, false)

	n, err := s.Prune(ctx(), now, 30*24*time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 6, n)

	count := func(q string) (c int) {
		require.NoError(t, pool.QueryRow(ctx(), q).Scan(&c))
		return c
	}
	assert.Equal(t, 1, count(`SELECT count(*) FROM login_sessions`))
	assert.Equal(t, 1, count(`SELECT count(*) FROM web_sessions`))
	var keys []string
	rows, _ := pool.Query(ctx(), `SELECT upload_key FROM upload_tokens`)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Expired tokens go regardless of state, or registered ones pile up forever.
	assert.Equal(t, []string{"live"}, keys)
	assert.Equal(t, 1, count(`SELECT count(*) FROM write_tokens`))
	assert.Equal(t, 3, count(`SELECT count(*) FROM invites`))
}

func TestPing(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.Ping(ctx()))
}

func TestOnlyTrashedAndChildCounts(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	seedQueryLibrary(t, s, u.LibraryID)
	r, _ := s.Library(ctx(), u.LibraryID)

	trash := collectVersions(t, r, repo.VersionsQuery{Type: repo.TypeItem, OnlyTrashed: true})
	assert.Equal(t, []string{"DDDDDDD2"}, kvKeys(trash))
	// OnlyTrashed wins over IncludeTrashed.
	trash = collectVersions(t, r, repo.VersionsQuery{Type: repo.TypeItem, OnlyTrashed: true, IncludeTrashed: true})
	assert.Equal(t, []string{"DDDDDDD2"}, kvKeys(trash))

	counts, err := r.ChildCounts(ctx(), []string{"AAAAAAA2", "BBBBBBB2"}, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"AAAAAAA2": 1}, counts)
	counts, err = r.ChildCounts(ctx(), nil, false)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

func TestCollectionParentFiltersAndSorts(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	coll := func(key string, v int64, data string) repo.Object {
		return repo.Object{Key: key, Version: v, Data: json.RawMessage(data), ContentHash: []byte(key)}
	}
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeCollection, []repo.Object{
			coll("COLLAAAA", 1, `{"name":"beta","parentCollection":false}`),
			coll("COLLBBBB", 2, `{"name":"Alpha"}`),
			coll("COLLCCCC", 3, `{"name":"child","parentCollection":"COLLAAAA"}`),
		})
	})
	r, _ := s.Library(ctx(), u.LibraryID)
	vq := func(f func(*repo.VersionsQuery)) []string {
		q := repo.VersionsQuery{Type: repo.TypeCollection}
		f(&q)
		return kvKeys(collectVersions(t, r, q))
	}
	assert.Equal(t, []string{"COLLAAAA", "COLLBBBB"}, vq(func(q *repo.VersionsQuery) { q.TopOnly = true }))
	assert.Equal(t, []string{"COLLCCCC"}, vq(func(q *repo.VersionsQuery) { q.ParentKey = "COLLAAAA" }))

	byName := collectObjects(t, r, repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeCollection}, Sort: "name"})
	assert.Equal(t, []string{"COLLBBBB", "COLLAAAA", "COLLCCCC"}, keysOf(byName))
}

func TestRaiseVersionNeverLowers(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	v := write(t, s, u.LibraryID, func(tx repo.LibraryTx) error { return tx.RaiseVersion(ctx(), 40) })
	assert.EqualValues(t, 40, v)
	v = write(t, s, u.LibraryID, func(tx repo.LibraryTx) error { return tx.RaiseVersion(ctx(), 7) })
	assert.EqualValues(t, 40, v)
	r, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	assert.EqualValues(t, 40, r.Version())
	v = write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		n, err := tx.NextVersion(ctx())
		assert.EqualValues(t, 41, n)
		return err
	})
	assert.EqualValues(t, 41, v)
}

func TestRecordImportedRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.RecordImported(ctx(), repo.TypeItem, []repo.ImportedObject{
			{Key: "AAAAAAAA", SourceVersion: 5, ContentHash: []byte{1}},
			{Key: "BBBBBBBB", SourceVersion: 6, ContentHash: []byte{2}},
		})
	})
	// A second record of the same key replaces the first.
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.RecordImported(ctx(), repo.TypeItem, []repo.ImportedObject{{Key: "AAAAAAAA", SourceVersion: 9, ContentHash: []byte{3}}})
	})
	r, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	got, err := r.ImportedObjects(ctx(), repo.TypeItem)
	require.NoError(t, err)
	assert.Equal(t, map[string]repo.ImportedObject{
		"AAAAAAAA": {Key: "AAAAAAAA", SourceVersion: 9, ContentHash: []byte{3}},
		"BBBBBBBB": {Key: "BBBBBBBB", SourceVersion: 6, ContentHash: []byte{2}},
	}, got)
	other, err := r.ImportedObjects(ctx(), repo.TypeCollection)
	require.NoError(t, err)
	assert.Empty(t, other, "records are per object type")
}

func TestExcludeNotes(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{
			item("AAAAAAA2", 1, nil),
			item("NNNNNNN2", 2, func(o *repo.Object) {
				o.ItemType = "note"
				o.ParentKey = "AAAAAAA2"
				o.Tags = []repo.ItemTag{{Tag: "secret"}}
			}),
			item("TTTTTTT2", 3, func(o *repo.Object) { o.ItemType = "note" }),
		})
	})
	r, _ := s.Library(ctx(), u.LibraryID)

	all := repo.VersionsQuery{Type: repo.TypeItem}
	assert.Equal(t, []string{"AAAAAAA2", "NNNNNNN2", "TTTTTTT2"}, kvKeys(collectVersions(t, r, all)))
	all.ExcludeNotes = true
	assert.Equal(t, []string{"AAAAAAA2"}, kvKeys(collectVersions(t, r, all)))

	q := repo.ListQuery{VersionsQuery: all}
	assert.Equal(t, []string{"AAAAAAA2"}, keysOf(collectObjects(t, r, q)))
	n, err := r.Count(ctx(), q)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	tags, err := r.Tags(ctx(), q)
	require.NoError(t, err)
	assert.Empty(t, tags, "tags that only live on hidden notes stay hidden")

	counts, err := r.ChildCounts(ctx(), []string{"AAAAAAA2"}, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"AAAAAAA2": 1}, counts)
	counts, err = r.ChildCounts(ctx(), []string{"AAAAAAA2"}, true)
	require.NoError(t, err)
	assert.Empty(t, counts)
}
