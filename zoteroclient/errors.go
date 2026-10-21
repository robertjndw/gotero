package zoteroclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/robertjndw/gotero/zotero"
)

// ErrNotModified is returned when the server answers 304 to a conditional
// read. Compare with errors.Is or IsNotModified. Methods that also return a
// library version still fill it in for this error.
var ErrNotModified = errors.New("zoteroclient: not modified")

// ErrLibraryChanged is returned by Objects when the library version changed
// between two chunks of one logical read. The data collected so far is not a
// consistent snapshot, so the caller should start the sync over.
var ErrLibraryChanged = errors.New("zoteroclient: library changed during read")

// maxErrorBody caps how much of an error response we keep as Message.
const maxErrorBody = 64 << 10

// APIError is a non-2xx answer from the server (304 is ErrNotModified instead).
type APIError struct {
	// StatusCode is the HTTP status.
	StatusCode int
	// Message is the response body as text. Zotero servers explain failures
	// there, e.g. why a precondition did not hold.
	Message string
	// Header holds the response headers, including Last-Modified-Version,
	// Retry-After and Zotero-Storage-Quota where present.
	Header http.Header
	// Method and URL identify the request that failed.
	Method string
	URL    string
}

// Error implements error.
func (e *APIError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	s := fmt.Sprintf("zoteroclient: %s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	if msg != "" {
		s += ": " + msg
	}
	return s
}

// LibraryVersion returns the Last-Modified-Version of the failed response, or
// 0 when it was absent. A 412 on a file authorization carries the attachment
// item's version here.
func (e *APIError) LibraryVersion() int64 { return versionHeader(e.Header) }

func newAPIError(resp *http.Response, method, url string) *APIError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	return &APIError{
		StatusCode: resp.StatusCode,
		Message:    string(b),
		Header:     resp.Header.Clone(),
		Method:     method,
		URL:        url,
	}
}

func hasStatus(err error, code int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == code
}

// IsPreconditionFailed reports whether err is a 412 response: the library (or
// file) changed since the version the caller passed.
func IsPreconditionFailed(err error) bool { return hasStatus(err, http.StatusPreconditionFailed) }

// IsNotFound reports whether err is a 404 response.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsForbidden reports whether err is a 403 response, usually a bad key or a
// key without the needed access.
func IsForbidden(err error) bool { return hasStatus(err, http.StatusForbidden) }

// IsConflict reports whether err is a 409 response. Deleted answers it when
// the requested since version is older than the server's delete log, which
// means the caller needs a full sync.
func IsConflict(err error) bool { return hasStatus(err, http.StatusConflict) }

// IsNotModified reports whether err is ErrNotModified.
func IsNotModified(err error) bool { return errors.Is(err, ErrNotModified) }

// versionHeader returns Last-Modified-Version, or 0 if it is absent or invalid.
func versionHeader(h http.Header) int64 {
	v, _, _ := zotero.ParseVersionHeader(h.Get("Last-Modified-Version"))
	return v
}

// versionOf extracts the library version from whichever of resp or err has it.
func versionOf(resp *http.Response, err error) int64 {
	if resp != nil {
		return versionHeader(resp.Header)
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.LibraryVersion()
	}
	return 0
}
