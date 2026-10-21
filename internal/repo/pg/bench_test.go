package pg_test

import (
	"testing"

	"github.com/robertjndw/gotero/internal/repo"
)

// BenchmarkVersions100k measures format=versions on a large library, the
// request a Zotero client makes on every full sync.
func BenchmarkVersions100k(b *testing.B) {
	s, pool := newStore(b)
	u, err := s.Users().Create(ctx(), repo.User{Username: "bench", PasswordHash: "x"})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := pool.Exec(ctx(), `
		INSERT INTO items (library_id, key, version, item_type, data, content_hash)
		SELECT $1, 'K' || lpad(g::text, 7, '0'), g, 'book', '{}', '\x00'
		FROM generate_series(1, 100000) g`, u.LibraryID); err != nil {
		b.Fatal(err)
	}
	if _, err := pool.Exec(ctx(), `VACUUM ANALYZE items`); err != nil {
		b.Fatal(err)
	}
	r, err := s.Library(ctx(), u.LibraryID)
	if err != nil {
		b.Fatal(err)
	}
	q := repo.VersionsQuery{Type: repo.TypeItem, IncludeTrashed: true}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		seq, err := r.Versions(ctx(), q)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for _, err := range seq {
			if err != nil {
				b.Fatal(err)
			}
			n++
		}
		if n != 100000 {
			b.Fatalf("got %d rows", n)
		}
	}
}
