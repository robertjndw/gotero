package zapi_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

func (e *testEnv) newSession(t testing.TB, body any, h ...string) (string, resp) {
	t.Helper()
	hdr := map[string]string{}
	for i := 0; i+1 < len(h); i += 2 {
		hdr[h[i]] = h[i+1]
	}
	r := e.do(t, req{method: "POST", path: "keys/sessions", body: body, header: hdr})
	if r.Status != 201 {
		return "", r
	}
	return r.json(t)["sessionToken"].(string), r
}

func TestLoginSessionLifecycle(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	tok, r := e.newSession(t, nil, "User-Agent", "Zotero/7.0 (Macintosh)")
	require.NotEmpty(t, tok, string(r.Body))
	assert.Len(t, tok, 64)
	assert.Equal(t, e.URL+"/login?session="+tok, r.json(t)["loginURL"])

	s, err := e.Store.LoginSessions().Get(ctx(), auth.HashToken(tok))
	require.NoError(t, err)
	assert.Equal(t, "Zotero", s.ClientName)
	assert.Nil(t, s.RequiredUserID)
	assert.WithinDuration(t, s.CreatedAt.Add(30*60e9), s.ExpiresAt, 5e9)

	poll := func() map[string]any {
		r := e.do(t, req{method: "GET", path: "keys/sessions/" + tok})
		require.Equal(t, 200, r.Status, string(r.Body))
		return r.json(t)
	}
	assert.Equal(t, map[string]any{"status": "pending"}, poll())

	raw := zotero.NewAPIKey()
	_, err = e.Store.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: auth.HashToken(raw), Prefix: raw[:4], Name: "x", Access: fullAccess})
	require.NoError(t, err)
	require.NoError(t, e.Store.LoginSessions().Complete(ctx(), auth.HashToken(tok), u.ID, raw))

	first := poll()
	assert.Equal(t, "completed", first["status"])
	assert.Equal(t, raw, first["apiKey"])
	assert.EqualValues(t, u.ID, first["userID"])
	assert.Equal(t, "alice", first["username"])
	assert.Equal(t, "alice", first["displayName"])

	again := poll()
	assert.Equal(t, "completed", again["status"])
	assert.NotContains(t, again, "apiKey", "the key is handed out once")
	s, err = e.Store.LoginSessions().Get(ctx(), auth.HashToken(tok))
	require.NoError(t, err)
	assert.Empty(t, s.APIKey)
}

func TestLoginSessionKeyGoesToOnePoller(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	tok, _ := e.newSession(t, nil)
	require.NoError(t, e.Store.LoginSessions().Complete(ctx(), auth.HashToken(tok), u.ID, "SECRETKEY"))

	const n = 8
	got := make(chan bool, n)
	for range n {
		go func() {
			r := e.do(t, req{method: "GET", path: "keys/sessions/" + tok})
			_, has := r.json(t)["apiKey"]
			got <- has
		}()
	}
	var winners int
	for range n {
		if <-got {
			winners++
		}
	}
	assert.Equal(t, 1, winners)
}

func TestLoginSessionBoundUserAndCancel(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)

	tok, _ := e.newSession(t, m{"userID": u.ID})
	s, err := e.Store.LoginSessions().Get(ctx(), auth.HashToken(tok))
	require.NoError(t, err)
	require.NotNil(t, s.RequiredUserID)
	assert.Equal(t, u.ID, *s.RequiredUserID)

	tok2, _ := e.newSession(t, m{"userID": 999999})
	s, _ = e.Store.LoginSessions().Get(ctx(), auth.HashToken(tok2))
	assert.Nil(t, s.RequiredUserID, "unknown users are ignored")

	_, r := e.newSession(t, "not an object")
	assert.Equal(t, 400, r.Status)

	assert.Equal(t, 204, e.do(t, req{method: "DELETE", path: "keys/sessions/" + tok}).Status)
	r = e.do(t, req{method: "GET", path: "keys/sessions/" + tok})
	assert.Equal(t, map[string]any{"status": "cancelled"}, r.json(t))
	assert.Equal(t, 204, e.do(t, req{method: "DELETE", path: "keys/sessions/" + tok}).Status, "cancelling twice is fine")
	assert.Equal(t, 404, e.do(t, req{method: "DELETE", path: "keys/sessions/nope"}).Status)
}

func TestLoginSessionExpiry(t *testing.T) {
	e := newEnv(t)
	tok, _ := e.newSession(t, nil)
	_, err := e.Pool.Exec(ctx(), `UPDATE login_sessions SET expires_at = now() - interval '1 second'`)
	require.NoError(t, err)
	assert.Equal(t, 410, e.do(t, req{method: "GET", path: "keys/sessions/" + tok}).Status)
	assert.Equal(t, 404, e.do(t, req{method: "GET", path: "keys/sessions/unknown"}).Status)
}

func TestLoginSessionClientName(t *testing.T) {
	e := newEnv(t)
	for ua, want := range map[string]string{
		"":                       "Zotero",
		"Zotero/7.0":             "Zotero",
		"Zotero Android 1.2":     "Zotero",
		"curl/8.1":               "curl",
		"/weird":                 "Zotero",
		strings.Repeat("a", 200): strings.Repeat("a", 60),
	} {
		tok, r := e.newSession(t, nil, "User-Agent", ua)
		require.NotEmpty(t, tok, string(r.Body))
		s, err := e.Store.LoginSessions().Get(ctx(), auth.HashToken(tok))
		require.NoError(t, err)
		assert.Equal(t, want, s.ClientName, ua)
	}
}

func TestLoginSessionRateLimit(t *testing.T) {
	e := newEnv(t)
	for i := range 30 {
		tok, r := e.newSession(t, nil)
		require.NotEmpty(t, tok, "request %d: %s", i, r.Body)
	}
	_, r := e.newSession(t, nil)
	require.Equal(t, 429, r.Status)
	ra := r.Header.Get("Retry-After")
	assert.Regexp(t, `^\d+$`, ra)
	assert.NotEqual(t, "0", ra)
}
