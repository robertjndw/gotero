package adminapi_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
)

// seedSession plays the part of POST /keys/sessions, which lives in the
// Zotero API.
func (e *env) seedSession(token string, required *int64, client string, expires time.Duration) {
	e.t.Helper()
	require.NoError(e.t, e.store.LoginSessions().Create(ctx(), repo.LoginSession{
		TokenHash: auth.HashToken(token), RequiredUserID: required, ClientName: client,
		ExpiresAt: time.Now().Add(expires),
	}))
}

func TestLoginSessionApprove(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice")
	e.seedSession("tok-1", nil, "Zotero on laptop", 10*time.Minute)
	c := e.login("alice", "password-alice")

	got := c.mustOK("GET", "/login-sessions/tok-1", nil)
	assert.Equal(t, "pending", got["status"])
	assert.Equal(t, "Zotero on laptop", got["clientName"])
	assert.Nil(t, got["requiredUserId"])
	assert.Equal(t, "tok-1", got["token"])

	res := c.do("POST", "/login-sessions/tok-1/approve", nil)
	require.Equal(t, 200, res.status, string(res.raw))
	assert.Equal(t, "completed", res.json()["status"])
	assert.Equal(t, []string{"tok-1"}, e.notify.tokens)

	s, err := e.store.LoginSessions().Get(ctx(), auth.HashToken("tok-1"))
	require.NoError(t, err)
	assert.Equal(t, "completed", s.Status)
	require.NotNil(t, s.UserID)
	assert.Equal(t, alice.ID, *s.UserID)
	require.Len(t, s.APIKey, 24)

	p, err := e.auth.Authenticate(ctx(), s.APIKey)
	require.NoError(t, err, "the delivered key is real")
	assert.Equal(t, alice.ID, p.User.ID)
	assert.Equal(t, "Zotero on laptop", p.Key.Name)
	assert.Equal(t, repo.KeyAccess{Library: true, Notes: true, Write: true, Files: true, Groups: "all"}, p.Key.Access)

	again := c.do("POST", "/login-sessions/tok-1/approve", nil)
	assert.Equal(t, 409, again.status)
	assert.Equal(t, "completed", c.mustOK("GET", "/login-sessions/tok-1", nil)["status"])
	keys := c.mustOK("GET", "/me/keys", nil)
	assert.Len(t, items(keys), 1, "no extra key from the failed second approval")
}

func TestLoginSessionDefaultKeyName(t *testing.T) {
	e := newEnv(t)
	u := e.user("alice")
	e.seedSession("tok", nil, "", time.Minute)
	e.login("alice", "password-alice").mustOK("POST", "/login-sessions/tok/approve", nil)
	keys, err := e.store.Keys().ListByUser(ctx(), u.ID)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, "Zotero", keys[0].Name)
}

func TestLoginSessionMismatch(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice")
	e.user("bob")
	e.seedSession("tok", &alice.ID, "Zotero", time.Minute)

	bob := e.login("bob", "password-bob")
	res := bob.do("POST", "/login-sessions/tok/approve", nil)
	assert.Equal(t, 403, res.status)
	assert.Equal(t, "user_mismatch", res.errCode())
	assert.Equal(t, "pending", bob.mustOK("GET", "/login-sessions/tok", nil)["status"])
	assert.Equal(t, float64(alice.ID), bob.mustOK("GET", "/login-sessions/tok", nil)["requiredUserId"])
	assert.Empty(t, e.notify.tokens)

	assert.Equal(t, 200, e.login("alice", "password-alice").do("POST", "/login-sessions/tok/approve", nil).status)
}

func TestLoginSessionExpiredCancelledUnknown(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	c := e.login("alice", "password-alice")

	e.seedSession("old", nil, "Zotero", -time.Minute)
	assert.Equal(t, "expired", c.mustOK("GET", "/login-sessions/old", nil)["status"])
	res := c.do("POST", "/login-sessions/old/approve", nil)
	assert.Equal(t, 410, res.status)
	assert.Equal(t, "expired", res.errCode())
	keys := c.mustOK("GET", "/me/keys", nil)
	assert.Empty(t, items(keys))

	e.seedSession("dead", nil, "Zotero", time.Minute)
	cancelled := c.mustOK("POST", "/login-sessions/dead/cancel", nil)
	assert.Equal(t, "cancelled", cancelled["status"])
	res = c.do("POST", "/login-sessions/dead/approve", nil)
	assert.Equal(t, 409, res.status)

	assert.Equal(t, 404, c.do("GET", "/login-sessions/nope", nil).status)
	assert.Equal(t, 404, c.do("POST", "/login-sessions/nope/approve", nil).status)
	assert.Equal(t, 404, c.do("POST", "/login-sessions/nope/cancel", nil).status)

	e.seedSession("later", nil, "Zotero", time.Hour)
	e.clock.Advance(2 * time.Hour)
	assert.Equal(t, 410, c.do("POST", "/login-sessions/later/approve", nil).status, "the server's clock decides")
}

func TestLoginSessionRequiresLoginAndCSRF(t *testing.T) {
	e := newEnv(t)
	e.user("alice")
	e.seedSession("tok", nil, "Zotero", time.Minute)
	assert.Equal(t, 401, e.client().do("POST", "/login-sessions/tok/approve", nil).status)
	c := e.login("alice", "password-alice")
	assert.Equal(t, 403, c.do("POST", "/login-sessions/tok/approve", nil, noCSRF()).status)
	s, _ := e.store.LoginSessions().Get(ctx(), auth.HashToken("tok"))
	assert.Equal(t, "pending", s.Status)
}
