package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robertjndw/gotero/conformance"
	"github.com/robertjndw/gotero/internal/repo/pg/pgtest"
	"github.com/robertjndw/gotero/zotero"
)

// TestLoad measures a large library and many concurrent clients. It is slow
// and only runs with GOTERO_LOAD=1:
//
//	GOTERO_LOAD=1 go test -run TestLoad -v -timeout 60m ./internal/server/
//
// GOTERO_LOAD_ITEMS (default 50000), GOTERO_LOAD_CLIENTS (default 50) and
// GOTERO_LOAD_SECONDS (default 60) size it. It fails on any error or 5xx;
// the timings are printed for comparison between runs.
func TestLoad(t *testing.T) {
	if os.Getenv("GOTERO_LOAD") != "1" {
		t.Skip("set GOTERO_LOAD=1")
	}
	items := envInt(t, "GOTERO_LOAD_ITEMS", 50000)
	clients := envInt(t, "GOTERO_LOAD_CLIENTS", 50)
	seconds := envInt(t, "GOTERO_LOAD_SECONDS", 60)
	ctx := t.Context()

	// The production pool size, not pgtest's small default.
	pcfg, err := pgxpool.ParseConfig(pgtest.NewDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	pcfg.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	srv := serveApp(t, simConfig(t), pool)
	prov := simProvisioner(t, srv)
	hc := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	lc := &loadClient{t: t, hc: hc, base: srv.URL + "/api/"}

	owner, err := prov.NewUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	big := fmt.Sprintf("users/%d", owner.ID)

	start := time.Now()
	v := lc.seed(owner, big, items)
	seedTook := time.Since(start)
	t.Logf("seed: %d items in %v (%.0f items/s), library version %d",
		items, seedTook.Round(time.Millisecond), float64(items)/seedTook.Seconds(), v)

	// A new device: the version listing, then every item in batches of 100,
	// one request at a time like Zotero.
	start = time.Now()
	versions := lc.versions(owner, big, 0)
	listTook := time.Since(start)
	keys := slices.Sorted(maps.Keys(versions))
	for i := 0; i < len(keys); i += 100 {
		lc.get(owner, big+"/items", "itemKey="+strings.Join(keys[i:min(i+100, len(keys))], ",")+"&includeTrashed=1")
	}
	fullTook := time.Since(start)
	if len(keys) != items+items/10 {
		t.Errorf("listing has %d items, want %d", len(keys), items+items/10)
	}
	t.Logf("full sync of a new device: version listing %v, everything %v (%.0f items/s)",
		listTook.Round(time.Millisecond), fullTook.Round(time.Millisecond), float64(len(keys))/fullTook.Seconds())
	start = time.Now()
	lc.versions(owner, big, v)
	t.Logf("incremental check with nothing new: %v", time.Since(start).Round(time.Microsecond))

	// Many clients at once: each user syncs and edits its own library, and
	// everyone shares one group, so writes to it contend for its lock.
	users := make([]conformance.User, clients)
	for i := range users {
		if users[i], err = prov.NewUser(ctx); err != nil {
			t.Fatal(err)
		}
	}
	g, err := prov.NewGroup(ctx, users[0], users[1:], nil)
	if err != nil {
		t.Fatal(err)
	}
	group := fmt.Sprintf("groups/%d", g.ID)

	var mu sync.Mutex
	lat := map[string][]time.Duration{}
	record := func(op string, d time.Duration) {
		mu.Lock()
		lat[op] = append(lat[op], d)
		mu.Unlock()
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	var wg sync.WaitGroup
	for i, u := range users {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(i), 7))
			own := fmt.Sprintf("users/%d", u.ID)
			ver := map[string]int64{own: 0, group: 0}
			for time.Now().Before(deadline) && ctx.Err() == nil {
				lib := own
				if rng.IntN(4) == 0 {
					lib = group
				}
				t0 := time.Now()
				vs := lc.versions(u, lib, ver[lib])
				record("sync check", time.Since(t0))
				if len(vs) > 0 {
					t0 = time.Now()
					lc.get(u, lib+"/items", "itemKey="+strings.Join(firstKeys(vs, 100), ",")+"&includeTrashed=1")
					record("download", time.Since(t0))
				}
				t0 = time.Now()
				nv, ok := lc.write(u, lib, ver[lib], newItems(rng, 1+rng.IntN(10)))
				if ok {
					record("upload", time.Since(t0))
					ver[lib] = nv
				} else {
					record("upload (412, retried)", time.Since(t0))
					ver[lib] = lc.version(u, lib)
				}
				time.Sleep(time.Duration(rng.IntN(200)) * time.Millisecond)
			}
		})
	}
	wg.Wait()
	for _, op := range slices.Sorted(maps.Keys(lat)) {
		ds := lat[op]
		slices.Sort(ds)
		t.Logf("%d clients, %-22s n=%-6d p50=%-8v p95=%-8v p99=%-8v max=%v", clients, op, len(ds),
			pct(ds, 50), pct(ds, 95), pct(ds, 99), ds[len(ds)-1].Round(time.Millisecond))
	}
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	t.Logf("heap in use after the run: %d MiB (includes the test's own clients)", ms.HeapInuse>>20)
}

type loadClient struct {
	t    *testing.T
	hc   *http.Client
	base string
}

type loadResponse struct {
	status int
	header http.Header
	body   []byte
}

func (lc *loadClient) lmv(r loadResponse) int64 {
	lc.t.Helper()
	v, err := strconv.ParseInt(r.header.Get("Last-Modified-Version"), 10, 64)
	if err != nil {
		lc.t.Fatal(err)
	}
	return v
}

func (lc *loadClient) do(u conformance.User, method, path, query string, body []byte, hdr map[string]string) loadResponse {
	lc.t.Helper()
	url := lc.base + path
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(lc.t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		lc.t.Fatal(err)
	}
	req.Header.Set("Zotero-API-Key", u.APIKey)
	req.Header.Set("Zotero-API-Version", "3")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := lc.hc.Do(req)
	if err != nil {
		lc.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		lc.t.Fatalf("%s %s: %v", method, path, err)
	}
	if resp.StatusCode >= 500 {
		lc.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return loadResponse{resp.StatusCode, resp.Header, b}
}

func (lc *loadClient) get(u conformance.User, path, query string) []byte {
	r := lc.do(u, http.MethodGet, path, query, nil, nil)
	if r.status != http.StatusOK {
		lc.t.Fatalf("GET %s: %d %s", path, r.status, r.body)
	}
	return r.body
}

func (lc *loadClient) versions(u conformance.User, lib string, since int64) map[string]int64 {
	var out map[string]int64
	b := lc.get(u, lib+"/items", "format=versions&includeTrashed=1&since="+strconv.FormatInt(since, 10))
	if err := json.Unmarshal(b, &out); err != nil {
		lc.t.Fatal(err)
	}
	return out
}

func (lc *loadClient) version(u conformance.User, lib string) int64 {
	r := lc.do(u, http.MethodGet, lib+"/items", "format=versions&since=999999999999", nil, nil)
	return lc.lmv(r)
}

// write uploads items and reports the new library version, or false on a
// 412 from a concurrent writer.
func (lc *loadClient) write(u conformance.User, lib string, since int64, items []map[string]any) (int64, bool) {
	body, err := json.Marshal(items)
	if err != nil {
		lc.t.Fatal(err)
	}
	r := lc.do(u, http.MethodPost, lib+"/items", "", body, map[string]string{
		"Content-Type": "application/json", "If-Unmodified-Since-Version": strconv.FormatInt(since, 10),
	})
	switch r.status {
	case http.StatusPreconditionFailed:
		return 0, false
	case http.StatusOK:
	default:
		lc.t.Fatalf("POST %s/items: %d %s", lib, r.status, r.body)
	}
	var wr struct {
		Failed map[string]any `json:"failed"`
	}
	if err := json.Unmarshal(r.body, &wr); err != nil || len(wr.Failed) > 0 {
		lc.t.Fatalf("POST %s/items: %s", lib, r.body)
	}
	return lc.lmv(r), true
}

// seed fills a library with n items, a tenth of them with a child note,
// spread over 200 collections, in the 50-object batches Zotero uploads.
func (lc *loadClient) seed(u conformance.User, lib string, n int) int64 {
	rng := rand.New(rand.NewPCG(1, 2))
	var cols []map[string]any
	var colKeys []string
	for i := range 200 {
		k := loadKey(rng)
		colKeys = append(colKeys, k)
		cols = append(cols, map[string]any{"key": k, "name": fmt.Sprintf("collection %d", i)})
	}
	var v int64
	for i := 0; i < len(cols); i += 50 {
		body, _ := json.Marshal(cols[i:min(i+50, len(cols))])
		r := lc.do(u, http.MethodPost, lib+"/collections", "", body, map[string]string{
			"Content-Type": "application/json", "If-Unmodified-Since-Version": strconv.FormatInt(v, 10),
		})
		if r.status != http.StatusOK {
			lc.t.Fatalf("seed collections: %d %s", r.status, r.body)
		}
		v = lc.lmv(r)
	}
	var batch []map[string]any
	flush := func() {
		if len(batch) == 0 {
			return
		}
		nv, ok := lc.write(u, lib, v, batch)
		if !ok {
			lc.t.Fatal("412 while seeding a library nobody else writes to")
		}
		v, batch = nv, nil
	}
	for i := range n {
		it := newItems(rng, 1)[0]
		it["collections"] = []string{colKeys[rng.IntN(len(colKeys))]}
		batch = append(batch, it)
		if i%10 == 0 {
			flush() // the parent has to exist before the note
			batch = append(batch, map[string]any{
				"itemType": "note", "parentItem": it["key"], "note": "<p>note " + strconv.Itoa(i) + "</p>",
			})
		}
		if len(batch) >= 50 {
			flush()
		}
	}
	flush()
	return v
}

func newItems(rng *rand.Rand, n int) []map[string]any {
	out := make([]map[string]any, n)
	for i := range out {
		out[i] = map[string]any{
			"key": loadKey(rng), "itemType": "journalArticle",
			"title":    fmt.Sprintf("A study of %d things", rng.IntN(1_000_000)),
			"creators": []any{map[string]any{"creatorType": "author", "firstName": "Ada", "lastName": "Lovelace"}},
			"date":     "2024", "publicationTitle": "Journal of Load", "abstractNote": strings.Repeat("words ", 40),
			"tags": []any{map[string]any{"tag": "t" + strconv.Itoa(rng.IntN(500))}},
		}
	}
	return out
}

func loadKey(rng *rand.Rand) string {
	b := make([]byte, zotero.KeyLength)
	for i := range b {
		b[i] = zotero.KeyAlphabet[rng.IntN(len(zotero.KeyAlphabet))]
	}
	return string(b)
}

func firstKeys(m map[string]int64, n int) []string {
	keys := make([]string, 0, min(n, len(m)))
	for k := range m {
		if len(keys) == n {
			break
		}
		keys = append(keys, k)
	}
	return keys
}

func pct(sorted []time.Duration, p int) time.Duration {
	return sorted[(len(sorted)-1)*p/100].Round(100 * time.Microsecond)
}

func envInt(t *testing.T, name string, def int) int {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		t.Fatalf("%s: want a positive number, got %q", name, s)
	}
	return v
}
