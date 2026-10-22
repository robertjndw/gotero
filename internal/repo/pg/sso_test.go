package pg_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

var ssoNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestGetByEmail(t *testing.T) {
	s, _ := newStore(t)
	u, err := s.Users().Create(ctx(), repo.User{Username: "alice", Email: "Alice@Example.com", PasswordHash: "x"})
	require.NoError(t, err)

	got, err := s.Users().GetByEmail(ctx(), "alice@example.COM")
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.ID)

	_, err = s.Users().GetByEmail(ctx(), "nobody@example.com")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestIdentityCRUD(t *testing.T) {
	s, _ := newStore(t)
	a, b := mustUser(t, s, "alice"), mustUser(t, s, "bob")

	i, err := s.Identities().Create(ctx(), repo.Identity{UserID: a.ID, Provider: "corp", Subject: "sub-1"})
	require.NoError(t, err)
	assert.Empty(t, i.Email, "NULL email reads back as empty")
	assert.Nil(t, i.LastLoginAt)

	got, err := s.Identities().Get(ctx(), "corp", "sub-1")
	require.NoError(t, err)
	assert.Equal(t, i, got)

	_, err = s.Identities().Get(ctx(), "corp", "other")
	assert.ErrorIs(t, err, repo.ErrNotFound)

	_, err = s.Identities().Create(ctx(), repo.Identity{UserID: b.ID, Provider: "corp", Subject: "sub-1"})
	assert.ErrorIs(t, err, repo.ErrConflict, "same external account twice")
	_, err = s.Identities().Create(ctx(), repo.Identity{UserID: a.ID, Provider: "corp", Subject: "sub-2"})
	assert.ErrorIs(t, err, repo.ErrConflict, "same provider twice for one user")
	_, err = s.Identities().Create(ctx(), repo.Identity{UserID: a.ID, Provider: "okta", Subject: "sub-1", Email: "a@example.com"})
	require.NoError(t, err)

	require.NoError(t, s.Identities().Touch(ctx(), i.ID, "new@example.com", ssoNow))
	got, err = s.Identities().Get(ctx(), "corp", "sub-1")
	require.NoError(t, err)
	assert.Equal(t, "new@example.com", got.Email)
	require.NotNil(t, got.LastLoginAt)
	assert.True(t, got.LastLoginAt.Equal(ssoNow))

	require.NoError(t, s.Identities().Touch(ctx(), i.ID, "", ssoNow.Add(time.Hour)))
	got, _ = s.Identities().Get(ctx(), "corp", "sub-1")
	assert.Equal(t, "new@example.com", got.Email, "an empty email keeps the stored one")
	assert.ErrorIs(t, s.Identities().Touch(ctx(), 9999, "", ssoNow), repo.ErrNotFound)

	list, err := s.Identities().ListByUser(ctx(), a.ID)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	m, err := s.Identities().ProvidersByUsers(ctx(), []int64{a.ID, b.ID})
	require.NoError(t, err)
	assert.Equal(t, map[int64][]string{a.ID: {"corp", "okta"}}, m)
	m, err = s.Identities().ProvidersByUsers(ctx(), nil)
	require.NoError(t, err)
	assert.Empty(t, m)

	require.NoError(t, s.Identities().Delete(ctx(), a.ID, "corp"))
	assert.ErrorIs(t, s.Identities().Delete(ctx(), a.ID, "corp"), repo.ErrNotFound)
}

func TestIdentitiesCascadeWithUser(t *testing.T) {
	s, _ := newStore(t)
	a := mustUser(t, s, "alice")
	_, err := s.Identities().Create(ctx(), repo.Identity{UserID: a.ID, Provider: "corp", Subject: "s"})
	require.NoError(t, err)
	require.NoError(t, s.Users().Delete(ctx(), a.ID))
	_, err = s.Identities().Get(ctx(), "corp", "s")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func newInvite(t *testing.T, s interface {
	Invites() repo.InviteRepo
}, hash string,
) repo.Invite {
	t.Helper()
	inv, err := s.Invites().Create(ctx(), repo.Invite{TokenHash: []byte(hash), ExpiresAt: ssoNow.Add(time.Hour)})
	require.NoError(t, err)
	return inv
}

func TestCreateExternal(t *testing.T) {
	s, _ := newStore(t)
	inv := newInvite(t, s, "tok")

	u, err := s.Users().CreateExternal(ctx(),
		repo.User{Username: "carol", PasswordHash: ""},
		repo.Identity{Provider: "corp", Subject: "c-1", Email: "c@example.com"}, inv.ID, ssoNow)
	require.NoError(t, err)
	assert.Empty(t, u.PasswordHash)

	i, err := s.Identities().Get(ctx(), "corp", "c-1")
	require.NoError(t, err)
	assert.Equal(t, u.ID, i.UserID)
	got, err := s.Invites().GetByHash(ctx(), []byte("tok"))
	require.NoError(t, err)
	require.NotNil(t, got.UsedBy)
	assert.Equal(t, u.ID, *got.UsedBy)
}

func TestCreateExternalRollsBack(t *testing.T) {
	s, _ := newStore(t)
	mustUser(t, s, "taken")
	inv := newInvite(t, s, "tok")

	_, err := s.Users().CreateExternal(ctx(), repo.User{Username: "taken"},
		repo.Identity{Provider: "corp", Subject: "x"}, inv.ID, ssoNow)
	assert.ErrorIs(t, err, repo.ErrConflict)
	got, _ := s.Invites().GetByHash(ctx(), []byte("tok"))
	assert.Nil(t, got.UsedBy, "invite stays usable")

	// A clashing identity must undo the user and the invite claim.
	other := mustUser(t, s, "other")
	_, err = s.Identities().Create(ctx(), repo.Identity{UserID: other.ID, Provider: "corp", Subject: "dup"})
	require.NoError(t, err)
	_, err = s.Users().CreateExternal(ctx(), repo.User{Username: "fresh"},
		repo.Identity{Provider: "corp", Subject: "dup"}, inv.ID, ssoNow)
	assert.ErrorIs(t, err, repo.ErrConflict)
	_, err = s.Users().GetByUsername(ctx(), "fresh")
	assert.ErrorIs(t, err, repo.ErrNotFound)
	got, _ = s.Invites().GetByHash(ctx(), []byte("tok"))
	assert.Nil(t, got.UsedBy)

	// An already used invite fails the whole creation too.
	require.NoError(t, s.Invites().Use(ctx(), inv.ID, other.ID, ssoNow))
	_, err = s.Users().CreateExternal(ctx(), repo.User{Username: "late"},
		repo.Identity{Provider: "corp", Subject: "late"}, inv.ID, ssoNow)
	assert.ErrorIs(t, err, repo.ErrConflict)
	_, err = s.Identities().Get(ctx(), "corp", "late")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func testState(id string, expires time.Time) repo.SSOState {
	return repo.SSOState{
		IDHash: []byte(id), BindingHash: []byte("bind"), Provider: "corp", Nonce: "n",
		PKCEVerifier: "v", RequestID: "r", InviteHash: []byte("inv"), ReturnTo: "/library",
		ExpiresAt: expires,
	}
}

func TestSSOStateTake(t *testing.T) {
	s, _ := newStore(t)
	uid := mustUser(t, s, "alice").ID
	st := testState("a", ssoNow.Add(time.Minute))
	st.LinkUserID = &uid
	require.NoError(t, s.SSOStates().Create(ctx(), st))

	got, err := s.SSOStates().Get(ctx(), []byte("a"), ssoNow)
	require.NoError(t, err)
	assert.Equal(t, "corp", got.Provider)
	assert.Equal(t, []byte("inv"), got.InviteHash)
	assert.Equal(t, &uid, got.LinkUserID)
	assert.Nil(t, got.Identity)
	assert.Equal(t, "/library", got.ReturnTo)

	taken, err := s.SSOStates().Take(ctx(), []byte("a"), ssoNow)
	require.NoError(t, err)
	assert.Equal(t, got.IDHash, taken.IDHash)
	_, err = s.SSOStates().Take(ctx(), []byte("a"), ssoNow)
	assert.ErrorIs(t, err, repo.ErrNotFound, "single use")

	require.NoError(t, s.SSOStates().Create(ctx(), testState("b", ssoNow.Add(time.Minute))))
	_, err = s.SSOStates().Get(ctx(), []byte("b"), ssoNow.Add(time.Minute))
	assert.ErrorIs(t, err, repo.ErrNotFound, "expired")
	_, err = s.SSOStates().Take(ctx(), []byte("b"), ssoNow.Add(time.Minute))
	assert.ErrorIs(t, err, repo.ErrNotFound)
	_, err = s.SSOStates().Take(ctx(), []byte("missing"), ssoNow)
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestSSOStateStash(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.SSOStates().Create(ctx(), testState("a", ssoNow.Add(time.Minute))))

	raw := []byte(`{"sub":"x\"y","email":"a@example.com"}`)
	require.NoError(t, s.SSOStates().Stash(ctx(), []byte("a"), raw, ssoNow.Add(2*time.Minute), ssoNow))
	assert.ErrorIs(t,
		s.SSOStates().Stash(ctx(), []byte("a"), []byte(`{}`), ssoNow.Add(time.Hour), ssoNow), repo.ErrNotFound,
		"stash works once")

	got, err := s.SSOStates().Get(ctx(), []byte("a"), ssoNow)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(got.Identity), "backslashes survive")
	assert.True(t, got.ExpiresAt.Equal(ssoNow.Add(2*time.Minute)))

	require.NoError(t, s.SSOStates().Create(ctx(), testState("old", ssoNow.Add(time.Minute))))
	assert.ErrorIs(t,
		s.SSOStates().Stash(ctx(), []byte("old"), raw, ssoNow.Add(time.Hour), ssoNow.Add(time.Minute)), repo.ErrNotFound,
		"expired states cannot be stashed")
	assert.ErrorIs(t,
		s.SSOStates().Stash(ctx(), []byte("none"), raw, ssoNow.Add(time.Hour), ssoNow), repo.ErrNotFound)
}

func TestSSOStatesCascadeAndPrune(t *testing.T) {
	s, pool := newStore(t)
	uid := mustUser(t, s, "alice").ID
	st := testState("linked", ssoNow.Add(time.Hour))
	st.LinkUserID = &uid
	require.NoError(t, s.SSOStates().Create(ctx(), st))
	require.NoError(t, s.SSOStates().Create(ctx(), testState("old", ssoNow.Add(-time.Minute))))
	require.NoError(t, s.SSOStates().Create(ctx(), testState("fresh", ssoNow.Add(time.Hour))))

	n, err := s.Prune(ctx(), ssoNow, time.Hour)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	_, err = s.SSOStates().Get(ctx(), []byte("fresh"), ssoNow)
	require.NoError(t, err)

	require.NoError(t, s.Users().Delete(ctx(), uid))
	var c int
	require.NoError(t, pool.QueryRow(ctx(), `SELECT count(*) FROM sso_states WHERE id_hash = 'linked'::bytea`).Scan(&c))
	assert.Zero(t, c, "link states die with their user")
}
