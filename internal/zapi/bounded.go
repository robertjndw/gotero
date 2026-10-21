package zapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	// maxJSONDepth caps nesting in anything a client uploads. Real Zotero
	// documents are three or four levels deep (annotation positions are the
	// worst case), so this only ever trips on hostile input.
	maxJSONDepth = 64
	// maxObjectKeys caps the top-level members of one uploaded object. Items
	// have well under a hundred fields; the cap keeps a megabytes-long
	// object of one-byte keys from turning into millions of map entries.
	maxObjectKeys = 256
	// maxSettingValueBytes caps one setting value.
	maxSettingValueBytes = 256 << 10
)

// checkJSONShape walks raw once, without allocating, and rejects documents
// nested deeper than maxDepth or with more than maxMembers direct members.
// It does not validate the JSON; the real decoder does that afterwards, so
// this only has to be correct for well-formed input and merely safe for
// malformed input.
func checkJSONShape(raw []byte, maxDepth, maxMembers int) error {
	depth, members := 0, 0
	inStr, esc := false, false
	for _, ch := range raw {
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return errStatus(http.StatusBadRequest, fmt.Sprintf("JSON is nested too deeply (max %d levels)", maxDepth))
			}
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				members++
				if members >= maxMembers {
					return errStatus(http.StatusRequestEntityTooLarge,
						fmt.Sprintf("Too many members in uploaded object (max %d)", maxMembers))
				}
			}
		}
	}
	return nil
}

// readArray decodes a top-level JSON array of objects straight off the wire.
// It stops at the 51st element instead of materializing the whole body first:
// a 32 MiB array of "0" would otherwise turn into gigabytes of RawMessage
// headers before the count was ever checked.
func readArray(c *gin.Context) ([]json.RawMessage, error) {
	dec := json.NewDecoder(c.Request.Body)
	notArray := errStatus(http.StatusBadRequest, "Uploaded data must be a JSON array")
	fail := func(err error) error {
		if isTooLarge(err) {
			return errStatus(http.StatusRequestEntityTooLarge, "Request body too large")
		}
		return notArray
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, fail(err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, notArray
	}
	raws := []json.RawMessage{}
	for dec.More() {
		if len(raws) == maxObjectsPerWrite {
			return nil, errStatus(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("Too many objects uploaded (max %d)", maxObjectsPerWrite))
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fail(err)
		}
		if err := checkJSONShape(raw, maxJSONDepth-1, maxObjectKeys); err != nil {
			return nil, err
		}
		raws = append(raws, raw)
	}
	if _, err := dec.Token(); err != nil {
		return nil, fail(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil && isTooLarge(err) {
			return nil, fail(err)
		}
		return nil, notArray
	}
	return raws, nil
}

// readObjectBody reads a single-object body and applies the same shape
// limits as array elements.
func readObjectBody(c *gin.Context, maxMembers int) ([]byte, error) {
	b, err := readBody(c)
	if err != nil {
		return nil, err
	}
	if err := checkJSONShape(b, maxJSONDepth, maxMembers); err != nil {
		return nil, err
	}
	return b, nil
}
