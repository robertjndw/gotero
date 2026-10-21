package pg_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestStatsCounts(t *testing.T) {
	s, _ := newStore(t)
	owner := mustUser(t, s, "alice")
	_, err := s.Groups().Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "g"})
	require.NoError(t, err)
	write(t, s, owner.LibraryID, func(tx repo.LibraryTx) error {
		v, err := tx.NextVersion(ctx())
		require.NoError(t, err)
		return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", v, nil)})
	})
	st, err := s.Stats(ctx())
	require.NoError(t, err)
	assert.Equal(t, repo.Stats{Users: 1, Groups: 1, Items: 1, Attachments: 0}, st)
}

func TestCreateWithInviteIsAtomic(t *testing.T) {
	s, pool := newStore(t)
	now := time.Now().UTC()
	taken := mustUser(t, s, "taken")
	inv, err := s.Invites().Create(ctx(), repo.Invite{TokenHash: []byte("t"), ExpiresAt: now.Add(time.Hour)})
	require.NoError(t, err)

	// A username clash rolls back, so the invite must stay usable.
	_, err = s.Users().CreateWithInvite(ctx(), repo.User{Username: "TAKEN", PasswordHash: "x"}, inv.ID, now)
	require.ErrorIs(t, err, repo.ErrConflict)
	got, _ := s.Invites().GetByHash(ctx(), []byte("t"))
	assert.Nil(t, got.UsedBy)
	var libs int
	require.NoError(t, pool.QueryRow(ctx(), `SELECT count(*) FROM libraries`).Scan(&libs))
	assert.Equal(t, 1, libs)

	u, err := s.Users().CreateWithInvite(ctx(), repo.User{Username: "new", PasswordHash: "x"}, inv.ID, now)
	require.NoError(t, err)
	assert.NotEqual(t, taken.ID, u.ID)
	got, _ = s.Invites().GetByHash(ctx(), []byte("t"))
	require.NotNil(t, got.UsedBy)
	assert.Equal(t, u.ID, *got.UsedBy)

	// Reuse fails and does not leave a second user behind.
	_, err = s.Users().CreateWithInvite(ctx(), repo.User{Username: "again", PasswordHash: "x"}, inv.ID, now)
	require.ErrorIs(t, err, repo.ErrConflict)
	_, err = s.Users().GetByUsername(ctx(), "again")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestGroupSetOwner(t *testing.T) {
	s, _ := newStore(t)
	a, b, c := mustUser(t, s, "a"), mustUser(t, s, "b"), mustUser(t, s, "c")
	g, err := s.Groups().Create(ctx(), repo.Group{OwnerID: a.ID, Name: "g"})
	require.NoError(t, err)
	require.NoError(t, s.Groups().SetMember(ctx(), g.ID, b.ID, "member"))

	assert.ErrorIs(t, s.Groups().SetOwner(ctx(), g.ID, c.ID), repo.ErrNotFound, "non-member")
	assert.ErrorIs(t, s.Groups().SetOwner(ctx(), 999, b.ID), repo.ErrNotFound)

	before, _ := s.Groups().Get(ctx(), g.ID)
	require.NoError(t, s.Groups().SetOwner(ctx(), g.ID, b.ID))
	after, _ := s.Groups().Get(ctx(), g.ID)
	assert.Equal(t, b.ID, after.OwnerID)
	assert.Greater(t, after.Version, before.Version)
	roles := map[int64]string{}
	ms, _ := s.Groups().Members(ctx(), g.ID)
	for _, m := range ms {
		roles[m.UserID] = m.Role
	}
	assert.Equal(t, map[int64]string{a.ID: "admin", b.ID: "owner"}, roles)
}

func TestWebSessionExtend(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	require.NoError(t, s.WebSessions().Create(ctx(), repo.WebSession{IDHash: []byte("h"), UserID: u.ID, CSRFToken: "c", ExpiresAt: exp}))
	later := exp.Add(48 * time.Hour)
	require.NoError(t, s.WebSessions().Extend(ctx(), []byte("h"), later))
	got, err := s.WebSessions().Get(ctx(), []byte("h"))
	require.NoError(t, err)
	assert.True(t, later.Equal(got.ExpiresAt))
	assert.ErrorIs(t, s.WebSessions().Extend(ctx(), []byte("ghost"), later), repo.ErrNotFound)
}
