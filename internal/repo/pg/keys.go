package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

const keyCols = `id, user_id, key_hash, key_prefix, name, access, created_at, last_used_at`

type keyRepo struct{ pool *pgxpool.Pool }

func (r keyRepo) Create(ctx context.Context, k repo.APIKey) (repo.APIKey, error) {
	rows, err := r.pool.Query(ctx, `
		INSERT INTO api_keys (user_id, key_hash, key_prefix, name, access)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+keyCols,
		k.UserID, k.Hash, k.Prefix, k.Name, k.Access)
	if err != nil {
		return repo.APIKey{}, wrap("create api key", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.APIKey])
	return out, wrap("create api key", err)
}

func (r keyRepo) GetByHash(ctx context.Context, hash []byte) (repo.APIKey, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+keyCols+` FROM api_keys WHERE key_hash = $1`, hash)
	if err != nil {
		return repo.APIKey{}, wrap("get api key", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.APIKey])
	return out, wrap("get api key", err)
}

func (r keyRepo) ListByUser(ctx context.Context, userID int64) ([]repo.APIKey, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+keyCols+` FROM api_keys WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, wrap("list api keys", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.APIKey])
	return out, wrap("list api keys", err)
}

func (r keyRepo) Delete(ctx context.Context, userID, id int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1 AND user_id = $2`, id, userID)
	return requireRow(tag, err, "delete api key")
}

func (r keyRepo) DeleteByHash(ctx context.Context, hash []byte) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM api_keys WHERE key_hash = $1`, hash)
	return requireRow(tag, err, "delete api key")
}

// Touch is called on every authenticated request, so it only writes when the
// stored value is at least a minute stale. That avoids dirtying a heap page
// per request on busy keys.
func (r keyRepo) Touch(ctx context.Context, id int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE api_keys SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, id, at)
	return wrap("touch api key", err)
}
