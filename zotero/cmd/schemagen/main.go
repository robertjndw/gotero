// Command schemagen generates schema/schema_gen.go from the official
// zotero-schema repository.
//
// It fetches schema.json at a pinned commit, then bisects the file's commit
// history to find, for every item type and field, the earliest schema
// version that contained it. Only factual names and ordering are written out.
//
//	go run ./cmd/schemagen -commit <sha> -out schema/schema_gen.go
//
// Set GITHUB_TOKEN to avoid the anonymous API rate limit.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/robertjndw/gotero/zotero/schema"
)

const repo = "zotero/zotero-schema"

type fetcher struct {
	client *http.Client
	token  string
	cache  map[string]*schema.Schema
	calls  int
}

func (f *fetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Accept", "application/vnd.github+json")
		if f.token != "" {
			req.Header.Set("Authorization", "Bearer "+f.token)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return body, nil
}

// commits lists the commits that touched schema.json, newest first.
func (f *fetcher) commits(ctx context.Context, head string) ([]string, error) {
	var shas []string
	for page := 1; ; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/commits?path=schema.json&sha=%s&per_page=100&page=%d", repo, head, page)
		body, err := f.get(ctx, url)
		if err != nil {
			return nil, err
		}
		var list []struct {
			SHA string `json:"sha"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, err
		}
		for _, c := range list {
			shas = append(shas, c.SHA)
		}
		if len(list) < 100 {
			return shas, nil
		}
	}
}

func (f *fetcher) schemaAt(ctx context.Context, sha string) (*schema.Schema, error) {
	if s, ok := f.cache[sha]; ok {
		return s, nil
	}
	var body []byte
	var err error
	for attempt := range 3 {
		body, err = f.get(ctx, fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/schema.json", repo, sha))
		if err == nil {
			break
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	if err != nil {
		return nil, err
	}
	f.calls++
	s, err := schema.Load(bytes.NewReader(body))
	if err != nil {
		// Very old commits may not parse; treat them as absent.
		f.cache[sha] = nil
		return nil, nil //nolint:nilerr // callers treat a nil schema as "skip this commit"
	}
	f.cache[sha] = s
	return s, nil
}

// snapshots returns the schema at the boundaries of every run of equal
// versions. Versions never decrease going forward in history, so when the two
// ends of a range agree nothing in between can differ.
func (f *fetcher) snapshots(ctx context.Context, shas []string) ([]*schema.Schema, error) {
	// shas is newest first; flip so index grows with time.
	chrono := make([]string, len(shas))
	for i, s := range shas {
		chrono[len(shas)-1-i] = s
	}
	var out []*schema.Schema
	seen := map[string]bool{}
	add := func(i int) (int, error) {
		s, err := f.schemaAt(ctx, chrono[i])
		if err != nil {
			return 0, err
		}
		if s == nil {
			return -1, nil
		}
		if !seen[chrono[i]] {
			seen[chrono[i]] = true
			out = append(out, s)
		}
		return s.Version(), nil
	}
	var solve func(lo, hi, vlo, vhi int) error
	solve = func(lo, hi, vlo, vhi int) error {
		if hi-lo <= 1 || vlo == vhi {
			return nil
		}
		mid := (lo + hi) / 2
		vm, err := add(mid)
		if err != nil {
			return err
		}
		if vm < 0 {
			// Unparseable middle commit: give up on this stretch rather than
			// guess.
			return nil
		}
		if err := solve(lo, mid, vlo, vm); err != nil {
			return err
		}
		return solve(mid, hi, vm, vhi)
	}
	n := len(chrono)
	vlo, err := add(0)
	if err != nil {
		return nil, err
	}
	vhi, err := add(n - 1)
	if err != nil {
		return nil, err
	}
	if vlo < 0 || vhi < 0 {
		return nil, errors.New("oldest or newest schema failed to parse")
	}
	if err := solve(0, n-1, vlo, vhi); err != nil {
		return nil, err
	}
	return out, nil
}

func main() {
	commit := flag.String("commit", "", "zotero-schema commit sha to pin (required)")
	out := flag.String("out", "schema/schema_gen.go", "output file")
	flag.Parse()
	if *commit == "" {
		fmt.Fprintln(os.Stderr, "schemagen: -commit is required")
		os.Exit(2)
	}
	ctx := context.Background()
	f := &fetcher{
		client: &http.Client{Timeout: 2 * time.Minute},
		token:  os.Getenv("GITHUB_TOKEN"),
		cache:  map[string]*schema.Schema{},
	}
	if err := run(ctx, f, *commit, *out); err != nil {
		fmt.Fprintln(os.Stderr, "schemagen:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, f *fetcher, commit, out string) error {
	head, err := f.schemaAt(ctx, commit)
	if err != nil {
		return err
	}
	if head == nil {
		return errors.New("pinned commit has no parseable schema.json")
	}
	shas, err := f.commits(ctx, commit)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "schemagen: %d commits touch schema.json\n", len(shas))
	snaps, err := f.snapshots(ctx, shas)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "schemagen: fetched %d snapshots, head version %d\n", f.calls, head.Version())

	src, err := render(head, snaps, commit)
	if err != nil {
		return err
	}
	return os.WriteFile(out, src, 0o644) //nolint:gosec // generated source is meant to be readable by everyone
}

func introduced(head *schema.Schema, snaps []*schema.Schema, typ, field string) int {
	best := head.Version()
	for _, s := range snaps {
		if s.ValidField(typ, field) && s.Version() < best {
			best = s.Version()
		}
	}
	return best
}

func render(head *schema.Schema, snaps []*schema.Schema, commit string) ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by cmd/schemagen; DO NOT EDIT.\n//\n")
	fmt.Fprintf(&b, "// Source: https://github.com/%s at commit %s (schema version %d).\n", repo, commit, head.Version())
	b.WriteString("// Only factual data is included: item type names, field order, base-field\n")
	b.WriteString("// mappings, creator types and the version each field first appeared in.\n\n")
	b.WriteString("package schema\n\n")
	fmt.Fprintf(&b, "const genVersion = %d\n\n", head.Version())
	b.WriteString("var genTypes = []genType{\n")
	for _, t := range head.ItemTypes() {
		fmt.Fprintf(&b, "\t{Name: %q,", t)
		fields := head.Fields(t)
		if len(fields) > 0 {
			b.WriteString("\n\t\tFields: []genField{\n")
			for _, f := range fields {
				fmt.Fprintf(&b, "\t\t\t{%q, %q, %d},\n", f, head.BaseField(t, f), introduced(head, snaps, t, f))
			}
			b.WriteString("\t\t},")
		}
		if ct := head.CreatorTypes(t); len(ct) > 0 {
			b.WriteString("\n\t\tCreators: []string{")
			for i, c := range ct {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%q", c)
			}
			b.WriteString("},")
		}
		b.WriteString("\n\t},\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}
