package pg

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

// Column order must match the field order of the repo struct, because rows
// are scanned with RowToStructByPos.
const userCols = `id, library_id, username::text, display_name, COALESCE(email::text, ''),
	password_hash, is_admin, disabled, quota_bytes, created_at, last_sync_at`

type userRepo struct{ pool *pgxpool.Pool }

// maxIDAttempts bounds the sequence skipping loop. Explicit IDs are inserted
// by the importer, so a collision is possible but long runs are not.
const maxIDAttempts = 10000

// allocateID inserts a row whose ID is either explicit or drawn from seq.
// insert must use ON CONFLICT (id) DO NOTHING and report whether a row was
// written. A username clash still surfaces as a unique violation because it
// is not covered by the conflict target.
func allocateID(ctx context.Context, tx pgx.Tx, seq string, id int64, insert func(id int64) (bool, error)) (int64, error) {
	if id != 0 {
		ok, err := insert(id)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, repo.ErrConflict
		}
		return id, nil
	}
	for range maxIDAttempts {
		if err := tx.QueryRow(ctx, `SELECT nextval('`+seq+`')`).Scan(&id); err != nil {
			return 0, err
		}
		ok, err := insert(id)
		if err != nil {
			return 0, err
		}
		if ok {
			return id, nil
		}
	}
	return 0, repo.ErrConflict
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (r userRepo) Create(ctx context.Context, u repo.User) (repo.User, error) {
	return r.create(ctx, u, nil, 0, time.Time{})
}

// CreateWithInvite claims the invite first inside the transaction. If the
// username is taken the whole thing rolls back and the invite stays usable.
func (r userRepo) CreateWithInvite(ctx context.Context, u repo.User, inviteID int64, at time.Time) (repo.User, error) {
	return r.create(ctx, u, nil, inviteID, at)
}

func (r userRepo) CreateExternal(ctx context.Context, u repo.User, ident repo.Identity, inviteID int64, at time.Time) (repo.User, error) {
	return r.create(ctx, u, &ident, inviteID, at)
}

func (r userRepo) create(ctx context.Context, u repo.User, ident *repo.Identity, inviteID int64, at time.Time) (repo.User, error) {
	var out repo.User
	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		var libID int64
		if err := tx.QueryRow(ctx, `INSERT INTO libraries (kind) VALUES ('user') RETURNING id`).Scan(&libID); err != nil {
			return err
		}
		_, err := allocateID(ctx, tx, "user_id_seq", u.ID, func(id int64) (bool, error) {
			tag, err := tx.Exec(ctx, `
				INSERT INTO users (id, library_id, username, display_name, email, password_hash, is_admin, disabled, quota_bytes)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				ON CONFLICT (id) DO NOTHING`,
				id, libID, u.Username, u.DisplayName, nullIfEmpty(u.Email), u.PasswordHash, u.IsAdmin, u.Disabled, u.QuotaBytes)
			return tag.RowsAffected() == 1, err
		})
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+userCols+` FROM users WHERE library_id = $1`, libID)
		if err != nil {
			return err
		}
		out, err = pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.User])
		if err != nil {
			return err
		}
		if ident != nil {
			_, err := tx.Exec(ctx, `
				INSERT INTO user_identities (user_id, provider, subject, email, last_login_at)
				VALUES ($1, $2, $3, $4, $5)`,
				out.ID, ident.Provider, ident.Subject, nullIfEmpty(ident.Email), at)
			if err != nil {
				return err
			}
		}
		if inviteID == 0 {
			return nil
		}
		tag, err := tx.Exec(ctx, `
			UPDATE invites SET used_by = $2, used_at = $3
			WHERE id = $1 AND used_at IS NULL AND expires_at > $3`, inviteID, out.ID, at)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return repo.ErrInviteUnavailable
		}
		return nil
	})
	return out, wrap("create user", err)
}

// inTx runs fn in a transaction on the pool and commits if it succeeds.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r userRepo) one(ctx context.Context, where string, arg any) (repo.User, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+userCols+` FROM users WHERE `+where, arg)
	if err != nil {
		return repo.User{}, wrap("get user", err)
	}
	u, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.User])
	return u, wrap("get user", err)
}

func (r userRepo) Get(ctx context.Context, id int64) (repo.User, error) {
	return r.one(ctx, `id = $1`, id)
}

func (r userRepo) GetByUsername(ctx context.Context, username string) (repo.User, error) {
	return r.one(ctx, `username = $1`, username)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (r userRepo) GetByEmail(ctx context.Context, email string) (repo.User, error) {
	return r.one(ctx, `email = $1`, email)
}

func (r userRepo) List(ctx context.Context, q string, limit int, cursor int64) ([]repo.User, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	pattern := "%" + likeEscaper.Replace(q) + "%"
	rows, err := r.pool.Query(ctx, `
		SELECT `+userCols+` FROM users
		WHERE id > $1 AND ($2 = '' OR username ILIKE $3 OR display_name ILIKE $3 OR email ILIKE $3)
		ORDER BY id LIMIT $4`, cursor, q, pattern, lim)
	if err != nil {
		return nil, wrap("list users", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.User])
	return out, wrap("list users", err)
}

func (r userRepo) Update(ctx context.Context, u repo.User) (repo.User, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE users SET username = $2, display_name = $3, email = $4, password_hash = $5,
			is_admin = $6, disabled = $7, quota_bytes = $8
		WHERE id = $1 RETURNING `+userCols,
		u.ID, u.Username, u.DisplayName, nullIfEmpty(u.Email), u.PasswordHash, u.IsAdmin, u.Disabled, u.QuotaBytes)
	if err != nil {
		return repo.User{}, wrap("update user", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.User])
	return out, wrap("update user", err)
}

// Delete removes the user and its library. Users that still own groups are
// refused with ErrConflict.
func (r userRepo) Delete(ctx context.Context, id int64) error {
	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		var libID int64
		if err := tx.QueryRow(ctx, `DELETE FROM users WHERE id = $1 RETURNING library_id`, id).Scan(&libID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM libraries WHERE id = $1`, libID)
		return err
	})
	return wrap("delete user", mapDeleteErr(err))
}

func (r userRepo) CountAdmins(ctx context.Context) (int64, error) {
	var n int64
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_admin AND NOT disabled`).Scan(&n)
	return n, wrap("count admins", err)
}

func (r userRepo) TouchSync(ctx context.Context, id int64, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET last_sync_at = $2 WHERE id = $1`, id, at)
	return requireRow(tag, err, "touch user sync")
}
