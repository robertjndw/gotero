package syncsim

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
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

var (
	// errPrecondition is a request-level 412: download, then upload again.
	errPrecondition = errors.New("library modified since last sync")
	// errFull is a per-object 412 or 404, after which Zotero resets the
	// library with a full sync.
	errFull = errors.New("object out of date, full sync needed")
	// errVersionChanged restarts a download whose Last-Modified-Version moved.
	errVersionChanged = errors.New("library version changed during download")
	// errBusy is a download that kept restarting, which Zotero reports as
	// "library in use" and retries on the next sync.
	errBusy = errors.New("library kept changing during download")
	// errNoProgress is an upload batch in which every object failed, which
	// stops Zotero's sync until next time.
	errNoProgress = errors.New("made no progress during upload")
)

const (
	maxSyncLoops       = 8
	maxDownloadRetries = 10
	maxWriteObjects    = 50
	maxFetchKeys       = 100
)

// sync runs one Zotero sync of every library the client has.
func (c *client) sync(ctx context.Context) error {
	for _, l := range c.libs {
		if err := c.syncLibrary(ctx, l); err != nil {
			return fmt.Errorf("%s: %w", l.path, err)
		}
	}
	return nil
}

func (c *client) syncLibrary(ctx context.Context, l *library) error {
	c.sim.stats.syncs.Add(1)
	for loop := 0; ; loop++ {
		if loop == maxSyncLoops {
			c.sim.stats.loopLimit.Add(1)
			return nil
		}
		if l.needFull {
			c.sim.stats.fullSyncs.Add(1)
			if err := c.download(ctx, l, true); err != nil {
				return err
			}
			l.needFull = false
		}
		err := c.upload(ctx, l)
		switch {
		case errors.Is(err, errPrecondition):
			c.sim.stats.preconditions.Add(1)
		case errors.Is(err, errFull):
			l.needFull = true
			continue
		case err != nil:
			return err
		}
		if err := c.download(ctx, l, false); err != nil {
			return err
		}
		if !l.hasDataChanges() {
			break
		}
	}
	return c.syncFiles(ctx, l)
}

func (c *client) ifUnmodified(l *library) map[string]string {
	return map[string]string{"If-Unmodified-Since-Version": strconv.FormatInt(l.version, 10)}
}

// --- upload ---------------------------------------------------------------

func (c *client) upload(ctx context.Context, l *library) error {
	// Zotero strips a deleted tag from local items once, when the user
	// deletes it. An item that picks the tag up from another client before
	// the deletion uploads keeps it locally, while the server strips it at
	// the DELETE's version, which the client then adopts. Stripping again
	// here closes that client-side gap so it cannot mask server bugs.
	for tag := range l.pendingTagDel {
		l.removeTag(tag)
	}
	if err := c.uploadSettings(ctx, l); err != nil {
		return err
	}
	for _, k := range kinds {
		if err := c.uploadObjects(ctx, l, k); err != nil {
			return err
		}
	}
	for _, k := range []kind{kItem, kSearch, kCollection} {
		if err := c.uploadDeletions(ctx, l, k); err != nil {
			return err
		}
	}
	return c.uploadTagDeletions(ctx, l)
}

func (c *client) uploadSettings(ctx context.Context, l *library) error {
	s := &l.tagColors
	path := l.path + "/settings"
	switch {
	case l.pendingSetDel:
		r, err := c.do(ctx, http.MethodDelete, path, reqOpts{
			query: url.Values{"settingKey": {"tagColors"}}, header: c.ifUnmodified(l),
		})
		if err != nil {
			return err
		}
		v, err := c.writeStatus(l, "DELETE", path, r, http.StatusNoContent)
		if err != nil {
			return err
		}
		l.version = v
		l.pendingSetDel = false
		*s = setting{synced: true}
	case s.value != nil && s.dirty():
		// Zotero sends settings without a version; the library precondition
		// is the only guard.
		body, _ := json.Marshal(map[string]any{"tagColors": map[string]any{"value": s.value}})
		r, err := c.do(ctx, http.MethodPost, path, reqOpts{body: body, ctype: "application/json", header: c.ifUnmodified(l)})
		if err != nil {
			return err
		}
		v, err := c.writeStatus(l, "POST", path, r, http.StatusNoContent)
		if err != nil {
			return err
		}
		l.version, s.version, s.cache, s.synced = v, v, s.value, true
		c.sim.hist.recordSetting(l.path, v, s.value)
	}
	return nil
}

// writeStatus handles the statuses shared by every upload request.
func (c *client) writeStatus(l *library, method, path string, r *response, ok int) (int64, error) {
	v, err := c.libResponse(l, method, path, r)
	if err != nil {
		return 0, err
	}
	switch r.status {
	case ok:
		return v, nil
	case http.StatusPreconditionFailed:
		if v <= l.version {
			return 0, protoErr("%s %s: 412 but Last-Modified-Version %d is not newer than our %d", method, path, v, l.version)
		}
		return 0, errPrecondition
	}
	return 0, protoErr("%s %s: unexpected %s", method, path, r)
}

// uploadOrder lists dirty objects with parents ahead of their children, as
// the server needs the parent to exist when the child arrives.
func (c *client) uploadOrder(l *library, k kind) []*object {
	parentField := k.parentField()
	depth := func(o *object) int {
		d := 0
		for p := str(o.data, parentField); p != "" && d < 100; d++ {
			po := l.objs[k][p]
			if po == nil {
				break
			}
			p = str(po.data, parentField)
		}
		return d
	}
	var out []*object
	for _, key := range l.sortedKeys(k) {
		if o := l.objs[k][key]; o.dirty() {
			out = append(out, o)
		}
	}
	if parentField != "" {
		slices.SortStableFunc(out, func(a, b *object) int { return depth(a) - depth(b) })
	}
	return out
}

func uploadJSON(o *object) obj {
	var body obj
	if o.cache == nil {
		body = clone(normalize(o.data))
	} else {
		body = patchFor(o.cache, o.data)
	}
	// File metadata belongs to the file upload, not the item upload.
	delete(body, "md5")
	delete(body, "mtime")
	body["key"] = o.key
	body["version"] = o.version
	return body
}

type writeResponse struct {
	Successful map[string]struct {
		Key     string      `json:"key"`
		Version json.Number `json:"version"`
		Data    obj         `json:"data"`
	} `json:"successful"`
	Unchanged map[string]string `json:"unchanged"`
	Failed    map[string]struct {
		Key     string            `json:"key"`
		Code    int               `json:"code"`
		Message string            `json:"message"`
		Data    map[string]string `json:"data"`
	} `json:"failed"`
}

func (c *client) uploadObjects(ctx context.Context, l *library, k kind) error {
	pending := c.uploadOrder(l, k)
	path := l.path + "/" + string(k)
	for len(pending) > 0 {
		// Zotero sends small batches; vary the size so batch boundaries move.
		n := min(len(pending), 1+c.rng.IntN(maxWriteObjects))
		batch := pending[:n]
		pending = pending[n:]
		bodies := make([]obj, len(batch))
		for i, o := range batch {
			bodies[i] = uploadJSON(o)
		}
		raw, _ := json.Marshal(bodies)
		r, err := c.do(ctx, http.MethodPost, path, reqOpts{body: raw, ctype: "application/json", header: c.ifUnmodified(l)})
		if err != nil {
			return err
		}
		v, err := c.writeStatus(l, "POST", path, r, http.StatusOK)
		if err != nil {
			return err
		}
		var wr writeResponse
		if err := decode(r.body, &wr); err != nil {
			return protoErr("POST %s: bad write response: %v", path, err)
		}
		needFull, progress := false, false
		for i, o := range batch {
			idx := strconv.Itoa(i)
			if s, ok := wr.Successful[idx]; ok {
				sv, _ := s.Version.Int64()
				if s.Key != o.key || sv != v || s.Data == nil {
					return protoErr("POST %s: successful[%s] = key %s version %d, want key %s version %d with data",
						path, idx, s.Key, sv, o.key, v)
				}
				// Objects go up before this sync's deletions, so the saved
				// copy can still list a collection deleted here. Zotero
				// cannot put the item back into it locally.
				saved := l.withoutDeletedCollections(o.key, normalize(s.Data))
				o.version, o.data, o.cache, o.synced = sv, saved, clone(saved), true
				// History gets what the server confirmed, membership included:
				// the deletion may never go up if the collection is restored.
				c.sim.hist.recordWrite(l.path, k, o.key, sv, normalize(s.Data), c.name)
				progress = true
				continue
			}
			if key, ok := wr.Unchanged[idx]; ok {
				if key != o.key {
					return protoErr("POST %s: unchanged[%s] = %s, want %s", path, idx, key, o.key)
				}
				// Zotero records the request's library version even though
				// the object kept its older one on the server, so local
				// versions can run ahead and the server has to accept that.
				o.version, o.cache, o.synced = v, clone(o.data), true
				progress = true
				continue
			}
			f, ok := wr.Failed[idx]
			if !ok {
				return protoErr("POST %s: no outcome for index %s", path, idx)
			}
			switch f.Code {
			case http.StatusPreconditionFailed, http.StatusNotFound:
				c.sim.stats.objectResets.Add(1)
				needFull = true
			case http.StatusConflict:
				if err := c.missingDependency(l, path, o, f.Data); err != nil {
					return err
				}
			default:
				return protoErr("POST %s: object %s failed with %d %q (body %s)", path, o.key, f.Code, f.Message, canon(bodies[i]))
			}
		}
		if len(wr.Successful) > 0 {
			c.sim.hist.recordBump(l.path, v, fmt.Sprintf("%s POST %s", c.name, path))
		}
		l.version = v
		if needFull {
			return errFull
		}
		if !progress {
			return errNoProgress
		}
	}
	return nil
}

// missingDependency handles a 409 for a parent or collection the server does
// not have, the way Zotero does: the dependency named in data is marked for
// upload, and the object waits for the next sync.
func (c *client) missingDependency(l *library, path string, o *object, data map[string]string) error {
	c.sim.stats.missingDeps.Add(1)
	var dep *object
	switch {
	case data["collection"] != "":
		dep = l.objs[kCollection][data["collection"]]
	case data["parentItem"] != "":
		dep = l.objs[kItem][data["parentItem"]]
	default:
		return protoErr("POST %s: 409 for %s without data.collection or data.parentItem, so Zotero cannot repair it", path, o.key)
	}
	if dep == nil {
		return protoErr("POST %s: 409 for %s names %v, which this client does not have either", path, o.key, data)
	}
	dep.synced = false
	return nil
}

func (c *client) uploadDeletions(ctx context.Context, l *library, k kind) error {
	keys := slices.Sorted(maps.Keys(l.pendingDel[k]))
	path := l.path + "/" + string(k)
	for len(keys) > 0 {
		n := min(len(keys), maxWriteObjects)
		batch := keys[:n]
		keys = keys[n:]
		r, err := c.do(ctx, http.MethodDelete, path, reqOpts{
			query: url.Values{k.keyParam(): {strings.Join(batch, ",")}}, header: c.ifUnmodified(l),
		})
		if err != nil {
			return err
		}
		v, err := c.writeStatus(l, "DELETE", path, r, http.StatusNoContent)
		if err != nil {
			return err
		}
		l.version = v
		for _, key := range batch {
			delete(l.pendingDel[k], key)
			if k == kCollection {
				delete(l.dropped, key)
			}
		}
		c.sim.hist.recordDelete(l.path, k, batch, v)
	}
	return nil
}

func (c *client) uploadTagDeletions(ctx context.Context, l *library) error {
	if len(l.pendingTagDel) == 0 {
		return nil
	}
	tags := slices.Sorted(maps.Keys(l.pendingTagDel))
	path := l.path + "/tags"
	r, err := c.do(ctx, http.MethodDelete, path, reqOpts{
		query: url.Values{"tag": {strings.Join(tags, " || ")}}, header: c.ifUnmodified(l),
	})
	if err != nil {
		return err
	}
	v, err := c.writeStatus(l, "DELETE", path, r, http.StatusNoContent)
	if err != nil {
		return err
	}
	l.version = v
	clear(l.pendingTagDel)
	return nil
}

// --- download -------------------------------------------------------------

func (c *client) download(ctx context.Context, l *library, full bool) error {
	for range maxDownloadRetries {
		err := c.downloadOnce(ctx, l, full)
		if !errors.Is(err, errVersionChanged) {
			return err
		}
		c.sim.stats.restarts.Add(1)
	}
	c.sim.stats.busy.Add(1)
	return errBusy
}

// downloader pins the library version seen by the first request, as Zotero
// does. Any later response with another version restarts the download.
type downloader struct {
	c     *client
	l     *library
	since int64
	full  bool
	first int64
}

func (d *downloader) get(ctx context.Context, path string, q url.Values, hdr map[string]string) (*response, error) {
	r, err := d.c.do(ctx, http.MethodGet, path, reqOpts{query: q, header: hdr})
	if err != nil {
		return nil, err
	}
	return d.check(path, q, r)
}

// check validates a download response and pins or compares its version.
func (d *downloader) check(path string, q url.Values, r *response) (*response, error) {
	v, err := d.c.libResponse(d.l, "GET", path, r)
	if err != nil {
		return nil, err
	}
	if r.status != http.StatusOK && r.status != http.StatusNotModified {
		return nil, protoErr("GET %s?%s: unexpected %s", path, q.Encode(), r)
	}
	if d.first < 0 {
		d.first = v
	} else if v != d.first {
		return nil, errVersionChanged
	}
	return r, nil
}

func (d *downloader) sinceQuery(q url.Values) url.Values {
	if !d.full {
		q.Set("since", strconv.FormatInt(d.since, 10))
	}
	return q
}

func (c *client) downloadOnce(ctx context.Context, l *library, full bool) error {
	d := &downloader{c: c, l: l, since: l.version, full: full, first: -1}
	if full {
		d.since = 0
		// A full sync replays every deletion first.
		if err := c.downloadDeletions(ctx, d); err != nil {
			return err
		}
	}
	if err := c.downloadSettings(ctx, d); err != nil {
		return err
	}
	for _, k := range kinds {
		if err := c.downloadObjects(ctx, d, k); err != nil {
			return err
		}
		if k == kCollection {
			if n := l.fixCollectionCycles(); n > 0 {
				c.sim.stats.cycleFixes.Add(int64(n))
			}
		}
	}
	if err := c.repairRestored(ctx, d); err != nil {
		return err
	}
	if !full {
		if err := c.downloadDeletions(ctx, d); err != nil {
			return err
		}
	}
	l.version = d.first
	return nil
}

func (c *client) downloadDeletions(ctx context.Context, d *downloader) error {
	r, err := d.get(ctx, d.l.path+"/deleted", url.Values{"since": {strconv.FormatInt(d.since, 10)}}, nil)
	if err != nil {
		return err
	}
	var del map[string][]string
	if err := decode(r.body, &del); err != nil {
		return protoErr("GET deleted: %v", err)
	}
	for _, k := range []string{"collections", "searches", "items", "tags", "settings"} {
		if _, ok := del[k]; !ok {
			return protoErr("GET deleted: missing %q in %s", k, clip(r.body, 200))
		}
	}
	c.sim.hist.recordSeenDeleted(d.l.path, kCollection, del["collections"], r.lmv())
	c.applyDeletions(d.l, del)
	return nil
}

func (c *client) downloadSettings(ctx context.Context, d *downloader) error {
	var hdr map[string]string
	if !d.full {
		hdr = map[string]string{"If-Modified-Since-Version": strconv.FormatInt(d.since, 10)}
	}
	r, err := d.get(ctx, d.l.path+"/settings", d.sinceQuery(url.Values{}), hdr)
	if err != nil || r.status == http.StatusNotModified {
		return err
	}
	var settings map[string]remoteSetting
	if err := decode(r.body, &settings); err != nil {
		return protoErr("GET settings: %v", err)
	}
	if s, ok := settings["tagColors"]; ok {
		v, _ := s.Version.Int64()
		c.applyRemoteSetting(d.l, s.Value, v)
	}
	return nil
}

func (c *client) applyRemoteSetting(l *library, value any, version int64) {
	s := &l.tagColors
	if s.dirty() || l.pendingSetDel {
		if !eqValue(s.value, value) {
			c.sim.stats.conflicts.Add(1)
		}
		if c.policy == PreferLocal {
			s.version, s.cache = version, value
			return
		}
	}
	l.pendingSetDel = false
	*s = setting{value: value, version: version, cache: value, synced: true}
}

func (c *client) downloadObjects(ctx context.Context, d *downloader, k kind) error {
	l := d.l
	q := d.sinceQuery(url.Values{"format": {"versions"}})
	if k == kItem {
		q.Set("includeTrashed", "1")
	}
	r, err := d.get(ctx, l.path+"/"+string(k), q, nil)
	if err != nil {
		return err
	}
	var versions map[string]json.Number
	if err := decode(r.body, &versions); err != nil {
		return protoErr("GET %s versions: %v", k, err)
	}
	var need []string
	for key, vn := range versions {
		v, err := vn.Int64()
		if err != nil {
			return protoErr("GET %s versions: bad version %q for %s", k, vn, key)
		}
		if v > d.first {
			// The server reads the library version before the listing, outside
			// one snapshot, so a write committing in between shows up early.
			// That is safe: the next response carries the newer version and
			// restarts the download. The reverse order could hide a deletion.
			c.sim.stats.aheadOfVersion.Add(1)
		}
		// Local versions may be ahead after an "unchanged" upload.
		if o := l.objs[k][key]; o == nil || o.version < v {
			need = append(need, key)
		}
	}
	for key := range l.fetch[k] {
		if _, listed := versions[key]; !listed {
			need = append(need, key)
		}
	}
	slices.Sort(need)
	type fetched struct {
		key  string
		v    int64
		data obj
	}
	var deferred []fetched
	for len(need) > 0 {
		n := min(len(need), maxFetchKeys)
		chunk := need[:n]
		need = need[n:]
		r, err := d.get(ctx, l.path+"/"+string(k), url.Values{
			k.keyParam(): {strings.Join(chunk, ",")}, "includeTrashed": {"1"}, "format": {"json"},
		}, nil)
		if err != nil {
			return err
		}
		var list []remoteObj
		if err := decode(r.body, &list); err != nil {
			return protoErr("GET %s by key: %v", k, err)
		}
		// Zotero applies each batch top-level objects first. A child whose
		// parent is deleted here fails to save and is retried after the
		// rest, by which time a parent restored in the same batch is back;
		// handling those last has the same outcome.
		if parentField := k.parentField(); parentField != "" {
			rank := func(e remoteObj) int {
				switch p := str(e.Data, parentField); {
				case p == "":
					return 0
				case l.pendingDel[k][p]:
					return 2
				}
				return 1
			}
			slices.SortStableFunc(list, func(a, b remoteObj) int { return cmp.Compare(rank(a), rank(b)) })
		}
		for _, e := range list {
			v, _ := e.Version.Int64()
			if dv, ok := e.Data["version"].(json.Number); ok && dv.String() != e.Version.String() {
				return protoErr("GET %s: %s has version %s but data.version %s", k, e.Key, e.Version, dv)
			}
			if !slices.Contains(chunk, e.Key) {
				return protoErr("GET %s: asked for %v, got %s", k, chunk, e.Key)
			}
			if !c.applyRemote(l, k, e.Key, v, normalize(e.Data)) {
				deferred = append(deferred, fetched{e.Key, v, normalize(e.Data)})
			}
		}
		for _, key := range chunk {
			delete(l.fetch[k], key)
			// Gone from the server, with no delete record this client will
			// see: it went with a parent this client deleted, after moving
			// there remotely. Zotero only drops the key from its retry queue
			// and keeps the stale copy; dropping it keeps that client gap
			// out of the checks.
			if o := l.objs[k][key]; o != nil && !o.dirty() && !slices.ContainsFunc(list, func(e remoteObj) bool { return e.Key == key }) {
				c.trace.add("sync: %s %s no longer exists on the server, dropping the stale copy", k, key)
				// Descendants that are under it only here were not part of
				// the deletion on the server, so they come back from there.
				for _, gone := range l.removeLocal(k, key) {
					if gone != key {
						c.trace.add("sync: %s %s went with %s here but not on the server", k, gone, key)
						l.fetch[k][gone] = true
						if k == kCollection {
							l.repairOnFetch[gone] = true
						}
					}
				}
			}
		}
	}
	// Children can arrive before their parents. Whatever still has no
	// parent after the rest is in is tried again next sync.
	for progress := true; progress && len(deferred) > 0; {
		progress = false
		rest := deferred[:0]
		for _, f := range deferred {
			if c.applyRemote(l, k, f.key, f.v, f.data) {
				progress = true
			} else {
				rest = append(rest, f)
			}
		}
		deferred = rest
	}
	for _, f := range deferred {
		c.trace.add("sync: %s %s refers to something not here yet, fetching again next sync", k, f.key)
		l.fetch[k][f.key] = true
	}
	if d.full {
		// Whatever the server does not have goes up again as new, synced or
		// not. Deletions were applied first, so these were never deleted.
		for _, key := range l.sortedKeys(k) {
			if _, ok := versions[key]; !ok {
				o := l.objs[k][key]
				o.version, o.cache, o.synced = 0, nil, false
			}
		}
	}
	return nil
}

// remoteObj is one object of a JSON listing.
type remoteObj struct {
	Key     string      `json:"key"`
	Version json.Number `json:"version"`
	Data    obj         `json:"data"`
}

// remoteSetting is one entry of a settings listing.
type remoteSetting struct {
	Value   any         `json:"value"`
	Version json.Number `json:"version"`
}

// applyRemote takes a newer server copy into the local database. It reports
// false when the object's parent or one of its collections is not here, and
// the object is then fetched again later.
func (c *client) applyRemote(l *library, k kind, key string, v int64, remote obj) bool {
	parentField := k.parentField()
	if p := str(remote, parentField); p != "" && l.objs[k][p] == nil {
		if l.pendingDel[k][p] {
			// Zotero cannot save it and retries it on later syncs. Usually
			// the parent's deletion uploads first and takes the child with it
			// on the server, so the retry finds nothing; if the parent is
			// restored instead, the retry brings the child back.
			c.trace.add("sync: skipping %s %s, its parent %s is deleted here", k, key, p)
		}
		return false
	}
	if l.pendingDel[k][key] {
		// Deleted here, changed there. Zotero restores collections and
		// searches by itself and asks about items, unless the remote item is
		// in the trash anyway.
		if k == kItem {
			if remote["deleted"] != nil {
				return true
			}
			c.sim.stats.conflicts.Add(1)
			if c.policy == PreferLocal {
				return true
			}
		}
		c.trace.add("sync: restoring %s %s deleted here, changed remotely", k, key)
		delete(l.pendingDel[k], key)
		if k == kCollection {
			l.restored = append(l.restored, key)
		}
	}
	if k == kItem {
		// An item in a collection this client does not have waits, as in
		// Zotero, until the collection arrives or the item leaves it.
		for _, coll := range strList(remote, "collections") {
			if l.objs[kCollection][coll] == nil && !l.pendingDel[kCollection][coll] {
				return false
			}
		}
		// Membership in a collection deleted here is dropped, as Zotero
		// does when it erases the collection; the deletion uploads later.
		remote = l.withoutDeletedCollections(key, remote)
	}
	if k == kCollection && l.repairOnFetch[key] {
		delete(l.repairOnFetch, key)
		l.restored = append(l.restored, key)
	}
	o := l.objs[k][key]
	switch {
	case o == nil:
		l.objs[k][key] = &object{kind: k, key: key, version: v, data: remote, cache: clone(remote), synced: true}
	case !o.dirty():
		o.version, o.data, o.cache = v, remote, clone(remote)
	default:
		base := o.cache
		if base == nil {
			base = obj{}
		}
		merged, n := merge3(base, o.data, remote, c.policy)
		c.sim.stats.conflicts.Add(int64(n))
		o.version, o.data, o.cache = v, merged, clone(remote)
		o.synced = equal(merged, remote)
	}
	return true
}

// repairRestored puts items back into collections that were deleted here
// and restored by a remote change, since deleting the collection took them
// out locally.
func (c *client) repairRestored(ctx context.Context, d *downloader) error {
	l := d.l
	for len(l.restored) > 0 {
		coll := l.restored[0]
		if l.objs[kCollection][coll] != nil {
			r, err := d.c.do(ctx, http.MethodGet, l.path+"/collections/"+coll+"/items/top", reqOpts{query: url.Values{"format": {"keys"}}})
			if err != nil {
				return err
			}
			if r.status == http.StatusNotFound {
				// Deleted again since; the next download brings that.
				l.restored = l.restored[1:]
				continue
			}
			if r, err = d.check(l.path+"/collections/"+coll+"/items/top", url.Values{}, r); err != nil {
				return err
			}
			for _, key := range strings.Fields(string(r.body)) {
				o := l.objs[kItem][key]
				if o == nil {
					delete(l.pendingDel[kItem], key)
					l.fetch[kItem][key] = true
					continue
				}
				if cols := strList(o.data, "collections"); !slices.Contains(cols, coll) {
					setStrList(o.data, "collections", append(cols, coll))
					o.data = normalize(o.data)
					o.synced = false
				}
				if o.data["deleted"] != nil {
					delete(o.data, "deleted")
					o.synced = false
				}
			}
			// The listing leaves out trashed items, and Zotero relies on
			// that (it untrashes whatever comes back), so a trashed member
			// stays out of the collection here while the server still has
			// it there. That happens against zotero.org too. Put such items
			// back from the last server copy so the gap does not hide
			// server bugs.
			for _, o := range l.objs[kItem] {
				if o.data["deleted"] == nil || !slices.Contains(strList(o.cache, "collections"), coll) && !l.dropped[coll][o.key] {
					continue
				}
				if cols := strList(o.data, "collections"); !slices.Contains(cols, coll) {
					c.trace.add("sync: trashed item %s left out of restored collection %s", o.key, coll)
					setStrList(o.data, "collections", append(cols, coll))
					o.data = normalize(o.data)
				}
			}
		}
		delete(l.dropped, coll)
		l.restored = l.restored[1:]
	}
	return nil
}

func (c *client) applyDeletions(l *library, del map[string][]string) {
	// Tags are not processed: their removal reaches clients as item
	// changes, because the deleting client uploads the items first.
	for _, k := range []kind{kCollection, kSearch, kItem} {
		for _, key := range del[string(k)] {
			for _, gone := range c.applyRemoteDelete(l, k, key) {
				// Zotero erases local descendants along with the parent,
				// including one moved under it here that the server never
				// saw there, and then never downloads it again. Fetching
				// those back closes that client gap.
				if gone != key && !slices.Contains(del[string(k)], gone) {
					c.trace.add("sync: %s %s went with its parent here but not on the server", k, gone)
					l.fetch[k][gone] = true
					if k == kCollection {
						l.repairOnFetch[gone] = true
					}
				}
			}
		}
	}
	if slices.Contains(del["settings"], "tagColors") {
		s := &l.tagColors
		// A setting changed here survives a remote deletion.
		if !s.dirty() && !l.pendingSetDel {
			*s = setting{synced: true}
		}
	}
}

// applyRemoteDelete returns the keys it removed locally.
func (c *client) applyRemoteDelete(l *library, k kind, key string) []string {
	if l.pendingDel[k][key] {
		delete(l.pendingDel[k], key)
		if k == kCollection {
			delete(l.dropped, key)
		}
		return nil
	}
	o := l.objs[k][key]
	if o == nil {
		return nil
	}
	c.trace.add("sync: remote deleted %s %s (local synced=%v)", k, key, o.synced)
	if o.dirty() {
		// Zotero keeps unsynced collections and searches without asking.
		// Their next upload names a missing object, which ends in a full
		// sync that uploads them again as new. Items are a conflict, except
		// that an item already in the local trash just goes.
		if k == kCollection {
			// Zotero keeps the items in it too, but the server dropped the
			// membership and nothing would ever send it again: the
			// collection comes back as new and the items stay synced. The
			// simulator re-sends them so that known client gap does not
			// hide server bugs.
			for _, it := range l.objs[kItem] {
				if slices.Contains(strList(it.data, "collections"), key) {
					it.synced = false
					if it.cache != nil {
						// Otherwise the patch leaves the unchanged list out.
						delete(it.cache, "collections")
					}
				}
			}
		}
		if k != kItem {
			return nil
		}
		if o.data["deleted"] == nil {
			c.sim.stats.conflicts.Add(1)
			if c.policy == PreferLocal {
				return nil
			}
		}
	}
	return l.removeLocal(k, key)
}

// --- files ----------------------------------------------------------------

func (c *client) syncFiles(ctx context.Context, l *library) error {
	for _, key := range l.sortedKeys(kItem) {
		o := l.objs[kItem][key]
		f := l.files[key]
		// A file here that the server lacks goes up, as Zotero does when an
		// attachment came back as new after being deleted elsewhere.
		if f != nil && !f.dirty && f.md5 != "" && o.version > 0 && o.synced && str(o.data, "md5") == "" {
			f.dirty, f.synced = true, ""
		}
		if f == nil || !f.dirty || o.version == 0 {
			continue
		}
		if err := c.uploadFile(ctx, l, o, f); err != nil {
			return err
		}
	}
	for _, key := range l.sortedKeys(kItem) {
		o := l.objs[kItem][key]
		want := str(o.data, "md5")
		f := l.files[key]
		if want == "" || (f != nil && (f.dirty || f.md5 == want)) {
			continue
		}
		if err := c.downloadFile(ctx, l, o); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) fileHeader(remoteMD5 string) map[string]string {
	if remoteMD5 == "" {
		return map[string]string{"If-None-Match": "*"}
	}
	return map[string]string{"If-Match": remoteMD5}
}

func (c *client) uploadFile(ctx context.Context, l *library, o *object, f *file) error {
	path := l.path + "/items/" + o.key + "/file"
	for attempt := 0; ; attempt++ {
		remote := str(o.cache, "md5")
		if remote == f.md5 {
			f.dirty, f.synced = false, f.md5
			return nil
		}
		// Zotero usually spots a file changed on both sides when it
		// downloads, before uploading. The simulation leaves that to the
		// server on purpose, so every such collision exercises its 412.
		mtime := time.Now().UnixMilli()
		base := f.synced
		form := url.Values{
			"md5": {f.md5}, "filename": {"file.txt"}, "filesize": {strconv.Itoa(len(f.content))},
			"mtime": {strconv.FormatInt(mtime, 10)},
		}
		r, err := c.do(ctx, http.MethodPost, path, reqOpts{
			body: []byte(form.Encode()), ctype: "application/x-www-form-urlencoded", header: c.fileHeader(base),
		})
		if err != nil {
			// An authorize for a file the server already has can apply it
			// on the spot, so a lost one may have gone through.
			if errors.Is(err, errNetwork) {
				c.sim.hist.recordMaybeFile(l.path, o.key, f.md5)
			}
			return err
		}
		switch r.status {
		case http.StatusOK:
		case http.StatusPreconditionFailed:
			if done, err := c.fileConflict(ctx, l, o, f, attempt); done || err != nil {
				return err
			}
			continue
		case http.StatusNotFound:
			// Deleted elsewhere; the next download removes the item.
			f.dirty = false
			return nil
		default:
			return protoErr("POST %s (authorize): unexpected %s", path, r)
		}
		var auth struct {
			Exists      int    `json:"exists"`
			URL         string `json:"url"`
			ContentType string `json:"contentType"`
			Prefix      string `json:"prefix"`
			Suffix      string `json:"suffix"`
			UploadKey   string `json:"uploadKey"`
		}
		if err := decode(r.body, &auth); err != nil {
			return protoErr("POST %s (authorize): %v", path, err)
		}
		if auth.Exists == 1 {
			if _, err := c.libResponse(l, "POST", path, r); err != nil {
				return err
			}
			// Nothing says whether this changed the item or found the file
			// already there, so it cannot anchor the precondition check.
			c.sim.hist.recordMaybeFile(l.path, o.key, f.md5)
			c.fileSynced(o, f, mtime)
			return nil
		}
		if auth.URL == "" || auth.UploadKey == "" {
			return protoErr("POST %s (authorize): no url or uploadKey in %s", path, r)
		}
		body := append(append([]byte(auth.Prefix), f.content...), auth.Suffix...)
		up, err := c.do(ctx, http.MethodPost, auth.URL, reqOpts{abs: true, noKey: true, body: body, ctype: auth.ContentType})
		if err != nil {
			return err
		}
		if up.status != http.StatusCreated {
			return protoErr("upload to %s: want 201, got %s", shortURL(auth.URL), up)
		}
		reg, err := c.do(ctx, http.MethodPost, path, reqOpts{
			body: []byte(url.Values{"upload": {auth.UploadKey}}.Encode()), ctype: "application/x-www-form-urlencoded",
			header: c.fileHeader(base),
		})
		if err != nil {
			if errors.Is(err, errNetwork) {
				c.sim.hist.recordMaybeFile(l.path, o.key, f.md5)
			}
			return err
		}
		switch reg.status {
		case http.StatusNoContent:
			v, err := c.libResponse(l, "POST", path, reg)
			if err != nil {
				return err
			}
			c.fileSynced(o, f, mtime)
			c.sim.hist.recordFile(l.path, o.key, f.md5, base, v, c.name)
			return nil
		case http.StatusPreconditionFailed:
			if done, err := c.fileConflict(ctx, l, o, f, attempt); done || err != nil {
				return err
			}
		case http.StatusNotFound:
			f.dirty = false
			return nil
		default:
			return protoErr("POST %s (register): unexpected %s", path, reg)
		}
	}
}

// fileSynced records a finished upload the way Zotero does: the item's
// synced copy now shows the new file, so the next upload sends If-Match
// with it instead of colliding with itself.
func (c *client) fileSynced(o *object, f *file, mtime int64) {
	f.dirty, f.synced = false, f.md5
	for _, d := range []obj{o.data, o.cache} {
		if d != nil {
			d["md5"] = f.md5
			d["mtime"] = json.Number(strconv.FormatInt(mtime, 10))
		}
	}
}

// fileConflict handles a 412 on authorize or register: someone else changed
// the file. With PreferRemote the local change is dropped and the remote file
// downloaded later; with PreferLocal the item is refreshed and the upload
// retried against the new md5.
func (c *client) fileConflict(ctx context.Context, l *library, o *object, f *file, attempt int) (bool, error) {
	c.sim.stats.fileConflicts.Add(1)
	if c.policy == PreferRemote || attempt >= 3 {
		f.dirty = false
		f.md5 = "" // forces a download of the remote file
		return true, nil
	}
	r, err := c.do(ctx, http.MethodGet, l.path+"/items/"+o.key, reqOpts{})
	if err != nil {
		return true, err
	}
	if r.status == http.StatusNotFound {
		f.dirty = false
		return true, nil
	}
	if r.status != http.StatusOK {
		return true, protoErr("GET item %s: unexpected %s", o.key, r)
	}
	var e struct {
		Version json.Number `json:"version"`
		Data    obj         `json:"data"`
	}
	if err := decode(r.body, &e); err != nil {
		return true, protoErr("GET item %s: %v", o.key, err)
	}
	v, _ := e.Version.Int64()
	if v > o.version {
		c.applyRemote(l, kItem, o.key, v, normalize(e.Data))
	}
	// Keeping the local file, as a user would in Zotero's conflict dialog,
	// means uploading over the file that is there now.
	f.synced = str(e.Data, "md5")
	return false, nil
}

func (c *client) downloadFile(ctx context.Context, l *library, o *object) error {
	path := l.path + "/items/" + o.key + "/file"
	r, err := c.do(ctx, http.MethodGet, path, reqOpts{})
	if err != nil {
		return err
	}
	if r.status == http.StatusNotFound {
		return nil
	}
	if r.status != http.StatusFound {
		return protoErr("GET %s: want 302, got %s", path, r)
	}
	loc := r.header.Get("Location")
	wantMD5 := r.header.Get("Zotero-File-MD5")
	if loc == "" || wantMD5 == "" {
		return protoErr("GET %s: 302 without Location or Zotero-File-MD5", path)
	}
	body, err := c.do(ctx, http.MethodGet, loc, reqOpts{abs: true, noKey: true})
	if err != nil {
		return err
	}
	if body.status == http.StatusNotFound {
		// The file changed between the redirect and the download; the
		// token is bound to the old one. Zotero tries again next sync.
		return nil
	}
	if body.status != http.StatusOK {
		return protoErr("GET %s (download target): want 200, got %d", path, body.status)
	}
	got := conformance.MD5Hex(body.body)
	if got != wantMD5 {
		return protoErr("GET %s: bytes have md5 %s, Zotero-File-MD5 says %s", path, got, wantMD5)
	}
	l.files[o.key] = &file{content: body.body, md5: got, synced: got}
	return nil
}
