package zapi_test

import (
	"archive/zip"
	"bytes"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // md5 is the Zotero file-sync checksum
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/storage"
)

func sum(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // see import
	return hex.EncodeToString(s[:])
}

type payload struct {
	name  string
	data  []byte
	md5   string
	mtime int64
}

func newPayload(name string, data []byte) payload {
	return payload{name: name, data: data, md5: sum(data), mtime: 1_700_000_000_123}
}

func (p payload) form() url.Values {
	return url.Values{
		"md5": {p.md5}, "filename": {p.name}, "filesize": {strconv.Itoa(len(p.data))},
		"mtime": {strconv.FormatInt(p.mtime, 10)},
	}
}

func (e *testEnv) attachment(t testing.TB, l *lib, name string) string {
	t.Helper()
	return l.create("items", m{
		"itemType": "attachment", "linkMode": "imported_file", "title": name,
		"filename": name, "contentType": "application/pdf",
	})
}

func (e *testEnv) form(t testing.TB, l *lib, key string, form url.Values, h ...string) resp {
	t.Helper()
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for i := 0; i+1 < len(h); i += 2 {
		hdr[h[i]] = h[i+1]
	}
	return e.do(t, req{method: "POST", path: l.path + "/items/" + key + "/file", key: l.key, raw: []byte(form.Encode()), header: hdr})
}

type authz struct {
	URL, UploadKey string
}

func (e *testEnv) authorize(t testing.TB, l *lib, key string, p payload) authz {
	t.Helper()
	r := e.form(t, l, key, p.form(), "If-None-Match", "*")
	require.Equal(t, 200, r.Status, string(r.Body))
	j := r.json(t)
	require.NotContains(t, j, "exists", string(r.Body))
	return authz{URL: j["url"].(string), UploadKey: j["uploadKey"].(string)}
}

func (e *testEnv) put(t testing.TB, target string, body io.Reader, size int64) resp {
	t.Helper()
	hr, err := http.NewRequestWithContext(ctx(), http.MethodPost, target, body)
	require.NoError(t, err)
	hr.ContentLength = size
	hr.Header.Set("Content-Type", "application/octet-stream")
	res, err := e.client().Do(hr)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return resp{Status: res.StatusCode, Header: res.Header, Body: b}
}

func (e *testEnv) register(t testing.TB, l *lib, key, uploadKey string, h ...string) resp {
	t.Helper()
	return e.form(t, l, key, url.Values{"upload": {uploadKey}}, h...)
}

// store runs the whole authorize, upload, register dance.
func (e *testEnv) store(t testing.TB, l *lib, key string, p payload) {
	t.Helper()
	a := e.authorize(t, l, key, p)
	require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
	r := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
	require.Equal(t, 204, r.Status, string(r.Body))
}

func (e *testEnv) used(t testing.TB, u user) int64 {
	t.Helper()
	r, err := e.Store.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	return r.StorageUsed()
}

func (e *testEnv) setQuota(t testing.TB, u user, q int64) {
	t.Helper()
	got, err := e.Store.Users().Get(ctx(), u.ID)
	require.NoError(t, err)
	got.QuotaBytes = &q
	_, err = e.Store.Users().Update(ctx(), got)
	require.NoError(t, err)
}

// patternReader yields n bytes of a repeating block without holding them.
type patternReader struct {
	block []byte
	left  int
	pos   int
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(b) && p.left > 0 {
		c := copy(b[n:], p.block[p.pos:])
		if c > p.left {
			c = p.left
		}
		n += c
		p.left -= c
		p.pos = (p.pos + c) % len(p.block)
	}
	return n, nil
}

func TestUploadStreamsWithBoundedHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("64 MiB upload skipped in -short mode")
	}
	const size = 64 << 20
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "big.pdf")

	block := make([]byte, 1<<20)
	for i := range block {
		block[i] = byte(i*31 + i>>8)
	}
	h := md5.New() //nolint:gosec // see import
	_, _ = io.Copy(h, &patternReader{block: block, left: size})
	p := payload{name: "big.pdf", md5: hex.EncodeToString(h.Sum(nil)), mtime: 1}
	form := url.Values{"md5": {p.md5}, "filename": {p.name}, "filesize": {strconv.Itoa(size)}, "mtime": {"1"}}
	r := e.form(t, l, key, form, "If-None-Match", "*")
	require.Equal(t, 200, r.Status, string(r.Body))
	a := r.json(t)

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	up := e.put(t, a["url"].(string), &patternReader{block: block, left: size}, size)
	close(stop)
	<-done
	require.Equal(t, 201, up.Status, string(up.Body))

	growth := int64(peak.Load()) - int64(base.HeapInuse)
	t.Logf("heap growth while uploading %d MiB: %.1f MiB", size>>20, float64(growth)/(1<<20))
	assert.Less(t, growth, int64(32<<20))

	r = e.register(t, l, key, a["uploadKey"].(string), "If-None-Match", "*")
	require.Equal(t, 204, r.Status, string(r.Body))
	assert.Equal(t, int64(size), e.used(t, u))
}

func TestConcurrentUploadsOfSameContent(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	p := newPayload("same.pdf", bytes.Repeat([]byte("same bytes "), 50_000))

	const n = 6
	keys := make([]string, n)
	auths := make([]authz, n)
	for i := range keys {
		keys[i] = e.attachment(t, l, "same.pdf")
		auths[i] = e.authorize(t, l, keys[i], p)
	}
	var wg sync.WaitGroup
	for i := range keys {
		wg.Go(func() {
			r := e.put(t, auths[i].URL, bytes.NewReader(p.data), int64(len(p.data)))
			assert.Equal(t, 201, r.Status, string(r.Body))
			r = e.register(t, l, keys[i], auths[i].UploadKey, "If-None-Match", "*")
			assert.Equal(t, 204, r.Status, string(r.Body))
		})
	}
	wg.Wait()

	assert.Equal(t, int64(n*len(p.data)), e.used(t, u))
	var blobs int
	require.NoError(t, e.Pool.QueryRow(ctx(), `SELECT count(*) FROM blobs WHERE md5 = $1`, p.md5).Scan(&blobs))
	assert.Equal(t, 1, blobs)
	for _, k := range keys {
		r := l.get("items/" + k + "/file")
		require.Equal(t, 302, r.Status)
		res, err := e.client().Get(r.Header.Get("Location"))
		require.NoError(t, err)
		got, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		assert.Equal(t, p.data, got)
	}
}

func TestQuotaBoundary(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	e.setQuota(t, u, 1000)

	first := e.attachment(t, l, "a.pdf")
	e.store(t, l, first, newPayload("a.pdf", bytes.Repeat([]byte("a"), 1000)))
	assert.EqualValues(t, 1000, e.used(t, u))

	second := e.attachment(t, l, "b.pdf")
	r := e.form(t, l, second, newPayload("b.pdf", []byte("b")).form(), "If-None-Match", "*")
	assert.Equal(t, 413, r.Status)
	assert.Equal(t, "1", r.Header.Get("Zotero-Storage-Usage"))
	assert.Equal(t, "1", r.Header.Get("Zotero-Storage-Quota"))

	// Replacing a file only counts the difference, so same size still fits.
	repl := newPayload("a.pdf", bytes.Repeat([]byte("z"), 1000))
	cur := l.dataOf("items", first)["md5"].(string)
	r = e.form(t, l, first, repl.form(), "If-Match", cur)
	assert.Equal(t, 200, r.Status, string(r.Body))
	repl = newPayload("a.pdf", bytes.Repeat([]byte("z"), 1001))
	r = e.form(t, l, first, repl.form(), "If-Match", cur)
	assert.Equal(t, 413, r.Status)
}

func TestGroupFileEditingAdmins(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t, "owner", fullAccess)
	admin := e.newUser(t, "admin", fullAccess)
	member := e.newUser(t, "member", fullAccess)
	g, err := e.Store.Groups().Create(ctx(), repo.Group{
		OwnerID: owner.ID, Name: "g", LibraryReading: "members", LibraryEditing: "members", FileEditing: "admins",
	})
	require.NoError(t, err)
	require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, admin.ID, "admin"))
	require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, member.ID, "member"))

	ml := e.groupLib(t, g.ID, member)
	key := e.attachment(t, ml, "g.pdf")
	p := newPayload("g.pdf", []byte("group payload"))
	r := e.form(t, ml, key, p.form(), "If-None-Match", "*")
	assert.Equal(t, 403, r.Status)

	al := e.groupLib(t, g.ID, admin)
	e.store(t, al, key, p)
	// Members may still read.
	assert.Equal(t, 302, ml.get("items/"+key+"/file").Status)

	// The group's bytes count against the group owner.
	lr, err := e.Store.Library(ctx(), g.LibraryID)
	require.NoError(t, err)
	assert.EqualValues(t, len(p.data), lr.StorageUsed())
}

func TestGroupQuotaChargedToOwner(t *testing.T) {
	e := newEnv(t)
	owner := e.newUser(t, "owner", fullAccess)
	admin := e.newUser(t, "admin", fullAccess)
	g, err := e.Store.Groups().Create(ctx(), repo.Group{
		OwnerID: owner.ID, Name: "g", LibraryReading: "members", LibraryEditing: "members", FileEditing: "members",
	})
	require.NoError(t, err)
	require.NoError(t, e.Store.Groups().SetMember(ctx(), g.ID, admin.ID, "admin"))
	e.setQuota(t, owner, 100)

	ol := e.userLib(t, owner)
	e.store(t, ol, e.attachment(t, ol, "own.pdf"), newPayload("own.pdf", bytes.Repeat([]byte("o"), 60)))

	al := e.groupLib(t, g.ID, admin)
	key := e.attachment(t, al, "g.pdf")
	r := e.form(t, al, key, newPayload("g.pdf", bytes.Repeat([]byte("g"), 41)).form(), "If-None-Match", "*")
	assert.Equal(t, 413, r.Status, "owner's personal files count towards the group upload")
	r = e.form(t, al, key, newPayload("g.pdf", bytes.Repeat([]byte("g"), 40)).form(), "If-None-Match", "*")
	assert.Equal(t, 200, r.Status, string(r.Body))
}

func TestDownloadRangeAndHeaders(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "ünï code.pdf")
	p := newPayload("ünï code.pdf", []byte("0123456789abcdef"))
	e.store(t, l, key, p)

	r := l.get("items/" + key + "/file")
	require.Equal(t, 302, r.Status)
	loc := r.Header.Get("Location")

	hr, _ := http.NewRequestWithContext(ctx(), http.MethodGet, loc, nil)
	hr.Header.Set("Range", "bytes=2-5")
	res, err := e.client().Do(hr)
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	assert.Equal(t, 206, res.StatusCode)
	assert.Equal(t, "2345", string(body))
	assert.Equal(t, "application/pdf", res.Header.Get("Content-Type"))
	assert.Equal(t, "attachment; filename*=UTF-8''%C3%BCn%C3%AF%20code.pdf", res.Header.Get("Content-Disposition"))
	assert.Equal(t, "no-store", res.Header.Get("Cache-Control"))
	assert.Equal(t, "16", r.Header.Get("Zotero-File-Size"))
}

func TestDownloadTokenRules(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "t.pdf")
	p := newPayload("t.pdf", []byte("token payload"))
	e.store(t, l, key, p)

	loc := l.get("items/" + key + "/file").Header.Get("Location")
	tok := loc[strings.LastIndex(loc, "/")+1:]
	base := loc[:strings.LastIndex(loc, "/")+1]
	get := func(token string) int {
		res, err := e.client().Get(base + token)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	assert.Equal(t, 200, get(tok))
	assert.Equal(t, 404, get(tok+"x"), "forged signature")
	assert.Equal(t, 404, get("garbage"))

	// A properly signed token that ran out: rebuild what files.SignDownload
	// does, with an expiry in the past.
	enc, _, _ := strings.Cut(tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(enc)
	parts := strings.Split(string(raw), ".")
	parts[3] = strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	payload := strings.Join(parts, ".")
	mac := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef"))
	mac.Write([]byte("gotero download token v1"))
	m2 := hmac.New(sha256.New, mac.Sum(nil))
	m2.Write([]byte(payload))
	expired := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(m2.Sum(nil))
	assert.Equal(t, 404, get(expired))

	// The attachment changed since the link was issued.
	other := newPayload("t.pdf", []byte("different"))
	a := e.authorize2(t, l, key, other, "If-Match", p.md5)
	require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(other.data), int64(len(other.data))).Status)
	require.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-Match", p.md5).Status)
	assert.Equal(t, 404, get(tok))
}

func (e *testEnv) authorize2(t testing.TB, l *lib, key string, p payload, h ...string) authz {
	t.Helper()
	r := e.form(t, l, key, p.form(), h...)
	require.Equal(t, 200, r.Status, string(r.Body))
	j := r.json(t)
	return authz{URL: j["url"].(string), UploadKey: j["uploadKey"].(string)}
}

func TestMismatchedUploadLeavesNoBlob(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "m.pdf")
	p := newPayload("m.pdf", []byte("AAAAAAAA"))
	a := e.authorize(t, l, key, p)

	for name, bad := range map[string][]byte{"wrong bytes": []byte("BBBBBBBB"), "too long": []byte("AAAAAAAAA"), "too short": []byte("AAAA")} {
		r := e.put(t, a.URL, bytes.NewReader(bad), int64(len(bad)))
		assert.Equal(t, 400, r.Status, name)
	}
	_, err := e.Files.Backend().Stat(ctx(), files.BlobKey(p.md5))
	assert.ErrorIs(t, err, storage.ErrNotFound)
	exists, err := e.Store.Files().BlobExists(ctx(), p.md5)
	require.NoError(t, err)
	assert.False(t, exists)
	assert.Equal(t, 400, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)

	// The token survives, so the client can retry with the right bytes.
	assert.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
	assert.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
	// A used key answers 201 again: the client may be retrying a lost reply.
	assert.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
}

func TestUploadTargetRejectsBadKeys(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "m.pdf")
	p := newPayload("m.pdf", []byte("payload"))
	a := e.authorize(t, l, key, p)

	missing := strings.Replace(a.URL, a.UploadKey, strings.Repeat("0", 32), 1)
	assert.Equal(t, 404, e.put(t, missing, bytes.NewReader(p.data), int64(len(p.data))).Status)

	_, err := e.Pool.Exec(ctx(), `UPDATE upload_tokens SET expires_at = now() - interval '1 minute' WHERE upload_key = $1`, a.UploadKey)
	require.NoError(t, err)
	assert.Equal(t, 403, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
}

func TestMultipartUploadStreamsFilePart(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "m.pdf")
	p := newPayload("m.pdf", []byte("multipart payload"))
	form := p.form()
	form.Set("params", "1")
	r := e.form(t, l, key, form, "If-None-Match", "*")
	require.Equal(t, 200, r.Status, string(r.Body))
	j := r.json(t)
	params := j["params"].(m)
	assert.Equal(t, j["uploadKey"], params["key"])

	mk := func(data []byte, filePart bool) (*bytes.Buffer, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("key", params["key"].(string))
		if filePart {
			fw, _ := mw.CreateFormFile("file", "m.pdf")
			_, _ = fw.Write(data)
		}
		_ = mw.Close()
		return &buf, mw.FormDataContentType()
	}
	post := func(buf *bytes.Buffer, ct string) int {
		hr, _ := http.NewRequestWithContext(ctx(), http.MethodPost, j["url"].(string), buf)
		hr.Header.Set("Content-Type", ct)
		res, err := e.client().Do(hr)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	buf, ct := mk(nil, false)
	assert.Equal(t, 400, post(buf, ct), "no file part")
	buf, ct = mk([]byte("wrong"), true)
	assert.Equal(t, 400, post(buf, ct))
	assert.Equal(t, 400, post(bytes.NewBufferString("--x\r\ngarbage"), "multipart/form-data; boundary=x"))
	buf, ct = mk(p.data, true)
	assert.Equal(t, 201, post(buf, ct))
}

func TestRegisterRules(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "r.pdf")
	other := e.attachment(t, l, "o.pdf")
	p := newPayload("r.pdf", []byte("register payload"))
	a := e.authorize(t, l, key, p)

	assert.Equal(t, 400, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status, "not uploaded yet")
	require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
	assert.Equal(t, 400, e.register(t, l, other, a.UploadKey, "If-None-Match", "*").Status, "wrong item")
	assert.Equal(t, 428, e.register(t, l, key, a.UploadKey).Status)
	before := l.dataOf("items", key)["version"].(float64)
	r := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
	require.Equal(t, 204, r.Status)
	assert.Greater(t, l.dataOf("items", key)["version"].(float64), before)
	assert.EqualValues(t, len(p.data), e.used(t, u))

	again := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
	assert.Equal(t, 204, again.Status, "a repeat after a lost 204 succeeds")
	assert.EqualValues(t, len(p.data), e.used(t, u), "no double count")

	// 412 reports the attachment's version, not the library's.
	itemVersion := strconv.Itoa(int(l.dataOf("items", key)["version"].(float64)))
	r = e.form(t, l, key, p.form(), "If-None-Match", "*")
	assert.Equal(t, 412, r.Status)
	assert.Equal(t, itemVersion, r.Header.Get("Last-Modified-Version"))
	r = e.form(t, l, key, p.form(), "If-Match", strings.Repeat("0", 32))
	assert.Equal(t, 412, r.Status)
}

func TestRepeatedUploadAnswers201(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "d.pdf")
	p := newPayload("d.pdf", []byte("upload twice"))
	a := e.authorize(t, l, key, p)

	assert.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
	assert.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status, "retry after a lost 201")

	// The body of a repeat is not read, so even a wrong one is answered.
	assert.Equal(t, 201, e.put(t, a.URL, strings.NewReader("junk"), 4).Status)
}

func TestRepeatedRegisterIsNoOp(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)

	t.Run("If-None-Match", func(t *testing.T) {
		key := e.attachment(t, l, "n.pdf")
		p := newPayload("n.pdf", []byte("first file"))
		a := e.authorize(t, l, key, p)
		require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)

		first := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
		require.Equal(t, 204, first.Status)
		itemVersion := l.dataOf("items", key)["version"]
		events := len(e.Pub.all())

		again := e.register(t, l, key, a.UploadKey, "If-None-Match", "*")
		assert.Equal(t, 204, again.Status)
		assert.Equal(t, first.Header.Get("Last-Modified-Version"), again.Header.Get("Last-Modified-Version"))
		assert.Equal(t, itemVersion, l.dataOf("items", key)["version"], "no item bump")
		assert.Len(t, e.Pub.all(), events, "no event for a repeat")
		assert.EqualValues(t, len(p.data), e.used(t, u))
	})

	t.Run("If-Match", func(t *testing.T) {
		key := e.attachment(t, l, "m.pdf")
		old := newPayload("m.pdf", []byte("old contents"))
		e.store(t, l, key, old)

		next := newPayload("m.pdf", []byte("replacement contents"))
		a := e.authorize2(t, l, key, next, "If-Match", old.md5)
		require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(next.data), int64(len(next.data))).Status)

		first := e.register(t, l, key, a.UploadKey, "If-Match", old.md5)
		require.Equal(t, 204, first.Status)
		itemVersion := l.dataOf("items", key)["version"]

		again := e.register(t, l, key, a.UploadKey, "If-Match", old.md5)
		assert.Equal(t, 204, again.Status)
		assert.Equal(t, first.Header.Get("Last-Modified-Version"), again.Header.Get("Last-Modified-Version"))
		assert.Equal(t, itemVersion, l.dataOf("items", key)["version"])
	})

	t.Run("a different token is still refused", func(t *testing.T) {
		key := e.attachment(t, l, "x.pdf")
		e.store(t, l, key, newPayload("x.pdf", []byte("stored file")))
		other := newPayload("x.pdf", []byte("another file"))
		a := e.authorize2(t, l, key, other, "If-Match", newPayload("x.pdf", []byte("stored file")).md5)
		require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(other.data), int64(len(other.data))).Status)
		// If-None-Match: * fails the precondition because a file exists.
		assert.Equal(t, 412, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
	})
}

func TestUploadAfterRegisterKeepsTokenRegistered(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "l.pdf")
	p := newPayload("l.pdf", []byte("late duplicate"))
	a := e.authorize(t, l, key, p)
	require.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
	require.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)

	assert.Equal(t, 201, e.put(t, a.URL, bytes.NewReader(p.data), int64(len(p.data))).Status)
	tok, err := e.Store.Files().GetUploadToken(ctx(), a.UploadKey)
	require.NoError(t, err)
	assert.Equal(t, "registered", tok.State)
	assert.Equal(t, 204, e.register(t, l, key, a.UploadKey, "If-None-Match", "*").Status)
}

func TestFormValidation(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "v.pdf")
	p := newPayload("v.pdf", []byte("x"))
	cases := map[string]func(url.Values){
		"short md5":      func(f url.Values) { f.Set("md5", "abc") },
		"empty filename": func(f url.Values) { f.Set("filename", "") },
		"negative size":  func(f url.Values) { f.Set("filesize", "-1") },
		"bad mtime":      func(f url.Values) { f.Set("mtime", "yesterday") },
		"bad zipMD5":     func(f url.Values) { f.Set("zipMD5", "nope") },
	}
	for name, mutate := range cases {
		f := p.form()
		mutate(f)
		assert.Equal(t, 400, e.form(t, l, key, f, "If-None-Match", "*").Status, name)
	}
	linked := l.create("items", m{"itemType": "attachment", "linkMode": "linked_url", "title": "l", "url": "https://example.org"})
	assert.Equal(t, 400, e.form(t, l, linked, p.form(), "If-None-Match", "*").Status)

	readOnly := e.newUser(t, "ro", repo.KeyAccess{Library: true, Files: true, Write: false, Groups: "none"})
	rl := e.userLib(t, readOnly)
	assert.Equal(t, 403, e.form(t, rl, key, p.form(), "If-None-Match", "*").Status)
}

func TestMaxUploadBytes(t *testing.T) {
	e := newEnv(t, withDeps(func(d *zapi.Deps) { d.MaxUploadBytes = 10 }))
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "v.pdf")
	assert.Equal(t, 200, e.form(t, l, key, newPayload("v.pdf", make([]byte, 10)).form(), "If-None-Match", "*").Status)
	assert.Equal(t, 413, e.form(t, l, key, newPayload("v.pdf", make([]byte, 11)).form(), "If-None-Match", "*").Status)
}

func TestExistsNeverLeaksOtherLibraries(t *testing.T) {
	e := newEnv(t)
	alice := e.newUser(t, "alice", fullAccess)
	bob := e.newUser(t, "bob", fullAccess)
	al, bl := e.userLib(t, alice), e.userLib(t, bob)
	p := newPayload("secret.pdf", []byte("only alice has this"))
	e.store(t, al, e.attachment(t, al, "secret.pdf"), p)

	key := e.attachment(t, bl, "secret.pdf")
	r := e.form(t, bl, key, p.form(), "If-None-Match", "*")
	require.Equal(t, 200, r.Status)
	assert.NotContains(t, r.json(t), "exists", "bob must upload the bytes himself")
}

func TestExistsViaReadableGroup(t *testing.T) {
	e := newEnv(t)
	alice := e.newUser(t, "alice", fullAccess)
	g, err := e.Store.Groups().Create(ctx(), repo.Group{OwnerID: alice.ID, Name: "g", LibraryReading: "members", LibraryEditing: "members", FileEditing: "members"})
	require.NoError(t, err)
	gl := e.groupLib(t, g.ID, alice)
	p := newPayload("shared.pdf", []byte("shared in the group"))
	e.store(t, gl, e.attachment(t, gl, "shared.pdf"), p)

	ul := e.userLib(t, alice)
	key := e.attachment(t, ul, "shared.pdf")
	before := ul.version()
	r := e.form(t, ul, key, p.form(), "If-None-Match", "*")
	require.Equal(t, 200, r.Status, string(r.Body))
	assert.EqualValues(t, 1, r.json(t)["exists"])
	assert.Greater(t, mustInt(t, r.version(t)), before)
	assert.EqualValues(t, len(p.data), e.used(t, alice))
	assert.NotEmpty(t, e.Pub.all())
}

func mustInt(t testing.TB, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err)
	return n
}

func TestRemoveStorageFiles(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	k1, k2 := e.attachment(t, l, "1.pdf"), e.attachment(t, l, "2.pdf")
	p1, p2 := newPayload("1.pdf", []byte("first")), newPayload("2.pdf", []byte("second!"))
	e.store(t, l, k1, p1)
	e.store(t, l, k2, p2)
	assert.EqualValues(t, 12, e.used(t, u))
	before := l.version()

	r := e.do(t, req{method: "POST", path: l.path + "/removestoragefiles", key: l.key})
	require.Equal(t, 204, r.Status)
	assert.Greater(t, mustInt(t, r.version(t)), before)
	assert.EqualValues(t, 0, e.used(t, u))
	for _, k := range []string{k1, k2} {
		d := l.dataOf("items", k)
		assert.Nil(t, d["md5"])
		assert.Nil(t, d["mtime"])
		assert.Equal(t, 404, l.get("items/"+k+"/file").Status)
	}
	assert.NotEmpty(t, e.Pub.all())

	// The bytes stay until GC finds them orphaned.
	_, err := e.Pool.Exec(ctx(), `UPDATE upload_tokens SET expires_at = now() - interval '1 minute'`)
	require.NoError(t, err)
	var orphans int
	seq, err := e.Store.Files().OrphanBlobs(ctx(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	for _, err := range seq {
		require.NoError(t, err)
		orphans++
	}
	assert.Equal(t, 2, orphans)

	// Without files access the request is refused.
	nf := e.newUser(t, "nofiles", repo.KeyAccess{Library: true, Write: true, Groups: "none"})
	assert.Equal(t, 403, e.do(t, req{method: "POST", path: e.userLib(t, nf).path + "/removestoragefiles", key: nf.Key}).Status)
}

func TestDeletingAttachmentReleasesStorage(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	parent := l.create("items", m{"itemType": "book", "title": "b"})
	child := l.create("items", m{
		"itemType": "attachment", "linkMode": "imported_file", "title": "c", "filename": "c.pdf",
		"parentItem": parent, "contentType": "application/pdf",
	})
	loose := e.attachment(t, l, "l.pdf")
	e.store(t, l, child, newPayload("c.pdf", []byte("12345")))
	e.store(t, l, loose, newPayload("l.pdf", []byte("1234567")))
	require.EqualValues(t, 12, e.used(t, u))

	v := l.version()
	r := l.send("DELETE", "items/"+parent, nil, "If-Unmodified-Since-Version", strconv.FormatInt(v, 10))
	require.Equal(t, 204, r.Status, string(r.Body))
	assert.EqualValues(t, 7, e.used(t, u), "deleting the parent removes its attachment's file")
	r = l.send("DELETE", "items/"+loose, nil, "If-Unmodified-Since-Version", strconv.FormatInt(l.version(), 10))
	require.Equal(t, 204, r.Status)
	assert.EqualValues(t, 0, e.used(t, u))
}

func TestSnapshotZipUpload(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := l.create("items", m{"itemType": "attachment", "linkMode": "imported_url", "title": "s", "filename": "index.html", "url": "https://example.org", "contentType": "text/html"})

	html := []byte("<html>snapshot</html>")
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("index.html")
	_, _ = w.Write(html)
	require.NoError(t, zw.Close())

	form := url.Values{
		"md5": {sum(html)}, "filename": {"index.html"}, "filesize": {strconv.Itoa(zb.Len())},
		"mtime": {"1700000000000"}, "zipMD5": {sum(zb.Bytes())}, "zipFilename": {"abc.zip"},
	}
	r := e.form(t, l, key, form, "If-None-Match", "*")
	require.Equal(t, 200, r.Status, string(r.Body))
	a := r.json(t)
	require.Equal(t, 201, e.put(t, a["url"].(string), bytes.NewReader(zb.Bytes()), int64(zb.Len())).Status)
	require.Equal(t, 204, e.register(t, l, key, a["uploadKey"].(string), "If-None-Match", "*").Status)

	assert.Equal(t, sum(html), l.dataOf("items", key)["md5"], "item keeps the md5 of the unzipped file")
	assert.EqualValues(t, zb.Len(), e.used(t, u))
	r = l.get("items/" + key + "/file")
	require.Equal(t, 302, r.Status)
	assert.Equal(t, sum(html), r.Header.Get("Zotero-File-MD5"))
	assert.Equal(t, "Yes", r.Header.Get("Zotero-File-Compressed"))
	res, err := e.client().Get(r.Header.Get("Location"))
	require.NoError(t, err)
	got, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	assert.Equal(t, "application/zip", res.Header.Get("Content-Type"))
	assert.Equal(t, zb.Bytes(), got)

	// Re-authorizing the unzipped md5 is judged by the item's md5.
	r = e.form(t, l, key, form, "If-Match", sum(html))
	assert.Equal(t, 200, r.Status)
	assert.EqualValues(t, 1, r.json(t)["exists"])
}

func TestNoFilesServiceAnswers501(t *testing.T) {
	e := newEnv(t, withDeps(func(d *zapi.Deps) { d.Files = nil }))
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "x.pdf")
	assert.Equal(t, 501, l.get("items/"+key+"/file").Status)
}

func TestDefaultQuotaFromSetting(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.Store.Settings().Set(ctx(), "default_quota_bytes", int64(10)))
	u := e.newUser(t, "alice", fullAccess)
	l := e.userLib(t, u)
	key := e.attachment(t, l, "q.pdf")
	assert.Equal(t, 413, e.form(t, l, key, newPayload("q.pdf", make([]byte, 11)).form(), "If-None-Match", "*").Status)
	assert.Equal(t, 200, e.form(t, l, key, newPayload("q.pdf", make([]byte, 10)).form(), "If-None-Match", "*").Status)
	e.setQuota(t, u, 1000)
	assert.Equal(t, 200, e.form(t, l, key, newPayload("q.pdf", make([]byte, 11)).form(), "If-None-Match", "*").Status, "the user's quota beats the default")
}

func TestQuotaRecheckedWhenRegistering(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "racer", fullAccess)
	l := e.userLib(t, u)
	e.setQuota(t, u, 100)

	// Both authorizations see an empty library, so both pass.
	pa, pb := newPayload("a.pdf", bytes.Repeat([]byte("a"), 60)), newPayload("b.pdf", bytes.Repeat([]byte("b"), 60))
	ka, kb := e.attachment(t, l, "a.pdf"), e.attachment(t, l, "b.pdf")
	aa, ab := e.authorize(t, l, ka, pa), e.authorize(t, l, kb, pb)
	require.Equal(t, 201, e.put(t, aa.URL, bytes.NewReader(pa.data), 60).Status)
	require.Equal(t, 201, e.put(t, ab.URL, bytes.NewReader(pb.data), 60).Status)

	r := e.register(t, l, ka, aa.UploadKey, "If-None-Match", "*")
	require.Equal(t, 204, r.Status, string(r.Body))
	r = e.register(t, l, kb, ab.UploadKey, "If-None-Match", "*")
	assert.Equal(t, 413, r.Status, string(r.Body))
	assert.Equal(t, "1", r.Header.Get("Zotero-Storage-Quota"))
	assert.EqualValues(t, 60, e.used(t, u), "the rejected registration must not be charged")
}

func TestQuotaChargesRealSizeOnExists(t *testing.T) {
	e := newEnv(t)
	u := e.newUser(t, "liar", fullAccess)
	l := e.userLib(t, u)
	e.setQuota(t, u, 100)

	p := newPayload("a.pdf", bytes.Repeat([]byte("a"), 60))
	e.store(t, l, e.attachment(t, l, "a.pdf"), p)

	// Same blob, but the client claims it is one byte.
	second := e.attachment(t, l, "copy.pdf")
	form := newPayload("copy.pdf", p.data).form()
	form.Set("filesize", "1")
	r := e.form(t, l, second, form, "If-None-Match", "*")
	assert.Equal(t, 413, r.Status, string(r.Body))
	assert.EqualValues(t, 60, e.used(t, u))
}
