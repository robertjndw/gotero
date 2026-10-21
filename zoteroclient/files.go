package zoteroclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// FileInfo describes a file to upload.
type FileInfo struct {
	// MD5 is the hex MD5 of the file content.
	MD5 string
	// Filename is the name stored on the attachment.
	Filename string
	// Size is the content length in bytes.
	Size int64
	// MtimeMS is the modification time in Unix milliseconds.
	MtimeMS int64
	// Body supplies the content, exactly Size bytes. It is streamed, never
	// buffered. If it is an io.ReaderAt (an *os.File is) it is read from
	// offset 0 and the upload can be retried after 429 and 503; an io.Seeker
	// also allows retries, from its current position. Other readers are sent
	// once. Body may be nil only if the server is expected to already hold
	// the file.
	Body io.Reader
	// RequestParams asks for the multipart form variant of the upload
	// (params=1). Without it a server may still answer with params, and the
	// client handles that too.
	RequestParams bool
}

// UploadResult is the outcome of UploadFile.
type UploadResult struct {
	// Exists is true when the server already had the content and no bytes
	// were sent.
	Exists bool
	// Version is the library version after registering the upload.
	Version int64
}

type uploadAuth struct {
	Exists      json.RawMessage            `json:"exists"`
	URL         string                     `json:"url"`
	ContentType string                     `json:"contentType"`
	Prefix      string                     `json:"prefix"`
	Suffix      string                     `json:"suffix"`
	UploadKey   string                     `json:"uploadKey"`
	Params      map[string]json.RawMessage `json:"params"`
}

func (a uploadAuth) exists() bool {
	s := strings.TrimSpace(string(a.Exists))
	return s == "1" || s == "true"
}

func fileHeaders(ifMatchMD5 string) http.Header {
	h := http.Header{}
	if ifMatchMD5 == "" {
		h.Set("If-None-Match", "*")
	} else {
		h.Set("If-Match", ifMatchMD5)
	}
	return h
}

func (f FileInfo) validate() error {
	switch {
	case len(f.MD5) != 32:
		return errors.New("zoteroclient: FileInfo.MD5 must be 32 hex characters")
	case f.Filename == "":
		return errors.New("zoteroclient: FileInfo.Filename is empty")
	case f.Size < 0:
		return errors.New("zoteroclient: FileInfo.Size is negative")
	}
	return nil
}

// UploadFile stores a file on an attachment item using Zotero File Storage:
// authorize, send the bytes to the returned URL, then register the upload.
//
// ifMatchMD5 is the MD5 of the file currently stored on the item, or empty for
// a first upload (If-None-Match: *). A mismatch is a 412 (IsPreconditionFailed).
// If the server already has identical content, UploadFile returns early with
// Exists set.
func (l *Library) UploadFile(ctx context.Context, itemKey string, f FileInfo, ifMatchMD5 string) (UploadResult, error) {
	if err := f.validate(); err != nil {
		return UploadResult{}, err
	}
	rel := l.path("items/" + url.PathEscape(itemKey) + "/file")
	hdr := fileHeaders(ifMatchMD5)

	form := url.Values{
		"md5":      {f.MD5},
		"filename": {f.Filename},
		"filesize": {strconv.FormatInt(f.Size, 10)},
		"mtime":    {strconv.FormatInt(f.MtimeMS, 10)},
	}
	if f.RequestParams {
		form.Set("params", "1")
	}
	var auth uploadAuth
	ver, err := l.c.postForm(ctx, rel, hdr, form, &auth)
	if err != nil {
		return UploadResult{Version: ver}, err
	}
	if auth.exists() {
		return UploadResult{Exists: true, Version: ver}, nil
	}
	if auth.URL == "" || auth.UploadKey == "" {
		return UploadResult{}, errors.New("zoteroclient: upload authorization has no url or uploadKey")
	}
	if f.Body == nil {
		return UploadResult{}, errors.New("zoteroclient: FileInfo.Body is nil but the server needs the content")
	}
	if err := l.c.sendFileBody(ctx, f, auth); err != nil {
		return UploadResult{}, err
	}
	ver, err = l.c.postForm(ctx, rel, hdr, url.Values{"upload": {auth.UploadKey}}, nil)
	if err != nil {
		return UploadResult{Version: ver}, err
	}
	return UploadResult{Version: ver}, nil
}

func (c *Client) postForm(ctx context.Context, rel string, hdr http.Header, form url.Values, out any) (int64, error) {
	r := c.newRequest(http.MethodPost, rel, nil, hdr)
	r.setBytes([]byte(form.Encode()), "application/x-www-form-urlencoded")
	return c.exec(ctx, r, out)
}

// sendFileBody streams the file to the storage URL. The URL authenticates
// itself and may be on another host, so no Zotero headers go along.
func (c *Client) sendFileBody(ctx context.Context, f FileInfo, auth uploadAuth) error {
	target, err := url.Parse(auth.URL)
	if err != nil || target.Scheme != "http" && target.Scheme != "https" || target.Host == "" {
		return fmt.Errorf("zoteroclient: upload URL %q is not an absolute http(s) URL", auth.URL)
	}

	prefix, suffix := []byte(auth.Prefix), []byte(auth.Suffix)
	contentType := auth.ContentType
	if len(auth.Params) > 0 {
		// Multipart form: the server's fields first, the file part last,
		// as S3 style endpoints insist on.
		var head bytes.Buffer
		mw := multipart.NewWriter(&head)
		keys := make([]string, 0, len(auth.Params))
		for k := range auth.Params {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if err := mw.WriteField(k, rawString(auth.Params[k])); err != nil {
				return err
			}
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, f.Filename))
		h.Set("Content-Type", "application/octet-stream")
		if _, err := mw.CreatePart(h); err != nil {
			return err
		}
		prefix = head.Bytes()
		suffix = []byte("\r\n--" + mw.Boundary() + "--\r\n")
		contentType = mw.FormDataContentType()
	}

	r := c.newRequest(http.MethodPost, "", nil, nil)
	r.url = auth.URL
	r.keyless = true
	if contentType != "" {
		r.header.Set("Content-Type", contentType)
	}
	r.length = int64(len(prefix)) + f.Size + int64(len(suffix))
	content := fileSource(f)
	r.replayable = content.replayable
	r.body = func() (io.Reader, error) {
		src, err := content.open()
		if err != nil {
			return nil, err
		}
		return io.MultiReader(bytes.NewReader(prefix), io.LimitReader(src, f.Size), bytes.NewReader(suffix)), nil
	}

	resp, err := c.roundTrip(ctx, r)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return newAPIError(resp, r.method, r.url)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	return resp.Body.Close()
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

type fileContent struct {
	open       func() (io.Reader, error)
	replayable bool
}

func fileSource(f FileInfo) fileContent {
	switch b := f.Body.(type) {
	case io.ReaderAt:
		return fileContent{replayable: true, open: func() (io.Reader, error) {
			return io.NewSectionReader(b, 0, f.Size), nil
		}}
	case io.Seeker:
		pos, err := b.Seek(0, io.SeekCurrent)
		if err == nil {
			return fileContent{replayable: true, open: func() (io.Reader, error) {
				_, err := b.Seek(pos, io.SeekStart)
				return f.Body, err
			}}
		}
	}
	used := false
	return fileContent{open: func() (io.Reader, error) {
		if used {
			return nil, errors.New("zoteroclient: file body can only be read once")
		}
		used = true
		return f.Body, nil
	}}
}

// Download is a file being downloaded. Close Body when done.
type Download struct {
	// Body streams the file content, which is the zip for compressed
	// (snapshot) attachments.
	Body io.ReadCloser
	// MD5 is the content MD5 from Zotero-File-MD5.
	MD5 string
	// MtimeMS is the modification time in Unix milliseconds, 0 if unknown.
	MtimeMS int64
	// Compressed is true when the file is a zipped snapshot.
	Compressed bool
	// Size is the content length, or -1 if the server didn't say.
	Size int64
	// LastModifiedVersion is the library version from the API response.
	LastModifiedVersion int64
}

// DownloadFile fetches an attachment's file. The API normally answers with a
// redirect to a self-authenticating URL, possibly on another host such as
// S3. That second request is made without any Zotero header so the API key
// can't leak to the storage host. Servers that return the bytes directly
// with 200 are supported too.
func (l *Library) DownloadFile(ctx context.Context, itemKey string) (*Download, error) {
	r := l.c.newRequest(http.MethodGet, l.path("items/"+url.PathEscape(itemKey)+"/file"), nil, nil)
	resp, err := l.c.roundTrip(ctx, r)
	if err != nil {
		return nil, err
	}
	d := &Download{
		MD5:                 resp.Header.Get("Zotero-File-MD5"),
		Compressed:          strings.EqualFold(resp.Header.Get("Zotero-File-Compressed"), "yes"),
		LastModifiedVersion: versionHeader(resp.Header),
		Size:                -1,
	}
	d.MtimeMS, _ = strconv.ParseInt(resp.Header.Get("Zotero-File-Modification-Time"), 10, 64)

	switch resp.StatusCode {
	case http.StatusOK:
		d.Body, d.Size = resp.Body, resp.ContentLength
		return d, nil
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil, newAPIError(resp, r.method, r.url)
	}
	_ = resp.Body.Close()

	loc := resp.Header.Get("Location")
	if loc == "" {
		return nil, errors.New("zoteroclient: file redirect has no Location")
	}
	target, err := l.c.base.Parse(loc)
	if err != nil {
		return nil, fmt.Errorf("zoteroclient: bad file redirect %q: %w", loc, err)
	}
	fr := l.c.newRequest(http.MethodGet, "", nil, nil)
	fr.url = target.String()
	fr.keyless = true
	// Identity keeps Go from transparently gunzipping, which would make the
	// bytes differ from the MD5 the server announced.
	fr.header.Set("Accept-Encoding", "identity")
	fresp, err := l.c.roundTrip(ctx, fr)
	if err != nil {
		return nil, err
	}
	if fresp.StatusCode != http.StatusOK {
		return nil, newAPIError(fresp, fr.method, fr.url)
	}
	d.Body, d.Size = fresp.Body, fresp.ContentLength
	return d, nil
}

// RemoveStorageFiles deletes every stored file of a user library
// (POST removestoragefiles). Group libraries are not supported by the API.
func (l *Library) RemoveStorageFiles(ctx context.Context) error {
	if l.group {
		return errors.New("zoteroclient: RemoveStorageFiles only works on user libraries")
	}
	_, err := l.c.exec(ctx, l.c.newRequest(http.MethodPost, l.path("removestoragefiles"), nil, nil), nil)
	return err
}
