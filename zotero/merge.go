package zotero

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Merge applies a POST patch to a stored object.
//
// Fields present in patch replace the stored value, fields that are absent
// are kept, and new fields are appended after the stored ones. "key" and
// "version" in the patch are bookkeeping for the caller: they never count as
// a change and are not copied into the result.
//
// changed is true only when some value differs semantically. Whitespace,
// key order inside nested objects and number spelling (1 vs 1.0) are not
// differences.
func Merge(stored, patch OrderedMap) (merged OrderedMap, changed bool) {
	merged = stored.Clone()
	for i, k := range patch.keys {
		if k == "key" || k == "version" {
			continue
		}
		v := patch.vals[i]
		if j := merged.find(k); j >= 0 {
			if EqualJSON(merged.vals[j], v) {
				continue
			}
			merged.vals[j] = v
		} else {
			merged.Set(k, v)
		}
		changed = true
	}
	return merged, changed
}

// EqualJSON reports whether a and b hold the same JSON value. Object key
// order and insignificant whitespace are ignored and numbers are compared by
// value. If either side is not valid JSON the raw bytes are compared.
func EqualJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	ca, err := appendCanonical(nil, a)
	if err != nil {
		return false
	}
	cb, err := appendCanonical(nil, b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// CanonicalHash returns a SHA-256 over a canonical form of m: object keys
// sorted at every level, numbers normalized and no whitespace. The top-level
// "key" and "version" are excluded so that the hash identifies content, not
// identity. Objects that Merge reports as unchanged hash the same.
//
// Values that are not valid JSON are hashed verbatim.
func CanonicalHash(m OrderedMap) [32]byte {
	order := make([]int, 0, len(m.keys))
	for i, k := range m.keys {
		if k != "key" && k != "version" {
			order = append(order, i)
		}
	}
	sort.Slice(order, func(a, b int) bool { return m.keys[order[a]] < m.keys[order[b]] })
	h := sha256.New()
	var buf []byte
	for _, i := range order {
		buf = appendString(buf[:0], m.keys[i])
		buf = append(buf, ':')
		var err error
		if buf, err = appendCanonical(buf, m.vals[i]); err != nil {
			buf = append(buf[:0], m.vals[i]...)
		}
		buf = append(buf, '\n')
		h.Write(buf)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// appendCanonical appends the canonical encoding of one JSON value.
func appendCanonical(dst []byte, raw json.RawMessage) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dst, err := canonValue(dst, dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errTrailing
	}
	return dst, nil
}

// maxCanonDepth keeps hostile nesting from exhausting the stack.
const maxCanonDepth = 512

var errTrailing = &json.SyntaxError{}

func canonValue(dst []byte, dec *json.Decoder, depth int) ([]byte, error) {
	if depth > maxCanonDepth {
		return nil, errTrailing
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '[' {
			dst = append(dst, '[')
			for first := true; dec.More(); first = false {
				if !first {
					dst = append(dst, ',')
				}
				if dst, err = canonValue(dst, dec, depth+1); err != nil {
					return nil, err
				}
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return append(dst, ']'), nil
		}
		type member struct {
			key string
			val []byte
		}
		var members []member
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := canonValue(nil, dec, depth+1)
			if err != nil {
				return nil, err
			}
			members = append(members, member{kt.(string), v})
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		// Stable so a repeated key keeps "last one wins" order semantics.
		sort.SliceStable(members, func(i, j int) bool { return members[i].key < members[j].key })
		dst = append(dst, '{')
		for i, mem := range members {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendString(dst, mem.key)
			dst = append(dst, ':')
			dst = append(dst, mem.val...)
		}
		return append(dst, '}'), nil
	case string:
		return appendString(dst, t), nil
	case json.Number:
		return append(dst, normalizeNumber(string(t))...), nil
	case bool:
		return strconv.AppendBool(dst, t), nil
	case nil:
		return append(dst, "null"...), nil
	}
	return nil, errTrailing
}

// normalizeNumber rewrites a JSON number as sign, significant digits and a
// decimal exponent so that 1, 1.0, 10e-1 and 0.1e1 all compare equal without
// going through float64 and losing precision.
func normalizeNumber(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, expStr, _ := strings.Cut(strings.ToLower(s), "e")
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	exp := int64(0)
	if expStr != "" {
		var err error
		exp, err = strconv.ParseInt(strings.TrimPrefix(expStr, "+"), 10, 64)
		if err != nil || exp > 1<<40 || exp < -(1<<40) {
			// An absurd exponent; fall back to the literal rather than
			// doing arithmetic on it.
			if neg {
				return "-" + s
			}
			return s
		}
	}
	exp -= int64(len(frac))
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "0"
	}
	sig := strings.TrimRight(trimmed, "0")
	exp += int64(len(trimmed) - len(sig))
	out := sig + "e" + strconv.FormatInt(exp, 10)
	if neg {
		return "-" + out
	}
	return out
}
