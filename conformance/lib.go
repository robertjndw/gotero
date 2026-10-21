package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// lib is one library (user or group) accessed with one user's key.
type lib struct {
	e    *env
	u    User
	path string // "users/1" or "groups/2"
}

func (e *env) provisionErr(t testing.TB, what string, err error) {
	t.Helper()
	if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrNotEnoughUsers) {
		t.Skipf("provisioner cannot %s: %v", what, err)
	}
	t.Fatalf("provisioner %s: %v", what, err)
}

func (e *env) newUser(t testing.TB) User {
	t.Helper()
	u, err := e.prov.NewUser(ctxOf(t))
	if err != nil {
		e.provisionErr(t, "create user", err)
	}
	return u
}

func (e *env) newGroup(t testing.TB, owner User, members, admins []User) Group {
	t.Helper()
	g, err := e.prov.NewGroup(ctxOf(t), owner, members, admins)
	if err != nil {
		e.provisionErr(t, "create group", err)
	}
	return g
}

// userLib provisions a fresh user and returns their personal library.
func (e *env) userLib(t testing.TB) *lib {
	t.Helper()
	u := e.newUser(t)
	return e.libFor(u)
}

func (e *env) libFor(u User) *lib {
	return &lib{e: e, u: u, path: fmt.Sprintf("users/%d", u.ID)}
}

func (e *env) groupLib(g Group, as User) *lib {
	return &lib{e: e, u: as, path: fmt.Sprintf("groups/%d", g.ID)}
}

func (l *lib) do(t testing.TB, method, rel string, opts ...reqOpt) *response {
	t.Helper()
	all := append([]reqOpt{withKey(l.u.APIKey)}, opts...)
	return l.e.do(t, method, l.path+"/"+rel, all...)
}

func (l *lib) get(t testing.TB, rel string, opts ...reqOpt) *response {
	t.Helper()
	return l.do(t, "GET", rel, opts...)
}

func (l *lib) post(t testing.TB, rel string, body any, opts ...reqOpt) *response {
	t.Helper()
	return l.do(t, "POST", rel, append([]reqOpt{withJSON(body)}, opts...)...)
}

// version reads the current library version.
func (l *lib) version(t testing.TB) int64 {
	t.Helper()
	r := l.get(t, "items?limit=1&format=keys")
	mustStatus(t, r, 200)
	return versionHeader(t, r)
}

// write posts objects and requires a 200 write response.
func (l *lib) write(t testing.TB, typ string, objs []obj, opts ...reqOpt) obj {
	t.Helper()
	r := l.post(t, typ, objs, opts...)
	mustStatus(t, r, 200)
	return parseObj(t, r)
}

// create writes one object and returns its key and new version.
func (l *lib) create(t testing.TB, typ string, o obj) (string, int64) {
	t.Helper()
	res := l.write(t, typ, []obj{o})
	succ := at[obj](t, res, "successful", "0")
	return at[string](t, succ, "key"), atInt(t, succ, "version")
}

// createN creates n items in batches of 50 and returns their keys.
func (l *lib) createN(t testing.TB, n int, mk func(i int) obj) []string {
	t.Helper()
	var keys []string
	for start := 0; start < n; start += 50 {
		var batch []obj
		for i := start; i < n && i < start+50; i++ {
			batch = append(batch, mk(i))
		}
		res := l.write(t, "items", batch)
		succ := at[obj](t, res, "successful")
		if len(succ) != len(batch) {
			t.Fatalf("created %d of %d items: %v", len(succ), len(batch), res)
		}
		for i := range batch {
			keys = append(keys, at[string](t, succ, fmt.Sprint(i), "key"))
		}
	}
	return keys
}

func (l *lib) item(t testing.TB, key string) obj {
	t.Helper()
	r := l.get(t, "items/"+key)
	mustStatus(t, r, 200)
	return parseObj(t, r)
}

func joinKeys(k []string) string { return strings.Join(k, ",") }

func esc(s string) string { return url.QueryEscape(s) }

func ctxOf(t testing.TB) context.Context {
	if c, ok := t.(interface{ Context() context.Context }); ok {
		return c.Context()
	}
	return context.Background()
}
