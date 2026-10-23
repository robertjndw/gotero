package zapi_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zotero"
)

func TestMiddleware(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	t.Run("only API version 3", func(t *testing.T) {
		for _, v := range []string{"2", "4", "abc"} {
			r := e.do(t, req{method: "GET", path: l.path + "/items", key: u.Key, header: map[string]string{"Zotero-API-Version": v}})
			assert.Equal(t, 400, r.Status, v)
		}
		r := e.do(t, req{method: "GET", path: l.path + "/items", key: u.Key, header: map[string]string{"Zotero-API-Version": "3"}})
		assert.Equal(t, 200, r.Status)
	})
	t.Run("missing API version means 3", func(t *testing.T) {
		hr := e.do(t, req{method: "GET", path: "keys/current", key: u.Key, header: map[string]string{"Zotero-API-Version": ""}})
		assert.Equal(t, 200, hr.Status)
	})
	t.Run("forbidden is plain text", func(t *testing.T) {
		r := e.do(t, req{method: "GET", path: l.path + "/items"})
		assert.Equal(t, 403, r.Status)
		assert.Equal(t, "Forbidden", string(r.Body))
		assert.Contains(t, r.Header.Get("Content-Type"), "text/plain")
		r = e.do(t, req{method: "GET", path: l.path + "/items", key: "nonexistentnonexistent24"})
		assert.Equal(t, 403, r.Status)
	})
	t.Run("version header on every library response", func(t *testing.T) {
		l.create("items", m{"itemType": "book"})
		cur := strconv.FormatInt(l.version(), 10)
		for _, c := range []struct {
			method, path string
			want         int
			h            []string
		}{
			{"GET", "items", 200, nil},
			{"GET", "items/ZZZZZZZ2", 404, nil},
			{"GET", "items?format=bogus", 400, nil},
			{"GET", "items", 304, []string{"If-Modified-Since-Version", cur}},
			{"DELETE", "items?itemKey=ZZZZZZZ2", 428, nil},
			{"DELETE", "items?itemKey=ZZZZZZZ2", 204, []string{"If-Unmodified-Since-Version", cur}},
			{"POST", "items", 413, []string{"If-Unmodified-Since-Version", cur}},
			{"POST", "items", 412, []string{"If-Unmodified-Since-Version", "0"}},
			{"GET", "deleted?since=x", 400, nil},
		} {
			var body any
			switch c.want {
			case 413:
				arr := make([]m, 51)
				for i := range arr {
					arr[i] = m{"itemType": "book"}
				}
				body = arr
			case 412:
				body = []m{{"itemType": "book"}}
			}
			r := l.send(c.method, c.path, body, c.h...)
			assert.Equal(t, c.want, r.Status, "%s %s: %s", c.method, c.path, r.Body)
			assert.Equal(t, cur, r.Header.Get("Last-Modified-Version"), "%s %s", c.method, c.path)
		}
	})
	t.Run("group resolution", func(t *testing.T) {
		r := e.do(t, req{method: "GET", path: "groups/99999/items", key: u.Key})
		assert.Equal(t, 404, r.Status)
		r = e.do(t, req{method: "GET", path: "groups/99999/items"})
		assert.Equal(t, 403, r.Status, "unauthenticated callers learn nothing about groups")
		r = e.do(t, req{method: "GET", path: "users/abc/items", key: u.Key})
		assert.Equal(t, 400, r.Status)
	})
}

func TestKeys(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", repo.KeyAccess{Library: true, Notes: true, Files: false, Write: false, Groups: "read"})

	t.Run("current reflects key access", func(t *testing.T) {
		r := e.do(t, req{method: "GET", path: "keys/current", key: u.Key})
		require.Equal(t, 200, r.Status)
		j := r.json(t)
		assert.Equal(t, u.Key, j["key"])
		assert.Equal(t, "alice", j["displayName"])
		assert.NotContains(t, j, "emails")
		acc := j["access"].(m)
		assert.Equal(t, m{"library": true, "notes": true}, acc["user"])
		assert.Equal(t, m{"all": m{"library": true}}, acc["groups"])
	})
	t.Run("includeEmails", func(t *testing.T) {
		r := e.do(t, req{method: "GET", path: "keys/current?includeEmails=1", key: u.Key})
		assert.Equal(t, []any{"alice@example.org"}, r.json(t)["emails"])
	})
	t.Run("no group access still lists groups.all", func(t *testing.T) {
		n := e.newUser(t, "nogroups", repo.KeyAccess{Library: true, Groups: "none"})
		j := e.do(t, req{method: "GET", path: "keys/current", key: n.Key}).json(t)
		assert.Contains(t, j["access"].(m)["groups"], "all")
	})
	t.Run("keys by key", func(t *testing.T) {
		assert.Equal(t, 200, e.do(t, req{method: "GET", path: "keys/" + u.Key}).Status)
		assert.Equal(t, 200, e.do(t, req{method: "GET", path: "keys/" + u.Key, key: u.Key}).Status)
		assert.Equal(t, 404, e.do(t, req{method: "GET", path: "keys/nosuchkeynosuchkey12345"}).Status)
		other := e.newUser(t, "bob", fullAccess)
		assert.Equal(t, 403, e.do(t, req{method: "GET", path: "keys/" + other.Key, key: u.Key}).Status)
	})
	t.Run("delete current invalidates the auth cache", func(t *testing.T) {
		d := e.newUser(t, "dave", fullAccess)
		assert.Equal(t, 200, e.do(t, req{method: "GET", path: "keys/current", key: d.Key}).Status)
		assert.Equal(t, 204, e.do(t, req{method: "DELETE", path: "keys/current", key: d.Key}).Status)
		assert.Equal(t, 403, e.do(t, req{method: "GET", path: "keys/current", key: d.Key}).Status)
		assert.Equal(t, 403, e.do(t, req{method: "DELETE", path: "keys/current", key: d.Key}).Status)
	})
}

func TestLegacyLogin(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	login := func(body any) resp { return e.do(t, req{method: "POST", path: "keys", body: body}) }

	t.Run("missing access defaults to full", func(t *testing.T) {
		r := login(m{"username": "alice", "password": testPassword, "name": "laptop"})
		require.Equal(t, 201, r.Status, string(r.Body))
		j := r.json(t)
		key := j["key"].(string)
		assert.Len(t, key, 24)
		acc := j["access"].(m)
		assert.Equal(t, m{"library": true, "files": true, "notes": true, "write": true}, acc["user"])
		assert.Equal(t, m{"all": m{"library": true, "write": true}}, acc["groups"])
		assert.EqualValues(t, u.ID, j["userID"])
		stored, err := e.Store.Keys().ListByUser(ctx(), u.ID)
		require.NoError(t, err)
		var names []string
		for _, k := range stored {
			names = append(names, k.Name)
		}
		assert.Contains(t, names, "laptop")
	})
	t.Run("requested access is stored", func(t *testing.T) {
		r := login(m{
			"username": "alice", "password": testPassword, "name": "ro",
			"access": m{"user": m{"library": true}, "groups": m{"all": m{"library": true}}},
		})
		require.Equal(t, 201, r.Status)
		cur := e.do(t, req{method: "GET", path: "keys/current", key: r.json(t)["key"].(string)}).json(t)
		assert.Equal(t, m{"library": true}, cur["access"].(m)["user"])
		// A read-only key cannot write.
		w := e.do(t, req{method: "POST", path: fmt.Sprintf("users/%d/items", u.ID), key: r.json(t)["key"].(string), body: []m{{"itemType": "book"}}})
		assert.Equal(t, 403, w.Status)
	})
	t.Run("bad credentials", func(t *testing.T) {
		assert.Equal(t, 403, login(m{"username": "alice", "password": "wrong"}).Status)
		assert.Equal(t, 403, login(m{"username": "ghost", "password": testPassword}).Status)
		assert.Equal(t, 400, e.do(t, req{method: "POST", path: "keys", raw: []byte("{")}).Status)
	})
	t.Run("disabled users are refused", func(t *testing.T) {
		d := e.newUser(t, "disabled", fullAccess)
		d.Disabled = true
		_, err := e.Store.Users().Update(ctx(), d.User)
		require.NoError(t, err)
		assert.Equal(t, 403, login(m{"username": "disabled", "password": testPassword}).Status)
	})
}

func TestGroups(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t, "owner", fullAccess)
	admin := e.newUser(t, "admin", fullAccess)
	member := e.newUser(t, "member", fullAccess)
	reader := e.newUser(t, "reader", repo.KeyAccess{Library: true, Notes: true, Write: true, Groups: "read"})
	outsider := e.newUser(t, "outsider", fullAccess)
	g, err := e.Store.Groups().Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "Lab", Description: "d", URL: "http://x", LibraryEditing: "admins", LibraryReading: "members", FileEditing: "none"})
	require.NoError(t, err)
	require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, admin.ID, "admin"))
	require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, member.ID, "member"))
	require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, reader.ID, "member"))

	t.Run("group json", func(t *testing.T) {
		r := e.do(t, req{method: "GET", path: fmt.Sprintf("groups/%d", g.ID), key: member.Key})
		require.Equal(t, 200, r.Status)
		j := r.json(t)
		d := j["data"].(m)
		assert.Equal(t, "Private", d["type"])
		assert.Equal(t, "Lab", d["name"])
		assert.Equal(t, "admins", d["libraryEditing"])
		assert.Equal(t, "members", d["libraryReading"])
		assert.Equal(t, "none", d["fileEditing"])
		assert.Equal(t, []any{float64(admin.ID)}, d["admins"], "owner is excluded from admins")
		assert.ElementsMatch(t, []any{float64(member.ID), float64(reader.ID)}, d["members"], "owner and admins are excluded from members")
		assert.EqualValues(t, owner.ID, d["owner"])
		assert.Contains(t, j["links"], "self")
		meta := j["meta"].(m)
		assert.Contains(t, meta, "numItems")
		assert.NotEmpty(t, meta["created"])
		assert.Equal(t, strconv.FormatInt(g.Version+3, 10), ver(r.Header), "group version header")
	})
	t.Run("empty admins and members are arrays", func(t *testing.T) {
		solo, err := e.Store.Groups().Create(ctx(), repo.Group{OwnerID: owner.ID, Name: "Solo"})
		require.NoError(t, err)
		r := e.do(t, req{method: "GET", path: fmt.Sprintf("groups/%d", solo.ID), key: owner.Key})
		assert.Contains(t, string(r.Body), `"admins":[]`)
		assert.Contains(t, string(r.Body), `"members":[]`)
	})
	t.Run("user groups listing", func(t *testing.T) {
		r := e.do(t, req{method: "GET", path: fmt.Sprintf("users/%d/groups", member.ID), key: member.Key})
		require.Equal(t, 200, r.Status)
		arr := r.array(t)
		require.Len(t, arr, 1)
		assert.EqualValues(t, g.ID, arr[0].(m)["id"])
		assert.Equal(t, "1", r.Header.Get("Total-Results"))

		r = e.do(t, req{method: "GET", path: fmt.Sprintf("users/%d/groups?format=versions", member.ID), key: member.Key})
		assert.Contains(t, r.json(t), strconv.FormatInt(g.ID, 10))

		r = e.do(t, req{method: "GET", path: fmt.Sprintf("users/%d/groups?format=versions", outsider.ID), key: outsider.Key})
		assert.Equal(t, m{}, r.json(t))
		r = e.do(t, req{method: "GET", path: fmt.Sprintf("users/%d/groups", member.ID), key: outsider.Key})
		assert.Equal(t, 403, r.Status)
		r = e.do(t, req{method: "GET", path: fmt.Sprintf("users/%d/groups?format=atom", member.ID), key: member.Key})
		assert.Equal(t, 400, r.Status)
	})
	t.Run("a key without group access sees no groups", func(t *testing.T) {
		n := e.newUser(t, "nogroup", repo.KeyAccess{Library: true, Groups: "none"})
		require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, n.ID, "member"))
		r := e.do(t, req{method: "GET", path: fmt.Sprintf("users/%d/groups?format=versions", n.ID), key: n.Key})
		assert.Equal(t, m{}, r.json(t))
		assert.Equal(t, 403, e.groupLib(t, g.ID, n).get("items").Status)
	})
	t.Run("write permissions follow group settings", func(t *testing.T) {
		assert.Equal(t, 403, e.groupLib(t, g.ID, member).post("items", []m{{"itemType": "book"}}).Status, "libraryEditing=admins")
		assert.Equal(t, 403, e.groupLib(t, g.ID, reader).post("items", []m{{"itemType": "book"}}).Status, "read-only key")
		assert.Equal(t, 200, e.groupLib(t, g.ID, admin).post("items", []m{{"itemType": "book"}}).Status)
		assert.Equal(t, 200, e.groupLib(t, g.ID, owner).post("items", []m{{"itemType": "book"}}).Status)
		assert.Equal(t, 200, e.groupLib(t, g.ID, member).get("items").Status)
		assert.Equal(t, 200, e.groupLib(t, g.ID, reader).get("items").Status)
		assert.Equal(t, 403, e.groupLib(t, g.ID, outsider).get("items").Status)
	})
	t.Run("group writes publish the group topic", func(t *testing.T) {
		before := len(e.Pub.all())
		e.groupLib(t, g.ID, admin).create("items", m{"itemType": "book"})
		evs := e.Pub.all()
		require.Len(t, evs, before+1)
		assert.Equal(t, fmt.Sprintf("/groups/%d", g.ID), evs[len(evs)-1].Topic)
	})
}

func TestPublisher(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	topic := fmt.Sprintf("/users/%d", u.ID)

	r := l.post("items", []m{{"itemType": "book"}, {"itemType": "book"}, {"itemType": "book"}})
	require.Equal(t, 200, r.Status)
	require.Len(t, e.Pub.all(), 1, "one event per request, not per object")
	assert.Equal(t, zapi.Event{Topic: topic, Version: 1}, e.Pub.all()[0])

	// Nothing changed, nothing published.
	data := r.json(t)["successful"].(m)["0"].(m)["data"]
	r = l.post("items", []m{data.(m)})
	require.Contains(t, r.json(t)["unchanged"], "0")
	assert.Len(t, e.Pub.all(), 1)

	// Failed writes publish nothing either.
	l.post("items", []m{{"itemType": "bogus"}})
	l.post("items", []m{{"itemType": "book"}}, "If-Unmodified-Since-Version", "0")
	assert.Len(t, e.Pub.all(), 1)

	l.post("settings", m{"tagColors": m{"value": []m{{"name": "a", "color": "#fff"}}, "version": 0}})
	assert.Len(t, e.Pub.all(), 2)
	assert.EqualValues(t, 2, e.Pub.all()[1].Version)

	// Reads publish nothing.
	l.get("items")
	assert.Len(t, e.Pub.all(), 2)
}

func TestWriteBodyLimits(t *testing.T) {
	e := newEnv(t, withDeps(func(d *zapi.Deps) { d.MaxRequestBytes = 4096 }))
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	big := []m{{"itemType": "book", "abstractNote": strings.Repeat("x", 8000)}}
	r := l.post("items", big)
	assert.Equal(t, 413, r.Status)
	assert.NotEmpty(t, r.Header.Get("Last-Modified-Version"))

	// A tiny gzip body that inflates past the limit must be refused too.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`[{"itemType":"book","abstractNote":"` + strings.Repeat("x", 100000) + `"}]`))
	_ = zw.Close()
	require.Less(t, buf.Len(), 4096)
	r = e.do(t, req{method: "POST", path: l.path + "/items", key: u.Key, raw: buf.Bytes(), header: map[string]string{"Content-Encoding": "gzip", "Content-Type": "application/json"}})
	assert.Equal(t, 413, r.Status)

	r = e.do(t, req{method: "POST", path: l.path + "/items", key: u.Key, raw: []byte("not gzip"), header: map[string]string{"Content-Encoding": "gzip"}})
	assert.Equal(t, 400, r.Status)

	r = e.do(t, req{method: "POST", path: l.path + "/items", key: u.Key, raw: []byte(`{"itemType":"book"}`)})
	assert.Equal(t, 400, r.Status, "an object is not an array")
	r = e.do(t, req{method: "POST", path: l.path + "/items", key: u.Key, raw: []byte(`[`)})
	assert.Equal(t, 400, r.Status)

	// A normal gzip body works.
	r = e.do(t, req{method: "POST", path: l.path + "/items", key: u.Key, body: []m{{"itemType": "book"}}, gzip: true})
	assert.Equal(t, 200, r.Status)
}

func TestBusyLibraryIs503(t *testing.T) {
	e := newEnv(t, withPG(pg.WithLockTimeout(150*time.Millisecond)))
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	hold, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := e.Store.InLibrary(ctx(), u.LibraryID, func(repo.LibraryTx) error {
			close(hold)
			<-release
			return nil
		})
		assert.NoError(t, err)
	}()
	<-hold
	r := l.post("items", []m{{"itemType": "book"}})
	close(release)
	wg.Wait()
	assert.Equal(t, 503, r.Status, string(r.Body))
	secs, err := strconv.Atoi(r.Header.Get("Retry-After"))
	require.NoError(t, err, "Retry-After must be an integer")
	assert.GreaterOrEqual(t, secs, 3)
	assert.LessOrEqual(t, secs, 8)
}

func TestWriteDetails(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	t.Run("response shape and server dates", func(t *testing.T) {
		r := l.post("items", []m{{"itemType": "book", "title": "T", "creators": []m{{"creatorType": "author", "firstName": "A", "lastName": "Smith"}}, "date": "2020-05"}})
		require.Equal(t, 200, r.Status)
		o := r.json(t)["successful"].(m)["0"].(m)
		assert.Contains(t, o["links"], "self")
		assert.Equal(t, "Smith", o["meta"].(m)["creatorSummary"])
		assert.Equal(t, "2020-05", o["meta"].(m)["parsedDate"])
		d := o["data"].(m)
		added, err := time.Parse(time.RFC3339, d["dateAdded"].(string))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now(), added, time.Minute)
		assert.True(t, strings.HasSuffix(d["dateAdded"].(string), "Z"))
	})
	t.Run("dateModified updates only for changes without a client date", func(t *testing.T) {
		clock := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		e2 := newEnv(t, withDeps(func(d *zapi.Deps) { d.Now = func() time.Time { return clock } }))
		l2 := e2.userLib(t, e2.newUser(t, "bob", fullAccess))
		key := l2.create("items", m{"itemType": "book", "title": "a"})
		assert.Equal(t, "2030-01-01T00:00:00Z", l2.dataOf("items", key)["dateModified"])
		clock = clock.Add(48 * time.Hour)
		v := l2.version()
		r := l2.post("items", []m{{"key": key, "version": v, "title": "a"}})
		assert.Contains(t, r.json(t)["unchanged"], "0")
		assert.Equal(t, "2030-01-01T00:00:00Z", l2.dataOf("items", key)["dateModified"])
		l2.post("items", []m{{"key": key, "version": v, "title": "b"}})
		d := l2.dataOf("items", key)
		assert.Equal(t, "2030-01-03T00:00:00Z", d["dateModified"])
		assert.Equal(t, "2030-01-01T00:00:00Z", d["dateAdded"])
	})
	t.Run("PUT keeps dateAdded and attachment file metadata", func(t *testing.T) {
		key := l.create("items", m{"itemType": "attachment", "linkMode": "imported_file", "title": "f", "filename": "f.pdf", "md5": "abc", "mtime": 5})
		before := l.dataOf("items", key)
		r := l.send("PUT", "items/"+key, m{"itemType": "attachment", "linkMode": "imported_file", "title": "g", "filename": "f.pdf"})
		require.Equal(t, 204, r.Status, string(r.Body))
		after := l.dataOf("items", key)
		assert.Equal(t, before["dateAdded"], after["dateAdded"])
		assert.Equal(t, "g", after["title"])
		assert.Equal(t, "abc", after["md5"])
		assert.EqualValues(t, 5, after["mtime"])
	})
	t.Run("PUT and PATCH errors", func(t *testing.T) {
		assert.Equal(t, 404, l.send("PUT", "items/ZZZZZZZ2", m{"itemType": "book"}).Status)
		assert.Equal(t, 404, l.send("PATCH", "items/ZZZZZZZ2", m{"title": "x"}).Status)
		key := l.create("items", m{"itemType": "book"})
		assert.Equal(t, 400, l.send("PATCH", "items/"+key, m{"key": "ZZZZZZZ2", "title": "x"}).Status)
		assert.Equal(t, 400, l.send("PATCH", "items/"+key, m{"bogus": "x"}).Status)
		r := l.send("PUT", "items/"+key, m{"title": "no type"})
		assert.Equal(t, 400, r.Status)
		assert.NotEmpty(t, ver(r.Header))
		assert.Equal(t, 400, l.send("PATCH", "items/"+key, []m{}).Status)
	})
	t.Run("unchanged single write keeps the version", func(t *testing.T) {
		key := l.create("items", m{"itemType": "book", "title": "same"})
		v := l.version()
		r := l.send("PATCH", "items/"+key, m{"title": "same"})
		assert.Equal(t, 204, r.Status)
		assert.Equal(t, strconv.FormatInt(v, 10), ver(r.Header))
	})
	t.Run("write token must be 32 characters", func(t *testing.T) {
		r := l.post("items", []m{{"itemType": "book"}}, "Zotero-Write-Token", "short")
		assert.Equal(t, 400, r.Status)
	})
	t.Run("failed writes do not burn the write token", func(t *testing.T) {
		tok := strings.Repeat("a", 32)
		r := l.post("items", []m{{"itemType": "book"}}, "Zotero-Write-Token", tok, "If-Unmodified-Since-Version", "0")
		require.Equal(t, 412, r.Status)
		r = l.post("items", []m{{"itemType": "book"}}, "Zotero-Write-Token", tok)
		assert.Equal(t, 200, r.Status, "a rolled back request leaves the token unused")
	})
	t.Run("recreating a deleted object clears the delete log", func(t *testing.T) {
		key := l.create("items", m{"itemType": "book"})
		require.Equal(t, 204, l.send("DELETE", "items?itemKey="+key, nil, "If-Unmodified-Since-Version", strconv.FormatInt(l.version(), 10)).Status)
		assert.Contains(t, string(l.get("deleted?since=0").Body), key)
		r := l.post("items", []m{{"key": key, "itemType": "book"}})
		require.Contains(t, r.json(t)["success"], "0")
		assert.NotContains(t, string(l.get("deleted?since=0").Body), key)
	})
	t.Run("item in the trash", func(t *testing.T) {
		key := l.create("items", m{"itemType": "book", "deleted": true})
		assert.EqualValues(t, 1, l.dataOf("items", key)["deleted"])
		assert.Contains(t, string(l.get("items/trash?format=keys").Body), key)
		v := l.version()
		l.post("items", []m{{"key": key, "version": v, "deleted": 0}})
		assert.NotContains(t, l.dataOf("items", key), "deleted")
		assert.NotContains(t, string(l.get("items/trash?format=keys").Body), key)
	})
	t.Run("unknown parent and collection are 409", func(t *testing.T) {
		r := l.post("items", []m{{"itemType": "note", "parentItem": "ZZZZZZZ2"}, {"itemType": "book", "collections": []string{"ZZZZZZZ2"}}})
		f := r.json(t)["failed"].(m)
		assert.EqualValues(t, 409, f["0"].(m)["code"])
		assert.EqualValues(t, 409, f["1"].(m)["code"])
	})
	t.Run("collection cycle", func(t *testing.T) {
		r := l.post("collections", []m{{"key": "SELFAAA2", "name": "x", "parentCollection": "SELFAAA2"}})
		assert.EqualValues(t, 409, r.json(t)["failed"].(m)["0"].(m)["code"])
	})
	t.Run("read-only key", func(t *testing.T) {
		ro := e.newUser(t, "reader", repo.KeyAccess{Library: true, Notes: true, Groups: "all"})
		rl := e.userLib(t, ro)
		assert.Equal(t, 403, rl.post("items", []m{{"itemType": "book"}}).Status)
		assert.Equal(t, 403, rl.send("DELETE", "items?itemKey=ZZZZZZZ2", nil, "If-Unmodified-Since-Version", "0").Status)
		assert.Equal(t, 403, rl.post("settings", m{"a": m{"value": 1}}).Status)
		assert.Equal(t, 403, rl.send("PUT", "items/ZZZZZZZ2/fulltext", m{"content": "x"}).Status)
		assert.Equal(t, 200, rl.get("items").Status)
	})
	t.Run("key without library access", func(t *testing.T) {
		nl := e.newUser(t, "nolib", repo.KeyAccess{Groups: "all"})
		assert.Equal(t, 403, e.userLib(t, nl).get("items").Status)
	})
}

func TestDeleteCascades(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	cur := func() string { return strconv.FormatInt(l.version(), 10) }

	t.Run("deleting a parent deletes its children", func(t *testing.T) {
		parent := l.create("items", m{"itemType": "book"})
		note := l.create("items", m{"itemType": "note", "parentItem": parent})
		att := l.create("items", m{"itemType": "attachment", "linkMode": "linked_url", "parentItem": parent})
		ann := l.create("items", m{"itemType": "annotation", "parentItem": att, "annotationType": "highlight"})
		other := l.create("items", m{"itemType": "book"})
		require.Equal(t, 204, l.send("DELETE", "items/"+parent, nil, "If-Unmodified-Since-Version", cur()).Status)
		for _, k := range []string{parent, note, att, ann} {
			assert.Equal(t, 404, l.get("items/"+k).Status, k)
		}
		assert.Equal(t, 200, l.get("items/"+other).Status)
		del := string(l.get("deleted?since=0").Body)
		for _, k := range []string{parent, note, att, ann} {
			assert.Contains(t, del, k)
		}
	})
	t.Run("single delete preconditions", func(t *testing.T) {
		k := l.create("items", m{"itemType": "book"})
		assert.Equal(t, 428, l.send("DELETE", "items/"+k, nil).Status)
		assert.Equal(t, 412, l.send("DELETE", "items/"+k, nil, "If-Unmodified-Since-Version", "1").Status)
		assert.Equal(t, 404, l.send("DELETE", "items/ZZZZZZZ2", nil, "If-Unmodified-Since-Version", cur()).Status)
		assert.Equal(t, 204, l.send("DELETE", "items/"+k, nil, "If-Unmodified-Since-Version", cur()).Status)
	})
	t.Run("deleting a collection detaches items and removes subcollections", func(t *testing.T) {
		top := l.create("collections", m{"name": "top"})
		sub := l.create("collections", m{"name": "sub", "parentCollection": top})
		keep := l.create("collections", m{"name": "keep"})
		it := l.create("items", m{"itemType": "book", "collections": []string{top, sub, keep}})
		untouched := l.create("items", m{"itemType": "book", "collections": []string{keep}})
		beforeV := l.dataOf("items", untouched)["dateModified"]
		before := l.version()

		r := l.send("DELETE", "collections?collectionKey="+top, nil, "If-Unmodified-Since-Version", cur())
		require.Equal(t, 204, r.Status)
		assert.Equal(t, strconv.FormatInt(before+1, 10), ver(r.Header), "one version bump")

		assert.Equal(t, 404, l.get("collections/"+top).Status)
		assert.Equal(t, 404, l.get("collections/"+sub).Status)
		assert.Equal(t, 200, l.get("collections/"+keep).Status)
		o := l.get("items/" + it).json(t)
		assert.Equal(t, []any{keep}, o["data"].(m)["collections"])
		assert.EqualValues(t, before+1, o["version"], "detached items get the new version")
		assert.EqualValues(t, before, l.get("items/" + untouched).json(t)["version"])
		assert.Equal(t, beforeV, l.dataOf("items", untouched)["dateModified"])
		del := string(l.get("deleted?since=0").Body)
		assert.Contains(t, del, top)
		assert.Contains(t, del, sub)
	})
	t.Run("delete validation", func(t *testing.T) {
		v := cur()
		assert.Equal(t, 400, l.send("DELETE", "items", nil, "If-Unmodified-Since-Version", v).Status)
		// Only malformed keys: nothing to delete, and no version bump.
		r := l.send("DELETE", "items?itemKey=bad", nil, "If-Unmodified-Since-Version", v)
		assert.Equal(t, 204, r.Status)
		assert.Equal(t, v, ver(r.Header))
		assert.Equal(t, v, cur())
		keys := make([]string, 51)
		for i := range keys {
			keys[i] = fmt.Sprintf("AAAAAA%02d", i+22)
		}
		assert.Equal(t, 413, l.send("DELETE", "items?itemKey="+strings.Join(keys, ","), nil, "If-Unmodified-Since-Version", v).Status)
		assert.Equal(t, 400, l.send("DELETE", "items?itemKey=AAAAAAA2", nil, "If-Unmodified-Since-Version", "x").Status)
	})
}

func TestReads(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	var keys []string
	for i := range 30 {
		keys = append(keys, l.create("items", m{
			"itemType": "book", "title": fmt.Sprintf("title %02d", 29-i),
			"tags": []m{{"tag": "common"}, {"tag": fmt.Sprintf("t%d", i%3), "type": i % 2}},
		}))
	}

	t.Run("pagination and Link header", func(t *testing.T) {
		r := l.get("items?limit=10&start=10&key=" + u.Key)
		require.Equal(t, 200, r.Status)
		assert.Equal(t, "30", r.Header.Get("Total-Results"))
		link := r.Header.Get("Link")
		for _, rel := range []string{`rel="first"`, `rel="prev"`, `rel="next"`, `rel="last"`} {
			assert.Contains(t, link, rel)
		}
		assert.Contains(t, link, "start=20")
		assert.Contains(t, link, "start=0")
		assert.NotContains(t, link, "key="+u.Key, "credentials must not leak into links")
		assert.Contains(t, link, "http://127.0.0.1")
		assert.Contains(t, link, "/api/users/")
		assert.Len(t, r.array(t), 10)

		r = l.get("items?limit=10&start=20")
		assert.NotContains(t, r.Header.Get("Link"), `rel="next"`)
	})
	t.Run("limit defaults and caps", func(t *testing.T) {
		assert.Len(t, l.get("items").array(t), 25)
		assert.Len(t, l.get("items?limit=1000").array(t), 30)
		assert.Equal(t, 400, l.get("items?limit=0").Status)
		assert.Equal(t, 400, l.get("items?limit=x").Status)
		assert.Equal(t, 400, l.get("items?start=-1").Status)
		assert.Equal(t, 400, l.get("items?sort=nope").Status)
		assert.Equal(t, 400, l.get("items?direction=sideways").Status)
		assert.Equal(t, 400, l.get("items?since=x").Status)
	})
	t.Run("sorting", func(t *testing.T) {
		titles := func(q string) []string {
			var out []string
			for _, o := range l.get("items?limit=100&" + q).array(t) {
				out = append(out, o.(m)["data"].(m)["title"].(string))
			}
			return out
		}
		asc := titles("sort=title&direction=asc")
		assert.Equal(t, "title 00", asc[0])
		assert.Equal(t, "title 29", asc[len(asc)-1])
		desc := titles("sort=title&direction=desc")
		assert.Equal(t, "title 29", desc[0])
		// Without parameters the newest item comes first, as on zotero.org.
		assert.Equal(t, "title 00", titles("")[0])
	})
	t.Run("tag filter and tag listing", func(t *testing.T) {
		assert.Len(t, strings.Fields(string(l.get("items?tag=common&format=keys").Body)), 30)
		assert.Len(t, strings.Fields(string(l.get("items?tag=common&tag=t0&format=keys").Body)), 10)
		assert.Len(t, strings.Fields(string(l.get("items?tag=nope&format=keys").Body)), 0)
		r := l.get("tags")
		require.Equal(t, 200, r.Status)
		var common m
		names := map[string]bool{}
		for _, o := range r.array(t) {
			tg := o.(m)
			names[tg["tag"].(string)] = true
			if tg["tag"] == "common" {
				common = tg
			}
		}
		assert.Equal(t, map[string]bool{"common": true, "t0": true, "t1": true, "t2": true}, names)
		assert.EqualValues(t, 30, common["meta"].(m)["numItems"])
		assert.Contains(t, common["links"], "self")
		item := l.get("items/" + keys[0] + "/tags").array(t)
		assert.Len(t, item, 2)
		assert.Equal(t, 200, l.get("tags/common").Status)
		assert.Equal(t, 404, l.get("tags/missing").Status)
		assert.Equal(t, 200, l.get("items/top/tags").Status)
	})
	t.Run("itemKey lists and bad keys", func(t *testing.T) {
		assert.Len(t, l.get("items?itemKey="+strings.Join(keys[:5], ",")).array(t), 5)
		// A filter with no valid keys matches nothing; it must not fall back
		// to listing the whole library.
		for _, param := range []string{"items?itemKey", "collections?collectionKey", "searches?searchKey"} {
			r := l.get(param + "=bad,!!")
			assert.Equal(t, 200, r.Status, param)
			assert.Equal(t, "0", r.Header.Get("Total-Results"), param)
			assert.NotEmpty(t, r.Header.Get("Last-Modified-Version"), param)
			assert.Empty(t, r.array(t), param)
			assert.Equal(t, "{}", strings.TrimSpace(string(l.get(param+"=bad&format=versions").Body)), param)
			assert.Empty(t, strings.TrimSpace(string(l.get(param+"=bad&format=keys").Body)), param)
		}
		many := strings.Repeat("AAAAAAA2,", 101)
		assert.Equal(t, 400, l.get("items?itemKey="+many[:len(many)-1]).Status)
	})
	t.Run("children, numChildren and top", func(t *testing.T) {
		parent := keys[0]
		l.create("items", m{"itemType": "note", "parentItem": parent})
		l.create("items", m{"itemType": "note", "parentItem": parent, "deleted": 1})
		o := l.get("items/" + parent).json(t)
		assert.EqualValues(t, 1, o["meta"].(m)["numChildren"], "trashed children are not counted")
		assert.Len(t, l.get("items/"+parent+"/children").array(t), 1)
		assert.Equal(t, 404, l.get("items/ZZZZZZZ2/children").Status)
		assert.Equal(t, "30", l.get("items/top").Header.Get("Total-Results"))
		for _, it := range l.get("items?limit=100").array(t) {
			o := it.(m)
			if o["key"] == parent {
				assert.EqualValues(t, 1, o["meta"].(m)["numChildren"])
			}
		}
		up := l.get("items/" + strings.Fields(string(l.get("items/" + parent + "/children?format=keys").Body))[0]).json(t)
		assert.Contains(t, up["links"].(m)["up"].(m)["href"], parent)
	})
	t.Run("collections", func(t *testing.T) {
		c1 := l.create("collections", m{"name": "B"})
		c2 := l.create("collections", m{"name": "A", "parentCollection": c1})
		l.post("items", []m{{"key": keys[1], "version": l.get("items/" + keys[1]).json(t)["version"], "collections": []string{c1}}})

		assert.Len(t, l.get("collections").array(t), 2)
		top := l.get("collections/top").array(t)
		require.Len(t, top, 1)
		assert.Equal(t, c1, top[0].(m)["key"])
		subs := l.get("collections/" + c1 + "/collections").array(t)
		require.Len(t, subs, 1)
		assert.Equal(t, c2, subs[0].(m)["key"])
		assert.Equal(t, 404, l.get("collections/ZZZZZZZ2/items").Status)
		assert.Equal(t, keys[1], strings.TrimSpace(string(l.get("collections/"+c1+"/items?format=keys").Body)))
		assert.Len(t, l.get("collections/"+c1+"/items/top").array(t), 1)
		assert.Len(t, l.get("collections/"+c1+"/tags").array(t), 2)

		o := l.get("collections/" + c1).json(t)
		meta := o["meta"].(m)
		assert.EqualValues(t, 1, meta["numItems"])
		assert.EqualValues(t, 1, meta["numCollections"])
		assert.Equal(t, "B", o["data"].(m)["name"])
		// Output order: key, version first, then name.
		raw := string(l.get("collections/" + c1).Body)
		assert.Less(t, strings.Index(raw, `"key":"`+c1+`","version"`), strings.Index(raw, `"name"`))
	})
	t.Run("searches", func(t *testing.T) {
		s := l.create("searches", m{"name": "S", "conditions": []m{{"condition": "title", "operator": "contains", "value": "x"}}})
		assert.Len(t, l.get("searches").array(t), 1)
		assert.Equal(t, "S", l.get("searches/" + s).json(t)["data"].(m)["name"])
		assert.Equal(t, s, strings.TrimSpace(string(l.get("searches?searchKey="+s+"&format=keys").Body)))
	})
	t.Run("schema version filters new fields", func(t *testing.T) {
		k := l.create("items", m{"itemType": "book", "title": "t", "citationKey": "ck"})
		full := string(l.get("items/" + k).Body)
		assert.Contains(t, full, "citationKey")
		old := l.get("items/"+k, "Zotero-Schema-Version", "1")
		assert.NotContains(t, string(old.Body), "citationKey")
		assert.Contains(t, string(old.Body), `"title"`)
	})
	t.Run("format keys over limit", func(t *testing.T) {
		got := strings.Fields(string(l.get("items?format=keys&limit=3").Body))
		assert.Len(t, got, 3)
		r := l.get("items?format=versions&limit=2")
		assert.Len(t, r.json(t), 2)
	})
	t.Run("includeTrashed", func(t *testing.T) {
		k := l.create("items", m{"itemType": "book", "deleted": 1})
		assert.NotContains(t, string(l.get("items?format=keys&limit=100").Body), k)
		assert.Contains(t, string(l.get("items?format=keys&includeTrashed=1").Body), k)
		assert.Equal(t, 200, l.get("items/"+k).Status, "single reads include the trash")
		assert.Contains(t, string(l.get("items?itemKey="+k+"&includeTrashed=1").Body), k)
		assert.NotContains(t, string(l.get("items?itemKey="+k).Body), k)
	})
}

func TestSettings(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	colors := func(v string, version int64) m {
		return m{"tagColors": m{"value": []m{{"name": "a", "color": v}}, "version": version}}
	}
	require.Equal(t, 204, l.post("settings", colors("#111", 0)).Status)

	t.Run("get one", func(t *testing.T) {
		r := l.get("settings/tagColors")
		require.Equal(t, 200, r.Status)
		assert.EqualValues(t, 1, r.json(t)["version"])
		assert.Equal(t, 404, l.get("settings/nope").Status)
	})
	t.Run("stale setting version is 412", func(t *testing.T) {
		require.Equal(t, 204, l.post("settings", colors("#222", 1)).Status)
		r := l.post("settings", colors("#333", 1))
		assert.Equal(t, 412, r.Status)
		assert.Equal(t, "2", ver(r.Header))
	})
	t.Run("explicit version zero on an existing setting is 412", func(t *testing.T) {
		r := l.post("settings", colors("#444", 0))
		assert.Equal(t, 412, r.Status)
		assert.Equal(t, "#222", l.get("settings/tagColors").json(t)["value"].([]any)[0].(map[string]any)["color"])
	})
	t.Run("version zero retry of the stored value is unchanged", func(t *testing.T) {
		before := ver(l.get("settings").Header)
		r := l.post("settings", colors("#222", 0))
		assert.Equal(t, 204, r.Status, string(r.Body))
		assert.Equal(t, before, ver(r.Header))
	})
	t.Run("client ahead of the setting version is accepted", func(t *testing.T) {
		assert.Equal(t, 204, l.post("settings", colors("#555", 99)).Status)
	})
	t.Run("PUT single", func(t *testing.T) {
		r := l.send("PUT", "settings/lastPageIndex_u_ABCD2345", m{"value": 7})
		assert.Equal(t, 204, r.Status)
		assert.EqualValues(t, 7, l.get("settings/lastPageIndex_u_ABCD2345").json(t)["value"])
		assert.Equal(t, 400, l.send("PUT", "settings/bad key!", m{"value": 7}).Status)
		assert.Equal(t, 400, l.send("PUT", "settings/x", m{"version": 7}).Status, "value is required")
	})
	t.Run("delete preconditions", func(t *testing.T) {
		assert.Equal(t, 428, l.send("DELETE", "settings/tagColors", nil).Status)
		assert.Equal(t, 412, l.send("DELETE", "settings/tagColors", nil, "If-Unmodified-Since-Version", "1").Status)
		cur := strconv.FormatInt(l.version(), 10)
		assert.Equal(t, 404, l.send("DELETE", "settings/nope", nil, "If-Unmodified-Since-Version", cur).Status)
		before := l.version()
		r := l.send("DELETE", "settings?settingKey=nope", nil, "If-Unmodified-Since-Version", cur)
		assert.Equal(t, 204, r.Status)
		assert.Equal(t, strconv.FormatInt(before, 10), ver(r.Header), "deleting nothing does not bump")
		r = l.send("DELETE", "settings?settingKey=tagColors,lastPageIndex_u_ABCD2345", nil, "If-Unmodified-Since-Version", cur)
		assert.Equal(t, 204, r.Status)
		assert.Equal(t, m{}, l.get("settings").json(t))
		assert.Equal(t, 400, l.send("DELETE", "settings", nil, "If-Unmodified-Since-Version", cur).Status)
	})
}

func TestDeletedAndTags(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	t.Run("delete log floor", func(t *testing.T) {
		l.create("items", m{"itemType": "book"})
		_, err := e.Pool.Exec(ctx(), `UPDATE libraries SET delete_log_floor = 5 WHERE id = $1`, u.LibraryID)
		require.NoError(t, err)
		assert.Equal(t, 409, l.get("deleted?since=3").Status)
		assert.Equal(t, 200, l.get("deleted?since=0").Status, "0 means everything and is always answerable")
		assert.Equal(t, 200, l.get("deleted?since=5").Status)
		assert.Equal(t, 200, l.get("deleted").Status)
	})
	t.Run("deleting tags rewrites items", func(t *testing.T) {
		k1 := l.create("items", m{"itemType": "book", "tags": []m{{"tag": "a"}, {"tag": "b", "type": 1}, {"tag": "c"}}})
		k2 := l.create("items", m{"itemType": "book", "tags": []m{{"tag": "b"}}, "deleted": 1})
		k3 := l.create("items", m{"itemType": "book", "tags": []m{{"tag": "c"}}})
		before := l.version()
		v3 := l.get("items/" + k3).json(t)["version"]
		r := l.send("DELETE", "tags?tag="+strings.ReplaceAll("a || b", " ", "%20"), nil, "If-Unmodified-Since-Version", strconv.FormatInt(before, 10))
		require.Equal(t, 204, r.Status)
		assert.Equal(t, strconv.FormatInt(before+1, 10), ver(r.Header))

		o := l.get("items/" + k1).json(t)
		assert.Equal(t, []any{m{"tag": "c"}}, o["data"].(m)["tags"])
		assert.EqualValues(t, before+1, o["version"])
		o2 := l.get("items/" + k2).json(t)
		assert.Equal(t, []any{}, o2["data"].(m)["tags"], "trashed items are cleaned too")
		assert.Equal(t, v3, l.get("items/" + k3).json(t)["version"])
		assert.Contains(t, string(l.get("deleted?since=0").Body), `"tags":["a","b"]`)
		assert.Empty(t, strings.Fields(string(l.get("items?tag=a&format=keys").Body)))
	})
	t.Run("tag delete requires a precondition unless bare", func(t *testing.T) {
		assert.Equal(t, 204, l.send("DELETE", "tags", nil).Status)
		assert.Equal(t, 428, l.send("DELETE", "tags?tag=zzz", nil).Status)
		assert.Equal(t, 412, l.send("DELETE", "tags?tag=zzz", nil, "If-Unmodified-Since-Version", "1").Status)
		many := make([]string, 51)
		for i := range many {
			many[i] = "t" + strconv.Itoa(i)
		}
		cur := strconv.FormatInt(l.version(), 10)
		assert.Equal(t, 413, l.send("DELETE", "tags?tag="+strings.Join(many, "%20%7C%7C%20"), nil, "If-Unmodified-Since-Version", cur).Status)
	})
}

func TestFulltext(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	k := l.create("items", m{"itemType": "attachment", "linkMode": "imported_file", "filename": "a.pdf"})
	cur := func() string { return strconv.FormatInt(l.version(), 10) }

	t.Run("batch needs a precondition", func(t *testing.T) {
		assert.Equal(t, 428, l.post("fulltext", []m{{"key": k, "content": "x"}}).Status)
		assert.Equal(t, 412, l.post("fulltext", []m{{"key": k, "content": "x"}}, "If-Unmodified-Since-Version", "0").Status)
	})
	t.Run("batch results", func(t *testing.T) {
		r := l.post("fulltext", []m{
			{"key": k, "content": "hello", "indexedChars": 5, "totalChars": 9},
			{"key": "ZZZZZZZ2", "content": "ghost"},
			{"key": "bad", "content": "x"},
			{"key": k, "content": "x", "totalChars": -1},
		}, "If-Unmodified-Since-Version", cur())
		require.Equal(t, 200, r.Status)
		j := r.json(t)
		assert.Equal(t, m{"0": k}, j["success"])
		f := j["failed"].(m)
		assert.EqualValues(t, 404, f["1"].(m)["code"])
		assert.EqualValues(t, 400, f["2"].(m)["code"])
		assert.EqualValues(t, 400, f["3"].(m)["code"])
		assert.Equal(t, m{}, j["unchanged"])
		c := l.get("items/" + k + "/fulltext").json(t)
		assert.Equal(t, "hello", c["content"])
		assert.EqualValues(t, 5, c["indexedChars"])
		assert.NotContains(t, c, "indexedPages")
	})
	t.Run("batch resend reports unchanged entries as objects", func(t *testing.T) {
		before := l.version()
		r := l.post("fulltext", []m{{"key": k, "content": "hello", "indexedChars": 5, "totalChars": 9}}, "If-Unmodified-Since-Version", cur())
		require.Equal(t, 200, r.Status)
		j := r.json(t)
		assert.Equal(t, m{}, j["success"])
		u := j["unchanged"].(m)["0"].(m)
		assert.Equal(t, k, u["key"])
		assert.NotZero(t, u["version"])
		assert.Equal(t, before, l.version())
	})
	t.Run("version and deletion", func(t *testing.T) {
		before := l.version()
		r := l.send("PUT", "items/"+k+"/fulltext", m{"content": "again", "indexedPages": 1, "totalPages": 2})
		require.Equal(t, 204, r.Status)
		assert.Equal(t, strconv.FormatInt(before+1, 10), ver(r.Header))
		vs := l.get("fulltext?since=" + strconv.FormatInt(before, 10)).json(t)
		assert.EqualValues(t, before+1, vs[k])
		assert.Equal(t, 400, l.send("PUT", "items/"+k+"/fulltext", []int{1}).Status)
		// Deleting the item takes its text along.
		require.Equal(t, 204, l.send("DELETE", "items/"+k, nil, "If-Unmodified-Since-Version", cur()).Status)
		assert.Equal(t, 404, l.get("items/"+k+"/fulltext").Status)
	})
}

func TestLegacyLoginIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.newUser(t, "limited", fullAccess)
	bad := func() resp {
		return e.do(t, req{method: "POST", path: "keys", body: m{"username": "limited", "password": "wrong"}})
	}
	var last resp
	for i := 0; i < 30 && last.Status != 429; i++ {
		last = bad()
		require.Contains(t, []int{403, 429}, last.Status)
	}
	require.Equal(t, 429, last.Status)
	retry, err := strconv.Atoi(last.Header.Get("Retry-After"))
	require.NoError(t, err, "Retry-After must be an integer")
	assert.GreaterOrEqual(t, retry, 1)

	// The correct password is refused too while the budget is spent.
	r := e.do(t, req{method: "POST", path: "keys", body: m{"username": "limited", "password": testPassword}})
	assert.Equal(t, 429, r.Status)
}

func TestLegacyLoginSharesBudgetWithGuard(t *testing.T) {
	var calls []string
	e := newEnv(t, withDeps(func(d *zapi.Deps) {
		d.Login = func(_ context.Context, user, _, ip string) (repo.User, error) {
			calls = append(calls, user+"@"+ip)
			return repo.User{}, &accounts.RateLimitedError{RetryAfter: 300 * time.Millisecond}
		}
	}))
	// A forged X-Forwarded-For must not choose the bucket.
	r := e.do(t, req{
		method: "POST", path: "keys", body: m{"username": "x", "password": "y"},
		header: map[string]string{"X-Forwarded-For": "203.0.113.9"},
	})
	assert.Equal(t, 429, r.Status)
	assert.Equal(t, "1", r.Header.Get("Retry-After"), "sub-second waits round up to one")
	require.Len(t, calls, 1)
	assert.Equal(t, "x@127.0.0.1", calls[0])
}

func TestLegacyLoginWithoutGuardRefuses(t *testing.T) {
	e := newEnv(t, withDeps(func(d *zapi.Deps) { d.Login = nil }))
	e.newUser(t, "nologin", fullAccess)
	r := e.do(t, req{method: "POST", path: "keys", body: m{"username": "nologin", "password": testPassword}})
	assert.Equal(t, 403, r.Status)
}

func TestKeyWithoutNotesAccess(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t, "noter", fullAccess)
	full := e.userLib(t, owner)

	parent := full.create("items", m{"itemType": "book", "title": "parent"})
	childNote := full.create("items", m{
		"itemType": "note", "note": "<p>child</p>", "parentItem": parent,
		"tags": []m{{"tag": "hidden-tag"}},
	})
	loneNote := full.create("items", m{"itemType": "note", "note": "<p>lone</p>"})
	coll := full.create("collections", m{"name": "c"})
	full.create("items", m{"itemType": "note", "note": "in c", "collections": []string{coll}})
	attach := full.create("items", m{
		"itemType": "attachment", "linkMode": "linked_url", "title": "a",
		"url": "http://x", "note": "attachment note", "parentItem": parent,
	})

	// A second key for the same user, without notes.
	raw := zotero.NewAPIKey()
	_, err := e.Store.Keys().Create(ctx(), repo.APIKey{
		UserID: owner.ID, Hash: auth.HashToken(raw), Prefix: raw[:4], Name: "no-notes",
		Access: repo.KeyAccess{Library: true, Notes: false, Write: true, Groups: "all"},
	})
	require.NoError(t, err)
	l := &lib{t: t, e: e, key: raw, path: full.path}

	t.Run("lists leave notes out and counts agree", func(t *testing.T) {
		for _, path := range []string{"items", "items/top", "collections/" + coll + "/items"} {
			r := l.get(path)
			require.Equal(t, 200, r.Status, path)
			for _, it := range r.array(t) {
				d := it.(m)["data"].(m)
				assert.NotEqual(t, "note", d["itemType"], path)
			}
		}
		r := l.get("items")
		assert.Equal(t, "2", r.Header.Get("Total-Results"), "parent and attachment")
		assert.Equal(t, "1", l.get("items/top").Header.Get("Total-Results"))
		assert.Equal(t, "0", l.get("collections/"+coll+"/items").Header.Get("Total-Results"))

		keys := l.get("items?format=keys")
		assert.NotContains(t, string(keys.Body), childNote)
		assert.NotContains(t, string(keys.Body), loneNote)
		assert.Equal(t, "2", keys.Header.Get("Total-Results"))
		vers := l.get("items?format=versions").json(t)
		assert.Len(t, vers, 2)
		assert.NotContains(t, vers, loneNote)
		assert.NotContains(t, string(l.get("items?itemKey="+loneNote+","+parent).Body), loneNote)

		// The owner's full key still sees everything.
		assert.Equal(t, "5", full.get("items").Header.Get("Total-Results"))
	})
	t.Run("single note is 404", func(t *testing.T) {
		assert.Equal(t, 404, l.get("items/"+loneNote).Status)
		assert.Equal(t, 404, l.get("items/"+childNote).Status)
		assert.Equal(t, 200, full.get("items/"+loneNote).Status)
		assert.Equal(t, 404, l.get("items/"+loneNote+"/children").Status)
	})
	t.Run("children and numChildren skip notes", func(t *testing.T) {
		kids := l.get("items/" + parent + "/children").array(t)
		require.Len(t, kids, 1)
		assert.Equal(t, attach, kids[0].(m)["key"])
		meta := l.get("items/" + parent).json(t)["meta"].(m)
		assert.EqualValues(t, 1, meta["numChildren"])
		assert.EqualValues(t, 2, full.get("items/" + parent).json(t)["meta"].(m)["numChildren"])
		cm := l.get("collections/" + coll).json(t)["meta"].(m)
		assert.EqualValues(t, 0, cm["numItems"])
		assert.Equal(t, "attachment note", l.get("items/" + attach).json(t)["data"].(m)["note"],
			"only note items are filtered, not note text on attachments")
	})
	t.Run("tags of hidden notes stay hidden", func(t *testing.T) {
		assert.NotContains(t, string(l.get("tags").Body), "hidden-tag")
		assert.Contains(t, string(full.get("tags").Body), "hidden-tag")
		assert.Equal(t, 404, l.get("tags/hidden-tag").Status)
	})
	t.Run("writes of notes are forbidden", func(t *testing.T) {
		r := l.post("items", []m{{"itemType": "note", "note": "x"}, {"itemType": "book", "title": "ok"}})
		require.Equal(t, 200, r.Status, string(r.Body))
		j := r.json(t)
		assert.EqualValues(t, 403, j["failed"].(m)["0"].(m)["code"])
		assert.Contains(t, j["success"], "1")

		v := full.version()
		r = l.send("PATCH", "items/"+loneNote, m{"note": "changed"})
		assert.Equal(t, 403, r.Status, string(r.Body))
		r = l.send("PUT", "items/"+loneNote, m{"itemType": "note", "note": "changed"})
		assert.Equal(t, 403, r.Status)
		r = l.send("PATCH", "items/"+parent, m{"itemType": "note", "note": "became a note"})
		assert.Equal(t, 403, r.Status, string(r.Body))

		r = l.send("DELETE", "items/"+loneNote, nil, "If-Unmodified-Since-Version", formatInt(v))
		assert.Equal(t, 403, r.Status)
		r = l.send("DELETE", "items?itemKey="+loneNote, nil, "If-Unmodified-Since-Version", formatInt(v))
		assert.Equal(t, 403, r.Status)
		assert.Equal(t, 200, full.get("items/"+loneNote).Status, "note must survive")
		assert.Equal(t, "<p>lone</p>", full.dataOf("items", loneNote)["note"])
	})
}
