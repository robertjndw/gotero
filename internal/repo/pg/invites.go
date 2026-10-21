package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

func isConflict(err error) bool { return errors.Is(err, repo.ErrConflict) }

const inviteCols = `id, token_hash, COALESCE(email::text, ''), is_admin, created_by,
	created_at, expires_at, used_by, used_at`

type inviteRepo struct{ pool *pgxpool.Pool }

func (r inviteRepo) Create(ctx context.Context, i repo.Invite) (repo.Invite, error) {
	rows, err := r.pool.Query(ctx, `
		INSERT INTO invites (token_hash, email, is_admin, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+inviteCols,
		i.TokenHash, nullIfEmpty(i.Email), i.IsAdmin, i.CreatedBy, i.ExpiresAt)
	if err != nil {
		return repo.Invite{}, wrap("create invite", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Invite])
	return out, wrap("create invite", err)
}

func (r inviteRepo) GetByHash(ctx context.Context, tokenHash []byte) (repo.Invite, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+inviteCols+` FROM invites WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return repo.Invite{}, wrap("get invite", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Invite])
	return out, wrap("get invite", err)
}

func (r inviteRepo) List(ctx context.Context, limit int, cursor int64) ([]repo.Invite, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+inviteCols+` FROM invites WHERE id > $1 ORDER BY id LIMIT $2`, cursor, lim)
	if err != nil {
		return nil, wrap("list invites", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.Invite])
	return out, wrap("list invites", err)
}

// Use is a compare-and-set, so two people racing on one invite cannot both win.
func (r inviteRepo) Use(ctx context.Context, id, userID int64, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE invites SET used_by = $2, used_at = $3
		WHERE id = $1 AND used_at IS NULL AND expires_at > $3`, id, userID, at)
	if err != nil {
		return wrap("use invite", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("use invite: unknown, used or expired: %w", repo.ErrConflict)
	}
	return nil
}

func (r inviteRepo) Delete(ctx context.Context, id int64) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM invites WHERE id = $1`, id)
	return requireRow(tag, err, "delete invite")
}

type instanceSettingsRepo struct{ pool *pgxpool.Pool }

func (r instanceSettingsRepo) Get(ctx context.Context, key string, dst any) (bool, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT value FROM instance_settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrap("get instance setting", err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return false, fmt.Errorf("decode instance setting %q: %w", key, err)
	}
	return true, nil
}

func (r instanceSettingsRepo) Set(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode instance setting %q: %w", key, err)
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO instance_settings (key, value) VALUES ($1, $2::text::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, string(raw))
	return wrap("set instance setting", err)
}
