package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

const loginCols = `token_hash, required_user_id, client_name, status, user_id,
	COALESCE(api_key, ''), created_at, expires_at`

type loginSessionRepo struct{ pool *pgxpool.Pool }

func (r loginSessionRepo) Create(ctx context.Context, s repo.LoginSession) error {
	status := s.Status
	if status == "" {
		status = "pending"
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO login_sessions (token_hash, required_user_id, client_name, status, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, s.TokenHash, s.RequiredUserID, s.ClientName, status, s.ExpiresAt)
	return wrap("create login session", err)
}

func (r loginSessionRepo) Get(ctx context.Context, tokenHash []byte) (repo.LoginSession, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+loginCols+` FROM login_sessions WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return repo.LoginSession{}, wrap("get login session", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.LoginSession])
	return out, wrap("get login session", err)
}

func (r loginSessionRepo) Complete(ctx context.Context, tokenHash []byte, userID int64, apiKey string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE login_sessions SET status = 'completed', user_id = $2, api_key = $3
		WHERE token_hash = $1 AND status = 'pending'`, tokenHash, userID, apiKey)
	if err != nil {
		return wrap("complete login session", err)
	}
	if tag.RowsAffected() == 0 {
		return r.missingOrConflict(ctx, tokenHash, "complete login session")
	}
	return nil
}

// ConsumeAPIKey is a single UPDATE ... RETURNING so that two concurrent polls
// cannot both receive the key.
func (r loginSessionRepo) ConsumeAPIKey(ctx context.Context, tokenHash []byte) (string, error) {
	var key string
	err := r.pool.QueryRow(ctx, `
		UPDATE login_sessions s SET api_key = NULL
		FROM (SELECT api_key FROM login_sessions WHERE token_hash = $1 AND status = 'completed'
		      AND api_key IS NOT NULL FOR UPDATE) old
		WHERE s.token_hash = $1
		RETURNING old.api_key`, tokenHash).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return key, wrap("consume login session key", err)
}

// missingOrConflict tells "no such session" apart from "not pending".
func (r loginSessionRepo) missingOrConflict(ctx context.Context, tokenHash []byte, op string) error {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM login_sessions WHERE token_hash = $1)`, tokenHash).Scan(&exists); err != nil {
		return wrap(op, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", op, repo.ErrNotFound)
	}
	return fmt.Errorf("%s: session is not pending: %w", op, repo.ErrConflict)
}

// Cancel is idempotent for sessions that already left the pending state.
func (r loginSessionRepo) Cancel(ctx context.Context, tokenHash []byte) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE login_sessions SET status = 'cancelled' WHERE token_hash = $1 AND status = 'pending'`, tokenHash)
	if err != nil {
		return wrap("cancel login session", err)
	}
	if tag.RowsAffected() == 0 {
		if err := r.missingOrConflict(ctx, tokenHash, "cancel login session"); err != nil && !isConflict(err) {
			return err
		}
	}
	return nil
}

type webSessionRepo struct{ pool *pgxpool.Pool }

func (r webSessionRepo) Create(ctx context.Context, s repo.WebSession) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO web_sessions (id_hash, user_id, csrf_token, expires_at) VALUES ($1, $2, $3, $4)`,
		s.IDHash, s.UserID, s.CSRFToken, s.ExpiresAt)
	return wrap("create web session", err)
}

func (r webSessionRepo) Get(ctx context.Context, idHash []byte) (repo.WebSession, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id_hash, user_id, csrf_token, created_at, expires_at FROM web_sessions WHERE id_hash = $1`, idHash)
	if err != nil {
		return repo.WebSession{}, wrap("get web session", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.WebSession])
	return out, wrap("get web session", err)
}

func (r webSessionRepo) Extend(ctx context.Context, idHash []byte, expiresAt time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE web_sessions SET expires_at = $2 WHERE id_hash = $1`, idHash, expiresAt)
	return requireRow(tag, err, "extend web session")
}

func (r webSessionRepo) Delete(ctx context.Context, idHash []byte) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM web_sessions WHERE id_hash = $1`, idHash)
	return wrap("delete web session", err)
}

func (r webSessionRepo) DeleteForUser(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM web_sessions WHERE user_id = $1`, userID)
	return wrap("delete user web sessions", err)
}
