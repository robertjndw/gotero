package webdav

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, body, reqURL, basePath string) ([]entry, error) {
	t.Helper()
	u, err := url.Parse(reqURL)
	if err != nil {
		t.Fatal(err)
	}
	var got []entry
	err = parseMultistatus(strings.NewReader(body), u, basePath, func(e entry) error {
		got = append(got, e)
		return nil
	})
	return got, err
}

var mod = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

func TestParseMultistatusSamples(t *testing.T) {
	apache := `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
<D:response xmlns:lp1="DAV:" xmlns:lp2="http://apache.org/dav/props/">
<D:href>/dav/store/ab/</D:href>
<D:propstat><D:prop><lp1:resourcetype><D:collection/></lp1:resourcetype><lp1:getlastmodified>Tue, 04 Mar 2025 05:06:07 GMT</lp1:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
</D:response>
<D:response xmlns:lp1="DAV:">
<D:href>/dav/store/ab/abcdef%20x.bin</D:href>
<D:propstat><D:prop><lp1:resourcetype/><lp1:getcontentlength>1234</lp1:getcontentlength><lp1:getlastmodified>Tue, 04 Mar 2025 05:06:07 GMT</lp1:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>
</D:response>
</D:multistatus>`

	nginx := `<?xml version="1.0" encoding="utf-8" ?>
<D:multistatus xmlns:D="DAV:">
<D:response><D:href>/dav/store/ab/</D:href><D:propstat><D:prop><D:getlastmodified>Tue, 04 Mar 2025 05:06:07 GMT</D:getlastmodified><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
<D:response><D:href>/dav/store/ab/f%C3%BC%2B1</D:href><D:propstat><D:prop><D:getcontentlength>7</D:getcontentlength><D:getlastmodified>Tue, 04 Mar 2025 05:06:07 GMT</D:getlastmodified><D:resourcetype></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
</D:multistatus>`

	rclone := `<?xml version="1.0" encoding="UTF-8"?>
<D:multistatus xmlns:D="DAV:"><D:response><D:href>http://localhost:8080/store/ab/</D:href><D:propstat><D:prop><D:resourcetype><D:collection xmlns:D="DAV:"/></D:resourcetype><D:getlastmodified>Tue, 04 Mar 2025 05:06:07 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response><D:response><D:href>http://localhost:8080/store/ab/c%20d</D:href><D:propstat><D:prop><D:resourcetype></D:resourcetype><D:getcontentlength>0</D:getcontentlength><D:getlastmodified>Tue, 04 Mar 2025 05:06:07 GMT</D:getlastmodified></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`

	lowercase := `<multistatus xmlns="DAV:"><response><href>/store/ab/x</href><propstat><prop><getcontentlength>5</getcontentlength></prop><status>HTTP/1.1 200 OK</status></propstat><propstat><prop><getlastmodified/></prop><status>HTTP/1.1 404 Not Found</status></propstat></response></multistatus>`

	notFoundProps := `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/store/gone</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response></d:multistatus>`

	tests := []struct {
		name, body, req, base string
		want                  []entry
	}{
		{"apache", apache, "http://h/dav/store/ab/", "/dav/store", []entry{
			{Path: "ab", IsDir: true, Size: -1, ModTime: mod},
			{Path: "ab/abcdef x.bin", Size: 1234, ModTime: mod},
		}},
		{"nginx", nginx, "http://h/dav/store/ab/", "/dav/store", []entry{
			{Path: "ab", IsDir: true, Size: -1, ModTime: mod},
			{Path: "ab/fü+1", Size: 7, ModTime: mod},
		}},
		{"rclone absolute urls", rclone, "http://localhost:8080/store/ab/", "/store", []entry{
			{Path: "ab", IsDir: true, Size: -1, ModTime: mod},
			{Path: "ab/c d", Size: 0, ModTime: mod},
		}},
		{"default namespace, partial 404 propstat", lowercase, "http://h/store/ab/x", "/store", []entry{
			{Path: "ab/x", Size: 5},
		}},
		{"root base", rclone, "http://localhost:8080/store/", "", []entry{
			{Path: "store/ab", IsDir: true, Size: -1, ModTime: mod},
			{Path: "store/ab/c d", Size: 0, ModTime: mod},
		}},
		{"missing resource skipped", notFoundProps, "http://h/store/", "/store", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parse(t, tc.body, tc.req, tc.base)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestParseMultistatusRejects(t *testing.T) {
	wrap := func(href string) string {
		return `<multistatus xmlns="DAV:"><response><href>` + href + `</href><propstat><prop/><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`
	}
	for _, href := range []string{
		"/store/../etc/passwd", "/store/a/%2e%2e/b", "/other/x", "http://h/store/a//b",
	} {
		if _, err := parse(t, wrap(href), "http://h/store/", "/store"); err == nil {
			t.Errorf("href %q accepted", href)
		}
	}
	if _, err := parse(t, "<multistatus><response>", "http://h/store/", "/store"); err == nil {
		t.Error("truncated XML accepted")
	}
}

func TestParseRelativeHref(t *testing.T) {
	body := `<multistatus xmlns="DAV:"><response><href>x</href><propstat><prop/><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`
	got, err := parse(t, body, "http://h/store/ab/", "/store")
	if err != nil || len(got) != 1 || got[0].Path != "ab/x" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func FuzzParseMultistatus(f *testing.F) {
	f.Add(`<multistatus xmlns="DAV:"><response><href>/s/a/b</href><propstat><prop><getcontentlength>3</getcontentlength></prop><status>HTTP/1.1 200 OK</status></propstat></response></multistatus>`)
	f.Add(`<D:multistatus xmlns:D="DAV:"><D:response><D:href>http://h/s/a%20b/</D:href><D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	f.Add(`<multistatus><response><href>../../etc</href></response>`)
	f.Add("")
	base, _ := url.Parse("http://h/s/")
	f.Fuzz(func(t *testing.T, body string) {
		_ = parseMultistatus(strings.NewReader(body), base, "/s", func(e entry) error {
			// Whatever the server says, entries must stay inside the store.
			if e.Path != "" {
				for _, seg := range strings.Split(e.Path, "/") {
					if seg == "" || seg == "." || seg == ".." {
						t.Fatalf("unsafe path %q", e.Path)
					}
				}
			}
			if e.Size < -1 {
				t.Fatalf("bad size %d", e.Size)
			}
			return nil
		})
	})
}
