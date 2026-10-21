package zapi_test

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/robertjndw/gotero/internal/zapi"
)

func TestHugeArrayBodyIsBounded(t *testing.T) {
	const size = 32 << 20
	e := newEnv(t, withDeps(func(d *zapi.Deps) { d.MaxRequestBytes = size }))
	u := e.newUser(t, "bounded", fullAccess)
	l := e.userLib(t, u)

	// "0," is two bytes, so this is about 32 MiB of array elements.
	body := append([]byte{'['}, bytes.Repeat([]byte("0,"), size/2-2)...)
	body = append(body, '0', ']')

	for _, path := range []string{"items", "collections", "fulltext"} {
		t.Run(path, func(t *testing.T) {
			hdr := map[string]string{"If-Unmodified-Since-Version": "0"}
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			r := e.do(t, req{method: "POST", path: l.path + "/" + path, key: l.key, raw: body, header: hdr})
			runtime.ReadMemStats(&after)

			assert.Equal(t, 413, r.Status, string(r.Body))
			// Unmarshalling into []json.RawMessage used to cost about 2.4 GB here.
			alloc := after.TotalAlloc - before.TotalAlloc
			assert.Less(t, alloc, uint64(64<<20), "allocated %d MiB", alloc>>20)
		})
	}
}

func TestArrayBodyLimits(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "limits", fullAccess)
	l := e.userLib(t, u)

	elems := func(n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(`{"itemType":"note","note":"x"},`, n), ",") + "]"
	}
	post := func(raw string) resp {
		return e.do(t, req{method: "POST", path: l.path + "/items", key: l.key, raw: []byte(raw)})
	}
	assert.Equal(t, 200, post(elems(50)).Status)
	assert.Equal(t, 413, post(elems(51)).Status)
	assert.Equal(t, 400, post(`{"a":1}`).Status)
	assert.Equal(t, 400, post(`[{"itemType":"note"}] trailing`).Status)
	assert.Equal(t, 400, post(`[{"itemType":"note"}`).Status)
}

func TestDeepNestingIsRejected(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "deep", fullAccess)
	l := e.userLib(t, u)

	deep := strings.Repeat("[", 200) + strings.Repeat("]", 200)
	key := l.create("items", m{"itemType": "note", "note": "x"})

	r := e.do(t, req{
		method: "POST", path: l.path + "/items", key: l.key,
		raw: []byte(fmt.Sprintf(`[{"itemType":"note","note":"x","relations":{"a":%s}}]`, deep)),
	})
	assert.Equal(t, 400, r.Status, string(r.Body))

	r = e.do(t, req{
		method: "PATCH", path: l.path + "/items/" + key, key: l.key,
		raw: []byte(fmt.Sprintf(`{"relations":{"a":%s}}`, deep)),
	})
	assert.Equal(t, 400, r.Status, string(r.Body))

	r = e.do(t, req{
		method: "POST", path: l.path + "/settings", key: l.key,
		raw: []byte(fmt.Sprintf(`{"s":{"value":%s}}`, deep)),
	})
	assert.Equal(t, 400, r.Status, string(r.Body))
}

func TestObjectBodyKeyLimits(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "keys", fullAccess)
	l := e.userLib(t, u)
	key := l.create("items", m{"itemType": "note", "note": "x"})

	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&sb, `"k%d":1,`, i)
	}
	sb.WriteString(`"note":"y"}`)
	r := e.do(t, req{method: "PATCH", path: l.path + "/items/" + key, key: l.key, raw: []byte(sb.String())})
	assert.Equal(t, 413, r.Status, string(r.Body))

	// Settings: too many keys, and one oversized value.
	sb.Reset()
	sb.WriteString("{")
	for i := 0; i < 51; i++ {
		fmt.Fprintf(&sb, `"s%d":{"value":1},`, i)
	}
	sb.WriteString(`"last":{"value":1}}`)
	r = e.do(t, req{method: "POST", path: l.path + "/settings", key: l.key, raw: []byte(sb.String())})
	assert.Equal(t, 413, r.Status, string(r.Body))

	big := fmt.Sprintf(`{"big":{"value":"%s"}}`, strings.Repeat("a", 300<<10))
	r = e.do(t, req{method: "POST", path: l.path + "/settings", key: l.key, raw: []byte(big)})
	assert.Equal(t, 413, r.Status, string(r.Body))

	ok := e.do(t, req{method: "POST", path: l.path + "/settings", key: l.key, raw: []byte(`{"fine":{"value":1}}`)})
	assert.Equal(t, 204, ok.Status, string(ok.Body))
}
