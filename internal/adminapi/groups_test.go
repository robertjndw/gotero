package adminapi_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func gpath(id int64, rest string) string { return fmt.Sprintf("/groups/%d%s", id, rest) }

func roles(t *testing.T, c *client, gid int64) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range items(c.mustOK("GET", gpath(gid, "/members"), nil)) {
		mm := m.(map[string]any)
		out[mm["username"].(string)] = mm["role"].(string)
	}
	return out
}

func TestGroupLifecycleAndRoles(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.user("alice"), e.user("bob"), e.user("carol")
	e.user("dave")
	ac := e.login("alice", "password-alice")
	bc := e.login("bob", "password-bob")
	cc := e.login("carol", "password-carol")
	dc := e.login("dave", "password-dave")

	res := ac.do("POST", "/groups", map[string]any{"name": "Lab", "description": "d", "libraryReading": "all", "libraryEditing": "admins", "fileEditing": "none"})
	require.Equal(t, 201, res.status, string(res.raw))
	g := res.json()
	gid := num(g, "id")
	assert.Equal(t, "Lab", g["name"])
	assert.Equal(t, map[string]any{"id": float64(alice.ID), "username": "alice"}, g["owner"])
	assert.Equal(t, "all", g["libraryReading"])
	assert.Equal(t, "admins", g["libraryEditing"])
	assert.Equal(t, "none", g["fileEditing"])
	assert.Equal(t, float64(1), g["memberCount"])
	assert.Equal(t, float64(0), g["usedBytes"])

	assert.Equal(t, map[string]string{"alice": "owner"}, roles(t, ac, gid))
	v0 := num(ac.mustOK("GET", gpath(gid, ""), nil), "version")

	// Owner adds a member and an admin; every change bumps the group version.
	m := ac.mustOK("PUT", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), map[string]any{"role": "member"})
	assert.Equal(t, map[string]any{"userId": float64(bob.ID), "username": "bob", "displayName": "bob", "role": "member"}, m)
	ac.mustOK("PUT", gpath(gid, fmt.Sprintf("/members/%d", carol.ID)), map[string]any{"role": "admin"})
	// Streaming clients of new members must learn about the group.
	assert.Subset(t, e.notify.accessed(), []int64{bob.ID, carol.ID})
	g2 := ac.mustOK("GET", gpath(gid, ""), nil)
	assert.Equal(t, float64(3), g2["memberCount"])
	assert.Equal(t, v0+2, num(g2, "version"))
	assert.Equal(t, map[string]string{"alice": "owner", "bob": "member", "carol": "admin"}, roles(t, bc, gid), "members can read the roster")

	// Outsiders cannot even tell the group exists.
	assert.Equal(t, 404, dc.do("GET", gpath(gid, ""), nil).status)
	assert.Equal(t, 404, dc.do("GET", gpath(gid, "/members"), nil).status)
	assert.Equal(t, 404, dc.do("PATCH", gpath(gid, ""), map[string]any{"name": "x"}).status)

	// Plain members can read but not manage.
	assert.Equal(t, 200, bc.do("GET", gpath(gid, ""), nil).status)
	assert.Equal(t, 403, bc.do("PATCH", gpath(gid, ""), map[string]any{"name": "x"}).status)
	assert.Equal(t, 403, bc.do("PUT", gpath(gid, fmt.Sprintf("/members/%d", e.admin.ID)), map[string]any{"role": "member"}).status)
	assert.Equal(t, 403, bc.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", carol.ID)), nil).status)
	assert.Equal(t, 403, bc.do("DELETE", gpath(gid, ""), nil).status)

	// A group admin edits the group and adds plain members, but cannot mint
	// admins, touch other admins, transfer or delete the group.
	assert.Equal(t, 200, cc.do("PATCH", gpath(gid, ""), map[string]any{"description": "new"}).status)
	dave := mustUserID(t, e, "dave")
	assert.Equal(t, 200, cc.do("PUT", gpath(gid, fmt.Sprintf("/members/%d", dave)), map[string]any{"role": "member"}).status)
	assert.Equal(t, 403, cc.do("PUT", gpath(gid, fmt.Sprintf("/members/%d", dave)), map[string]any{"role": "admin"}).status)
	assert.Equal(t, 403, cc.do("PATCH", gpath(gid, ""), map[string]any{"ownerId": carol.ID}).status)
	assert.Equal(t, 403, cc.do("DELETE", gpath(gid, ""), nil).status)
	assert.Equal(t, 204, cc.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", dave)), nil).status)

	// Owner protections.
	res = ac.do("PUT", gpath(gid, fmt.Sprintf("/members/%d", alice.ID)), map[string]any{"role": "member"})
	assert.Equal(t, 409, res.status)
	assert.Equal(t, "owner_role", res.errCode())
	assert.Equal(t, 409, ac.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", alice.ID)), nil).status)
	assert.Equal(t, 409, cc.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", alice.ID)), nil).status)
	assert.Equal(t, 422, ac.do("PUT", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), map[string]any{"role": "owner"}).status)
	assert.Equal(t, 422, ac.do("PUT", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), map[string]any{"role": "king"}).status)
	assert.Equal(t, 404, ac.do("PUT", gpath(gid, "/members/99999"), map[string]any{"role": "member"}).status)

	// Members may leave on their own.
	assert.Equal(t, 204, bc.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), nil).status)
	assert.Equal(t, 404, bc.do("GET", gpath(gid, ""), nil).status)
	assert.Equal(t, 404, ac.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), nil).status)

	// A plain admin cannot remove another admin; the owner can.
	ac.mustOK("PUT", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), map[string]any{"role": "admin"})
	assert.Equal(t, 403, cc.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), nil).status)
	assert.Equal(t, 204, ac.do("DELETE", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), nil).status)

	// Validation.
	res = ac.do("PATCH", gpath(gid, ""), map[string]any{"name": "", "libraryReading": "everyone", "fileEditing": "x"})
	assert.Equal(t, 422, res.status)
	for _, f := range []string{"name", "libraryReading", "fileEditing"} {
		assert.Contains(t, res.fields(), f)
	}
	assert.Equal(t, 422, ac.do("POST", "/groups", map[string]any{"description": "no name"}).status)

	// Deleting removes the group and its library.
	grp, err := e.store.Groups().Get(ctx(), gid)
	require.NoError(t, err)
	assert.Equal(t, 204, ac.do("DELETE", gpath(gid, ""), nil).status)
	assert.Equal(t, 404, ac.do("GET", gpath(gid, ""), nil).status)
	_, err = e.store.Library(ctx(), grp.LibraryID)
	assert.ErrorIs(t, err, repo.ErrNotFound, "library is gone")
}

func mustUserID(t *testing.T, e *env, name string) int64 {
	t.Helper()
	u, err := e.store.Users().GetByUsername(ctx(), name)
	require.NoError(t, err)
	return u.ID
}

func TestGroupOwnershipTransfer(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.user("alice"), e.user("bob")
	e.user("carol")
	ac := e.login("alice", "password-alice")
	bc := e.login("bob", "password-bob")

	gid := num(ac.mustOK("POST", "/groups", map[string]any{"name": "G"}), "id")
	ac.mustOK("PUT", gpath(gid, fmt.Sprintf("/members/%d", bob.ID)), map[string]any{"role": "member"})

	carol := mustUserID(t, e, "carol")
	res := ac.do("PATCH", gpath(gid, ""), map[string]any{"ownerId": carol})
	assert.Equal(t, 422, res.status, "the new owner must be a member")
	assert.Contains(t, res.fields(), "ownerId")

	before := num(ac.mustOK("GET", gpath(gid, ""), nil), "version")
	g := ac.mustOK("PATCH", gpath(gid, ""), map[string]any{"ownerId": bob.ID, "name": "Renamed"})
	assert.Equal(t, "Renamed", g["name"])
	assert.Equal(t, map[string]any{"id": float64(bob.ID), "username": "bob"}, g["owner"])
	assert.Greater(t, num(g, "version"), before)
	assert.Equal(t, map[string]string{"alice": "admin", "bob": "owner"}, roles(t, bc, gid), "old owner becomes admin")

	// The old owner is now just an admin: no more transfers or deletion.
	assert.Equal(t, 403, ac.do("PATCH", gpath(gid, ""), map[string]any{"ownerId": alice.ID}).status)
	assert.Equal(t, 403, ac.do("DELETE", gpath(gid, ""), nil).status)
	assert.Equal(t, 204, bc.do("DELETE", gpath(gid, ""), nil).status)
}

func TestInstanceAdminManagesAnyGroup(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob")
	bc := e.login("bob", "password-bob")
	adm := e.adminClient()

	gid := num(bc.mustOK("POST", "/groups", map[string]any{"name": "Bobs"}), "id")
	assert.Equal(t, gid, num(items(adm.mustOK("GET", "/groups", nil))[0].(map[string]any), "id"), "admin sees every group")
	assert.Equal(t, 200, adm.do("GET", gpath(gid, ""), nil).status, "without being a member")
	adm.mustOK("PUT", gpath(gid, fmt.Sprintf("/members/%d", e.admin.ID)), map[string]any{"role": "member"})
	adm.mustOK("PATCH", gpath(gid, ""), map[string]any{"name": "Renamed"})
	assert.Equal(t, 409, adm.do("DELETE", fmt.Sprintf("/admin/users/%d", bob.ID), nil).status)
	assert.Equal(t, 204, adm.do("DELETE", gpath(gid, ""), nil).status)
}

func TestGroupListsAndMyGroups(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("bob")
	ac := e.login("alice", "password-alice")
	bc := e.login("bob", "password-bob")
	for i := range 3 {
		ac.mustOK("POST", "/groups", map[string]any{"name": fmt.Sprintf("g%d", i)})
	}
	bc.mustOK("POST", "/groups", map[string]any{"name": "bobs"})

	first := ac.mustOK("GET", "/groups?limit=2", nil)
	assert.Len(t, items(first), 2)
	cursor, _ := first["nextCursor"].(string)
	require.NotEmpty(t, cursor)
	rest := ac.mustOK("GET", "/groups?limit=2&cursor="+cursor, nil)
	assert.Len(t, items(rest), 1)
	assert.Nil(t, rest["nextCursor"])

	assert.Len(t, items(bc.mustOK("GET", "/groups", nil)), 1)
	mine := items(ac.mustOK("GET", "/me/groups", nil))
	require.Len(t, mine, 3)
	assert.Equal(t, "owner", mine[0].(map[string]any)["role"])
	assert.Equal(t, "g0", mine[0].(map[string]any)["name"])
}

func TestUserLookup(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.user("alicia")
	e.user("bob")
	disabled := e.user("alison")
	e.adminClient().mustOK("PATCH", fmt.Sprintf("/admin/users/%d", disabled.ID), map[string]any{"disabled": true})
	c := e.login("bob", "password-bob")

	res := c.mustOK("GET", "/users/lookup?q=ali", nil)
	got := map[string]bool{}
	for _, it := range items(res) {
		got[it.(map[string]any)["username"].(string)] = true
	}
	assert.Equal(t, map[string]bool{"alice": true, "alicia": true}, got)
	item := items(res)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"id", "username", "displayName"}, keysOf(item), "no email or flags leak")

	r := c.do("GET", "/users/lookup?q=a", nil)
	assert.Equal(t, 422, r.status)
	assert.Contains(t, r.fields(), "q")
	// Email addresses are not searchable by regular users.
	assert.Empty(t, items(c.mustOK("GET", "/users/lookup?q=example.com", nil)))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
