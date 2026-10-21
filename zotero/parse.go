package zotero

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MaxKeyList is the most keys ParseKeyList accepts. Zotero clients send at
// most 100 keys per read and 50 per delete.
const MaxKeyList = 100

// ErrTooManyKeys is returned when a key list is longer than allowed.
var ErrTooManyKeys = errors.New("zotero: too many keys")

// ParseKeyList splits a comma separated list of object keys and validates
// each one. An empty string yields an empty list. Surrounding spaces are
// tolerated, empty elements and malformed keys are not.
func ParseKeyList(s string) ([]string, error) { return ParseKeyListMax(s, MaxKeyList) }

// ParseKeyListMax is ParseKeyList with an explicit upper bound.
func ParseKeyListMax(s string, limit int) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	// Check the count before allocating so a huge query string can't make
	// us build a huge slice.
	if n := strings.Count(s, ",") + 1; n > limit {
		return nil, fmt.Errorf("%w: %d (max %d)", ErrTooManyKeys, n, limit)
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if !ValidKey(p) {
			return nil, fmt.Errorf("zotero: invalid key %q", truncate(p, 32))
		}
		out = append(out, p)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ParseTagList splits tags separated by "||" (Zotero sends " || "). Each tag
// is trimmed and empty ones are dropped. A lone "|" belongs to the tag.
func ParseTagList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(s, "||") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ParseVersionHeader parses the value of If-Modified-Since-Version or
// If-Unmodified-Since-Version. The bool is false when the header is absent.
func ParseVersionHeader(s string) (version int64, present bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, true, fmt.Errorf("zotero: invalid version %q", truncate(s, 32))
	}
	return v, true, nil
}
