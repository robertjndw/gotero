package pg

import (
	"context"
	"iter"

	"github.com/jackc/pgx/v5"

	"github.com/robertjndw/gotero/internal/repo"
)

// stream runs the query when the sequence is ranged over, not when it is
// built. That way an iterator that is never consumed cannot pin a connection,
// and rows are always closed, even when the consumer breaks out early.
func stream[T any](ctx context.Context, q querier, sql string, args []any, scan func(pgx.Rows) (T, error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			yield(zero, mapErr(err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scan(rows)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(v, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, mapErr(err))
		}
	}
}

type libReader struct {
	q       querier
	id      int64
	version int64
	floor   int64
	used    int64
}

func (r *libReader) ID() int64             { return r.id }
func (r *libReader) Version() int64        { return r.version }
func (r *libReader) DeleteLogFloor() int64 { return r.floor }
func (r *libReader) StorageUsed() int64    { return r.used }

// Library returns an unlocked reader. The header values are read once here.
func (s *Store) Library(ctx context.Context, libraryID int64) (repo.LibraryReader, error) {
	r := &libReader{q: s.pool, id: libraryID}
	// Every request starts here, so this is where a saturated pool should
	// push back instead of queueing.
	conn, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	err = conn.QueryRow(ctx,
		`SELECT version, delete_log_floor, storage_used FROM libraries WHERE id = $1`, libraryID,
	).Scan(&r.version, &r.floor, &r.used)
	if err != nil {
		return nil, wrap("load library", err)
	}
	return r, nil
}

func (r *libReader) Versions(ctx context.Context, q repo.VersionsQuery) (iter.Seq2[repo.KeyVersion, error], error) {
	sql, args, err := versionsSQL(r.id, q)
	if err != nil {
		return nil, err
	}
	return stream(ctx, r.q, sql, args, func(rows pgx.Rows) (kv repo.KeyVersion, err error) {
		err = rows.Scan(&kv.Key, &kv.Version)
		return kv, err
	}), nil
}

func scanObject(rows pgx.Rows) (o repo.Object, err error) {
	err = rows.Scan(&o.Key, &o.Version, &o.Data, &o.ContentHash, &o.ItemType, &o.ParentKey, &o.InTrash)
	return o, err
}

func (r *libReader) Objects(ctx context.Context, q repo.ListQuery) (iter.Seq2[repo.Object, error], error) {
	sql, args, err := objectsSQL(r.id, q)
	if err != nil {
		return nil, err
	}
	return stream(ctx, r.q, sql, args, scanObject), nil
}

func (r *libReader) Count(ctx context.Context, q repo.ListQuery) (int64, error) {
	sql, args, err := countSQL(r.id, q)
	if err != nil {
		return 0, err
	}
	var n int64
	return n, wrap("count objects", r.q.QueryRow(ctx, sql, args...).Scan(&n))
}

func (r *libReader) Get(ctx context.Context, t repo.ObjectType, keys []string) (map[string]repo.Object, error) {
	sql, args, err := objectsSQL(r.id, repo.ListQuery{
		VersionsQuery: repo.VersionsQuery{Type: t, Keys: nonNil(keys), IncludeTrashed: true},
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]repo.Object, len(keys))
	for o, err := range stream(ctx, r.q, sql, args, scanObject) {
		if err != nil {
			return nil, wrap("get objects", err)
		}
		out[o.Key] = o
	}
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Deleted returns the log entries after since together with the delete log
// floor. Both come from one statement, so a prune committing between two
// reads cannot hand back a floor older than the entries it already removed.
func (r *libReader) Deleted(ctx context.Context, since int64) (map[repo.ObjectType][]string, int64, error) {
	rows, err := r.q.Query(ctx, `
		SELECT l.delete_log_floor, d.object_type, d.keys
		FROM libraries l
		LEFT JOIN (
			SELECT object_type, array_agg(key ORDER BY key) AS keys
			FROM deleted_log WHERE library_id = $1 AND version > $2
			GROUP BY object_type
		) d ON true
		WHERE l.id = $1`, r.id, since)
	if err != nil {
		return nil, 0, wrap("list deleted", err)
	}
	defer rows.Close()
	out := map[repo.ObjectType][]string{}
	var floor int64
	for rows.Next() {
		var t *string
		var keys []string
		if err := rows.Scan(&floor, &t, &keys); err != nil {
			return nil, 0, wrap("list deleted", err)
		}
		if t != nil {
			out[repo.ObjectType(*t)] = keys
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, wrap("list deleted", err)
	}
	return out, floor, nil
}

func (r *libReader) Settings(ctx context.Context, since int64) ([]repo.Setting, error) {
	rows, err := r.q.Query(ctx,
		`SELECT key, value, version FROM settings WHERE library_id = $1 AND version > $2 ORDER BY key`,
		r.id, since)
	if err != nil {
		return nil, wrap("list settings", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (s repo.Setting, err error) {
		err = row.Scan(&s.Key, &s.Value, &s.Version)
		return s, err
	})
	return out, wrap("list settings", err)
}

func (r *libReader) Tags(ctx context.Context, q repo.ListQuery) ([]repo.ItemTag, error) {
	sql, args := tagsSQL(r.id, q)
	rows, err := r.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap("list tags", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (t repo.ItemTag, err error) {
		err = row.Scan(&t.Tag, &t.Type, &t.NumItems)
		return t, err
	})
	return out, wrap("list tags", err)
}

func (r *libReader) ChildCounts(ctx context.Context, parentKeys []string, excludeNotes bool) (map[string]int, error) {
	out := make(map[string]int, len(parentKeys))
	if len(parentKeys) == 0 {
		return out, nil
	}
	rows, err := r.q.Query(ctx, `
		SELECT parent_key, count(*) FROM items
		WHERE library_id = $1 AND parent_key = ANY($2) AND NOT in_trash
		AND NOT ($3 AND item_type = 'note')
		GROUP BY parent_key`, r.id, parentKeys, excludeNotes)
	if err != nil {
		return nil, wrap("count children", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, wrap("count children", err)
		}
		out[k] = n
	}
	return out, wrap("count children", rows.Err())
}

func (r *libReader) FulltextVersions(ctx context.Context, since int64) (map[string]int64, error) {
	rows, err := r.q.Query(ctx,
		`SELECT item_key, version FROM fulltext WHERE library_id = $1 AND version > $2`, r.id, since)
	if err != nil {
		return nil, wrap("list fulltext versions", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var v int64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, wrap("list fulltext versions", err)
		}
		out[k] = v
	}
	return out, wrap("list fulltext versions", rows.Err())
}

func (r *libReader) Fulltext(ctx context.Context, itemKey string) (repo.Fulltext, error) {
	f := repo.Fulltext{ItemKey: itemKey}
	err := r.q.QueryRow(ctx, `
		SELECT content, indexed_chars, total_chars, indexed_pages, total_pages, version
		FROM fulltext WHERE library_id = $1 AND item_key = $2`, r.id, itemKey,
	).Scan(&f.Content, &f.IndexedChars, &f.TotalChars, &f.IndexedPages, &f.TotalPages, &f.Version)
	return f, wrap("get fulltext", err)
}

func (r *libReader) AttachmentFile(ctx context.Context, itemKey string) (repo.AttachmentFile, error) {
	f := repo.AttachmentFile{ItemKey: itemKey}
	err := r.q.QueryRow(ctx, `
		SELECT md5, filename, mtime_ms, size, compressed
		FROM attachment_files WHERE library_id = $1 AND item_key = $2`, r.id, itemKey,
	).Scan(&f.MD5, &f.Filename, &f.MtimeMS, &f.Size, &f.Compressed)
	return f, wrap("get attachment file", err)
}

func (r *libReader) AttachmentFilesSize(ctx context.Context, itemKeys []string) (int64, error) {
	if len(itemKeys) == 0 {
		return 0, nil
	}
	var n int64
	err := r.q.QueryRow(ctx, `
		SELECT COALESCE(sum(size), 0)::bigint FROM attachment_files
		WHERE library_id = $1 AND item_key = ANY($2)`, r.id, itemKeys).Scan(&n)
	return n, wrap("attachment files size", err)
}

// InLibrary runs fn in a transaction that holds the library row lock.
func (s *Store) InLibrary(ctx context.Context, libraryID int64, fn func(tx repo.LibraryTx) error) (int64, error) {
	conn, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, wrap("begin", err)
	}
	// Deferred after Release, so it runs first and the connection goes back
	// to the pool clean. Rollback after a successful commit is a harmless
	// no-op. Using a non-cancelable context means an aborted request still
	// releases the lock instead of leaving the connection mid-transaction.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	lt := &libTx{libReader: libReader{q: tx, id: libraryID}}

	// One round trip for the timeouts and the lock. set_config(..., true) is
	// SET LOCAL, so nothing leaks back into the pool.
	b := &pgx.Batch{}
	b.Queue(`SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
		ms(s.cfg.lockTimeout), ms(s.cfg.statementTimeout))
	b.Queue(`SELECT version, delete_log_floor, storage_used FROM libraries WHERE id = $1 FOR UPDATE`, libraryID)
	br := tx.SendBatch(ctx, b)
	_, err = br.Exec()
	if err == nil {
		err = br.QueryRow().Scan(&lt.version, &lt.floor, &lt.used)
	}
	if cerr := br.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, wrap("lock library", err)
	}

	if err := fn(lt); err != nil {
		return 0, mapErr(err)
	}
	// A client that hangs up mid-commit must not turn a write that went
	// through into an error: the version event would never be published.
	if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
		return 0, wrap("commit", err)
	}
	return lt.version, nil
}

type libTx struct {
	libReader
	bumped bool
}

func (t *libTx) NextVersion(ctx context.Context) (int64, error) {
	if t.bumped {
		return t.version, nil
	}
	err := t.q.QueryRow(ctx,
		`UPDATE libraries SET version = version + 1 WHERE id = $1 RETURNING version`, t.id,
	).Scan(&t.version)
	if err != nil {
		return 0, wrap("bump library version", err)
	}
	t.bumped = true
	return t.version, nil
}

func (t *libTx) RaiseVersion(ctx context.Context, min int64) error {
	if min <= t.version {
		return nil
	}
	err := t.q.QueryRow(ctx,
		`UPDATE libraries SET version = GREATEST(version, $2) WHERE id = $1 RETURNING version`, t.id, min,
	).Scan(&t.version)
	return wrap("raise library version", err)
}

func (t *libTx) ClaimWriteToken(ctx context.Context, token string) (bool, error) {
	tag, err := t.q.Exec(ctx,
		`INSERT INTO write_tokens (library_id, token) VALUES ($1, $2) ON CONFLICT DO NOTHING`, t.id, token)
	if err != nil {
		return false, wrap("claim write token", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (t *libTx) PutObjects(ctx context.Context, typ repo.ObjectType, objs []repo.Object) error {
	if len(objs) == 0 {
		return nil
	}
	table, err := objectTable(typ)
	if err != nil {
		return err
	}
	objs = dedupeObjects(objs)

	n := len(objs)
	keys := make([]string, n)
	versions := make([]int64, n)
	datas := make([]string, n)
	hashes := make([][]byte, n)
	for i, o := range objs {
		keys[i], versions[i], datas[i], hashes[i] = o.Key, o.Version, string(o.Data), o.ContentHash
		if hashes[i] == nil {
			hashes[i] = []byte{}
		}
	}

	// Everything goes out in one batch: a single round trip for the upsert and
	// the tag replacement, which keeps the lock hold time short.
	b := &pgx.Batch{}
	if typ == repo.TypeItem {
		itemTypes := make([]string, n)
		parents := make([]string, n)
		trash := make([]bool, n)
		var tagItems, tagNames []string
		var tagTypes []int16
		for i, o := range objs {
			itemTypes[i], parents[i], trash[i] = o.ItemType, o.ParentKey, o.InTrash
			for _, tg := range o.Tags {
				tagItems = append(tagItems, o.Key)
				tagNames = append(tagNames, tg.Tag)
				tagTypes = append(tagTypes, int16(tg.Type))
			}
		}
		b.Queue(`
			INSERT INTO items (library_id, key, version, item_type, parent_key, in_trash, data, content_hash)
			SELECT $1, k, v, it, NULLIF(pk, ''), tr, d::jsonb, h
			FROM unnest($2::text[], $3::bigint[], $4::text[], $5::text[], $6::bool[], $7::text[], $8::bytea[])
				AS u(k, v, it, pk, tr, d, h)
			ON CONFLICT (library_id, key) DO UPDATE SET
				version = EXCLUDED.version, item_type = EXCLUDED.item_type,
				parent_key = EXCLUDED.parent_key, in_trash = EXCLUDED.in_trash,
				data = EXCLUDED.data, content_hash = EXCLUDED.content_hash`,
			t.id, keys, versions, itemTypes, parents, trash, datas, hashes)
		b.Queue(`DELETE FROM item_tags WHERE library_id = $1 AND item_key = ANY($2)`, t.id, keys)
		if len(tagNames) > 0 {
			b.Queue(`
				INSERT INTO item_tags (library_id, item_key, tag, type)
				SELECT $1, i, g, y FROM unnest($2::text[], $3::text[], $4::smallint[]) AS u(i, g, y)
				ON CONFLICT DO NOTHING`, t.id, tagItems, tagNames, tagTypes)
		}
	} else {
		// table comes from objectTable's whitelist.
		b.Queue(`
			INSERT INTO `+table+` (library_id, key, version, data, content_hash)
			SELECT $1, k, v, d::jsonb, h
			FROM unnest($2::text[], $3::bigint[], $4::text[], $5::bytea[]) AS u(k, v, d, h)
			ON CONFLICT (library_id, key) DO UPDATE SET
				version = EXCLUDED.version, data = EXCLUDED.data, content_hash = EXCLUDED.content_hash`,
			t.id, keys, versions, datas, hashes)
	}
	return wrap("put objects", execBatch(t.q.SendBatch(ctx, b), b.Len()))
}

func execBatch(br pgx.BatchResults, n int) error {
	for range n {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return err
		}
	}
	return br.Close()
}

// dedupeObjects keeps the last occurrence of each key. Postgres refuses to
// upsert the same key twice in one statement.
func dedupeObjects(objs []repo.Object) []repo.Object {
	seen := make(map[string]int, len(objs))
	out := make([]repo.Object, 0, len(objs))
	for _, o := range objs {
		if i, ok := seen[o.Key]; ok {
			out[i] = o
			continue
		}
		seen[o.Key] = len(out)
		out = append(out, o)
	}
	return out
}

func (t *libTx) DeleteObjects(ctx context.Context, typ repo.ObjectType, keys []string, version int64) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	table, err := objectTable(typ)
	if err != nil {
		return nil, err
	}
	// The data-modifying CTE always runs, so the log write does not depend on
	// being referenced. Child rows (tags, fulltext, attachment files) go via
	// ON DELETE CASCADE.
	rows, err := t.q.Query(ctx, `
		WITH del AS (
			DELETE FROM `+table+` WHERE library_id = $1 AND key = ANY($2) RETURNING key
		), logged AS (
			INSERT INTO deleted_log (library_id, object_type, key, version)
			SELECT $1, $3, key, $4 FROM del
			ON CONFLICT (library_id, object_type, key)
			DO UPDATE SET version = EXCLUDED.version, created_at = now()
		)
		SELECT key FROM del`, t.id, keys, string(typ), version)
	if err != nil {
		return nil, wrap("delete objects", err)
	}
	removed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return removed, wrap("delete objects", err)
}

func (t *libTx) LogDeleted(ctx context.Context, typ repo.ObjectType, keys []string, version int64) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := t.q.Exec(ctx, `
		INSERT INTO deleted_log (library_id, object_type, key, version)
		SELECT $1, $2, k, $4 FROM unnest($3::text[]) AS u(k)
		ON CONFLICT (library_id, object_type, key)
		DO UPDATE SET version = EXCLUDED.version, created_at = now()`, t.id, string(typ), keys, version)
	return wrap("log deleted", err)
}

func (t *libTx) ClearDeleted(ctx context.Context, typ repo.ObjectType, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := t.q.Exec(ctx,
		`DELETE FROM deleted_log WHERE library_id = $1 AND object_type = $2 AND key = ANY($3)`,
		t.id, string(typ), keys)
	return wrap("clear deleted", err)
}

func (t *libTx) PutSettings(ctx context.Context, settings []repo.Setting) error {
	if len(settings) == 0 {
		return nil
	}
	keys := make([]string, len(settings))
	values := make([]string, len(settings))
	versions := make([]int64, len(settings))
	for i, s := range settings {
		keys[i], values[i], versions[i] = s.Key, string(s.Value), s.Version
	}
	_, err := t.q.Exec(ctx, `
		INSERT INTO settings (library_id, key, value, version)
		SELECT $1, k, v::jsonb, ver FROM unnest($2::text[], $3::text[], $4::bigint[]) AS u(k, v, ver)
		ON CONFLICT (library_id, key) DO UPDATE SET value = EXCLUDED.value, version = EXCLUDED.version`,
		t.id, keys, values, versions)
	return wrap("put settings", err)
}

func (t *libTx) DeleteSettings(ctx context.Context, keys []string, version int64) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := t.q.Query(ctx, `
		WITH del AS (
			DELETE FROM settings WHERE library_id = $1 AND key = ANY($2) RETURNING key
		), logged AS (
			INSERT INTO deleted_log (library_id, object_type, key, version)
			SELECT $1, 'setting', key, $3 FROM del
			ON CONFLICT (library_id, object_type, key)
			DO UPDATE SET version = EXCLUDED.version, created_at = now()
		)
		SELECT key FROM del`, t.id, keys, version)
	if err != nil {
		return nil, wrap("delete settings", err)
	}
	removed, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return removed, wrap("delete settings", err)
}

func (t *libTx) PutFulltext(ctx context.Context, fs []repo.Fulltext) error {
	if len(fs) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, f := range fs {
		b.Queue(`
			INSERT INTO fulltext (library_id, item_key, content, indexed_chars, total_chars, indexed_pages, total_pages, version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (library_id, item_key) DO UPDATE SET
				content = EXCLUDED.content, indexed_chars = EXCLUDED.indexed_chars,
				total_chars = EXCLUDED.total_chars, indexed_pages = EXCLUDED.indexed_pages,
				total_pages = EXCLUDED.total_pages, version = EXCLUDED.version`,
			t.id, f.ItemKey, f.Content, f.IndexedChars, f.TotalChars, f.IndexedPages, f.TotalPages, f.Version)
	}
	return wrap("put fulltext", execBatch(t.q.SendBatch(ctx, b), b.Len()))
}

func (t *libTx) PutAttachmentFile(ctx context.Context, f repo.AttachmentFile) error {
	_, err := t.q.Exec(ctx, `
		INSERT INTO attachment_files (library_id, item_key, md5, filename, mtime_ms, size, compressed)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (library_id, item_key) DO UPDATE SET
			md5 = EXCLUDED.md5, filename = EXCLUDED.filename, mtime_ms = EXCLUDED.mtime_ms,
			size = EXCLUDED.size, compressed = EXCLUDED.compressed`,
		t.id, f.ItemKey, f.MD5, f.Filename, f.MtimeMS, f.Size, f.Compressed)
	return wrap("put attachment file", err)
}

func (t *libTx) DeleteAttachmentFiles(ctx context.Context) ([]repo.AttachmentFile, error) {
	rows, err := t.q.Query(ctx, `
		DELETE FROM attachment_files WHERE library_id = $1
		RETURNING item_key, md5, filename, mtime_ms, size, compressed`, t.id)
	if err != nil {
		return nil, wrap("delete attachment files", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.AttachmentFile])
	return out, wrap("delete attachment files", err)
}

func (t *libTx) AddStorageUsed(ctx context.Context, delta int64) error {
	// Clamped at zero so a double-counted delete can never wedge the quota.
	err := t.q.QueryRow(ctx,
		`UPDATE libraries SET storage_used = GREATEST(storage_used + $2, 0) WHERE id = $1 RETURNING storage_used`,
		t.id, delta).Scan(&t.used)
	return wrap("adjust storage used", err)
}

func (r *libReader) ImportedObjects(ctx context.Context, typ repo.ObjectType) (map[string]repo.ImportedObject, error) {
	rows, err := r.q.Query(ctx,
		`SELECT key, source_version, content_hash FROM import_objects WHERE library_id = $1 AND object_type = $2`,
		r.id, string(typ))
	if err != nil {
		return nil, wrap("list imported objects", err)
	}
	defer rows.Close()
	out := map[string]repo.ImportedObject{}
	for rows.Next() {
		var o repo.ImportedObject
		if err := rows.Scan(&o.Key, &o.SourceVersion, &o.ContentHash); err != nil {
			return nil, wrap("list imported objects", err)
		}
		out[o.Key] = o
	}
	return out, wrap("list imported objects", rows.Err())
}

func (t *libTx) RecordImported(ctx context.Context, typ repo.ObjectType, objs []repo.ImportedObject) error {
	if len(objs) == 0 {
		return nil
	}
	keys := make([]string, len(objs))
	versions := make([]int64, len(objs))
	hashes := make([][]byte, len(objs))
	for i, o := range objs {
		keys[i], versions[i], hashes[i] = o.Key, o.SourceVersion, o.ContentHash
		if hashes[i] == nil {
			hashes[i] = []byte{}
		}
	}
	_, err := t.q.Exec(ctx, `
		INSERT INTO import_objects (library_id, object_type, key, source_version, content_hash)
		SELECT $1, $2, k, v, h FROM unnest($3::text[], $4::bigint[], $5::bytea[]) AS u(k, v, h)
		ON CONFLICT (library_id, object_type, key) DO UPDATE SET
			source_version = EXCLUDED.source_version, content_hash = EXCLUDED.content_hash`,
		t.id, string(typ), keys, versions, hashes)
	return wrap("record imported objects", err)
}
