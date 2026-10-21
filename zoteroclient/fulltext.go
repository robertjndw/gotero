package zoteroclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/robertjndw/gotero/zotero"
)

// FulltextVersions lists items whose full-text content changed after since, as
// item key -> content version. With since > 0 an unchanged library yields
// ErrNotModified.
func (l *Library) FulltextVersions(ctx context.Context, since int64) (map[string]int64, int64, error) {
	var q url.Values
	var hdr http.Header
	if since > 0 {
		q = url.Values{"since": {strconv.FormatInt(since, 10)}}
		hdr = versionHdr("If-Modified-Since-Version", since)
	}
	var out map[string]int64
	ver, err := l.c.get(ctx, l.path("fulltext"), q, hdr, &out)
	if err != nil {
		return nil, ver, err
	}
	return out, ver, nil
}

// Fulltext fetches the indexed text of one item. An item without content is
// a 404 (IsNotFound).
func (l *Library) Fulltext(ctx context.Context, itemKey string) (*zotero.FulltextContent, int64, error) {
	var out zotero.FulltextContent
	ver, err := l.c.get(ctx, l.path("items/"+url.PathEscape(itemKey)+"/fulltext"), nil, nil, &out)
	if err != nil {
		return nil, ver, err
	}
	return &out, ver, nil
}

// WriteFulltext uploads full-text content for several items in one request.
// Each entry needs its Key set. The server requires the precondition.
func (l *Library) WriteFulltext(ctx context.Context, contents []zotero.FulltextContent, ifUnmodifiedSince int64) (*zotero.WriteResponse, int64, error) {
	body, err := json.Marshal(contents)
	if err != nil {
		return nil, 0, err
	}
	var raw rawWrite
	ver, err := l.c.sendJSON(ctx, http.MethodPost, l.path("fulltext"), nil,
		versionHdr("If-Unmodified-Since-Version", ifUnmodifiedSince), body, &raw)
	if err != nil {
		return nil, ver, err
	}
	w, err := decodeWrite(raw)
	if err != nil {
		return nil, ver, err
	}
	return w, ver, nil
}

// SetFulltext replaces the full-text content of one item. Key in content is
// ignored. PUT is idempotent, so this is retried on server errors.
func (l *Library) SetFulltext(ctx context.Context, itemKey string, content zotero.FulltextContent) (int64, error) {
	content.Key = ""
	body, err := json.Marshal(content)
	if err != nil {
		return 0, err
	}
	return l.c.sendJSON(ctx, http.MethodPut, l.path("items/"+url.PathEscape(itemKey)+"/fulltext"), nil, nil, body, nil)
}
