package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatalf("unexpected format %q", h)
	}
	if ok, err := VerifyPassword(h, "correct horse battery"); !ok || err != nil {
		t.Fatalf("verify failed: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "wrong"); ok {
		t.Fatal("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("salt not random")
	}
}

func TestVerifyRejectsTamperedHashes(t *testing.T) {
	for _, h := range []string{
		"", "plain", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=999999999,t=1,p=1$c2FsdA$aGFzaA", // memory bomb
		"$argon2id$v=19$m=1024,t=99,p=1$c2FsdA$aGFzaA",     // cpu bomb
		"$argon2id$v=19$m=1024,t=1,p=1$!!$aGFzaA",
	} {
		if ok, err := VerifyPassword(h, "x"); ok || err == nil {
			t.Errorf("%q: ok=%v err=%v", h, ok, err)
		}
	}
}

func TestKeyFromRequest(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?key=q", nil)
	if KeyFromRequest(r) != "q" {
		t.Fatal("query key")
	}
	r.Header.Set("Authorization", "Bearer b")
	if KeyFromRequest(r) != "b" {
		t.Fatal("bearer key")
	}
	r.Header.Set("Zotero-API-Key", "h")
	if KeyFromRequest(r) != "h" {
		t.Fatal("header key wins")
	}
}

type fakeKeys struct {
	repo.KeyRepo
	keys  map[string]repo.APIKey
	calls int
}

func (f *fakeKeys) GetByHash(_ context.Context, h []byte) (repo.APIKey, error) {
	f.calls++
	k, ok := f.keys[string(h)]
	if !ok {
		return repo.APIKey{}, repo.ErrNotFound
	}
	return k, nil
}
func (f *fakeKeys) Touch(context.Context, int64, time.Time) error { return nil }

type fakeUsers struct {
	repo.UserRepo
	users map[int64]repo.User
}

func (f *fakeUsers) Get(_ context.Context, id int64) (repo.User, error) {
	u, ok := f.users[id]
	if !ok {
		return repo.User{}, repo.ErrNotFound
	}
	return u, nil
}

func TestAuthenticatorCacheAndInvalidation(t *testing.T) {
	keys := &fakeKeys{keys: map[string]repo.APIKey{string(HashToken("good")): {ID: 1, UserID: 7}}}
	users := &fakeUsers{users: map[int64]repo.User{7: {ID: 7, Username: "u"}}}
	a := NewAuthenticator(keys, users)
	ctx := context.Background()

	if _, err := a.Authenticate(ctx, ""); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := a.Authenticate(ctx, "bad"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("bad: %v", err)
	}
	p, err := a.Authenticate(ctx, "good")
	if err != nil || p.User.ID != 7 || p.Raw != "good" {
		t.Fatalf("good: %+v %v", p, err)
	}
	before := keys.calls
	if _, err := a.Authenticate(ctx, "good"); err != nil || keys.calls != before {
		t.Fatal("second lookup should hit the cache")
	}

	// Revocation must be visible immediately once invalidated.
	delete(keys.keys, string(HashToken("good")))
	a.Invalidate(HashToken("good"))
	if _, err := a.Authenticate(ctx, "good"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("revoked key still accepted: %v", err)
	}

	keys.keys[string(HashToken("good"))] = repo.APIKey{ID: 1, UserID: 7}
	_, _ = a.Authenticate(ctx, "good")
	users.users[7] = repo.User{ID: 7, Disabled: true}
	a.InvalidateUser(7)
	if _, err := a.Authenticate(ctx, "good"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("disabled user accepted: %v", err)
	}
}

func TestAuthenticatorCacheExpires(t *testing.T) {
	keys := &fakeKeys{keys: map[string]repo.APIKey{string(HashToken("k")): {ID: 1, UserID: 1}}}
	users := &fakeUsers{users: map[int64]repo.User{1: {ID: 1}}}
	a := NewAuthenticator(keys, users)
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	_, _ = a.Authenticate(context.Background(), "k")
	now = now.Add(time.Minute)
	before := keys.calls
	_, _ = a.Authenticate(context.Background(), "k")
	if keys.calls == before {
		t.Fatal("expired entry served from cache")
	}
}

func TestLibraryAccess(t *testing.T) {
	full := repo.KeyAccess{Library: true, Notes: true, Write: true, Files: true, Groups: "all"}
	readOnly := repo.KeyAccess{Library: true, Groups: "read"}
	p := func(a repo.KeyAccess) Principal {
		return Principal{User: repo.User{ID: 1}, Key: repo.APIKey{Access: a}}
	}

	if got := UserLibraryAccess(p(full), 2); got != (LibraryAccess{}) {
		t.Fatalf("foreign user library accessible: %+v", got)
	}
	if got := UserLibraryAccess(p(full), 1); !got.Write || !got.FileWrite {
		t.Fatalf("own library: %+v", got)
	}
	if got := UserLibraryAccess(p(readOnly), 1); got.Write || got.FileWrite || got.FileRead {
		t.Fatalf("read-only key: %+v", got)
	}

	g := repo.Group{LibraryReading: "members", LibraryEditing: "admins", FileEditing: "admins"}
	cases := []struct {
		name string
		key  repo.KeyAccess
		role string
		g    repo.Group
		want LibraryAccess
	}{
		{"non-member private", full, "", g, LibraryAccess{}},
		{"member admins-edit", full, "member", g, LibraryAccess{Read: true, Notes: true, FileRead: true}},
		{"admin", full, "admin", g, LibraryAccess{Read: true, Write: true, Notes: true, FileRead: true, FileWrite: true}},
		{"admin read key", readOnly, "admin", g, LibraryAccess{Read: true, Notes: true, FileRead: true}},
		{"no group access", repo.KeyAccess{Library: true, Groups: "none"}, "owner", g, LibraryAccess{}},
		{"public group outsider", full, "", repo.Group{LibraryReading: "all", LibraryEditing: "members", FileEditing: "members"}, LibraryAccess{Read: true, Notes: true, FileRead: true}},
		{"member members-edit files none", full, "member", repo.Group{LibraryReading: "members", LibraryEditing: "members", FileEditing: "none"}, LibraryAccess{Read: true, Write: true, Notes: true, FileRead: true}},
		{"owner files none", full, "owner", repo.Group{LibraryReading: "members", LibraryEditing: "members", FileEditing: "none"}, LibraryAccess{Read: true, Write: true, Notes: true, FileRead: true}},
	}
	for _, c := range cases {
		if got := GroupLibraryAccess(p(c.key), c.g, c.role); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

// A key deleted straight from the database (the CLI, another instance)
// stays cached for the TTL. Stream revalidation exists to catch exactly that,
// so its lookup must not be answered from the cache.
func TestAuthenticateFreshIgnoresCache(t *testing.T) {
	keys := &fakeKeys{keys: map[string]repo.APIKey{string(HashToken("k")): {ID: 1, UserID: 1}}}
	users := &fakeUsers{users: map[int64]repo.User{1: {ID: 1}}}
	a := NewAuthenticator(keys, users)
	if _, err := a.Authenticate(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	delete(keys.keys, string(HashToken("k")))

	if _, err := a.Authenticate(context.Background(), "k"); err != nil {
		t.Fatalf("cached lookup should still succeed within the TTL: %v", err)
	}
	if _, err := a.AuthenticateFresh(context.Background(), "k"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("fresh lookup of a deleted key: got %v, want ErrInvalidKey", err)
	}
}

// Write, Notes and Files are refinements of Library, not grants of their own:
// a key without library access must not reach the library through them.
func TestUserLibraryAccessNeedsLibraryFlag(t *testing.T) {
	p := Principal{User: repo.User{ID: 1}, Key: repo.APIKey{Access: repo.KeyAccess{Notes: true, Files: true, Write: true, Groups: "all"}}}
	if got := UserLibraryAccess(p, 1); got != (LibraryAccess{}) {
		t.Fatalf("key without library access got %+v", got)
	}
}

func TestVerifyRejectsMalformedHashSegments(t *testing.T) {
	good, err := HashPassword("pw-for-the-test")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	for name, h := range map[string]string{
		// An empty derived key would compare equal to an empty argon2 output,
		// so every password would match.
		"empty hash":     strings.Join(append(parts[:5:5], ""), "$"),
		"old version":    strings.Replace(good, "$v=19$", "$v=16$", 1),
		"future version": strings.Replace(good, "$v=19$", "$v=20$", 1),
	} {
		for _, pw := range []string{"pw-for-the-test", ""} {
			ok, err := VerifyPassword(h, pw)
			if ok || !errors.Is(err, ErrInvalidHash) {
				t.Errorf("%s, password %q: ok=%v err=%v, want ErrInvalidHash", name, pw, ok, err)
			}
		}
	}
}

// Clearing the whole cache when it fills is a spray defence; below the cap
// every valid key must stay cached alongside the others.
func TestAuthenticatorCachesSeveralKeys(t *testing.T) {
	keys := &fakeKeys{keys: map[string]repo.APIKey{
		string(HashToken("one")): {ID: 1, UserID: 7},
		string(HashToken("two")): {ID: 2, UserID: 7},
	}}
	users := &fakeUsers{users: map[int64]repo.User{7: {ID: 7}}}
	a := NewAuthenticator(keys, users)
	ctx := context.Background()
	for _, k := range []string{"one", "two"} {
		if _, err := a.Authenticate(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	before := keys.calls
	for _, k := range []string{"one", "two"} {
		if p, err := a.Authenticate(ctx, k); err != nil || p.Raw != k {
			t.Fatalf("%s: %+v %v", k, p, err)
		}
	}
	if keys.calls != before {
		t.Fatalf("cached keys were looked up again: %d extra DB calls", keys.calls-before)
	}
}
