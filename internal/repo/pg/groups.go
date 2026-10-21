package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/internal/repo"
)

const groupCols = `id, library_id, owner_id, name, description, url, library_reading,
	library_editing, file_editing, version, created_at`

type groupRepo struct{ pool *pgxpool.Pool }

func (r groupRepo) Create(ctx context.Context, g repo.Group) (repo.Group, error) {
	var out repo.Group
	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		var libID int64
		if err := tx.QueryRow(ctx, `INSERT INTO libraries (kind) VALUES ('group') RETURNING id`).Scan(&libID); err != nil {
			return err
		}
		// Empty policy fields fall back to the column defaults.
		id, err := allocateID(ctx, tx, "group_id_seq", g.ID, func(id int64) (bool, error) {
			tag, err := tx.Exec(ctx, `
				INSERT INTO groups (id, library_id, owner_id, name, description, url,
					library_reading, library_editing, file_editing)
				VALUES ($1, $2, $3, $4, $5, $6,
					COALESCE($7, 'members'), COALESCE($8, 'members'), COALESCE($9, 'members'))
				ON CONFLICT (id) DO NOTHING`,
				id, libID, g.OwnerID, g.Name, g.Description, g.URL,
				nullIfEmpty(g.LibraryReading), nullIfEmpty(g.LibraryEditing), nullIfEmpty(g.FileEditing))
			return tag.RowsAffected() == 1, err
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_members (group_id, user_id, role) VALUES ($1, $2, 'owner')`, id, g.OwnerID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+groupCols+` FROM groups WHERE id = $1`, id)
		if err != nil {
			return err
		}
		out, err = pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Group])
		return err
	})
	return out, wrap("create group", err)
}

func (r groupRepo) Get(ctx context.Context, id int64) (repo.Group, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+groupCols+` FROM groups WHERE id = $1`, id)
	if err != nil {
		return repo.Group{}, wrap("get group", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Group])
	return out, wrap("get group", err)
}

func (r groupRepo) List(ctx context.Context, limit int, cursor int64) ([]repo.Group, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+groupCols+` FROM groups WHERE id > $1 ORDER BY id LIMIT $2`, cursor, lim)
	if err != nil {
		return nil, wrap("list groups", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.Group])
	return out, wrap("list groups", err)
}

func (r groupRepo) ListForUser(ctx context.Context, userID int64) ([]repo.Group, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+groupCols+` FROM groups
		WHERE id IN (SELECT group_id FROM group_members WHERE user_id = $1) ORDER BY id`, userID)
	if err != nil {
		return nil, wrap("list user groups", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.Group])
	return out, wrap("list user groups", err)
}

func (r groupRepo) Update(ctx context.Context, g repo.Group) (repo.Group, error) {
	rows, err := r.pool.Query(ctx, `
		UPDATE groups SET name = $2, description = $3, url = $4, library_reading = $5,
			library_editing = $6, file_editing = $7, version = version + 1
		WHERE id = $1 RETURNING `+groupCols,
		g.ID, g.Name, g.Description, g.URL, g.LibraryReading, g.LibraryEditing, g.FileEditing)
	if err != nil {
		return repo.Group{}, wrap("update group", err)
	}
	out, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[repo.Group])
	return out, wrap("update group", err)
}

// Delete removes the group and its library, which cascades to all content.
func (r groupRepo) Delete(ctx context.Context, id int64) error {
	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		var libID int64
		if err := tx.QueryRow(ctx, `DELETE FROM groups WHERE id = $1 RETURNING library_id`, id).Scan(&libID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM libraries WHERE id = $1`, libID)
		return err
	})
	return wrap("delete group", err)
}

func (r groupRepo) SetOwner(ctx context.Context, groupID, newOwnerID int64) error {
	err := inTx(ctx, r.pool, func(tx pgx.Tx) error {
		// FOR UPDATE on the group row serializes concurrent owner changes, so
		// two of them cannot both demote "the" old owner.
		var oldOwner int64
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM groups WHERE id = $1 FOR UPDATE`, groupID).Scan(&oldOwner); err != nil {
			return err
		}
		var isMember bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM group_members WHERE group_id = $1 AND user_id = $2)`,
			groupID, newOwnerID).Scan(&isMember); err != nil {
			return err
		}
		if !isMember {
			return fmt.Errorf("new owner is not a member: %w", repo.ErrNotFound)
		}
		if oldOwner == newOwnerID {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE group_members SET role = 'admin' WHERE group_id = $1 AND user_id = $2`, groupID, oldOwner); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE group_members SET role = 'owner' WHERE group_id = $1 AND user_id = $2`, groupID, newOwnerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE groups SET owner_id = $2, version = version + 1 WHERE id = $1`, groupID, newOwnerID)
		return err
	})
	return wrap("set group owner", err)
}

func (r groupRepo) Members(ctx context.Context, groupID int64) ([]repo.GroupMember, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.group_id, m.user_id, u.username::text, u.display_name, m.role
		FROM group_members m JOIN users u ON u.id = m.user_id
		WHERE m.group_id = $1 ORDER BY m.user_id`, groupID)
	if err != nil {
		return nil, wrap("list members", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[repo.GroupMember])
	return out, wrap("list members", err)
}

// SetMember bumps the version and writes the membership in one statement, so
// the two cannot drift apart.
func (r groupRepo) SetMember(ctx context.Context, groupID, userID int64, role string) error {
	tag, err := r.pool.Exec(ctx, `
		WITH bumped AS (UPDATE groups SET version = version + 1 WHERE id = $1 RETURNING id)
		INSERT INTO group_members (group_id, user_id, role)
		SELECT id, $2, $3 FROM bumped
		ON CONFLICT (group_id, user_id) DO UPDATE SET role = EXCLUDED.role`, groupID, userID, role)
	return requireRow(tag, err, "set group member")
}

func (r groupRepo) RemoveMember(ctx context.Context, groupID, userID int64) error {
	tag, err := r.pool.Exec(ctx, `
		WITH removed AS (
			DELETE FROM group_members WHERE group_id = $1 AND user_id = $2 RETURNING 1
		)
		UPDATE groups SET version = version + 1 WHERE id = $1 AND EXISTS (SELECT 1 FROM removed)`,
		groupID, userID)
	return requireRow(tag, err, "remove group member")
}
