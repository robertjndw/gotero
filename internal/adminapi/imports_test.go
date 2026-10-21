package adminapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/importer"
)

func TestImportJobs(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice")
	e.user("bob")
	c := e.login("alice", "password-alice")
	bob := e.login("bob", "password-bob")

	assert.Empty(t, items(c.mustOK("GET", "/imports", nil)))

	res := c.do("POST", "/imports", map[string]any{"apiKey": "", "includeGroups": true, "includeFiles": true})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "apiKey")
	res = c.do("POST", "/imports", map[string]any{"apiKey": "bad key!"})
	assert.Equal(t, 422, res.status)
	assert.Contains(t, res.fields(), "apiKey")

	res = c.do("POST", "/imports", map[string]any{"apiKey": "AbCdEfGh1234567890abcdEF", "includeGroups": true, "includeFiles": false})
	require.Equal(t, 201, res.status, string(res.raw))
	job := res.json()
	assert.Equal(t, "queued", job["status"])
	assert.Equal(t, "zotero.org", job["source"])
	assert.Equal(t, []any{}, job["libraries"])
	assert.Nil(t, job["error"])
	assert.Nil(t, job["finishedAt"])
	assert.NotContains(t, string(res.raw), "AbCdEfGh", "the source key never leaves the server")
	id := num(job, "id")

	stored, err := e.store.Imports().Get(ctx(), id)
	require.NoError(t, err)
	assert.Equal(t, alice.ID, stored.UserID)
	assert.True(t, stored.IncludeGroups)
	assert.False(t, stored.IncludeFiles)
	assert.Equal(t, "AbCdEfGh1234567890abcdEF", stored.SourceAPIKey)

	assert.Len(t, items(c.mustOK("GET", "/imports", nil)), 1)
	assert.Equal(t, "queued", c.mustOK("GET", fmt.Sprintf("/imports/%d", id), nil)["status"])
	assert.Empty(t, items(bob.mustOK("GET", "/imports", nil)))
	assert.Equal(t, 404, bob.do("GET", fmt.Sprintf("/imports/%d", id), nil).status)
	assert.Equal(t, 404, bob.do("POST", fmt.Sprintf("/imports/%d/cancel", id), nil).status)

	cancelled := c.mustOK("POST", fmt.Sprintf("/imports/%d/cancel", id), nil)
	assert.Equal(t, "cancelled", cancelled["status"])
	assert.NotNil(t, cancelled["finishedAt"])
	stored, _ = e.store.Imports().Get(ctx(), id)
	assert.Empty(t, stored.SourceAPIKey, "key wiped on cancel")
	again := c.mustOK("POST", fmt.Sprintf("/imports/%d/cancel", id), nil)
	assert.Equal(t, "cancelled", again["status"], "idempotent")
	assert.Equal(t, 404, c.do("GET", "/imports/99999", nil).status)
}

func TestImportProgressIsReported(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice")
	c := e.login("alice", "password-alice")
	job, err := e.store.Imports().Create(ctx(), importJob(alice.ID, `{"objects":5,"objectsTotal":10,"files":1,"filesTotal":2,"bytes":2048,"libraries":[{"name":"My Library","items":10,"files":2,"done":false}]}`))
	require.NoError(t, err)
	got := c.mustOK("GET", fmt.Sprintf("/imports/%d", job.ID), nil)
	assert.Equal(t, map[string]any{"objects": float64(5), "objectsTotal": float64(10), "files": float64(1), "filesTotal": float64(2), "bytes": float64(2048)}, got["progress"])
	libs := got["libraries"].([]any)
	require.Len(t, libs, 1)
	assert.Equal(t, "My Library", libs[0].(map[string]any)["name"])
}

// fakeZoteroOrg answers keys/current like zotero.org does.
func fakeZoteroOrg(t *testing.T, library bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/keys/current" || r.Header.Get("Zotero-API-Key") != "GoodKeyGoodKeyGoodKey1234" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"key":"x","userID":1,"username":"u","displayName":"U","access":{"user":{"library":%t,"files":true},"groups":{}}}`, library)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestImportKeyIsCheckedAgainstZoteroOrg(t *testing.T) {
	zo := fakeZoteroOrg(t, true)
	e := newEnv(t, envOpts{validator: importer.New(importer.Deps{BaseURL: zo.URL + "/"})})
	e.user("alice")
	c := e.login("alice", "password-alice")

	res := c.do("POST", "/imports", map[string]any{"apiKey": "WrongKeyWrongKeyWrongKey1", "includeFiles": true})
	assert.Equal(t, 422, res.status, string(res.raw))
	assert.Contains(t, string(res.raw), "invalid_key")
	assert.Contains(t, res.fields(), "apiKey")
	assert.Empty(t, items(c.mustOK("GET", "/imports", nil)), "a rejected key queues nothing")

	res = c.do("POST", "/imports", map[string]any{"apiKey": "GoodKeyGoodKeyGoodKey1234", "includeFiles": true})
	require.Equal(t, 201, res.status, string(res.raw))
	assert.Equal(t, "queued", res.json()["status"])
	stored, err := e.store.Imports().Get(ctx(), num(res.json(), "id"))
	require.NoError(t, err)
	var progress map[string]any
	require.NoError(t, json.Unmarshal(stored.Progress, &progress))
	assert.Equal(t, true, progress["fileAccess"], "the job notes the key's file access")
}

func TestImportKeyNeedsLibraryAccess(t *testing.T) {
	zo := fakeZoteroOrg(t, false)
	e := newEnv(t, envOpts{validator: importer.New(importer.Deps{BaseURL: zo.URL + "/"})})
	e.user("alice")
	c := e.login("alice", "password-alice")
	res := c.do("POST", "/imports", map[string]any{"apiKey": "GoodKeyGoodKeyGoodKey1234"})
	assert.Equal(t, 422, res.status, string(res.raw))
	assert.Contains(t, res.fields(), "apiKey")
}

func TestImportKeyCheckNetworkErrorIs502(t *testing.T) {
	zo := fakeZoteroOrg(t, true)
	base := zo.URL + "/"
	zo.Close()
	e := newEnv(t, envOpts{validator: importer.New(importer.Deps{BaseURL: base})})
	e.user("alice")
	c := e.login("alice", "password-alice")
	res := c.do("POST", "/imports", map[string]any{"apiKey": "GoodKeyGoodKeyGoodKey1234"})
	assert.Equal(t, 502, res.status, string(res.raw))
	assert.Contains(t, string(res.raw), "upstream_error")
}
