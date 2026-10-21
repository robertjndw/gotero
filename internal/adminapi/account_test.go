package adminapi_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupIsPublic(t *testing.T) {
	e := newEnv(t)
	res := e.client().do("GET", "/setup", nil)
	require.Equal(t, 200, res.status)
	m := res.json()
	assert.Equal(t, false, m["openRegistration"])
	assert.Equal(t, e.srv.URL, m["publicUrl"])
	assert.Equal(t, "test", m["version"])
}

func TestPatchMe(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice", "password-alice")
	m := c.mustOK("PATCH", "/me", map[string]any{"displayName": "Alice A", "email": "alice@example.com"})
	assert.Equal(t, "Alice A", m["displayName"])
	assert.Equal(t, "alice@example.com", m["email"])
	assert.NotEmpty(t, m["csrfToken"])

	res := c.do("PATCH", "/me", map[string]any{"email": "not-an-email"})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "email")

	res = c.do("PATCH", "/me", map[string]any{"email": "root@example.com"})
	assert.Equal(t, 409, res.status, "email already used by root")
	assert.Contains(t, res.fields(), "email")
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	u := e.user("alice")
	c := e.login("alice", "password-alice")

	res := c.do("POST", "/me/password", map[string]string{"currentPassword": "wrong", "newPassword": "a-new-password"})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "currentPassword")
	res = c.do("POST", "/me/password", map[string]string{"currentPassword": "password-alice", "newPassword": "short"})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "newPassword")

	// A cached API key must keep working: passwords and keys are independent.
	_, raw := mustKey(t, e, u.ID)
	_, err := e.auth.Authenticate(ctx(), raw)
	require.NoError(t, err)

	res = c.do("POST", "/me/password", map[string]string{"currentPassword": "password-alice", "newPassword": "a-new-password"})
	require.Equal(t, 204, res.status, string(res.raw))
	assert.Equal(t, 200, c.do("GET", "/me", nil).status, "caller keeps a working session")

	assert.Equal(t, 401, e.client().do("POST", "/login", map[string]string{"username": "alice", "password": "password-alice"}).status)
	assert.Equal(t, 200, e.client().do("POST", "/login", map[string]string{"username": "alice", "password": "a-new-password"}).status)
}

func TestOtherSessionsDieOnPasswordChange(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	one := e.login("alice", "password-alice")
	two := e.login("alice", "password-alice")
	require.Equal(t, 204, one.do("POST", "/me/password", map[string]string{"currentPassword": "password-alice", "newPassword": "a-new-password"}).status)
	assert.Equal(t, 401, two.do("GET", "/me", nil).status)
}

func TestAPIKeys(t *testing.T) {
	e := newEnv(t)
	a := e.user("alice")
	e.user("bob")
	c := e.login("alice", "password-alice")
	bob := e.login("bob", "password-bob")

	access := map[string]any{"library": true, "notes": true, "write": false, "files": true, "groups": "read"}
	res := c.do("POST", "/me/keys", map[string]any{"name": "laptop", "access": access})
	require.Equal(t, 201, res.status, string(res.raw))
	k := res.json()
	raw := k["key"].(string)
	assert.Len(t, raw, 24)
	assert.Equal(t, raw[:4], k["prefix"])
	assert.Equal(t, "laptop", k["name"])
	assert.Equal(t, access, k["access"])
	assert.Nil(t, k["lastUsedAt"])

	list := c.mustOK("GET", "/me/keys", nil)
	require.Len(t, items(list), 1)
	assert.NotContains(t, fmt.Sprint(list), raw, "the full key is never listed")

	// Authenticate once so the key is cached, then revoke: it must stop
	// working immediately, not after the cache TTL.
	p, err := e.auth.Authenticate(ctx(), raw)
	require.NoError(t, err)
	assert.Equal(t, a.ID, p.User.ID)

	id := num(k, "id")
	assert.Equal(t, 404, bob.do("DELETE", fmt.Sprintf("/me/keys/%d", id), nil).status, "not bob's key")
	assert.Equal(t, 204, c.do("DELETE", fmt.Sprintf("/me/keys/%d", id), nil).status)
	_, err = e.auth.Authenticate(ctx(), raw)
	assert.Error(t, err)
	assert.Equal(t, 404, c.do("DELETE", fmt.Sprintf("/me/keys/%d", id), nil).status)

	res = c.do("POST", "/me/keys", map[string]any{"name": "", "access": access})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "name")
	res = c.do("POST", "/me/keys", map[string]any{"name": "x", "access": map[string]any{"groups": "bogus"}})
	assert.Equal(t, 422, res.status)
}

func TestAPIEndpointsRequireLogin(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	for _, p := range []string{
		"GET /me", "GET /me/keys", "GET /me/groups", "GET /groups", "GET /imports",
		"GET /users/lookup?q=ab", "GET /login-sessions/x", "GET /admin/stats", "GET /admin/users", "GET /admin/settings",
	} {
		method, path := p[:3], p[4:]
		assert.Equal(t, 401, c.do(method, path, nil).status, p)
	}
}
