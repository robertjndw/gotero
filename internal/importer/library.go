package importer

import (
	"bytes"
	"context"
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
	// lmvFirst and drift detect a source library that changed while we read it.
	lmvFirst int64
	seen     bool
	drift    bool
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

// observe records the library version a response carried.
func (l *lib) observe(r *run, v int64) {
	r.update(func() { l.st.LMV = max(l.st.LMV, v) })
	if !l.seen {
		l.lmvFirst, l.seen = v, true
	} else if v != l.lmvFirst {
		l.drift = true
	}
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
		// A change on zotero.org mid-read means the phases saw different
		// library states. The diff makes another pass cheap.
		if !l.drift || st.Pass+1 >= maxPasses {
			break
		}
		r.update(func() { st.Pass++; st.Phase = phaseCollections })
		l.seen, l.drift = false, false
	}

	if err := r.syncFiles(ctx, l); err != nil {
		return err
	}
	v, err := r.w.Store.InLibrary(ctx, l.id, func(tx repo.LibraryTx) error {
		// Only a fresh import adopts the source version. Elsewhere it would
		// just open a gap, and objects already carry versions of their own.
		if l.fresh {
			return tx.RaiseVersion(ctx, st.LMV)
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

// syncObjects brings collections, searches or items up to date.
func (r *run) syncObjects(ctx context.Context, l *lib, t repo.ObjectType) error {
	phase := string(t) + "s"
	if t == repo.TypeSearch {
		phase = phaseSearches
	}
	remote, lmv, err := l.src.Versions(ctx, zType(t), zoteroclient.VersionsQuery{IncludeTrashed: t == repo.TypeItem})
	if err != nil {
		return err
	}
	l.observe(r, lmv)
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
	for k, v := range remote {
		if l.fresh {
			if local[k] != v {
				need = append(need, k)
			}
		} else if rec, ok := imported[k]; !ok || rec.SourceVersion != v {
			need = append(need, k)
		}
	}
	slices.Sort(need)
	r.update(func() {
		if l.st.Total == nil {
			l.st.Total, l.st.Finished = map[string]int64{}, map[string]int64{}
		}
		l.st.Total[phase] = int64(len(remote))
		l.st.Finished[phase] = int64(len(remote) - len(need))
	})

	if t == repo.TypeItem {
		// Parents first: top-level items, then their children.
		tops, v, err := l.src.Versions(ctx, zType(t), zoteroclient.VersionsQuery{Top: true, IncludeTrashed: true})
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
		objs, _, err := l.src.Objects(ctx, zType(t), chunk)
		if errors.Is(err, zoteroclient.ErrLibraryChanged) {
			l.drift = true
		} else if err != nil {
			return err
		}
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
	_, err = r.w.Store.InLibrary(ctx, l.id, func(tx repo.LibraryTx) error {
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
