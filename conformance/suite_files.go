package conformance

import (
	"bytes"
	"crypto/md5" //nolint:gosec // md5 is the Zotero file-sync checksum, not a security control
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

var filesTests = []testCase{
	{"FullUploadDownloadFlow", testFilesFullFlow},
	{"ReauthorizeSameMD5Exists", testFilesExists},
	{"SameMD5OnOtherItemExists", testFilesDedupe},
	{"WrongIfMatchFails412", testFilesWrongIfMatch},
	{"IfNoneMatchOnExistingFile412", testFilesNoneMatchExisting},
	{"MissingPrecondition428", testFilesNoPrecondition},
	{"WrongMD5UploadRejected", testFilesWrongMD5},
	{"RepeatedRegisterIsNoOp", testFilesRepeatRegister},
	{"ParamsMultipartUpload", testFilesParams},
	{"QuotaExceeded413", testFilesQuota},
	{"OtherUserCannotDownload", testFilesOtherUser},
	{"DownloadWithoutFile404", testFilesNoFile},
	{"AuthorizeNonAttachment404", testFilesNonAttachment},
	{"RemoveStorageFiles", testFilesRemoveAll},
	{"LargeFileStreamed", testFilesLarge},
}

type fileSpec struct {
	name  string
	data  []byte
	md5   string
	mtime int64
}

// MD5Hex is the hex md5 Zotero uses as a file checksum.
func MD5Hex(b []byte) string {
	s := md5.Sum(b) //nolint:gosec // see import
	return hex.EncodeToString(s[:])
}

func newFileSpec(name string, data []byte) fileSpec {
	return fileSpec{name: name, data: data, md5: MD5Hex(data), mtime: time.Now().UnixMilli()}
}

func (f fileSpec) form() url.Values {
	return url.Values{
		"md5":      {f.md5},
		"filename": {f.name},
		"filesize": {strconv.Itoa(len(f.data))},
		"mtime":    {strconv.FormatInt(f.mtime, 10)},
	}
}

var (
	ifNone  = withHeader("If-None-Match", "*")
	ifMatch = func(md5 string) reqOpt { return withHeader("If-Match", md5) }
)

func (l *lib) authorize(t testing.TB, key string, form url.Values, pre ...reqOpt) *response {
	t.Helper()
	return l.do(t, "POST", "items/"+key+"/file", append([]reqOpt{withForm(form)}, pre...)...)
}

type authInfo struct {
	url, contentType, prefix, suffix, uploadKey string
}

func parseAuth(t testing.TB, r *response) authInfo {
	t.Helper()
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	a := authInfo{url: at[string](t, o, "url"), uploadKey: at[string](t, o, "uploadKey")}
	a.contentType = at[string](t, o, "contentType")
	a.prefix = at[string](t, o, "prefix")
	a.suffix = at[string](t, o, "suffix")
	if !strings.HasPrefix(a.url, "http://") && !strings.HasPrefix(a.url, "https://") {
		t.Errorf("upload url %q is not absolute", a.url)
	}
	if len(a.uploadKey) != 32 {
		t.Errorf("uploadKey %q: want 32 hex characters", a.uploadKey)
	}
	return a
}

// upload sends the bytes the way the Zotero client does: no API key, the
// upload URL is self-authenticating.
func (l *lib) upload(t testing.TB, a authInfo, body io.Reader) *response {
	t.Helper()
	return l.e.do(t, "POST", a.url, withReader(io.MultiReader(strings.NewReader(a.prefix), body, strings.NewReader(a.suffix)), a.contentType))
}

func (l *lib) register(t testing.TB, key string, uploadKey string, pre reqOpt) *response {
	t.Helper()
	return l.authorize(t, key, url.Values{"upload": {uploadKey}}, pre)
}

// uploadFile runs authorize, upload and register, returning the item version
// after registration.
func (l *lib) uploadFile(t testing.TB, key string, f fileSpec) int64 {
	t.Helper()
	a := parseAuth(t, l.authorize(t, key, f.form(), ifNone))
	mustStatus(t, l.upload(t, a, bytes.NewReader(f.data)), 201)
	r := l.register(t, key, a.uploadKey, ifNone)
	mustStatus(t, r, 204)
	return versionHeader(t, r)
}

func (l *lib) newAttachment(t *testing.T, name string) (string, int64) {
	return l.create(t, "items", attachmentItem(name))
}

func testFilesFullFlow(t *testing.T, e *env) {
	l := e.userLib(t)
	key, v0 := l.newAttachment(t, "paper.pdf")
	f := newFileSpec("paper.pdf", []byte("%PDF-1.4 conformance payload "+randHex(64)))
	regVersion := l.uploadFile(t, key, f)
	if regVersion <= v0 {
		t.Errorf("library version %d -> %d after register", v0, regVersion)
	}
	d := at[obj](t, l.item(t, key), "data")
	if d["md5"] != f.md5 || atInt(t, d, "mtime") != f.mtime {
		t.Errorf("item md5/mtime = %v/%v, want %s/%d", d["md5"], d["mtime"], f.md5, f.mtime)
	}
	if atInt(t, d, "version") <= v0 {
		t.Errorf("item version %d did not increase past %d", atInt(t, d, "version"), v0)
	}

	r := l.get(t, "items/"+key+"/file")
	mustStatus(t, r, 302)
	versionHeader(t, r)
	if got := mustHeader(t, r, "Zotero-File-MD5"); got != f.md5 {
		t.Errorf("Zotero-File-MD5 = %q", got)
	}
	if got := mustHeader(t, r, "Zotero-File-Size"); got != strconv.Itoa(len(f.data)) {
		t.Errorf("Zotero-File-Size = %q, want %d", got, len(f.data))
	}
	if got := mustHeader(t, r, "Zotero-File-Modification-Time"); got != strconv.FormatInt(f.mtime, 10) {
		t.Errorf("Zotero-File-Modification-Time = %q, want %d", got, f.mtime)
	}
	if got := mustHeader(t, r, "Zotero-File-Compressed"); got != "No" {
		t.Errorf("Zotero-File-Compressed = %q, want No", got)
	}
	if cc := r.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	loc := mustHeader(t, r, "Location")
	if !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
		t.Fatalf("Location %q is not absolute", loc)
	}
	// The client follows without the API key, so the target must authenticate itself.
	dl := e.do(t, "GET", loc)
	mustStatus(t, dl, 200)
	if !bytes.Equal(dl.Body, f.data) {
		t.Errorf("downloaded %d bytes, differ from the %d uploaded", len(dl.Body), len(f.data))
	}
}

func uploaded(t *testing.T, e *env) (*lib, string, fileSpec) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "x.pdf")
	f := newFileSpec("x.pdf", []byte("payload "+randHex(32)))
	l.uploadFile(t, key, f)
	return l, key, f
}

func testFilesExists(t *testing.T, e *env) {
	l, key, f := uploaded(t, e)
	f.mtime += 5000
	r := l.authorize(t, key, f.form(), ifMatch(f.md5))
	mustStatus(t, r, 200)
	if atInt(t, parseObj(t, r), "exists") != 1 {
		t.Errorf("want exists:1, got %s", r)
	}
	versionHeader(t, r)
	if got := atInt(t, at[obj](t, l.item(t, key), "data"), "mtime"); got != f.mtime {
		t.Errorf("mtime after exists = %d, want %d", got, f.mtime)
	}
}

func testFilesDedupe(t *testing.T, e *env) {
	l, _, f := uploaded(t, e)
	other, _ := l.newAttachment(t, "y.pdf")
	r := l.authorize(t, other, f.form(), ifNone)
	mustStatus(t, r, 200)
	if atInt(t, parseObj(t, r), "exists") != 1 {
		t.Fatalf("want exists:1, got %s", r)
	}
	if got := at[obj](t, l.item(t, other), "data")["md5"]; got != f.md5 {
		t.Errorf("md5 on second item = %v", got)
	}
}

func testFilesWrongIfMatch(t *testing.T, e *env) {
	l, key, f := uploaded(t, e)
	r := l.authorize(t, key, f.form(), ifMatch(strings.Repeat("0", 32)))
	mustStatus(t, r, 412)
	// On a file 412 this header carries the attachment item's version, not the library's.
	itemVersion := atInt(t, at[obj](t, l.item(t, key), "data"), "version")
	if got := versionHeader(t, r); got != itemVersion {
		t.Errorf("412 Last-Modified-Version = %d, want item version %d", got, itemVersion)
	}
}

func testFilesNoneMatchExisting(t *testing.T, e *env) {
	l, key, f := uploaded(t, e)
	mustStatus(t, l.authorize(t, key, f.form(), ifNone), 412)
}

func testFilesNoPrecondition(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "x.pdf")
	f := newFileSpec("x.pdf", []byte("abc"))
	mustStatus(t, l.authorize(t, key, f.form()), 428)
}

func testFilesWrongMD5(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "x.pdf")
	f := newFileSpec("x.pdf", []byte("AAAA"))
	a := parseAuth(t, l.authorize(t, key, f.form(), ifNone))
	r := l.upload(t, a, strings.NewReader("BBBB"))
	if r.Status < 400 {
		t.Errorf("upload of mismatching bytes answered %s", r)
	}
	r = l.register(t, key, a.uploadKey, ifNone)
	if r.Status < 400 {
		t.Errorf("register after bad upload answered %s", r)
	}
	if got := at[obj](t, l.item(t, key), "data")["md5"]; got != nil {
		t.Errorf("item md5 = %v after failed upload", got)
	}
}

// A client that never saw the 204 sends the register call again and expects
// the same answer, not a precondition failure on the file it just stored.
func testFilesRepeatRegister(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "x.pdf")
	f := newFileSpec("x.pdf", []byte("register twice "+randHex(16)))
	a := parseAuth(t, l.authorize(t, key, f.form(), ifNone))
	mustStatus(t, l.upload(t, a, bytes.NewReader(f.data)), 201)
	first := l.register(t, key, a.uploadKey, ifNone)
	mustStatus(t, first, 204)
	itemVersion := atInt(t, at[obj](t, l.item(t, key), "data"), "version")

	again := l.register(t, key, a.uploadKey, ifNone)
	mustStatus(t, again, 204)
	if got, want := versionHeader(t, again), versionHeader(t, first); got != want {
		t.Errorf("repeated register Last-Modified-Version = %d, want %d", got, want)
	}
	if got := atInt(t, at[obj](t, l.item(t, key), "data"), "version"); got != itemVersion {
		t.Errorf("item version moved %d -> %d on a repeated register", itemVersion, got)
	}
}

func testFilesParams(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "m.pdf")
	f := newFileSpec("m.pdf", []byte("multipart payload "+randHex(16)))
	form := f.form()
	form.Set("params", "1")
	r := l.authorize(t, key, form, ifNone)
	mustStatus(t, r, 200)
	o := parseObj(t, r)
	target := at[string](t, o, "url")
	uploadKey := at[string](t, o, "uploadKey")
	params := at[obj](t, o, "params")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		_ = mw.WriteField(k, fmt.Sprint(params[k]))
	}
	fw, _ := mw.CreateFormFile("file", f.name)
	_, _ = fw.Write(f.data)
	_ = mw.Close()
	up := e.do(t, "POST", target, withRaw(buf.Bytes(), mw.FormDataContentType()))
	mustStatus(t, up, 201)
	reg := l.register(t, key, uploadKey, ifNone)
	mustStatus(t, reg, 204)
	versionHeader(t, reg)
	if at[obj](t, l.item(t, key), "data")["md5"] != f.md5 {
		t.Error("md5 not stored after multipart upload")
	}
}

func testFilesQuota(t *testing.T, e *env) {
	qs, ok := e.prov.(QuotaSetter)
	if !ok {
		t.Skip("provisioner has no QuotaSetter")
	}
	l := e.userLib(t)
	if err := qs.SetQuota(ctxOf(t), l.u, 1<<20); err != nil {
		e.provisionErr(t, "set quota", err)
	}
	key, _ := l.newAttachment(t, "big.pdf")
	f := newFileSpec("big.pdf", nil)
	form := f.form()
	form.Set("filesize", strconv.Itoa(5<<20))
	r := l.authorize(t, key, form, ifNone)
	mustStatus(t, r, 413)
	for _, h := range []string{"Zotero-Storage-Usage", "Zotero-Storage-Quota"} {
		if _, err := strconv.Atoi(mustHeader(t, r, h)); err != nil {
			t.Errorf("%s = %q, want integer MB", h, r.Header.Get(h))
		}
	}
}

func testFilesOtherUser(t *testing.T, e *env) {
	l, key, _ := uploaded(t, e)
	other := e.newUser(t)
	r := e.do(t, "GET", fmt.Sprintf("users/%d/items/%s/file", l.u.ID, key), withKey(other.APIKey))
	mustStatusAny(t, r, 403, 404)
	r = e.do(t, "GET", fmt.Sprintf("users/%d/items/%s/file", l.u.ID, key))
	mustStatusAny(t, r, 403, 404)
}

func testFilesNoFile(t *testing.T, e *env) {
	l := e.userLib(t)
	key, _ := l.newAttachment(t, "x.pdf")
	r := l.get(t, "items/"+key+"/file")
	mustStatus(t, r, 404)
	versionHeader(t, r)
}

func testFilesNonAttachment(t *testing.T, e *env) {
	l := e.userLib(t)
	book, _ := l.create(t, "items", bookItem("b"))
	f := newFileSpec("x.pdf", []byte("abc"))
	mustStatus(t, l.authorize(t, book, f.form(), ifNone), 404)
	mustStatus(t, l.authorize(t, newKey(), f.form(), ifNone), 404)
}

func testFilesRemoveAll(t *testing.T, e *env) {
	l, key, _ := uploaded(t, e)
	mustStatus(t, l.do(t, "POST", "removestoragefiles"), 204)
	mustStatus(t, l.get(t, "items/"+key+"/file"), 404)
}

// patternReader yields n bytes of a repeating block without holding them all.
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

func testFilesLarge(t *testing.T, e *env) {
	if testing.Short() {
		t.Skip("large file skipped in -short mode")
	}
	const size = 20 << 20
	block := []byte(randHex(512 << 10))
	h := md5.New() //nolint:gosec // see import
	_, _ = io.Copy(h, &patternReader{block: block, left: size})
	sum := hex.EncodeToString(h.Sum(nil))

	l := e.userLib(t)
	key, _ := l.newAttachment(t, "large.pdf")
	form := url.Values{"md5": {sum}, "filename": {"large.pdf"}, "filesize": {strconv.Itoa(size)}, "mtime": {strconv.FormatInt(time.Now().UnixMilli(), 10)}}
	a := parseAuth(t, l.authorize(t, key, form, ifNone))
	mustStatus(t, l.upload(t, a, &patternReader{block: block, left: size}), 201)
	mustStatus(t, l.register(t, key, a.uploadKey, ifNone), 204)

	r := l.get(t, "items/"+key+"/file")
	mustStatus(t, r, 302)
	dl := e.do(t, "GET", mustHeader(t, r, "Location"))
	mustStatus(t, dl, 200)
	if len(dl.Body) != size || MD5Hex(dl.Body) != sum {
		t.Errorf("downloaded %d bytes with md5 %s, want %d bytes with %s", len(dl.Body), MD5Hex(dl.Body), size, sum)
	}
}
