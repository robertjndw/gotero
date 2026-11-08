package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/zapi"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

// batchSize is how many objects are fetched and written together. It matches
// the 100 keys zotero.org accepts per request.
const batchSize = 100

// lib is one library being imported.
type lib struct {
	st  *libState
	src *zoteroclient.Library
	id  int64 // gotero library ID
	// fresh is true while importing into a library that was empty when the
	// import began.
	fresh bool
}

// addConflicts counts objects of a phase that were left as edited in gotero.
func (r *run) addConflicts(l *lib, phase string, n int64) {
	if n == 0 {
		return
	}
	r.update(func() {
		if l.st.Conflicts == nil {
			l.st.Conflicts = map[string]int64{}
		}
		l.st.Conflicts[phase] += n
	})
}

// observe records the library version a response carried. Any response of a
// pass carrying another version than the first means the phases did not all
// see the same library.
func (l *lib) observe(r *run, v int64) {
	if v <= 0 {
		return
	}
	r.update(func() {
		st := l.st
		st.LMV = max(st.LMV, v)
		if st.PassStart == 0 {
			st.PassStart = v
		} else if v != st.PassStart {
			st.Drift = true
		}
	})
}

// listed records the version a phase's listing was taken at.
func (l *lib) listed(r *run, phase string, v int64) {
	l.observe(r, v)
	r.update(func() {
		if l.st.Listed == nil {
			l.st.Listed = map[string]int64{}
		}
		l.st.Listed[phase] = v
	})
}

// lowWater is the source version every phase of the last pass is complete
// up to: each phase holds everything its listing showed, so the oldest
// listing bounds them all.
func lowWater(st *libState) int64 {
	low := int64(-1)
	for _, v := range st.Listed {
		if low < 0 || v < low {
			low = v
		}
	}
	if low < 0 {
		// A cursor written before phases recorded their listing.
		return st.LMV
	}
	return low
}

// write runs fn in a library transaction. In fresh mode it remembers the
// version the library ended up at, see libState.Local.
func (r *run) write(ctx context.Context, l *lib, fn func(tx repo.LibraryTx) error) (int64, error) {
	v, err := r.w.Store.InLibrary(ctx, l.id, fn)
	if err == nil && l.fresh {
		r.update(func() { l.st.Local = max(l.st.Local, v) })
	}
	return v, err
}

func (r *run) importLibrary(ctx context.Context, st *libState) error {
	l := &lib{st: st}
	if st.GroupID == 0 {
		l.src = r.client.User(r.userID)
		l.id = r.user.LibraryID
	} else {
		l.src = r.client.Group(st.GroupID)
		id, skip, err := r.openGroup(ctx, st)
		if err != nil {
			return err
		}
		if skip != "" {
			r.log.WarnContext(ctx, "importer: group skipped", "group", st.GroupID, "reason", skip)
			r.update(func() { st.Error, st.Done = skip, true })
			return nil
		}
		l.id = id
	}

	if st.Mode == "" {
		rd, err := r.w.Store.Library(ctx, l.id)
		if err != nil {
			return err
		}
		// Version 0 means nothing was ever written, so zotero.org's versions
		// cannot collide with or hide anything.
		mode := modeMerge
		if rd.Version() == 0 {
			mode = modeFresh
		} else if fresh, err := r.continuesFresh(ctx, st.ID, rd.Version()); err != nil {
			return err
		} else if fresh {
			mode = modeFresh
		}
		r.update(func() { st.Mode = mode })
		// Saved before the first write: after a crash the library is no
		// longer empty, and the resume must not mistake it for a merge.
		if err := r.checkpoint(ctx, true); err != nil {
			return err
		}
	}
	l.fresh = st.Mode == modeFresh

	for {
		if err := r.runPhases(ctx, l); err != nil {
			return err
		}
		if !st.Drift {
			// Every response of the pass carried the same version, so the
			// library was copied as it stood at that version.
			r.update(func() { st.Copied = max(st.PassStart, lowWater(st)) })
			break
		}
		if st.Pass+1 >= maxPasses {
			// Still changing. Only what every phase saw counts as copied;
			// summary() turns the persisted Drift into a warning.
			r.log.WarnContext(ctx, "importer: source kept changing", "library", st.ID, "copied", lowWater(st), "lmv", st.LMV)
			r.update(func() { st.Copied = lowWater(st) })
			break
		}
		// A catch-up pass asks each phase only for what changed after its
		// own listing, which also catches a change to a phase that was
		// already done when the change was noticed.
		r.update(func() {
			st.Pass++
			st.Phase = phaseCollections
			st.Since, st.Listed = st.Listed, nil
			st.PassStart, st.Drift = 0, false
		})
		if err := r.checkpoint(ctx, true); err != nil {
			return err
		}
	}

	if err := r.syncFiles(ctx, l); err != nil {
		return err
	}
	v, err := r.write(ctx, l, func(tx repo.LibraryTx) error {
		// Only a fresh import adopts the source version. Elsewhere it would
		// just open a gap, and objects already carry versions of their own.
		if l.fresh {
			return tx.RaiseVersion(ctx, st.Copied)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("set library version: %w", err)
	}
	r.update(func() {
		st.Done = true
		if n := sum(st.Conflicts); n > 0 && st.Error == "" {
			st.Error = conflictMessage(n)
		}
	})
	if r.w.Publisher != nil {
		topic := "/users/" + strconv.FormatInt(r.user.ID, 10)
		if st.GroupID != 0 {
			topic = "/groups/" + strconv.FormatInt(st.GroupID, 10)
		}
		r.w.Publisher.Publish(zapi.Event{Topic: topic, Version: v})
	}
	return nil
}

// continuesFresh reports whether a library that is no longer empty may still
// be imported in fresh mode, because the content is all from an earlier fresh
// import of the same zotero.org account that did not get to the end: it
// failed, was cancelled, or the source kept changing. Without this, one
// transient error would turn every rerun into a merge, and the library would
// never get zotero.org's versions. The library version must still be the one
// that import left behind, or something else wrote to it in between.
func (r *run) continuesFresh(ctx context.Context, libID string, version int64) (bool, error) {
	jobs, err := r.w.Store.Imports().ListByUser(ctx, r.job.UserID)
	if err != nil {
		return false, err
	}
	for _, j := range jobs {
		if j.ID >= r.job.ID || len(j.Cursor) == 0 {
			continue
		}
		var c cursor
		if json.Unmarshal(j.Cursor, &c) != nil {
			continue
		}
		for _, st := range c.Libs {
			if st.ID != libID {
				continue
			}
			// The newest earlier job that touched the library decides.
			return c.Source == r.userID && st.Mode == modeFresh && (!st.Done || st.Drift) &&
				st.Local == version, nil
		}
	}
	return false, nil
}

// runPhases runs the metadata phases from the saved one onwards.
func (r *run) runPhases(ctx context.Context, l *lib) error {
	st := l.st
	if st.Phase == "" {
		st.Phase = phaseCollections
	}
	start := slices.Index(phaseOrder, st.Phase)
	if start < 0 || st.Phase == phaseFiles {
		return nil
	}
	for _, phase := range phaseOrder[start:] {
		if phase == phaseFiles {
			break
		}
		r.update(func() { st.Phase = phase })
		var err error
		switch phase {
		case phaseCollections:
			err = r.syncObjects(ctx, l, repo.TypeCollection)
		case phaseSearches:
			err = r.syncObjects(ctx, l, repo.TypeSearch)
		case phaseItems:
			err = r.syncObjects(ctx, l, repo.TypeItem)
		case phaseSettings:
			err = r.syncSettings(ctx, l)
		case phaseFulltext:
			err = r.syncFulltext(ctx, l)
		}
		if err != nil {
			return fmt.Errorf("%s of %s: %w", phase, st.Name, err)
		}
		if err := r.checkpoint(ctx, false); err != nil {
			return err
		}
	}
	r.update(func() { st.Phase = phaseFiles })
	return nil
}

// openGroup finds or creates the gotero group for a zotero.org group. A
// non-empty skip reason means the group cannot be imported but the job goes on.
func (r *run) openGroup(ctx context.Context, st *libState) (libraryID int64, skip string, err error) {
	gid := st.GroupID
	groups := r.w.Store.Groups()
	existing, err := groups.Get(ctx, gid)
	if errors.Is(err, repo.ErrNotFound) {
		g, gerr := r.client.GetGroup(ctx, gid)
		if zoteroclient.IsForbidden(gerr) || zoteroclient.IsNotFound(gerr) {
			return 0, "zotero.org does not let this API key read the group.", nil
		}
		if gerr != nil {
			return 0, "", fmt.Errorf("read group %d: %w", gid, gerr)
		}
		r.update(func() { st.Name = g.Data.Name })
		existing, err = groups.Create(ctx, repo.Group{
			ID: gid, OwnerID: r.user.ID, Name: g.Data.Name, Description: g.Data.Description, URL: g.Data.URL,
			LibraryReading: pick(g.Data.LibraryReading, "all", "members"),
			LibraryEditing: pick(g.Data.LibraryEditing, "admins", "members"),
			FileEditing:    pick(g.Data.FileEditing, "none", "admins", "members"),
		})
		if errors.Is(err, repo.ErrConflict) {
			// Someone took the ID between our check and the insert.
			existing, err = groups.Get(ctx, gid)
		}
	}
	if err != nil {
		return 0, "", fmt.Errorf("prepare group %d: %w", gid, err)
	}
	if existing.OwnerID != r.user.ID {
		return 0, fmt.Sprintf("Group ID %d is already used by a group you do not own, so it was skipped.", gid), nil
	}
	r.update(func() { st.Name = existing.Name })
	if r.w.Notifier != nil {
		r.w.Notifier.AccessChanged(r.user.ID)
	}
	return existing.LibraryID, "", nil
}

// pick returns v if it is one of the allowed policy values, else "" so the
// column default applies.
func pick(v string, allowed ...string) string {
	if slices.Contains(allowed, v) {
		return v
	}
	return ""
}

// localVersions lists key -> version of what gotero holds for the library.
func (r *run) localVersions(ctx context.Context, libID int64, t repo.ObjectType) (map[string]int64, error) {
	rd, err := r.w.Store.Library(ctx, libID)
	if err != nil {
		return nil, err
	}
	seq, err := rd.Versions(ctx, repo.VersionsQuery{Type: t, IncludeTrashed: true})
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for kv, err := range seq {
		if err != nil {
			return nil, err
		}
		out[kv.Key] = kv.Version
	}
	return out, nil
}

func zType(t repo.ObjectType) zotero.ObjectType { return zotero.ObjectType(t) }

// listVersions is Versions with a 304 for an unchanged library read as an
// empty answer. It only happens with since > 0.
func listVersions(ctx context.Context, src *zoteroclient.Library, t repo.ObjectType, q zoteroclient.VersionsQuery) (map[string]int64, int64, error) {
	out, v, err := src.Versions(ctx, zType(t), q)
	if zoteroclient.IsNotModified(err) {
		return map[string]int64{}, v, nil
	}
	return out, v, err
}

// syncObjects brings collections, searches or items up to date.
func (r *run) syncObjects(ctx context.Context, l *lib, t repo.ObjectType) error {
	phase := string(t) + "s"
	if t == repo.TypeSearch {
		phase = phaseSearches
	}
	since := l.st.Since[phase]
	// Collections and searches can sit in the trash since Zotero 7 too, and
	// a library copy without them would lose the trash.
	remote, lmv, err := listVersions(ctx, l.src, t, zoteroclient.VersionsQuery{Since: since, IncludeTrashed: true})
	if err != nil {
		return err
	}
	l.listed(r, phase, lmv)
	local, err := r.localVersions(ctx, l.id, t)
	if err != nil {
		return err
	}
	// Outside a fresh import gotero's versions say nothing about the source,
	// so what was already copied is told apart by the source version the
	// importer recorded.
	var imported map[string]repo.ImportedObject
	if !l.fresh {
		rd, err := r.w.Store.Library(ctx, l.id)
		if err != nil {
			return err
		}
		if imported, err = rd.ImportedObjects(ctx, t); err != nil {
			return err
		}
	}
	var need []string
	var added int64
	for k, v := range remote {
		if l.fresh {
			if local[k] != v {
				need = append(need, k)
			}
		} else if rec, ok := imported[k]; !ok || rec.SourceVersion != v {
			need = append(need, k)
		}
		if _, ok := local[k]; !ok {
			added++
		}
	}
	slices.Sort(need)
	r.update(func() {
		if l.st.Total == nil {
			l.st.Total, l.st.Finished = map[string]int64{}, map[string]int64{}
		}
		if since == 0 {
			l.st.Total[phase] = int64(len(remote))
			l.st.Finished[phase] = int64(len(remote) - len(need))
			return
		}
		// A catch-up pass lists only what changed. Changed objects were
		// counted by the earlier pass already, so only new ones add to the
		// total, and the changed ones count as done again once written.
		changed := max(0, int64(len(need))-added)
		l.st.Total[phase] += added
		l.st.Finished[phase] = max(0, l.st.Finished[phase]-changed)
	})

	if t == repo.TypeItem {
		// Parents first: top-level items, then their children.
		tops, v, err := listVersions(ctx, l.src, t, zoteroclient.VersionsQuery{Since: since, Top: true, IncludeTrashed: true})
		if err != nil {
			return err
		}
		l.observe(r, v)
		slices.SortStableFunc(need, func(a, b string) int {
			_, ta := tops[a]
			_, tb := tops[b]
			switch {
			case ta == tb:
				return 0
			case ta:
				return -1
			}
			return 1
		})
	}

	for chunk := range slices.Chunk(need, batchSize) {
		objs, v, err := l.src.Objects(ctx, zType(t), chunk)
		if errors.Is(err, zoteroclient.ErrLibraryChanged) {
			// Part of the chunk is missing, so this phase is not complete
			// up to its listing. Listing from the old point again in the
			// next pass picks the rest up.
			r.update(func() { l.st.Drift, l.st.Listed[phase] = true, since })
		} else if err != nil {
			return err
		}
		// An object changed after the listing comes back newer than listed,
		// which is a drift just like a newer listing.
		l.observe(r, v)
		if t == repo.TypeCollection {
			objs = sortCollections(objs)
		}
		n, conflicts, err := r.putObjects(ctx, l, t, objs, imported)
		if err != nil {
			return err
		}
		r.addConflicts(l, phase, conflicts)
		r.update(func() { l.st.Finished[phase] += n })
		if err := r.checkpoint(ctx, false); err != nil {
			return err
		}
	}
	return nil
}

// sortCollections orders a batch so parents precede their children where both
// are in it. Nothing in storage depends on the order; it keeps the writes
// reading naturally and a partial import free of dangling parents.
func sortCollections(objs []zotero.Object) []zotero.Object {
	parent := func(o zotero.Object) string {
		var m zotero.OrderedMap
		if m.UnmarshalJSON(o.Data) != nil {
			return ""
		}
		p, _ := m.GetString("parentCollection")
		return p
	}
	byKey := map[string]zotero.Object{}
	for _, o := range objs {
		byKey[o.Key] = o
	}
	var out []zotero.Object
	done := map[string]bool{}
	var visit func(o zotero.Object, depth int)
	visit = func(o zotero.Object, depth int) {
		if done[o.Key] {
			return
		}
		if p, ok := byKey[parent(o)]; ok && depth < len(objs) {
			visit(p, depth+1)
		}
		done[o.Key] = true
		out = append(out, o)
	}
	for _, o := range objs {
		visit(o, 0)
	}
	return out
}

// putObjects stores one batch in a single transaction. It returns how many
// objects were handled and how many of those were kept because they were
// edited in gotero.
func (r *run) putObjects(ctx context.Context, l *lib, t repo.ObjectType, in []zotero.Object, imported map[string]repo.ImportedObject) (handled, conflicts int64, err error) {
	objs := make([]repo.Object, 0, len(in))
	keys := make([]string, 0, len(in))
	for _, o := range in {
		obj, ok := convertObject(t, o)
		if !ok {
			r.update(func() { r.skipped++ })
			continue
		}
		objs = append(objs, obj)
		keys = append(keys, obj.Key)
	}
	if len(objs) == 0 {
		return int64(len(in)), 0, nil
	}
	_, err = r.write(ctx, l, func(tx repo.LibraryTx) error {
		conflicts = 0
		write := objs
		var stored map[string]repo.Object
		if !l.fresh {
			var err error
			if stored, err = tx.Get(ctx, t, keys); err != nil {
				return err
			}
			write = make([]repo.Object, 0, len(objs))
		}
		// Source versions to remember for everything that now matches.
		records := make([]repo.ImportedObject, 0, len(objs))
		var top, next int64
		for _, obj := range objs {
			rec := repo.ImportedObject{Key: obj.Key, SourceVersion: obj.Version, ContentHash: obj.ContentHash}
			if l.fresh {
				records = append(records, rec)
				top = max(top, obj.Version)
				continue
			}
			cur, exists := stored[obj.Key]
			last, wasImported := imported[obj.Key]
			switch {
			case exists && bytes.Equal(cur.ContentHash, obj.ContentHash):
				records = append(records, rec)
			case exists && wasImported && bytes.Equal(cur.ContentHash, last.ContentHash),
				!exists && !wasImported:
				if next == 0 {
					v, err := tx.NextVersion(ctx)
					if err != nil {
						return err
					}
					next = v
				}
				obj.Version = next
				write = append(write, obj)
				records = append(records, rec)
			default:
				// Edited or deleted in gotero since the importer wrote it, or
				// an unrelated object with the same key.
				conflicts++
			}
		}
		// A key deleted in gotero earlier is alive again.
		revived := make([]string, 0, len(write))
		for _, o := range write {
			revived = append(revived, o.Key)
		}
		if err := tx.ClearDeleted(ctx, t, revived); err != nil {
			return err
		}
		if err := tx.PutObjects(ctx, t, write); err != nil {
			return err
		}
		if err := tx.RecordImported(ctx, t, records); err != nil {
			return err
		}
		if l.fresh {
			return tx.RaiseVersion(ctx, top)
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("store %ss: %w", t, err)
	}
	return int64(len(in)), conflicts, nil
}
