package syncsim

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robertjndw/gotero/conformance"
)

// snapshot is the server's view of one library.
type snapshot struct {
	version  int64
	objs     map[kind]map[string]*object
	deleted  map[string][]string
	setting  any
	settingV int64
	files    map[string][]byte // attachment key -> downloaded bytes
}

// verifierFor returns a fault-free client that can read the library.
func (s *sim) verifierFor(path string) *client {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.verifiers[path]; v != nil {
		return v
	}
	for _, c := range s.clients {
		for _, l := range c.libs {
			if l.path == path {
				v := s.newClient("verifier("+path+")", c.user, 0, nil)
				s.verifiers[path] = v
				return v
			}
		}
	}
	panic("syncsim: no client has " + path)
}

func (s *sim) serverVersion(ctx context.Context, path string) (int64, error) {
	r, err := s.verifierFor(path).do(ctx, http.MethodGet, path+"/collections", reqOpts{
		query: url.Values{"format": {"versions"}, "since": {strconv.FormatInt(1<<62, 10)}},
	})
	if err != nil {
		return 0, err
	}
	if v := r.lmv(); r.status == http.StatusOK && v >= 0 {
		return v, nil
	}
	return 0, fmt.Errorf("version of %s: %s", path, r)
}

// readSnapshot reads a library in full. The reads are not atomic, so it
// retries until the version stays put; nothing writes during the checks.
func (s *sim) readSnapshot(ctx context.Context, path string) (*snapshot, error) {
	v := s.verifierFor(path)
	var last error
	for range 5 {
		snap, err := v.readSnapshotOnce(ctx, path)
		if err == nil {
			return snap, nil
		}
		last = err
	}
	return nil, last
}

func (c *client) readSnapshotOnce(ctx context.Context, path string) (*snapshot, error) {
	snap := &snapshot{version: -1, objs: map[kind]map[string]*object{}, files: map[string][]byte{}}
	get := func(p string, q url.Values) (*response, error) {
		r, err := c.do(ctx, http.MethodGet, path+p, reqOpts{query: q})
		if err != nil {
			return nil, err
		}
		if r.status != http.StatusOK {
			return nil, protoErr("GET %s%s: %s", path, p, r)
		}
		if v := r.lmv(); snap.version < 0 {
			snap.version = v
		} else if v != snap.version {
			return nil, errVersionChanged
		}
		return r, nil
	}
	for _, k := range kinds {
		snap.objs[k] = map[string]*object{}
		r, err := get("/"+string(k), url.Values{"format": {"versions"}, "includeTrashed": {"1"}})
		if err != nil {
			return nil, err
		}
		var versions map[string]json.Number
		if err := decode(r.body, &versions); err != nil {
			return nil, err
		}
		keys := slices.Sorted(maps.Keys(versions))
		for len(keys) > 0 {
			n := min(len(keys), maxFetchKeys)
			r, err := get("/"+string(k), url.Values{
				k.keyParam(): {strings.Join(keys[:n], ",")}, "includeTrashed": {"1"}, "format": {"json"},
			})
			if err != nil {
				return nil, err
			}
			keys = keys[n:]
			var list []remoteObj
			if err := decode(r.body, &list); err != nil {
				return nil, err
			}
			for _, e := range list {
				v, _ := e.Version.Int64()
				snap.objs[k][e.Key] = &object{kind: k, key: e.Key, version: v, data: normalize(e.Data)}
			}
		}
		if len(snap.objs[k]) != len(versions) {
			return nil, protoErr("%s %s: versions lists %d objects, fetching them by key returned %d",
				path, k, len(versions), len(snap.objs[k]))
		}
	}
	r, err := get("/deleted", url.Values{"since": {"0"}})
	if err != nil {
		return nil, err
	}
	if err := decode(r.body, &snap.deleted); err != nil {
		return nil, err
	}
	r, err = get("/settings", nil)
	if err != nil {
		return nil, err
	}
	var settings map[string]remoteSetting
	if err := decode(r.body, &settings); err != nil {
		return nil, err
	}
	if tc, ok := settings["tagColors"]; ok {
		snap.setting = tc.Value
		snap.settingV, _ = tc.Version.Int64()
	}
	return snap, nil
}

func (s *sim) libraries() []string {
	seen := map[string]bool{}
	for _, c := range s.clients {
		for _, l := range c.libs {
			seen[l.path] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func (s *sim) check(ctx context.Context) {
	for _, path := range s.libraries() {
		snap, err := s.readSnapshot(ctx, path)
		if err != nil {
			s.fail("%s: reading final state: %v", path, err)
			continue
		}
		s.checkServer(ctx, path, snap)
		s.checkHistory(path, snap)
		s.checkFiles(ctx, path, snap)
		for _, c := range s.clients {
			for _, l := range c.libs {
				if l.path == path {
					s.checkClient(c, l, snap)
				}
			}
		}
		s.checkStream(ctx, path, snap)
	}
	s.hist.mu.Lock()
	for _, p := range s.hist.problems {
		s.fail("%s", p)
	}
	s.hist.mu.Unlock()
	for _, c := range s.clients {
		c.stream.mu.Lock()
		for _, p := range c.stream.problems {
			s.fail("%s: %s", c.name, p)
		}
		c.stream.mu.Unlock()
	}
}

// checkServer looks for states no Zotero client can sync: dangling
// references, collection cycles, objects that are alive and deleted at once,
// and incremental listings that disagree with the full one.
func (s *sim) checkServer(ctx context.Context, path string, snap *snapshot) {
	for _, k := range kinds {
		for key, o := range snap.objs[k] {
			if o.version > snap.version || o.version <= 0 {
				s.fail("%s: %s %s has version %d, library is at %d", path, k, key, o.version, snap.version)
			}
			if slices.Contains(snap.deleted[string(k)], key) {
				s.fail("%s: %s %s is both live and in the delete log", path, k, key)
			}
		}
	}
	colls, items := snap.objs[kCollection], snap.objs[kItem]
	for key, o := range colls {
		if p := str(o.data, "parentCollection"); p != "" && colls[p] == nil {
			s.fail("%s: collection %s has missing parent %s", path, key, p)
		}
		seen := map[string]bool{key: true}
		for p := str(o.data, "parentCollection"); p != "" && colls[p] != nil; p = str(colls[p].data, "parentCollection") {
			if seen[p] {
				s.fail("%s: collection %s is in a parent cycle", path, key)
				break
			}
			seen[p] = true
		}
	}
	for key, o := range items {
		if p := str(o.data, "parentItem"); p != "" {
			if items[p] == nil {
				s.fail("%s: item %s has missing parent %s", path, key, p)
			} else if str(items[p].data, "parentItem") != "" {
				s.fail("%s: item %s has a child item %s as parent", path, key, p)
			}
		}
		for _, ck := range strList(o.data, "collections") {
			if colls[ck] == nil {
				s.fail("%s: item %s is in missing collection %s", path, key, ck)
			}
		}
	}

	// Incremental sync must see exactly what changed after a version.
	v := s.verifierFor(path)
	sinces := []int64{0, snap.version / 3, snap.version / 2, snap.version - 1, snap.version}
	for _, since := range sinces {
		if since < 0 {
			continue
		}
		for _, k := range kinds {
			q := url.Values{"format": {"versions"}, "since": {strconv.FormatInt(since, 10)}, "includeTrashed": {"1"}}
			r, err := v.do(ctx, http.MethodGet, path+"/"+string(k), reqOpts{query: q})
			if err != nil || r.status != http.StatusOK {
				s.fail("%s: GET %s versions since %d: %v %v", path, k, since, err, r)
				continue
			}
			var got map[string]json.Number
			if err := decode(r.body, &got); err != nil {
				s.fail("%s: %s versions: %v", path, k, err)
				continue
			}
			want := map[string]string{}
			for key, o := range snap.objs[k] {
				if o.version > since {
					want[key] = strconv.FormatInt(o.version, 10)
				}
			}
			gotS := map[string]string{}
			for key, n := range got {
				gotS[key] = n.String()
			}
			if !maps.Equal(gotS, want) {
				s.fail("%s: %s versions since %d = %v, full listing says %v", path, k, since, gotS, want)
			}
		}
		if since == 0 {
			continue
		}
		r, err := v.do(ctx, http.MethodGet, path+"/items/top", reqOpts{query: url.Values{
			"format": {"versions"}, "since": {strconv.FormatInt(since, 10)}, "includeTrashed": {"1"},
		}})
		if err != nil || r.status != http.StatusOK {
			s.fail("%s: GET items/top: %v %v", path, err, r)
			continue
		}
		var top map[string]json.Number
		_ = decode(r.body, &top)
		for key, o := range items {
			_, listed := top[key]
			if want := o.version > since && str(o.data, "parentItem") == ""; listed != want {
				s.fail("%s: items/top since %d lists %s=%v, want %v", path, since, key, listed, want)
			}
		}
	}
}

// sameExceptLeftCollections compares an acknowledged object with what the
// server holds at the same version. Deleting a collection removes it from
// items without a new version, as on zotero.org, so the stored item may be
// in fewer collections than acknowledged, never in more.
func sameExceptLeftCollections(acked, stored obj) bool {
	a, s := normalize(acked), normalize(stored)
	ac, sc := strList(a, "collections"), strList(s, "collections")
	for _, c := range sc {
		if !slices.Contains(ac, c) {
			return false
		}
	}
	delete(a, "collections")
	delete(s, "collections")
	return equal(a, s)
}

// checkHistory compares the final state with every acknowledged write: the
// newest one of each object must still be there, unless something newer
// replaced or deleted it.
func (s *sim) checkHistory(path string, snap *snapshot) {
	h := s.hist
	h.mu.Lock()
	defer h.mu.Unlock()
	deletedAt := map[string]bool{}
	for _, k := range kinds {
		for _, key := range snap.deleted[string(k)] {
			deletedAt[objID(k, key)] = true
		}
	}
	for id, w := range h.writes[path] {
		k, key, _ := strings.Cut(id, "/")
		o := snap.objs[kind(k)][key]
		switch {
		case o == nil && !deletedAt[id]:
			s.fail("%s: %s was acknowledged at version %d by %s, is gone and not in the delete log (lost write)",
				path, id, w.version, w.by)
		case o == nil:
		case o.version < w.version:
			s.fail("%s: %s acknowledged at version %d by %s, server now has older version %d (lost write)",
				path, id, w.version, w.by, o.version)
		case o.version == w.version && !sameExceptLeftCollections(w.data, o.data):
			s.fail("%s: %s at version %d differs from what was acknowledged by %s:\n  acked:  %s\n  stored: %s",
				path, id, w.version, w.by, canon(w.data), canon(o.data))
		}
	}
	for id, dv := range h.deletes[path] {
		k, key, _ := strings.Cut(id, "/")
		// Deleting a missing key is a no-op, so only a key that is still
		// there with an older version means a delete was lost.
		if o := snap.objs[kind(k)][key]; o != nil && o.version < dv {
			s.fail("%s: %s was deleted at version %d but is still there at version %d", path, id, dv, o.version)
		}
	}
	// Registering a file bumps the item, so an item still at that version
	// must still point at that file.
	for key, a := range h.files[path] {
		o := snap.objs[kItem][key]
		switch {
		case o == nil:
		case o.version < a.version:
			s.fail("%s: file %s of item %s was registered at version %d, item is back at version %d (lost write)",
				path, a.data["md5"], key, a.version, o.version)
		case o.version == a.version && str(o.data, "md5") != a.data["md5"]:
			s.fail("%s: file %s of item %s was registered at version %d, item at that version has md5 %q (lost write)",
				path, a.data["md5"], key, a.version, str(o.data, "md5"))
		}
	}
	// Each registration must have replaced the file its If-Match named.
	// Without this, a server that ignores If-Match converges just as well,
	// by quietly letting the last upload win.
	for key, regs := range h.fileRegs[path] {
		regs = slices.SortedFunc(slices.Values(regs), func(a, b fileReg) int { return cmp.Compare(a.version, b.version) })
		for i, r := range regs[1:] {
			prev := regs[i]
			if r.base == "" || r.base == prev.md5 || h.maybeFiles[path][key][r.base] {
				continue
			}
			s.fail("%s: %s registered file %s of item %s at version %d with If-Match %s, but the file there was %s from %s at version %d (If-Match not enforced)",
				path, r.by, r.md5, key, r.version, r.base, prev.md5, prev.by, prev.version)
		}
	}
	if a, ok := h.settings[path]; ok && snap.setting != nil && snap.settingV == a.version &&
		!eqValue(snap.setting, a.data["value"]) {
		s.fail("%s: tagColors at version %d differs from what was acknowledged", path, a.version)
	}
}

func (s *sim) checkFiles(ctx context.Context, path string, snap *snapshot) {
	if !s.cfg.Files {
		return
	}
	v := s.verifierFor(path)
	for _, key := range slices.Sorted(maps.Keys(snap.objs[kItem])) {
		want := str(snap.objs[kItem][key].data, "md5")
		if want == "" {
			continue
		}
		// Reuse the client download path, which checks the redirect, the
		// headers and the bytes.
		l := newLibrary(path)
		o := &object{kind: kItem, key: key}
		if err := v.downloadFile(ctx, l, o); err != nil {
			s.fail("%s: downloading file of %s: %v", path, key, err)
			continue
		}
		f := l.files[key]
		if f == nil {
			s.fail("%s: item %s says md5 %s but the server has no file", path, key, want)
			continue
		}
		if f.md5 != want {
			s.fail("%s: item %s says md5 %s, the stored file has %s", path, key, want, f.md5)
		}
		snap.files[key] = f.content
	}
}

// checkClient compares one client's library with the server's.
func (s *sim) checkClient(c *client, l *library, snap *snapshot) {
	who := c.name + " " + l.path
	if l.version != snap.version {
		s.fail("%s: at library version %d, server at %d", who, l.version, snap.version)
	}
	if d, f := l.unsynced(); len(d)+len(f) > 0 {
		s.fail("%s: still has unsynced changes: %s", who, strings.Join(append(d, f...), "; "))
	}
	for _, k := range kinds {
		local, remote := l.objs[k], snap.objs[k]
		for _, key := range slices.Sorted(maps.Keys(remote)) {
			ro, lo := remote[key], local[key]
			switch {
			case lo == nil:
				s.fail("%s: missing %s %s (version %d)", who, k, key, ro.version)
			case lo.version < ro.version:
				// Ahead is fine: Zotero records the request's version for
				// objects the server reported unchanged.
				s.fail("%s: %s %s at version %d, server has newer %d", who, k, key, lo.version, ro.version)
			case !equal(lo.data, ro.data) && k == kItem && s.onlyStaleMemberships(l.path, lo.data, lo.version, ro.data):
				s.cfg.Logf("%s: item %s is still in collections deleted and re-created since (known gap)", who, key)
			case !equal(lo.data, ro.data):
				s.fail("%s: %s %s differs:\n  local:  %s\n  server: %s", who, k, key, canon(lo.data), canon(ro.data))
			}
		}
		for _, key := range slices.Sorted(maps.Keys(local)) {
			if remote[key] == nil {
				s.fail("%s: has %s %s that the server does not", who, k, key)
			}
		}
	}
	if !eqValue(l.tagColors.value, snap.setting) {
		s.fail("%s: tagColors %s, server %s", who, canon(l.tagColors.value), canon(snap.setting))
	}
	if !s.cfg.Files {
		return
	}
	for key, content := range snap.files {
		f := l.files[key]
		switch {
		case f == nil:
			s.fail("%s: never downloaded the file of %s", who, key)
		case string(f.content) != string(content):
			s.fail("%s: file of %s has md5 %s, server's has %s", who, key, conformance.MD5Hex(f.content), str(snap.objs[kItem][key].data, "md5"))
		}
	}
}

// onlyStaleMemberships reports whether a local item differs from the server
// only by membership in collections that were deleted after the local copy
// was current. Deleting a collection detaches its items without new
// versions, as on zotero.org. When another client re-creates the collection
// before this one syncs, the deletion never shows up here and the stale
// membership stays. That is a protocol gap, not a server bug.
func (s *sim) onlyStaleMemberships(path string, local obj, version int64, server obj) bool {
	lc, sc := strList(local, "collections"), strList(server, "collections")
	for _, c := range sc {
		if !slices.Contains(lc, c) {
			return false
		}
	}
	s.hist.mu.Lock()
	defer s.hist.mu.Unlock()
	for _, c := range lc {
		id := objID(kCollection, c)
		if !slices.Contains(sc, c) && max(s.hist.deletes[path][id], s.hist.seenDeleted[path][id]) <= version {
			return false
		}
	}
	return sameExceptLeftCollections(local, server)
}

// checkStream waits for every client to have heard about the final version.
// Zotero relies on that push to sync without polling.
func (s *sim) checkStream(ctx context.Context, path string, snap *snapshot) {
	if s.cfg.StreamURL == "" || snap.version == 0 {
		return
	}
	topic := "/" + path
	deadline := time.Now().Add(10 * time.Second)
	for _, c := range s.clients {
		has := slices.ContainsFunc(c.libs, func(l *library) bool { return l.path == path })
		if !has {
			continue
		}
		s.mu.Lock()
		floor, restarted := s.restartFloor[path]
		s.mu.Unlock()
		if restarted && snap.version <= floor {
			// Nothing was written after everyone subscribed again, and the
			// restart already checked that they did.
			continue
		}
		for c.stream.version(topic) < snap.version && time.Now().Before(deadline) && ctx.Err() == nil {
			time.Sleep(20 * time.Millisecond)
		}
		switch got := c.stream.version(topic); {
		case got < snap.version:
			s.fail("%s: newest topicUpdated for %s is version %d, library is at %d", c.name, topic, got, snap.version)
		case got > snap.version:
			s.fail("%s: topicUpdated for %s announced version %d, library is only at %d", c.name, topic, got, snap.version)
		}
	}
}
