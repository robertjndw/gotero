package zapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
)

// Fail aborts the request with a plain-text error body, the way zotero.org
// answers. Extension routes use it so errors look the same everywhere.
func Fail(c *gin.Context, status int, msg string) {
	c.Header("Content-Type", "text/plain; charset=UTF-8")
	c.Header("Cache-Control", "no-store")
	c.String(status, "%s", msg)
	c.Abort()
}

// httpError carries a status code out of a library transaction. Returning
// one from the InLibrary callback rolls the transaction back.
type httpError struct {
	status int
	msg    string
	// version, when set, is sent as Last-Modified-Version with the error.
	version int64
}

func (e *httpError) Error() string { return e.msg }

func errStatus(status int, msg string) error { return &httpError{status: status, msg: msg} }

// FailErr maps err to a response. Client mistakes keep their status, a busy
// library becomes 503 with Retry-After, and everything else is logged and
// answered with a generic 500 so internals never leak.
func (a *API) FailErr(c *gin.Context, err error) {
	var he *httpError
	switch {
	case errors.As(err, &he):
		if he.version != 0 {
			setVersion(c, he.version)
		}
		Fail(c, he.status, he.msg)
	case httpx.IsTooLarge(err):
		Fail(c, http.StatusRequestEntityTooLarge, "Request body too large")
	case errors.Is(err, repo.ErrBusy):
		c.Header("Retry-After", "5")
		Fail(c, http.StatusServiceUnavailable, "Library is busy, try again shortly")
	case errors.Is(c.Request.Context().Err(), context.Canceled):
		// The client went away; there is nobody to answer.
		c.Abort()
	default:
		a.log.ErrorContext(c.Request.Context(), "zapi: request failed", "method", c.Request.Method,
			"path", c.Request.URL.Path, "err", err)
		Fail(c, http.StatusInternalServerError, "Internal server error")
	}
}

func setVersion(c *gin.Context, v int64) {
	c.Header("Last-Modified-Version", strconv.FormatInt(v, 10))
}

// decodeBody adapts httpx.DecodeBody to gin. A body over the limit surfaces
// as an error while reading, which handlers map to 413.
func decodeBody(max int64) gin.HandlerFunc {
	return httpx.Gin(httpx.DecodeBody(max))
}

func isTooLarge(err error) bool { return httpx.IsTooLarge(err) }
