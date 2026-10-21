package zoteroclient_test

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Zotero's protocol uses MD5
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/robertjndw/gotero/zoteroclient"
)

func fileInfo(data []byte) zoteroclient.FileInfo {
	sum := md5.Sum(data) //nolint:gosec
	return zoteroclient.FileInfo{
		MD5: hex.EncodeToString(sum[:]), Filename: "a b.pdf", Size: int64(len(data)),
		MtimeMS: 1700000000123, Body: bytes.NewReader(data),
	}
}

func TestUploadFileExists(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "*" || r.Header.Get("If-Match") != "" {
			t.Errorf("preconditions: %v", r.Header)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("filename") != "a b.pdf" || r.PostForm.Get("filesize") != "5" || r.PostForm.Get("mtime") != "1700000000123" {
			t.Errorf("form %v", r.PostForm)
		}
		w.Header().Set("Last-Modified-Version", "12")
		writeJSON(w, 200, `{"exists":1}`)
	})
	res, err := c.User(1).UploadFile(context.Background(), "ABCD2345", fileInfo([]byte("hello")), "")
	if err != nil || !res.Exists || res.Version != 12 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestUploadFileFullFlow(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 1000)
	var uploaded []byte
	var uploadHeaders http.Header
	var uploadLen int64
	var registered atomic.Bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		uploadHeaders, uploadLen = r.Header.Clone(), r.ContentLength
		w.WriteHeader(http.StatusCreated)
	}))
	defer storage.Close()

	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("If-Match") != "oldmd5" {
			t.Errorf("If-Match = %q", r.Header.Get("If-Match"))
		}
		if r.PostForm.Has("upload") {
			if r.PostForm.Get("upload") != "UPKEY" {
				t.Errorf("upload key %v", r.PostForm)
			}
			registered.Store(true)
			w.Header().Set("Last-Modified-Version", "31")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, 200, fmt.Sprintf(`{"url":%q,"contentType":"application/x-pdf","prefix":"PRE","suffix":"SUF","uploadKey":"UPKEY"}`, storage.URL+"/up"))
	})
	res, err := c.User(1).UploadFile(context.Background(), "ABCD2345", fileInfo(data), "oldmd5")
	if err != nil {
		t.Fatal(err)
	}
	if res.Exists || res.Version != 31 || !registered.Load() {
		t.Errorf("%+v registered=%v", res, registered.Load())
	}
	if string(uploaded) != "PRE"+string(data)+"SUF" || uploadLen != int64(len(data)+6) {
		t.Errorf("uploaded %d bytes, content-length %d", len(uploaded), uploadLen)
	}
	if uploadHeaders.Get("Content-Type") != "application/x-pdf" {
		t.Errorf("content type %q", uploadHeaders.Get("Content-Type"))
	}
	for _, h := range []string{"Zotero-Api-Key", "Zotero-Api-Version"} {
		if uploadHeaders.Get(h) != "" {
			t.Errorf("%s leaked to the storage host", h)
		}
	}
}

func TestUploadFileParamsVariant(t *testing.T) {
	data := []byte("file-bytes")
	var gotFields map[string]string
	var gotFile string
	var order []string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/form-data" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		gotFields = map[string]string{}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			order = append(order, p.FormName())
			if p.FormName() == "file" {
				gotFile = string(b)
			} else {
				gotFields[p.FormName()] = string(b)
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer storage.Close()

	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Has("upload") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.PostForm.Get("params") != "1" {
			t.Error("params=1 not requested")
		}
		writeJSON(w, 200, fmt.Sprintf(`{"url":%q,"params":{"key":"k/1","acl":"private","n":5},"uploadKey":"U"}`, storage.URL))
	})
	f := fileInfo(data)
	f.RequestParams = true
	if _, err := c.User(1).UploadFile(context.Background(), "ABCD2345", f, ""); err != nil {
		t.Fatal(err)
	}
	if gotFile != "file-bytes" || gotFields["key"] != "k/1" || gotFields["acl"] != "private" || gotFields["n"] != "5" {
		t.Errorf("file=%q fields=%v", gotFile, gotFields)
	}
	if order[len(order)-1] != "file" {
		t.Errorf("file part must come last: %v", order)
	}
}

func TestUploadRetriesAfter429WithFullBody(t *testing.T) {
	var n atomic.Int32
	var bodies []string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer storage.Close()
	c, clk, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Has("upload") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, 200, fmt.Sprintf(`{"url":%q,"prefix":"<","suffix":">","uploadKey":"U"}`, storage.URL))
	})
	if _, err := c.User(1).UploadFile(context.Background(), "ABCD2345", fileInfo([]byte("data")), ""); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != "<data>" || bodies[1] != "<data>" || len(clk.Sleeps()) != 1 {
		t.Errorf("bodies=%q sleeps=%v", bodies, clk.Sleeps())
	}
}

func TestUploadErrors(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer storage.Close()
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("If-Match") == "stale" {
			w.Header().Set("Last-Modified-Version", "77")
			writeJSON(w, 412, "file changed")
			return
		}
		writeJSON(w, 200, fmt.Sprintf(`{"url":%q,"uploadKey":"U"}`, storage.URL))
	})
	lib := c.User(1)
	res, err := lib.UploadFile(context.Background(), "ABCD2345", fileInfo([]byte("x")), "stale")
	if !zoteroclient.IsPreconditionFailed(err) || res.Version != 77 {
		t.Errorf("res=%+v err=%v", res, err)
	}
	if _, err := lib.UploadFile(context.Background(), "ABCD2345", fileInfo([]byte("x")), ""); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("storage failure not reported: %v", err)
	}
	bad := fileInfo([]byte("x"))
	bad.MD5 = "short"
	if _, err := lib.UploadFile(context.Background(), "ABCD2345", bad, ""); err == nil {
		t.Error("invalid md5 accepted")
	}
}

func TestDownloadFileRedirectStripsAPIKey(t *testing.T) {
	content := strings.Repeat("streamed content ", 5000)
	var storageHeaders http.Header
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageHeaders = r.Header.Clone()
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = io.WriteString(w, content)
	}))
	defer storage.Close()

	var apiKeySeen string
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		apiKeySeen = r.Header.Get("Zotero-API-Key")
		h := w.Header()
		h.Set("Location", storage.URL+"/blob?token=abc")
		h.Set("Zotero-File-MD5", "d41d8cd98f00b204e9800998ecf8427e")
		h.Set("Zotero-File-Modification-Time", "1700000000123")
		h.Set("Zotero-File-Compressed", "Yes")
		h.Set("Last-Modified-Version", "6")
		w.WriteHeader(http.StatusFound)
	})
	d, err := c.User(1).DownloadFile(context.Background(), "ABCD2345")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Body.Close() }()
	got, _ := io.ReadAll(d.Body)
	if string(got) != content || d.Size != int64(len(content)) {
		t.Errorf("got %d bytes, size %d", len(got), d.Size)
	}
	if d.MD5 != "d41d8cd98f00b204e9800998ecf8427e" || d.MtimeMS != 1700000000123 || !d.Compressed || d.LastModifiedVersion != 6 {
		t.Errorf("%+v", d)
	}
	if apiKeySeen != "KEY" {
		t.Errorf("first request key = %q", apiKeySeen)
	}
	for _, h := range []string{"Zotero-Api-Key", "Zotero-Api-Version", "Authorization"} {
		if v := storageHeaders.Get(h); v != "" {
			t.Errorf("second request carries %s=%q", h, v)
		}
	}
}

func TestDownloadFileDirect200AndNotFound(t *testing.T) {
	c, _, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "MISSING") {
			writeJSON(w, 404, "no file")
			return
		}
		w.Header().Set("Zotero-File-MD5", "abc")
		_, _ = io.WriteString(w, "direct")
	})
	d, err := c.User(1).DownloadFile(context.Background(), "ABCD2345")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(d.Body)
	_ = d.Body.Close()
	if string(b) != "direct" || d.MD5 != "abc" || d.Compressed {
		t.Errorf("%q %+v", b, d)
	}
	if _, err := c.User(1).DownloadFile(context.Background(), "MISSING22"); !zoteroclient.IsNotFound(err) {
		t.Errorf("err = %v", err)
	}
}
