package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

const identityCols = `id, user_id, provider, subject, COALESCE(email::text, ''), created_at, last_login_at`

type identityRepo struct{ pool *pgxpool.Pool }

func (r identityRepo) Get(ctx context.Context, provider, subject string) (repo.Identity, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+identityCols+` FROM user_identities WHERE provider = $1 AND subject = $2`, provider, subject)
	if err != nil {
		return repo.Identity{}, wrap("get identity", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Identity])
	return out, wrap("get identity", err)
}

func (r identityRepo) ListByUser(ctx context.Context, userID int64) ([]repo.Identity, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+identityCols+` FROM user_identities WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, wrap("list identities", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.Identity])
	return out, wrap("list identities", err)
}

func (r identityRepo) ProvidersByUsers(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := r.pool.Query(ctx,
		`SELECT user_id, provider FROM user_identities WHERE user_id = ANY($1) ORDER BY user_id, provider`, ids)
	if err != nil {
		return nil, wrap("list identity providers", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, wrap("list identity providers", err)
		}
		out[id] = append(out[id], p)
	}
	return out, wrap("list identity providers", rows.Err())
}

func (r identityRepo) Create(ctx context.Context, i repo.Identity) (repo.Identity, error) {
	rows, err := r.pool.Query(ctx, `
		INSERT INTO user_identities (user_id, provider, subject, email, last_login_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+identityCols,
		i.UserID, i.Provider, i.Subject, nullIfEmpty(i.Email), i.LastLoginAt)
	if err != nil {
		return repo.Identity{}, wrap("create identity", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Identity])
	return out, wrap("create identity", err)
}

func (r identityRepo) Touch(ctx context.Context, id int64, email string, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE user_identities SET email = COALESCE($2, email), last_login_at = $3 WHERE id = $1`,
		id, nullIfEmpty(email), at)
	return requireRow(tag, err, "touch identity")
}

func (r identityRepo) Delete(ctx context.Context, userID int64, provider string) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM user_identities WHERE user_id = $1 AND provider = $2`, userID, provider)
	return requireRow(tag, err, "delete identity")
}
