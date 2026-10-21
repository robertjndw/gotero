package webdav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// entry is one resource from a PROPFIND multistatus answer.
type entry struct {
	// Path is relative to the backend's base URL, percent-decoded, without
	// leading or trailing slashes. It is empty for the base collection itself.
	Path    string
	IsDir   bool
	Size    int64 // -1 when the server did not say
	ModTime time.Time
}

type msResponse struct {
	Hrefs     []string     `xml:"href"`
	Status    string       `xml:"status"`
	Propstats []msPropstat `xml:"propstat"`
}

type msPropstat struct {
	Status string `xml:"status"`
	Prop   struct {
		ResourceType struct {
			Collection *struct{} `xml:"collection"`
		} `xml:"resourcetype"`
		ContentLength string `xml:"getcontentlength"`
		LastModified  string `xml:"getlastmodified"`
	} `xml:"prop"`
}

// parseMultistatus streams a 207 Multi-Status body and calls fn for every
// resource that has successful properties. reqURL is the URL the PROPFIND was
// sent to: relative hrefs resolve against it. basePath is the (decoded) path
// of the store root; hrefs must live below it.
//
// Servers disagree wildly about href formatting (absolute URLs, bare paths,
// percent-encoding, with or without trailing slash, any namespace prefix),
// so everything is normalised here and nowhere else.
func parseMultistatus(r io.Reader, reqURL *url.URL, basePath string, fn func(entry) error) error {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("webdav: parse multistatus: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "response" {
			continue
		}
		var rs msResponse
		if err := dec.DecodeElement(&rs, &se); err != nil {
			return fmt.Errorf("webdav: parse multistatus: %w", err)
		}
		e, ok, err := rs.toEntry(reqURL, basePath)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
}

func (rs *msResponse) toEntry(reqURL *url.URL, basePath string) (entry, bool, error) {
	if len(rs.Hrefs) == 0 {
		return entry{}, false, nil
	}
	rel, hrefIsDir, err := relativePath(strings.TrimSpace(rs.Hrefs[0]), reqURL, basePath)
	if err != nil {
		return entry{}, false, err
	}
	e := entry{Path: rel, Size: -1, IsDir: hrefIsDir}
	found := false
	for _, ps := range rs.Propstats {
		if !statusOK(ps.Status) {
			continue
		}
		found = true
		if ps.Prop.ResourceType.Collection != nil {
			e.IsDir = true
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.ContentLength), 10, 64); err == nil && n >= 0 {
			e.Size = n
		}
		if t, err := http.ParseTime(strings.TrimSpace(ps.Prop.LastModified)); err == nil {
			e.ModTime = t
		}
	}
	// A response-level status (and no propstat) means the resource is
	// reported as missing or forbidden.
	if !found && (len(rs.Propstats) > 0 || !statusOK(rs.Status)) {
		return entry{}, false, nil
	}
	return e, true, nil
}

// statusOK reports whether an HTTP status line such as "HTTP/1.1 200 OK" is 2xx.
func statusOK(line string) bool {
	f := strings.Fields(line)
	if len(f) < 2 {
		return line == "" // be lenient with servers that omit it
	}
	return len(f[1]) == 3 && f[1][0] == '2'
}

// relativePath converts an href into a clean path relative to basePath.
func relativePath(href string, reqURL *url.URL, basePath string) (rel string, dir bool, err error) {
	if href == "" {
		return "", false, errors.New("webdav: empty href in multistatus")
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", false, fmt.Errorf("webdav: bad href %q: %w", href, err)
	}
	if reqURL != nil {
		u = reqURL.ResolveReference(u)
	}
	p := u.Path // already percent-decoded by net/url
	dir = strings.HasSuffix(p, "/")
	base := strings.TrimRight(basePath, "/")
	switch {
	case base == "":
	case p == base || p == base+"/":
		p = ""
	case strings.HasPrefix(p, base+"/"):
		p = p[len(base)+1:]
	default:
		return "", false, fmt.Errorf("webdav: href %q is outside the base path %q", href, basePath)
	}
	p = strings.Trim(p, "/")
	if p == "" {
		return "", dir, nil
	}
	// A hostile or buggy server must not be able to smuggle "../" into keys.
	if clean := path.Clean(p); clean != p || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\x00") {
		return "", false, fmt.Errorf("webdav: unsafe href %q", href)
	}
	return p, dir, nil
}
