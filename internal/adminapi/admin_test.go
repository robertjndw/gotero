package adminapi_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/repo"
)

func registerBody(name string, extra map[string]any) map[string]any {
	m := map[string]any{"username": name, "password": "password-" + name, "email": name + "@example.com", "displayName": name}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func inviteToken(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	tok := u.Query().Get("invite")
	require.NotEmpty(t, tok)
	return tok
}

func TestRegistrationClosedAndOpen(t *testing.T) {
	e := newEnv(t)
	anon := e.client()

	res := anon.do("POST", "/register", registerBody("newbie", nil))
	assert.Equal(t, 403, res.status)
	assert.Equal(t, "registration_closed", res.errCode())

	adm := e.adminClient()
	s := adm.mustOK("PATCH", "/admin/settings", map[string]any{"openRegistration": true})
	assert.Equal(t, true, s["openRegistration"])
	assert.Equal(t, true, anon.mustOK("GET", "/setup", nil)["openRegistration"])

	res = anon.do("POST", "/register", registerBody("newbie", nil))
	require.Equal(t, 201, res.status, string(res.raw))
	assert.Equal(t, "newbie", res.json()["username"])
	assert.Equal(t, false, res.json()["isAdmin"])
	assert.Equal(t, 200, anon.do("GET", "/me", nil).status, "registering logs in")

	dup := e.client().do("POST", "/register", registerBody("NEWBIE", nil))
	assert.Equal(t, 409, dup.status)
	assert.Contains(t, dup.fields(), "username")

	bad := e.client().do("POST", "/register", map[string]any{"username": "x", "password": "short", "email": "nope"})
	assert.Equal(t, 422, bad.status)
	for _, f := range []string{"username", "password", "email"} {
		assert.Contains(t, bad.fields(), f)
	}

	// Registration cannot be used to escalate: the flags in the body are ignored.
	res = e.client().do("POST", "/register", registerBody("sneaky", map[string]any{"isAdmin": true, "quotaBytes": nil}))
	require.Equal(t, 201, res.status)
	assert.Equal(t, false, res.json()["isAdmin"])

	adm.mustOK("PATCH", "/admin/settings", map[string]any{"openRegistration": false})
	assert.Equal(t, 403, e.client().do("POST", "/register", registerBody("late", nil)).status)
}

func TestOpenRegistrationDefaultsFromConfig(t *testing.T) {
	e := newEnv(t)
	e.cfg.OpenRegistration = true
	assert.Equal(t, true, e.client().mustOK("GET", "/setup", nil)["openRegistration"])
	e.adminClient().mustOK("PATCH", "/admin/settings", map[string]any{"openRegistration": false})
	assert.Equal(t, false, e.client().mustOK("GET", "/setup", nil)["openRegistration"], "stored setting wins")
}

func TestInviteFlow(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()
	anon := e.client()

	res := adm.do("POST", "/admin/invites", map[string]any{"isAdmin": false, "expiresInHours": 24})
	require.Equal(t, 201, res.status, string(res.raw))
	inv := res.json()
	assert.Equal(t, "root", inv["createdBy"])
	assert.Nil(t, inv["usedBy"])
	assert.Nil(t, inv["email"])
	require.True(t, strings.HasPrefix(inv["url"].(string), e.srv.URL+"/register?invite="), inv["url"])
	token := inviteToken(t, inv["url"].(string))
	exp, err := time.Parse(time.RFC3339, inv["expiresAt"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), exp, time.Minute)

	// The listing reproduces the same link, even though only a hash is stored.
	list := adm.mustOK("GET", "/admin/invites", nil)
	require.Len(t, items(list), 1)
	assert.Equal(t, inv["url"], items(list)[0].(map[string]any)["url"])

	look := anon.mustOK("GET", "/invites/"+token, nil)
	assert.Equal(t, true, look["valid"])
	assert.Nil(t, look["email"])
	assert.Equal(t, false, anon.mustOK("GET", "/invites/nonsense", nil)["valid"])

	res = anon.do("POST", "/register", registerBody("invited", map[string]any{"inviteToken": token}))
	require.Equal(t, 201, res.status, string(res.raw))
	assert.Equal(t, false, res.json()["isAdmin"])

	assert.Equal(t, false, anon.mustOK("GET", "/invites/"+token, nil)["valid"], "used")
	reuse := e.client().do("POST", "/register", registerBody("second", map[string]any{"inviteToken": token}))
	assert.Equal(t, 403, reuse.status)
	assert.Equal(t, "invalid_invite", reuse.errCode())
	_, err = e.store.Users().GetByUsername(ctx(), "second")
	assert.ErrorIs(t, err, repo.ErrNotFound)

	list = adm.mustOK("GET", "/admin/invites", nil)
	assert.Equal(t, "invited", items(list)[0].(map[string]any)["usedBy"])
}

func TestInviteFailureKeepsInviteUsable(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()
	inv := adm.mustOK("POST", "/admin/invites", map[string]any{"expiresInHours": 1})
	token := inviteToken(t, inv["url"].(string))

	// Username clash: the whole signup rolls back, the invite is not burned.
	res := e.client().do("POST", "/register", registerBody("root", map[string]any{"inviteToken": token}))
	assert.Equal(t, 409, res.status)
	assert.Equal(t, true, e.client().mustOK("GET", "/invites/"+token, nil)["valid"])
	assert.Equal(t, 201, e.client().do("POST", "/register", registerBody("fresh", map[string]any{"inviteToken": token})).status)
}

func TestInviteAdminEmailAndExpiry(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()

	inv := adm.mustOK("POST", "/admin/invites", map[string]any{"email": "boss@example.com", "isAdmin": true, "expiresInHours": 2})
	assert.Equal(t, "boss@example.com", inv["email"])
	token := inviteToken(t, inv["url"].(string))
	assert.Equal(t, "boss@example.com", e.client().mustOK("GET", "/invites/"+token, nil)["email"])

	res := e.client().do("POST", "/register", registerBody("boss", map[string]any{"inviteToken": token, "email": "other@example.com"}))
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "email")

	res = e.client().do("POST", "/register", registerBody("boss", map[string]any{"inviteToken": token, "email": "BOSS@example.com"}))
	require.Equal(t, 201, res.status, string(res.raw))
	assert.Equal(t, true, res.json()["isAdmin"], "the invite grants admin")

	exp := adm.mustOK("POST", "/admin/invites", map[string]any{"expiresInHours": 1})
	expTok := inviteToken(t, exp["url"].(string))
	e.clock.Advance(2 * time.Hour)
	assert.Equal(t, false, e.client().mustOK("GET", "/invites/"+expTok, nil)["valid"])
	res = e.client().do("POST", "/register", registerBody("toolate", map[string]any{"inviteToken": expTok}))
	assert.Equal(t, 403, res.status)
}

func TestInviteValidationAndRevoke(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()
	res := adm.do("POST", "/admin/invites", map[string]any{"email": "bad", "expiresInHours": 1})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "email")
	res = adm.do("POST", "/admin/invites", map[string]any{"expiresInHours": 99999})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "expiresInHours")

	inv := adm.mustOK("POST", "/admin/invites", map[string]any{"expiresInHours": 1})
	token := inviteToken(t, inv["url"].(string))
	assert.Equal(t, 204, adm.do("DELETE", fmt.Sprintf("/admin/invites/%d", num(inv, "id")), nil).status)
	assert.Equal(t, false, e.client().mustOK("GET", "/invites/"+token, nil)["valid"])
	assert.Equal(t, 404, adm.do("DELETE", fmt.Sprintf("/admin/invites/%d", num(inv, "id")), nil).status)
}

func TestAdminEndpointsForbiddenForUsers(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice", "password-alice")
	for _, p := range []string{
		"GET /admin/stats", "GET /admin/users", "POST /admin/users", "PATCH /admin/users/1",
		"DELETE /admin/users/1", "GET /admin/invites", "POST /admin/invites", "DELETE /admin/invites/1",
		"GET /admin/settings", "PATCH /admin/settings", "POST /admin/gc",
	} {
		i := strings.IndexByte(p, ' ')
		res := c.do(p[:i], p[i+1:], map[string]any{})
		assert.Equal(t, 403, res.status, p)
		assert.Equal(t, "forbidden", res.errCode(), p)
	}
}

func TestAdminUsersCRUD(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()

	res := adm.do("POST", "/admin/users", map[string]any{
		"username": "carol", "password": "carol-password", "email": "carol@example.com", "displayName": "Carol",
		"isAdmin": false, "quotaBytes": 1000,
	})
	require.Equal(t, 201, res.status, string(res.raw))
	carol := res.json()
	assert.Equal(t, float64(1000), carol["quotaBytes"])
	assert.Equal(t, float64(0), carol["usedBytes"])
	assert.Equal(t, "Carol", carol["displayName"])
	id := num(carol, "id")

	dup := adm.do("POST", "/admin/users", map[string]any{"username": "CAROL", "password": "carol-password"})
	assert.Equal(t, 409, dup.status)
	assert.Contains(t, dup.fields(), "username")
	dupEmail := adm.do("POST", "/admin/users", map[string]any{"username": "carol2", "password": "carol-password", "email": "CAROL@example.com"})
	assert.Equal(t, 409, dupEmail.status)
	assert.Contains(t, dupEmail.fields(), "email")

	bad := adm.do("POST", "/admin/users", map[string]any{"username": "a b", "password": "short", "email": "x", "quotaBytes": -5})
	assert.Equal(t, 422, bad.status)
	for _, f := range []string{"username", "password", "email", "quotaBytes"} {
		assert.Contains(t, bad.fields(), f)
	}

	// Explicit null means unlimited; absent means the instance default.
	adm.mustOK("PATCH", "/admin/settings", map[string]any{"defaultQuotaBytes": 5000})
	unl := adm.mustOK("POST", "/admin/users", map[string]any{"username": "unl", "password": "unlimited-pass", "quotaBytes": nil})
	assert.Nil(t, unl["quotaBytes"])
	def := adm.mustOK("POST", "/admin/users", map[string]any{"username": "def", "password": "default-pass1"})
	assert.Equal(t, float64(5000), def["quotaBytes"])

	got := adm.mustOK("PATCH", fmt.Sprintf("/admin/users/%d", id), map[string]any{"displayName": "C2", "isAdmin": true, "quotaBytes": nil})
	assert.Equal(t, "C2", got["displayName"])
	assert.Equal(t, true, got["isAdmin"])
	assert.Nil(t, got["quotaBytes"])
	assert.Equal(t, "carol@example.com", got["email"], "untouched fields stay")
	got = adm.mustOK("PATCH", fmt.Sprintf("/admin/users/%d", id), map[string]any{"quotaBytes": 2048, "isAdmin": false})
	assert.Equal(t, float64(2048), got["quotaBytes"])

	assert.Equal(t, 404, adm.do("PATCH", "/admin/users/9999", map[string]any{"displayName": "x"}).status)
	assert.Equal(t, 404, adm.do("PATCH", "/admin/users/abc", map[string]any{"displayName": "x"}).status)
	res = adm.do("PATCH", fmt.Sprintf("/admin/users/%d", id), map[string]any{"password": "tiny"})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "password")

	// An admin-set password takes effect and ends the user's sessions.
	carolSess := e.login("carol", "carol-password")
	adm.mustOK("PATCH", fmt.Sprintf("/admin/users/%d", id), map[string]any{"password": "brand-new-password"})
	assert.Equal(t, 401, carolSess.do("GET", "/me", nil).status)
	e.login("carol", "brand-new-password")

	list := adm.mustOK("GET", "/admin/users?q=car", nil)
	assert.Len(t, items(list), 1)
	assert.Nil(t, list["nextCursor"])

	assert.Equal(t, 204, adm.do("DELETE", fmt.Sprintf("/admin/users/%d", id), nil).status)
	assert.Equal(t, 404, adm.do("DELETE", fmt.Sprintf("/admin/users/%d", id), nil).status)
	assert.Equal(t, 401, e.client().do("POST", "/login", map[string]string{"username": "carol", "password": "brand-new-password"}).status)
}

func TestAdminUsersPagination(t *testing.T) {
	e := newEnv(t)
	for i := range 4 {
		e.user(fmt.Sprintf("user%d", i))
	}
	adm := e.adminClient()
	first := adm.mustOK("GET", "/admin/users?limit=2", nil)
	require.Len(t, items(first), 2)
	cursor, _ := first["nextCursor"].(string)
	require.NotEmpty(t, cursor)
	second := adm.mustOK("GET", "/admin/users?limit=2&cursor="+cursor, nil)
	require.Len(t, items(second), 2)
	third := adm.mustOK("GET", "/admin/users?limit=2&cursor="+second["nextCursor"].(string), nil)
	assert.Len(t, items(third), 1)
	assert.Nil(t, third["nextCursor"])
	assert.Equal(t, 422, adm.do("GET", "/admin/users?limit=0", nil).status)
}

func TestAdminSelfProtection(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()
	path := fmt.Sprintf("/admin/users/%d", e.admin.ID)

	for name, body := range map[string]map[string]any{"demote": {"isAdmin": false}, "disable": {"disabled": true}} {
		res := adm.do("PATCH", path, body)
		assert.Equal(t, 409, res.status, name)
		assert.Equal(t, "self_modification", res.errCode(), name)
	}
	res := adm.do("DELETE", path, nil)
	assert.Equal(t, 409, res.status)
	assert.Equal(t, "self_modification", res.errCode())

	// Harmless edits to yourself are fine.
	adm.mustOK("PATCH", path, map[string]any{"displayName": "Root", "isAdmin": true})
}

func TestLastAdminProtection(t *testing.T) {
	e := newEnv(t)
	// Over HTTP an admin can never be the last one standing while acting, so
	// the guard is exercised through the service, as the CLI would.
	other, err := e.store.Users().Create(ctx(), repo.User{Username: "other", PasswordHash: "x", IsAdmin: true})
	require.NoError(t, err)
	svc := e.acc
	no := false
	require.NoError(t, svc.DeleteUser(ctx(), e.admin.ID, 0), "two active admins: removing one is fine")
	err = svc.DeleteUser(ctx(), other.ID, 0)
	assert.ErrorIs(t, err, accounts.ErrLastAdmin)
	_, err = svc.UpdateUser(ctx(), other.ID, accounts.UserPatch{IsAdmin: &no}, 0)
	assert.ErrorIs(t, err, accounts.ErrLastAdmin)
}

func TestDeleteUserOwningGroups(t *testing.T) {
	e := newEnv(t)
	owner := e.user("owner")
	oc := e.login("owner", "password-owner")
	oc.mustOK("POST", "/groups", map[string]any{"name": "g"})
	adm := e.adminClient()
	res := adm.do("DELETE", fmt.Sprintf("/admin/users/%d", owner.ID), nil)
	assert.Equal(t, 409, res.status)
	assert.Equal(t, "owns_groups", res.errCode())
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()
	s := adm.mustOK("GET", "/admin/settings", nil)
	assert.Equal(t, false, s["openRegistration"])
	assert.Nil(t, s["defaultQuotaBytes"])

	s = adm.mustOK("PATCH", "/admin/settings", map[string]any{"defaultQuotaBytes": 123})
	assert.Equal(t, float64(123), s["defaultQuotaBytes"])
	assert.Equal(t, false, s["openRegistration"], "partial update leaves the rest")
	s = adm.mustOK("PATCH", "/admin/settings", map[string]any{"defaultQuotaBytes": nil})
	assert.Nil(t, s["defaultQuotaBytes"], "null clears the quota")
	res := adm.do("PATCH", "/admin/settings", map[string]any{"defaultQuotaBytes": -1})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "defaultQuotaBytes")
}

func TestSettingsDefaultQuotaFromConfig(t *testing.T) {
	e := newEnv(t)
	e.cfg.DefaultQuotaMB = 2
	s := e.adminClient().mustOK("GET", "/admin/settings", nil)
	assert.Equal(t, float64(2<<20), s["defaultQuotaBytes"])
}

func TestStatsAndGC(t *testing.T) {
	e := newEnv(t)
	adm := e.adminClient()
	e.user("alice")
	st := adm.mustOK("GET", "/admin/stats", nil)
	assert.Equal(t, float64(2), st["users"])
	assert.Equal(t, float64(0), st["groups"])
	assert.Equal(t, "test", st["version"])
	storage := st["storage"].(map[string]any)
	assert.Equal(t, "local", storage["backend"])
	assert.Contains(t, storage, "blobCount")

	res := adm.mustOK("POST", "/admin/gc", map[string]any{"dryRun": true})
	assert.Equal(t, map[string]any{"orphanBlobs": float64(3), "orphanBytes": float64(99), "deleted": false}, res)
	res = adm.mustOK("POST", "/admin/gc", map[string]any{"dryRun": false})
	assert.Equal(t, true, res["deleted"])
	assert.Equal(t, []bool{true, false}, e.gc.dry)
}
