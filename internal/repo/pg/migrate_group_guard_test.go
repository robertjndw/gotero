package pg_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
)

func TestMigrateConcurrent(t *testing.T) {
	pool := pgtest.New(t)
	// New hands out a pre-migrated clone; wipe it so the goroutines race on
	// real migrations.
	_, err := pool.Exec(ctx(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public`)
	require.NoError(t, err)

	errs := make(chan error, 3)
	for range 3 {
		go func() { errs <- pg.Migrate(ctx(), pool, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	}
	for range 3 {
		require.NoError(t, <-errs)
	}
}

func TestGroupOwnerGuards(t *testing.T) {
	s, _ := newStore(t)
	owner, member := mustUser(t, s, "owner"), mustUser(t, s, "member")
	gr := s.Groups()
	g, err := gr.Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "G"})
	require.NoError(t, err)
	version := func() int64 {
		got, err := gr.Get(ctx(), g.ID)
		require.NoError(t, err)
		return got.Version
	}

	require.NoError(t, gr.SetMember(ctx(), g.ID, member.ID, "member"))
	v := version()

	assert.ErrorIs(t, gr.RemoveMember(ctx(), g.ID, owner.ID), repo.ErrConflict)
	assert.ErrorIs(t, gr.SetMember(ctx(), g.ID, owner.ID, "admin"), repo.ErrConflict)
	assert.ErrorIs(t, gr.RemoveMember(ctx(), g.ID, 999), repo.ErrNotFound)
	assert.Equal(t, v, version(), "rejected changes do not bump the version")
	members, err := gr.Members(ctx(), g.ID)
	require.NoError(t, err)
	require.Len(t, members, 2)
	assert.Equal(t, "owner", members[0].Role)

	require.NoError(t, gr.SetMember(ctx(), g.ID, member.ID, "admin"))
	require.NoError(t, gr.SetOwner(ctx(), g.ID, member.ID))
	// The new owner is protected, the former owner is an ordinary admin now.
	assert.ErrorIs(t, gr.RemoveMember(ctx(), g.ID, member.ID), repo.ErrConflict)
	require.NoError(t, gr.SetMember(ctx(), g.ID, owner.ID, "member"))
	require.NoError(t, gr.RemoveMember(ctx(), g.ID, owner.ID))
}
