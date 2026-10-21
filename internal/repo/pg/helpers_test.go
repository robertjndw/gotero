package pg_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/repo/pg"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func newStore(t testing.TB, opts ...pg.Option) (*pg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := pgtest.New(t)
	return pg.New(pool, opts...), pool
}

func ctx() context.Context { return context.Background() }

func mustUser(t testing.TB, s *pg.Store, name string) repo.User {
	t.Helper()
	u, err := s.Users().Create(ctx(), repo.User{Username: name, PasswordHash: "x"})
	require.NoError(t, err)
	return u
}

// write runs fn in a library transaction and fails the test on error.
func write(t testing.TB, s *pg.Store, lib int64, fn func(tx repo.LibraryTx) error) int64 {
	t.Helper()
	v, err := s.InLibrary(ctx(), lib, fn)
	require.NoError(t, err)
	return v
}

func item(key string, ver int64, mutate func(*repo.Object)) repo.Object {
	o := repo.Object{
		Key: key, Version: ver, ItemType: "book",
		Data:        json.RawMessage(fmt.Sprintf(`{"key":%q,"itemType":"book"}`, key)),
		ContentHash: []byte(key),
	}
	if mutate != nil {
		mutate(&o)
	}
	return o
}

func itemData(key, added, modified string, collections ...string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"key": key, "dateAdded": added, "dateModified": modified, "collections": collections,
	})
	return b
}

func collectVersions(t testing.TB, r repo.LibraryReader, q repo.VersionsQuery) []repo.KeyVersion {
	t.Helper()
	seq, err := r.Versions(ctx(), q)
	require.NoError(t, err)
	var out []repo.KeyVersion
	for kv, err := range seq {
		require.NoError(t, err)
		out = append(out, kv)
	}
	return out
}

func collectObjects(t testing.TB, r repo.LibraryReader, q repo.ListQuery) []repo.Object {
	t.Helper()
	seq, err := r.Objects(ctx(), q)
	require.NoError(t, err)
	var out []repo.Object
	for o, err := range seq {
		require.NoError(t, err)
		out = append(out, o)
	}
	return out
}

func keysOf(objs []repo.Object) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Key
	}
	return out
}

func kvKeys(kvs []repo.KeyVersion) []string {
	out := make([]string, len(kvs))
	for i, kv := range kvs {
		out[i] = kv.Key
	}
	return out
}

func contextWithCancel() (context.Context, context.CancelFunc) { return context.WithCancel(ctx()) }
