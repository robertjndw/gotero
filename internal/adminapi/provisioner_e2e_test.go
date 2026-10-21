package adminapi_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/conformance"
	"github.com/robertjndw/gotero/conformance/gotero"
	"github.com/robertjndw/gotero/internal/auth"
)

// TestConformanceProvisioner drives the independent client from
// conformance/gotero against this implementation. If either side drifts
// from docs/dev/admin-api.md, this is where it shows.
func TestConformanceProvisioner(t *testing.T) {
	e := newEnv(t)
	p, err := gotero.New(gotero.Options{AdminURL: e.srv.URL + "/admin/api", AdminUser: "root", AdminPass: adminPass})
	require.NoError(t, err)
	c := t.Context()

	owner, err := p.NewUser(c)
	require.NoError(t, err)
	member, err := p.NewUser(c)
	require.NoError(t, err)
	admin, err := p.NewUser(c)
	require.NoError(t, err)
	late, err := p.NewUser(c)
	require.NoError(t, err)

	for _, u := range []conformance.User{owner, member, admin} {
		pr, err := e.auth.Authenticate(c, u.APIKey)
		require.NoError(t, err, "the provisioned key authenticates")
		assert.Equal(t, u.ID, pr.User.ID)
		assert.True(t, pr.Key.Access.Write && pr.Key.Access.Files && pr.Key.Access.Library && pr.Key.Access.Notes)
		assert.Equal(t, "all", pr.Key.Access.Groups)
	}

	g, err := p.NewGroup(c, owner, []conformance.User{member}, []conformance.User{admin})
	require.NoError(t, err)
	got, err := e.store.Groups().Members(c, g.ID)
	require.NoError(t, err)
	roles := map[int64]string{}
	for _, m := range got {
		roles[m.UserID] = m.Role
	}
	assert.Equal(t, map[int64]string{owner.ID: "owner", member.ID: "member", admin.ID: "admin"}, roles)

	// The instance admin may change membership of a group it does not belong to.
	require.NoError(t, p.AddMember(c, g, late, true))
	got, _ = e.store.Groups().Members(c, g.ID)
	assert.Len(t, got, 4)
	require.NoError(t, p.AddMember(c, g, late, false))
	got, _ = e.store.Groups().Members(c, g.ID)
	for _, m := range got {
		if m.UserID == late.ID {
			assert.Equal(t, "member", m.Role)
		}
	}

	require.NoError(t, p.SetQuota(c, owner, 2048))
	u, err := e.store.Users().Get(c, owner.ID)
	require.NoError(t, err)
	require.NotNil(t, u.QuotaBytes)
	assert.EqualValues(t, 2048, *u.QuotaBytes)

	// Authenticating first fills the cache, proving revoke evicts it.
	_, err = e.auth.Authenticate(c, member.APIKey)
	require.NoError(t, err)
	require.NoError(t, p.RevokeKey(c, member))
	_, err = e.auth.Authenticate(c, member.APIKey)
	assert.ErrorIs(t, err, auth.ErrInvalidKey)

	e.seedSession("e2e-token", nil, "Zotero", 10*time.Minute)
	require.NoError(t, p.ApproveSession(c, owner, "e2e-token"))
	s, err := e.store.LoginSessions().Get(c, auth.HashToken("e2e-token"))
	require.NoError(t, err)
	assert.Equal(t, "completed", s.Status)
	require.NotNil(t, s.UserID)
	assert.Equal(t, owner.ID, *s.UserID)
	pr, err := e.auth.Authenticate(c, s.APIKey)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, pr.User.ID)

	// Approving for the wrong account is rejected through the same client.
	e.seedSession("e2e-other", &owner.ID, "Zotero", 10*time.Minute)
	assert.Error(t, p.ApproveSession(c, admin, "e2e-other"))
}
