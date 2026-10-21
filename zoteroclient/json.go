package zoteroclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func decodeJSON(r io.Reader, out any) error {
	return json.NewDecoder(r).Decode(out)
}

// lenientMap decodes a JSON object into a map but accepts an empty array in
// its place. zotero.org is PHP behind the scenes and renders empty
// associative arrays as [], so "unchanged": [] is a normal response.
func lenientMap[T any](raw json.RawMessage) (map[string]T, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	if raw[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, err
		}
		if len(arr) != 0 {
			return nil, fmt.Errorf("expected object, got non-empty array")
		}
		return nil, nil
	}
	var m map[string]T
	err := json.Unmarshal(raw, &m)
	return m, err
}
