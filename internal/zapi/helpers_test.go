package zapi_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

type user struct {
	repo.User
	Key string
}

func (e *testEnv) newUser(t testing.TB, name string, access repo.KeyAccess) user {
	t.Helper()
	u, err := e.Store.Users().Create(ctx(), repo.User{Username: name, DisplayName: name, Email: name + "@example.org", PasswordHash: testPasswordHash})
	require.NoError(t, err)
	key := zotero.NewAPIKey()
	_, err = e.Store.Keys().Create(ctx(), repo.APIKey{UserID: u.ID, Hash: auth.HashToken(key), Prefix: key[:4], Name: "t", Access: access})
	require.NoError(t, err)
	return user{User: u, Key: key}
}

var fullAccess = repo.KeyAccess{Library: true, Notes: true, Write: true, Files: true, Groups: "all"}

type resp struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r resp) json(t testing.TB) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.Body, &m), string(r.Body))
	return m
}

func (r resp) array(t testing.TB) []any {
	t.Helper()
	var a []any
	require.NoError(t, json.Unmarshal(r.Body, &a), string(r.Body))
	return a
}

func (r resp) version(t testing.TB) string {
	t.Helper()
	v := r.Header.Get("Last-Modified-Version")
	require.NotEmpty(t, v, "missing Last-Modified-Version on %d %s", r.Status, r.Body)
	return v
}

// req describes one HTTP call.
type req struct {
	method, path string
	key          string
	body         any
	raw          []byte
	gzip         bool
	header       map[string]string
}

func (e *testEnv) do(t testing.TB, r req) resp {
	t.Helper()
	var body io.Reader
	switch {
	case r.raw != nil:
		body = bytes.NewReader(r.raw)
	case r.body != nil:
		b, err := json.Marshal(r.body)
		require.NoError(t, err)
		body = bytes.NewReader(b)
	}
	if r.gzip && body != nil {
		raw, _ := io.ReadAll(body)
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		body = &buf
	}
	hr, err := http.NewRequestWithContext(ctx(), r.method, e.URL+"/api/"+strings.TrimLeft(r.path, "/"), body)
	require.NoError(t, err)
	hr.Header.Set("Zotero-API-Version", "3")
	if r.key != "" {
		hr.Header.Set("Zotero-API-Key", r.key)
	}
	if r.gzip {
		hr.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range r.header {
		hr.Header.Set(k, v)
	}
	res, err := e.client().Do(hr)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return resp{Status: res.StatusCode, Header: res.Header, Body: b}
}

// lib is a convenience wrapper for one library accessed with one key.
type lib struct {
	t    testing.TB
	e    *testEnv
	key  string
	path string
}

func (e *testEnv) userLib(t testing.TB, u user) *lib {
	return &lib{t: t, e: e, key: u.Key, path: fmt.Sprintf("users/%d", u.ID)}
}

func (e *testEnv) groupLib(t testing.TB, groupID int64, u user) *lib {
	return &lib{t: t, e: e, key: u.Key, path: fmt.Sprintf("groups/%d", groupID)}
}

func (l *lib) get(path string, h ...string) resp {
	return l.send("GET", path, nil, h...)
}

func (l *lib) send(method, path string, body any, h ...string) resp {
	l.t.Helper()
	hdr := map[string]string{}
	for i := 0; i+1 < len(h); i += 2 {
		hdr[h[i]] = h[i+1]
	}
	return l.e.do(l.t, req{method: method, path: l.path + "/" + path, key: l.key, body: body, header: hdr})
}

func (l *lib) post(path string, body any, h ...string) resp { return l.send("POST", path, body, h...) }

func (l *lib) version() int64 {
	l.t.Helper()
	r := l.get("items?limit=1&format=keys")
	require.Equal(l.t, 200, r.Status)
	var v int64
	_, err := fmt.Sscan(r.version(l.t), &v)
	require.NoError(l.t, err)
	return v
}

type m = map[string]any

// create writes one object and returns its key.
func (l *lib) create(typ string, o m) string {
	l.t.Helper()
	r := l.post(typ, []m{o})
	require.Equal(l.t, 200, r.Status, string(r.Body))
	succ, ok := r.json(l.t)["success"].(m)
	require.True(l.t, ok, string(r.Body))
	key, ok := succ["0"].(string)
	require.True(l.t, ok, "write failed: %s", r.Body)
	return key
}

func (l *lib) dataOf(typ, key string) m {
	l.t.Helper()
	r := l.get(typ + "/" + key)
	require.Equal(l.t, 200, r.Status, string(r.Body))
	d, ok := r.json(l.t)["data"].(m)
	require.True(l.t, ok)
	return d
}

func ver(h http.Header) string { return h.Get("Last-Modified-Version") }

func formatInt(n int64) string { return fmt.Sprintf("%d", n) }
