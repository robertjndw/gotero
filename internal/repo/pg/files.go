package pg

import (
	"context"
	"encoding/json"
	"iter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

type fileRepo struct{ pool *pgxpool.Pool }

func (r fileRepo) BlobExists(ctx context.Context, md5 string) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE md5 = $1)`, md5).Scan(&ok)
	return ok, wrap("blob exists", err)
}

func (r fileRepo) BlobReferencedIn(ctx context.Context, md5 string, libraryIDs []int64) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM attachment_files WHERE md5 = $1 AND library_id = ANY($2))`,
		md5, libraryIDs).Scan(&ok)
	return ok, wrap("blob referenced", err)
}

// PutBlob is idempotent. Touching created_at on conflict matters: a fresh
// upload of an old orphan must get the full grace period before the
// attachment is registered, or GC could remove bytes about to be referenced.
func (r fileRepo) PutBlob(ctx context.Context, b repo.Blob) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO blobs (md5, size) VALUES ($1, $2) ON CONFLICT (md5) DO UPDATE SET created_at = now()`, b.MD5, b.Size)
	return wrap("put blob", err)
}

const uploadCols = `upload_key, library_id, item_key, md5, filename, size, mtime_ms,
	compressed, state, created_at, expires_at, item_md5`

func (r fileRepo) CreateUploadToken(ctx context.Context, t repo.UploadToken) error {
	state := t.State
	if state == "" {
		state = "pending"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO upload_tokens (upload_key, library_id, item_key, md5, filename, size, mtime_ms,
			compressed, state, expires_at, item_md5)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		t.UploadKey, t.LibraryID, t.ItemKey, t.MD5, t.Filename, t.Size, t.MtimeMS, t.Compressed, state, t.ExpiresAt, t.ItemMD5)
	return wrap("create upload token", err)
}

func (r fileRepo) GetUploadToken(ctx context.Context, uploadKey string) (repo.UploadToken, error) {
	return getUploadToken(ctx, r.pool, uploadKey)
}

func (r fileRepo) SetUploadTokenStateFrom(ctx context.Context, uploadKey, from, to string) (bool, error) {
	return setUploadTokenStateFrom(ctx, r.pool, uploadKey, from, to)
}

func getUploadToken(ctx context.Context, q querier, uploadKey string) (repo.UploadToken, error) {
	rows, err := q.Query(ctx, `SELECT `+uploadCols+` FROM upload_tokens WHERE upload_key = $1`, uploadKey)
	if err != nil {
		return repo.UploadToken{}, wrap("get upload token", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.UploadToken])
	return out, wrap("get upload token", err)
}

func setUploadTokenStateFrom(ctx context.Context, q querier, uploadKey, from, to string) (bool, error) {
	tag, err := q.Exec(ctx,
		`UPDATE upload_tokens SET state = $3 WHERE upload_key = $1 AND state = $2`, uploadKey, from, to)
	if err != nil {
		return false, wrap("set upload token state", err)
	}
	return tag.RowsAffected() == 1, nil
}

// OrphanBlobs uses the database clock for token expiry on purpose: it is the
// same clock that stamps tokens, so skew between app servers cannot make a
// live upload look abandoned.
func (r fileRepo) OrphanBlobs(ctx context.Context, olderThan time.Time) (iter.Seq2[repo.Blob, error], error) {
	const sql = `
		SELECT b.md5, b.size, b.created_at FROM blobs b
		WHERE b.created_at < $1` + orphanCond
	return stream(ctx, r.pool, sql, []any{olderThan}, func(rows pgx.Rows) (repo.Blob, error) {
		return pgx.RowToStructByPos[repo.Blob](rows)
	}), nil
}

const orphanCond = `
		  AND NOT EXISTS (SELECT 1 FROM attachment_files a WHERE a.md5 = b.md5)
		  AND NOT EXISTS (SELECT 1 FROM upload_tokens u WHERE u.md5 = b.md5 AND u.expires_at > now())`

// DeleteOrphanBlob re-checks every orphan condition in the DELETE itself, so
// a blob that became referenced (or got a fresh upload) since OrphanBlobs
// listed it survives.
func (r fileRepo) DeleteOrphanBlob(ctx context.Context, md5 string, olderThan time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM blobs b WHERE b.md5 = $1 AND b.created_at < $2`+orphanCond, md5, olderThan)
	if err != nil {
		return false, wrap("delete orphan blob", mapDeleteErr(err))
	}
	return tag.RowsAffected() == 1, nil
}

func (r fileRepo) Stats(ctx context.Context) (repo.FileStats, error) {
	var s repo.FileStats
	err := r.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(b.size), 0)::bigint,
			COALESCE(sum(b.size) FILTER (WHERE NOT EXISTS (
				SELECT 1 FROM attachment_files a WHERE a.md5 = b.md5)), 0)::bigint
		FROM blobs b`).Scan(&s.BlobCount, &s.BlobBytes, &s.OrphanBytes)
	return s, wrap("file stats", err)
}

const importCols = `id, user_id, status, COALESCE(source_api_key, ''), include_groups, include_files,
	cursor, progress, COALESCE(error, ''), created_at, started_at, finished_at`

type importRepo struct{ pool *pgxpool.Pool }

func jsonOrEmpty(m json.RawMessage) string {
	if len(m) == 0 {
		return "{}"
	}
	return string(m)
}

func (r importRepo) Create(ctx context.Context, j repo.ImportJob) (repo.ImportJob, error) {
	rows, err := r.pool.Query(ctx, `
		INSERT INTO import_jobs (user_id, source_api_key, include_groups, include_files, cursor, progress)
		VALUES ($1, $2, $3, $4, $5::text::jsonb, $6::text::jsonb) RETURNING `+importCols,
		j.UserID, nullIfEmpty(j.SourceAPIKey), j.IncludeGroups, j.IncludeFiles,
		jsonOrEmpty(j.Cursor), jsonOrEmpty(j.Progress))
	if err != nil {
		return repo.ImportJob{}, wrap("create import job", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.ImportJob])
	return out, wrap("create import job", err)
}

func (r importRepo) Get(ctx context.Context, id int64) (repo.ImportJob, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+importCols+` FROM import_jobs WHERE id = $1`, id)
	if err != nil {
		return repo.ImportJob{}, wrap("get import job", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.ImportJob])
	return out, wrap("get import job", err)
}

func (r importRepo) ListByUser(ctx context.Context, userID int64) ([]repo.ImportJob, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+importCols+` FROM import_jobs WHERE user_id = $1 ORDER BY id DESC`, userID)
	if err != nil {
		return nil, wrap("list import jobs", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.ImportJob])
	return out, wrap("list import jobs", err)
}

// ClaimNext lets several workers poll the queue without blocking each other:
// SKIP LOCKED passes over a job another worker is claiming right now.
func (r importRepo) ClaimNext(ctx context.Context) (repo.ImportJob, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE import_jobs SET status = 'running', started_at = now()
		WHERE id = (
			SELECT id FROM import_jobs WHERE status = 'queued'
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
		)
		RETURNING `+importCols)
	if err != nil {
		return repo.ImportJob{}, wrap("claim import job", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.ImportJob])
	return out, wrap("claim import job", err)
}

func (r importRepo) Update(ctx context.Context, j repo.ImportJob) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_jobs SET status = $2, source_api_key = $3, cursor = $4::text::jsonb,
			progress = $5::text::jsonb, error = $6, started_at = $7, finished_at = $8
		WHERE id = $1`,
		j.ID, j.Status, nullIfEmpty(j.SourceAPIKey), jsonOrEmpty(j.Cursor), jsonOrEmpty(j.Progress),
		nullIfEmpty(j.Error), j.StartedAt, j.FinishedAt)
	return requireRow(tag, err, "update import job")
}

func (r importRepo) SaveProgress(ctx context.Context, id int64, cursor, progress json.RawMessage) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_jobs SET cursor = $2::text::jsonb, progress = $3::text::jsonb
		WHERE id = $1 AND status = 'running'`, id, jsonOrEmpty(cursor), jsonOrEmpty(progress))
	return tag.RowsAffected() == 1, wrap("save import progress", err)
}

func (r importRepo) Finish(ctx context.Context, id int64, status, errMsg string, at time.Time) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE import_jobs SET status = $2, error = $3, source_api_key = NULL, finished_at = $4
		WHERE id = $1 AND status = 'running'`, id, status, nullIfEmpty(errMsg), at)
	return tag.RowsAffected() == 1, wrap("finish import job", err)
}

func (r importRepo) Requeue(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx, `UPDATE import_jobs SET status = 'queued' WHERE id = $1 AND status = 'running'`, id)
	return wrap("requeue import job", err)
}

func (r importRepo) RequeueRunning(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `UPDATE import_jobs SET status = 'queued' WHERE status = 'running'`)
	return tag.RowsAffected(), wrap("requeue running import jobs", err)
}
