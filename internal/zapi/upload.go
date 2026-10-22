package zapi

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
)

// multipartSlack is how much a multipart body may exceed the announced file
// size: boundaries, part headers and the small parameter fields.
const multipartSlack = 1 << 20

var errNoFilePart = errors.New("zapi: multipart upload has no file part")

// filePart walks a multipart body with the streaming reader and returns the
// part named "file". Parameter fields before it are skipped unread. The body
// is never parsed into memory or temp files, as ParseMultipartForm would do.
func filePart(body io.Reader, boundary string) (io.Reader, error) {
	mr := multipart.NewReader(body, boundary)
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, errNoFilePart
		}
		if err != nil {
			return nil, err
		}
		if p.FormName() == "file" {
			return p, nil
		}
	}
}

// errTracker remembers why a body read failed, so a broken request body
// (400) can be told apart from a storage failure (500).
type errTracker struct {
	r   io.Reader
	err error
}

func (t *errTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

// uploadTarget is POST /files/upload/:uploadKey. The upload key is the
// credential, so it sits outside the API key middleware.
func (a *API) uploadTarget(c *gin.Context) {
	if !a.fileService(c) {
		return
	}
	ctx := c.Request.Context()
	tok, err := a.store.Files().GetUploadToken(ctx, c.Param("uploadKey"))
	if errors.Is(err, repo.ErrNotFound) {
		Fail(c, http.StatusNotFound, "Unknown upload key")
		return
	}
	if err != nil {
		a.FailErr(c, err)
		return
	}
	switch {
	case !tok.ExpiresAt.After(a.now()):
		Fail(c, http.StatusForbidden, "Upload key has expired")
		return
	case tok.State == "uploaded" || tok.State == "registered":
		// The client retries when it never saw our 201. The bytes are already
		// stored, but the body is still drained: answering mid-upload makes
		// the server close the connection, and a client that is still
		// sending sees a reset instead of the 201.
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(c.Writer, c.Request.Body, tok.Size+multipartSlack))
		c.Status(http.StatusCreated)
		return
	case tok.State != "pending":
		Fail(c, http.StatusConflict, "Upload key has already been used")
		return
	}

	var src io.Reader = http.MaxBytesReader(c.Writer, c.Request.Body, tok.Size+multipartSlack)
	if mt, params, err := mime.ParseMediaType(c.GetHeader("Content-Type")); err == nil && mt == "multipart/form-data" {
		part, err := filePart(src, params["boundary"])
		if err != nil {
			a.uploadReadFailed(c, err)
			return
		}
		src = part
	}
	track := &errTracker{r: src}
	err = a.files.Receive(ctx, track, tok.MD5, tok.Size)
	switch {
	case errors.Is(err, files.ErrMismatch):
		// The token stays pending so the client can send the bytes again.
		Fail(c, http.StatusBadRequest, "Uploaded file does not match the announced md5 or size")
	case err != nil && track.err != nil:
		a.uploadReadFailed(c, track.err)
	case err != nil:
		a.FailErr(c, err)
	default:
		// Conditional, so a duplicate that finishes after the register call
		// cannot move the token from registered back to uploaded. Not moving
		// is fine: someone else already got it there.
		if _, err := a.store.Files().SetUploadTokenStateFrom(ctx, tok.UploadKey, "pending", "uploaded"); err != nil {
			a.FailErr(c, err)
			return
		}
		c.Status(http.StatusCreated)
	}
}

func (a *API) uploadReadFailed(c *gin.Context, err error) {
	if httpx.IsTooLarge(err) {
		Fail(c, http.StatusRequestEntityTooLarge, "Upload is larger than announced")
		return
	}
	if c.Request.Context().Err() != nil {
		c.Abort()
		return
	}
	Fail(c, http.StatusBadRequest, "Malformed upload body")
}
