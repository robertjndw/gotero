package zapi_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkVersions100k measures what a full sync downloads first:
// items?format=versions over HTTP on a 100k item library.
func BenchmarkVersions100k(b *testing.B) {
	e := newEnv(b)
	u := e.newUser(b, "bench", fullAccess)
	_, err := e.Pool.Exec(ctx(), `
		INSERT INTO items (library_id, key, version, item_type, data, content_hash)
		SELECT $1, 'K' || lpad(g::text, 7, '0'), g, 'book', '{"itemType":"book"}', '\x00'
		FROM generate_series(1, 100000) g`, u.LibraryID)
	require.NoError(b, err)
	_, err = e.Pool.Exec(ctx(), `UPDATE libraries SET version = 100000 WHERE id = $1`, u.LibraryID)
	require.NoError(b, err)
	_, err = e.Pool.Exec(ctx(), `VACUUM ANALYZE items`)
	require.NoError(b, err)

	url := e.URL + "/api/" + "users/" + itoa(u.ID) + "/items?format=versions&includeTrashed=1"
	hc := e.client()
	b.ReportAllocs()
	b.ResetTimer()
	var size int64
	for range b.N {
		r, err := http.NewRequestWithContext(ctx(), http.MethodGet, url, nil)
		require.NoError(b, err)
		r.Header.Set("Zotero-API-Key", u.Key)
		res, err := hc.Do(r)
		require.NoError(b, err)
		size, err = io.Copy(io.Discard, res.Body)
		require.NoError(b, err)
		_ = res.Body.Close()
		require.Equal(b, 200, res.StatusCode)
	}
	b.ReportMetric(float64(size)/1e6, "MB/response")
}

func itoa(n int64) string { return formatInt(n) }
