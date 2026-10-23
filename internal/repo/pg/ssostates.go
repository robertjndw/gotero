package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

const ssoStateCols = `id_hash, binding_hash, provider, nonce, pkce_verifier, request_id,
	invite_hash, link_user_id, return_to, identity, created_at, expires_at`

type ssoStateRepo struct{ pool *pgxpool.Pool }

func (r ssoStateRepo) Create(ctx context.Context, s repo.SSOState) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO sso_states (id_hash, binding_hash, provider, nonce, pkce_verifier, request_id,
			invite_hash, link_user_id, return_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		s.IDHash, s.BindingHash, s.Provider, s.Nonce, s.PKCEVerifier, s.RequestID,
		s.InviteHash, s.LinkUserID, s.ReturnTo, s.ExpiresAt)
	return wrap("create sso state", err)
}

func (r ssoStateRepo) Get(ctx context.Context, idHash []byte, now time.Time) (repo.SSOState, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+ssoStateCols+` FROM sso_states WHERE id_hash = $1 AND expires_at > $2`, idHash, now)
	if err != nil {
		return repo.SSOState{}, wrap("get sso state", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.SSOState])
	return out, wrap("get sso state", err)
}

func (r ssoStateRepo) Take(ctx context.Context, idHash []byte, now time.Time) (repo.SSOState, error) {
	rows, err := r.pool.Query(ctx,
		`DELETE FROM sso_states WHERE id_hash = $1 AND expires_at > $2 RETURNING `+ssoStateCols, idHash, now)
	if err != nil {
		return repo.SSOState{}, wrap("take sso state", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.SSOState])
	return out, wrap("take sso state", err)
}

func (r ssoStateRepo) Stash(ctx context.Context, idHash []byte, identity []byte, newExpiry, now time.Time) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE sso_states SET identity = $2, expires_at = $3
		WHERE id_hash = $1 AND identity IS NULL AND expires_at > $4`,
		idHash, identity, newExpiry, now)
	return requireRow(tag, err, "stash sso identity")
}
