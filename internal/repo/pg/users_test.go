package pg_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestUserCreateAndGet(t *testing.T) {
	s, _ := newStore(t)
	quota := int64(1 << 20)
	u, err := s.Users().Create(ctx(), repo.User{
		Username: "Alice", DisplayName: "Alice A", Email: "a@example.com",
		PasswordHash: "hash", IsAdmin: true, QuotaBytes: &quota,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 1, u.ID)
	assert.NotZero(t, u.LibraryID)
	assert.False(t, u.CreatedAt.IsZero())
	assert.Nil(t, u.LastSyncAt)

	got, err := s.Users().Get(ctx(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, u, got)
	assert.Equal(t, "Alice", got.Username)
	assert.Equal(t, &quota, got.QuotaBytes)

	byName, err := s.Users().GetByUsername(ctx(), "aLiCe")
	require.NoError(t, err, "usernames are case-insensitive")
	assert.Equal(t, u.ID, byName.ID)

	lib, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, lib.Version())

	_, err = s.Users().Get(ctx(), 999)
	assert.ErrorIs(t, err, repo.ErrNotFound)
	_, err = s.Users().GetByUsername(ctx(), "nobody")
	assert.ErrorIs(t, err, repo.ErrNotFound)
}

func TestUserUsernameConflict(t *testing.T) {
	s, pool := newStore(t)
	_, err := s.Users().Create(ctx(), repo.User{Username: "alice", PasswordHash: "x"})
	require.NoError(t, err)
	_, err = s.Users().Create(ctx(), repo.User{Username: "ALICE", PasswordHash: "x"})
	require.ErrorIs(t, err, repo.ErrConflict)

	// The failed create must not leave an orphan library behind.
	var n int
	require.NoError(t, pool.QueryRow(ctx(), `SELECT count(*) FROM libraries`).Scan(&n))
	assert.Equal(t, 1, n)

	_, err = s.Users().Create(ctx(), repo.User{Username: "bob", Email: "A@x.com", PasswordHash: "x"})
	require.NoError(t, err)
	_, err = s.Users().Create(ctx(), repo.User{Username: "carol", Email: "a@X.com", PasswordHash: "x"})
	assert.ErrorIs(t, err, repo.ErrConflict, "emails are unique case-insensitively")

	// Users without email do not collide with each other.
	_, err = s.Users().Create(ctx(), repo.User{Username: "dave", PasswordHash: "x"})
	require.NoError(t, err)
	_, err = s.Users().Create(ctx(), repo.User{Username: "erin", PasswordHash: "x"})
	require.NoError(t, err)
}

func TestUserIDAllocationSkipsExplicitIDs(t *testing.T) {
	s, _ := newStore(t)
	for _, id := range []int64{1, 2, 4} {
		u, err := s.Users().Create(ctx(), repo.User{ID: id, Username: "imp" + string(rune('0'+id)), PasswordHash: "x"})
		require.NoError(t, err)
		assert.Equal(t, id, u.ID)
	}
	var ids []int64
	for _, name := range []string{"a", "b", "c"} {
		u, err := s.Users().Create(ctx(), repo.User{Username: name, PasswordHash: "x"})
		require.NoError(t, err)
		ids = append(ids, u.ID)
	}
	assert.Equal(t, []int64{3, 5, 6}, ids)

	_, err := s.Users().Create(ctx(), repo.User{ID: 3, Username: "dup", PasswordHash: "x"})
	assert.ErrorIs(t, err, repo.ErrConflict)
}

func TestUserConcurrentAllocation(t *testing.T) {
	s, _ := newStore(t)
	const n = 15
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := s.Users().Create(ctx(), repo.User{Username: fmt.Sprintf("u%d", i), PasswordHash: "x"})
			assert.NoError(t, err)
			ids[i] = u.ID
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, id := range ids {
		assert.False(t, seen[id])
		seen[id] = true
	}
}

func TestUserListUpdateDelete(t *testing.T) {
	s, _ := newStore(t)
	var users []repo.User
	for _, n := range []string{"alice", "bob", "carol", "al_ex"} {
		users = append(users, mustUser(t, s, n))
	}
	all, err := s.Users().List(ctx(), "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, all, 4)

	page, err := s.Users().List(ctx(), "", 2, users[1].ID)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "carol", page[0].Username)

	found, err := s.Users().List(ctx(), "AL", 0, 0)
	require.NoError(t, err)
	assert.Len(t, found, 2)
	found, err = s.Users().List(ctx(), "al_", 0, 0)
	require.NoError(t, err)
	assert.Len(t, found, 1, "LIKE wildcards in the query are escaped")
	found, _ = s.Users().List(ctx(), "%", 0, 0)
	assert.Empty(t, found)

	u := users[0]
	u.DisplayName = "Alice Changed"
	u.IsAdmin = true
	u.Email = "alice@example.com"
	updated, err := s.Users().Update(ctx(), u)
	require.NoError(t, err)
	assert.Equal(t, "Alice Changed", updated.DisplayName)
	assert.True(t, updated.IsAdmin)
	assert.Equal(t, "alice@example.com", updated.Email)

	u.Username = "bob"
	_, err = s.Users().Update(ctx(), u)
	assert.ErrorIs(t, err, repo.ErrConflict)
	_, err = s.Users().Update(ctx(), repo.User{ID: 999, Username: "ghost"})
	assert.ErrorIs(t, err, repo.ErrNotFound)

	n, err := s.Users().CountAdmins(ctx())
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	u.Username, u.Disabled = "alice", true
	_, err = s.Users().Update(ctx(), u)
	require.NoError(t, err)
	n, _ = s.Users().CountAdmins(ctx())
	assert.Zero(t, n, "disabled admins do not count")

	at := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, s.Users().TouchSync(ctx(), u.ID, at))
	got, _ := s.Users().Get(ctx(), u.ID)
	require.NotNil(t, got.LastSyncAt)
	assert.True(t, at.Equal(*got.LastSyncAt))
	assert.ErrorIs(t, s.Users().TouchSync(ctx(), 999, at), repo.ErrNotFound)

	require.NoError(t, s.Users().Delete(ctx(), u.ID))
	_, err = s.Users().Get(ctx(), u.ID)
	assert.ErrorIs(t, err, repo.ErrNotFound)
	_, err = s.Library(ctx(), u.LibraryID)
	assert.ErrorIs(t, err, repo.ErrNotFound, "the library goes with the user")
	assert.ErrorIs(t, s.Users().Delete(ctx(), u.ID), repo.ErrNotFound)
}

func TestUserDeleteCascadesContent(t *testing.T) {
	s, pool := newStore(t)
	u := mustUser(t, s, "alice")
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		return tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, func(o *repo.Object) {
			o.Tags = []repo.ItemTag{{Tag: "t"}}
		})})
	})
	_, err := s.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: []byte("h"), Prefix: "p", Name: "n"})
	require.NoError(t, err)
	require.NoError(t, s.Users().Delete(ctx(), u.ID))
	for _, tbl := range []string{"items", "item_tags", "api_keys", "libraries"} {
		var n int
		require.NoError(t, pool.QueryRow(ctx(), `SELECT count(*) FROM `+tbl).Scan(&n))
		assert.Zerof(t, n, tbl)
	}
}

func TestUserDeleteBlockedByOwnedGroup(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	_, err := s.Groups().Create(ctx(), repo.Group{OwnerID: u.ID, Name: "g"})
	require.NoError(t, err)
	assert.ErrorIs(t, s.Users().Delete(ctx(), u.ID), repo.ErrConflict)
	_, err = s.Users().Get(ctx(), u.ID)
	assert.NoError(t, err)
}

func TestKeys(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	access := repo.KeyAccess{Library: true, Write: true, Groups: "all"}
	k, err := s.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: []byte("hash1"), Prefix: "abcd", Name: "laptop", Access: access})
	require.NoError(t, err)
	assert.NotZero(t, k.ID)
	assert.Equal(t, access, k.Access)

	got, err := s.Keys().GetByHash(ctx(), []byte("hash1"))
	require.NoError(t, err)
	assert.Equal(t, k, got)
	_, err = s.Keys().GetByHash(ctx(), []byte("nope"))
	assert.ErrorIs(t, err, repo.ErrNotFound)

	_, err = s.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: []byte("hash1"), Prefix: "x", Name: "dup"})
	assert.ErrorIs(t, err, repo.ErrConflict)

	k2, err := s.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: []byte("hash2"), Prefix: "efgh", Name: "phone"})
	require.NoError(t, err)
	list, err := s.Keys().ListByUser(ctx(), u.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, k2.ID, list[1].ID)

	other := mustUser(t, s, "bob")
	assert.ErrorIs(t, s.Keys().Delete(ctx(), other.ID, k.ID), repo.ErrNotFound, "keys are scoped to their user")
	require.NoError(t, s.Keys().Delete(ctx(), u.ID, k.ID))
	require.NoError(t, s.Keys().DeleteByHash(ctx(), []byte("hash2")))
	assert.ErrorIs(t, s.Keys().DeleteByHash(ctx(), []byte("hash2")), repo.ErrNotFound)
}

func TestKeyTouchCoalesces(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	k, err := s.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: []byte("h"), Prefix: "p", Name: "n"})
	require.NoError(t, err)
	assert.Nil(t, k.LastUsedAt)

	t0 := time.Now().UTC().Truncate(time.Microsecond)
	last := func() time.Time {
		got, err := s.Keys().GetByHash(ctx(), []byte("h"))
		require.NoError(t, err)
		require.NotNil(t, got.LastUsedAt)
		return *got.LastUsedAt
	}
	require.NoError(t, s.Keys().Touch(ctx(), k.ID, t0))
	assert.True(t, t0.Equal(last()))
	require.NoError(t, s.Keys().Touch(ctx(), k.ID, t0.Add(30*time.Second)))
	assert.True(t, t0.Equal(last()), "touches within a minute are dropped")
	require.NoError(t, s.Keys().Touch(ctx(), k.ID, t0.Add(61*time.Second)))
	assert.True(t, t0.Add(61*time.Second).Equal(last()))
}

func TestGroups(t *testing.T) {
	s, _ := newStore(t)
	owner, member := mustUser(t, s, "owner"), mustUser(t, s, "member")

	g, err := s.Groups().Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "Lab", Description: "d", URL: "u"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, g.ID)
	assert.EqualValues(t, 1, g.Version)
	assert.Equal(t, "members", g.LibraryReading)
	assert.Equal(t, "members", g.FileEditing)
	lib, err := s.Library(ctx(), g.LibraryID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, lib.Version())

	members, err := s.Groups().Members(ctx(), g.ID)
	require.NoError(t, err)
	assert.Equal(t, []repo.GroupMember{{GroupID: g.ID, UserID: owner.ID, Username: "owner", Role: "owner"}}, members)

	require.NoError(t, s.Groups().SetMember(ctx(), g.ID, member.ID, "member"))
	got, _ := s.Groups().Get(ctx(), g.ID)
	assert.EqualValues(t, 2, got.Version)
	require.NoError(t, s.Groups().SetMember(ctx(), g.ID, member.ID, "admin"))
	got, _ = s.Groups().Get(ctx(), g.ID)
	assert.EqualValues(t, 3, got.Version)
	members, _ = s.Groups().Members(ctx(), g.ID)
	require.Len(t, members, 2)
	assert.Equal(t, "admin", members[1].Role)

	assert.ErrorIs(t, s.Groups().SetMember(ctx(), 999, member.ID, "member"), repo.ErrNotFound)
	assert.ErrorIs(t, s.Groups().SetMember(ctx(), g.ID, 999, "member"), repo.ErrNotFound)
	got, _ = s.Groups().Get(ctx(), g.ID)
	assert.EqualValues(t, 3, got.Version, "failed member changes do not bump the version")

	g.Name, g.LibraryReading, g.FileEditing = "Lab 2", "all", "admins"
	updated, err := s.Groups().Update(ctx(), g)
	require.NoError(t, err)
	assert.EqualValues(t, 4, updated.Version)
	assert.Equal(t, "Lab 2", updated.Name)
	assert.Equal(t, "all", updated.LibraryReading)

	forUser, err := s.Groups().ListForUser(ctx(), member.ID)
	require.NoError(t, err)
	require.Len(t, forUser, 1)
	assert.Equal(t, g.ID, forUser[0].ID)

	require.NoError(t, s.Groups().RemoveMember(ctx(), g.ID, member.ID))
	got, _ = s.Groups().Get(ctx(), g.ID)
	assert.EqualValues(t, 5, got.Version)
	assert.ErrorIs(t, s.Groups().RemoveMember(ctx(), g.ID, member.ID), repo.ErrNotFound)
	got, _ = s.Groups().Get(ctx(), g.ID)
	assert.EqualValues(t, 5, got.Version)
	forUser, _ = s.Groups().ListForUser(ctx(), member.ID)
	assert.Empty(t, forUser)

	_, err = s.Groups().Update(ctx(), repo.Group{ID: 999, Name: "x", LibraryReading: "all", LibraryEditing: "members", FileEditing: "none"})
	assert.ErrorIs(t, err, repo.ErrNotFound)

	_, err = s.Groups().Update(ctx(), repo.Group{ID: g.ID, Name: "x", LibraryReading: "bogus", LibraryEditing: "members", FileEditing: "none"})
	require.Error(t, err, "check constraints reject bad policy values")

	g2, err := s.Groups().Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "Second"})
	require.NoError(t, err)
	list, err := s.Groups().List(ctx(), 10, g.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, g2.ID, list[0].ID)

	require.NoError(t, s.Groups().Delete(ctx(), g.ID))
	_, err = s.Groups().Get(ctx(), g.ID)
	assert.ErrorIs(t, err, repo.ErrNotFound)
	_, err = s.Library(ctx(), g.LibraryID)
	assert.ErrorIs(t, err, repo.ErrNotFound)
	assert.ErrorIs(t, s.Groups().Delete(ctx(), g.ID), repo.ErrNotFound)
}

func TestGroupIDAllocationSkipsExplicitIDs(t *testing.T) {
	s, _ := newStore(t)
	owner := mustUser(t, s, "owner")
	for _, id := range []int64{1, 3} {
		_, err := s.Groups().Create(ctx(), repo.Group{ID: id, OwnerID: owner.ID, Name: "imp"})
		require.NoError(t, err)
	}
	var ids []int64
	for range 2 {
		g, err := s.Groups().Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "auto"})
		require.NoError(t, err)
		ids = append(ids, g.ID)
	}
	assert.Equal(t, []int64{2, 4}, ids)
	_, err := s.Groups().Create(ctx(), repo.Group{ID: 2, OwnerID: owner.ID, Name: "dup"})
	assert.ErrorIs(t, err, repo.ErrConflict)
	_, err = s.Groups().Create(ctx(), repo.Group{OwnerID: 999, Name: "orphan"})
	assert.Error(t, err, "unknown owner")
}
