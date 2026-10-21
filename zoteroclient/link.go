package zoteroclient

import "strings"

// parseLink parses the value of a Link header (RFC 8288) into a map from rel
// to target URL. Targets without a rel are ignored, and when a rel repeats the
// first one wins. It is lenient: malformed pieces are skipped, never fatal,
// because a broken pagination header should end paging, not crash a sync.
func parseLink(h string) map[string]string {
	out := map[string]string{}
	for {
		h = strings.TrimLeft(h, " \t,")
		if h == "" {
			return out
		}
		if h[0] != '<' {
			h = skipToComma(h)
			continue
		}
		end := strings.IndexByte(h, '>')
		if end < 0 {
			return out
		}
		target := h[1:end]
		h = h[end+1:]

		var rels []string
		for {
			h = strings.TrimLeft(h, " \t")
			if h == "" || h[0] == ',' {
				break
			}
			if h[0] != ';' {
				h = skipToComma(h)
				break
			}
			var name, value string
			name, value, h = parseParam(h[1:])
			if strings.EqualFold(name, "rel") {
				rels = append(rels, strings.Fields(value)...)
			}
		}
		for _, r := range rels {
			r = strings.ToLower(r)
			if _, dup := out[r]; !dup {
				out[r] = target
			}
		}
	}
}

// parseParam reads `name=value` (value maybe quoted) and returns the rest.
func parseParam(s string) (name, value, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, "=;,")
	if i < 0 {
		return strings.TrimSpace(s), "", ""
	}
	name = strings.TrimSpace(s[:i])
	if s[i] != '=' {
		return name, "", s[i:]
	}
	s = s[i+1:]
	if s != "" && s[0] == '"' {
		var b strings.Builder
		j := 1
		for ; j < len(s); j++ {
			if s[j] == '\\' && j+1 < len(s) {
				j++
				b.WriteByte(s[j])
				continue
			}
			if s[j] == '"' {
				j++
				break
			}
			b.WriteByte(s[j])
		}
		return name, b.String(), s[j:]
	}
	j := strings.IndexAny(s, ";,")
	if j < 0 {
		return name, strings.TrimSpace(s), ""
	}
	return name, strings.TrimSpace(s[:j]), s[j:]
}

func skipToComma(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[i+1:]
	}
	return ""
}
