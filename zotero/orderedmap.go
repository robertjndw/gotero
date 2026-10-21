package zotero

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"
)

// indexThreshold is the size above which an OrderedMap keeps a hash index.
// Item data usually has 20 to 40 keys; scanning that many short strings is
// cheaper than building and hashing a map for every parsed object.
const indexThreshold = 16

// OrderedMap is a JSON object that remembers the order of its keys.
//
// Values are kept as raw JSON, so numbers keep their exact textual form and
// nested structures are never re-encoded. Use Get and Set with
// json.RawMessage, or SetValue to encode a Go value.
//
// Nested arrays and objects are compacted when parsed. Values passed to Set
// are written as given and must be valid JSON.
//
// The zero value is an empty map ready to use. Reads are safe for concurrent
// use as long as nobody writes.
type OrderedMap struct {
	keys []string
	vals []json.RawMessage
	idx  map[string]int // nil while the map is small
}

// Len returns the number of keys.
func (m OrderedMap) Len() int { return len(m.keys) }

// Keys returns the keys in order. The caller owns the slice.
func (m OrderedMap) Keys() []string { return slices.Clone(m.keys) }

// find returns the position of key or -1.
func (m *OrderedMap) find(key string) int {
	if m.idx != nil {
		if i, ok := m.idx[key]; ok {
			return i
		}
		return -1
	}
	for i, k := range m.keys {
		if k == key {
			return i
		}
	}
	return -1
}

// Get returns the raw value stored under key. The slice is shared with the
// map and must not be modified.
func (m OrderedMap) Get(key string) (json.RawMessage, bool) {
	if i := m.find(key); i >= 0 {
		return m.vals[i], true
	}
	return nil, false
}

// Has reports whether key is present.
func (m OrderedMap) Has(key string) bool { return m.find(key) >= 0 }

// Set stores a raw JSON value. A new key is appended; an existing key keeps
// its position.
func (m *OrderedMap) Set(key string, value json.RawMessage) {
	if i := m.find(key); i >= 0 {
		m.vals[i] = value
		return
	}
	m.keys = append(m.keys, key)
	m.vals = append(m.vals, value)
	if m.idx != nil {
		m.idx[key] = len(m.keys) - 1
	} else if len(m.keys) > indexThreshold {
		m.reindex()
	}
}

// SetDefault stores raw under key unless the key is already present.
func (m *OrderedMap) SetDefault(key string, raw json.RawMessage) {
	if !m.Has(key) {
		m.Set(key, raw)
	}
}

// SetValue encodes v with encoding/json and stores it under key.
func (m *OrderedMap) SetValue(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.Set(key, b)
	return nil
}

// Delete removes key and reports whether it was present.
func (m *OrderedMap) Delete(key string) bool {
	i := m.find(key)
	if i < 0 {
		return false
	}
	m.keys = slices.Delete(m.keys, i, i+1)
	m.vals = slices.Delete(m.vals, i, i+1)
	if m.idx != nil {
		m.reindex()
	}
	return true
}

func (m *OrderedMap) reindex() {
	if len(m.keys) <= indexThreshold {
		m.idx = nil
		return
	}
	m.idx = make(map[string]int, len(m.keys))
	for i, k := range m.keys {
		m.idx[k] = i
	}
}

// Clone returns a copy that shares no mutable state with m. Raw values are
// treated as immutable and shared.
func (m OrderedMap) Clone() OrderedMap {
	c := OrderedMap{
		keys: slices.Clone(m.keys),
		vals: slices.Clone(m.vals),
	}
	c.reindex()
	return c
}

// Each calls fn for every entry in order until fn returns false.
func (m OrderedMap) Each(fn func(key string, value json.RawMessage) bool) {
	for i, k := range m.keys {
		if !fn(k, m.vals[i]) {
			return
		}
	}
}

// UnmarshalJSON parses a JSON object, keeping key order. A repeated key keeps
// its first position and its last value, matching encoding/json. A JSON null
// leaves an empty map.
func (m *OrderedMap) UnmarshalJSON(data []byte) error {
	*m = OrderedMap{}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("zotero: OrderedMap: expected a JSON object")
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := kt.(string)
		if !ok {
			return errors.New("zotero: OrderedMap: object key is not a string")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if c := raw[0]; c == '{' || c == '[' {
			var buf bytes.Buffer
			buf.Grow(len(raw))
			if err := json.Compact(&buf, raw); err != nil {
				return err
			}
			raw = buf.Bytes()
		}
		m.Set(key, raw)
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("zotero: OrderedMap: trailing data after object")
	}
	return nil
}

// MarshalJSON encodes the map as a JSON object in key order.
func (m OrderedMap) MarshalJSON() ([]byte, error) {
	return m.AppendJSON(make([]byte, 0, m.encodedSize())), nil
}

func (m OrderedMap) encodedSize() int {
	n := 2
	for i, k := range m.keys {
		n += len(k) + len(m.vals[i]) + 4
	}
	return n
}

// AppendJSON appends the encoded object to dst without the extra copy that
// MarshalJSON incurs, and without the HTML escaping encoding/json applies to
// Marshaler output.
func (m OrderedMap) AppendJSON(dst []byte) []byte {
	dst = append(dst, '{')
	for i, k := range m.keys {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendString(dst, k)
		dst = append(dst, ':')
		v := m.vals[i]
		if len(v) == 0 {
			v = []byte("null")
		}
		dst = append(dst, v...)
	}
	return append(dst, '}')
}

const hexDigits = "0123456789abcdef"

// appendString writes s as a JSON string, escaping what JSON requires and
// replacing invalid UTF-8 with U+FFFD like encoding/json does.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				dst = append(dst, '\\', c)
			case c == '\n':
				dst = append(dst, '\\', 'n')
			case c == '\r':
				dst = append(dst, '\\', 'r')
			case c == '\t':
				dst = append(dst, '\\', 't')
			case c < 0x20:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			default:
				dst = append(dst, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, `�`...)
		case r == ' ' || r == ' ':
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[r&0xF])
		default:
			dst = append(dst, s[i:i+size]...)
		}
		i += size
	}
	return append(dst, '"')
}

// String returns the JSON encoding, mostly for debugging and test output.
func (m OrderedMap) String() string {
	return string(m.AppendJSON(nil))
}

// GetString decodes the value under key as a JSON string.
func (m OrderedMap) GetString(key string) (string, bool) {
	raw, ok := m.Get(key)
	if !ok || len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// GetInt decodes the value under key as an integer.
func (m OrderedMap) GetInt(key string) (int64, bool) {
	raw, ok := m.Get(key)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
