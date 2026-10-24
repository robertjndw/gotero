package syncsim

import (
	"fmt"
	"sync"
)

// ack is a write the server confirmed.
type ack struct {
	version int64
	data    obj
	by      string
}

// history records what the server acknowledged, across all clients. The
// final state is checked against it: an acknowledged write may be
// overwritten or deleted later, but never silently lost.
type history struct {
	mu sync.Mutex
	// writes[lib][kind/key] is the newest acknowledged write of each object.
	writes map[string]map[string]ack
	// deletes[lib][kind/key] is the newest acknowledged delete request.
	deletes map[string]map[string]int64
	// seenDeleted[lib][kind/key] is the newest library version at which a
	// client saw the key in /deleted. It covers cascades too, but the
	// version is only an upper bound, so it explains state, never fails it.
	seenDeleted map[string]map[string]int64
	// bumps[lib][version] names the write request that produced a version.
	bumps    map[string]map[int64]string
	settings map[string]ack
	files    map[string]map[string]ack
	// fileRegs[lib][key] is every acknowledged file registration, with the
	// If-Match it was sent with.
	fileRegs map[string]map[string][]fileReg
	// maybeFiles[lib][key] holds md5s that may have become the item's file
	// without a registration on record: lost responses and dedupe answers.
	maybeFiles map[string]map[string]map[string]bool
	problems   []string
}

type fileReg struct {
	version   int64
	md5, base string
	by        string
}

func newHistory() *history {
	return &history{
		writes:      map[string]map[string]ack{},
		deletes:     map[string]map[string]int64{},
		seenDeleted: map[string]map[string]int64{},
		bumps:       map[string]map[int64]string{},
		settings:    map[string]ack{},
		files:       map[string]map[string]ack{},
		fileRegs:    map[string]map[string][]fileReg{},
		maybeFiles:  map[string]map[string]map[string]bool{},
	}
}

func objID(k kind, key string) string { return string(k) + "/" + key }

func sub[V any](m map[string]map[string]V, lib string) map[string]V {
	if m[lib] == nil {
		m[lib] = map[string]V{}
	}
	return m[lib]
}

func (h *history) recordWrite(lib string, k kind, key string, version int64, data obj, by string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := sub(h.writes, lib)
	id := objID(k, key)
	prev, ok := w[id]
	if ok && prev.version == version && !equal(prev.data, data) {
		h.problems = append(h.problems, fmt.Sprintf(
			"%s %s: version %d acknowledged twice with different data (%s by %s, %s by %s)",
			lib, id, version, canon(prev.data), prev.by, canon(normalize(data)), by))
	}
	if !ok || version >= prev.version {
		w[id] = ack{version: version, data: clone(data), by: by}
	}
}

func (h *history) recordSeenDeleted(lib string, k kind, keys []string, version int64) {
	h.recordDeletion(h.seenDeleted, lib, k, keys, version)
}

func (h *history) recordDelete(lib string, k kind, keys []string, version int64) {
	h.recordDeletion(h.deletes, lib, k, keys, version)
}

func (h *history) recordDeletion(m map[string]map[string]int64, lib string, k kind, keys []string, version int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := sub(m, lib)
	for _, key := range keys {
		id := objID(k, key)
		d[id] = max(d[id], version)
	}
}

// recordBump notes that a request changed objects at version v. Two requests
// sharing a version would mean two writers held the library at once.
func (h *history) recordBump(lib string, v int64, desc string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bumps[lib] == nil {
		h.bumps[lib] = map[int64]string{}
	}
	if prev, ok := h.bumps[lib][v]; ok {
		h.problems = append(h.problems, fmt.Sprintf("%s: version %d handed to two writes: %s and %s", lib, v, prev, desc))
		return
	}
	h.bumps[lib][v] = desc
}

func (h *history) recordSetting(lib string, v int64, value any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev, ok := h.settings[lib]; !ok || v >= prev.version {
		h.settings[lib] = ack{version: v, data: obj{"value": value}}
	}
}

func (h *history) recordFile(lib, key, md5, base string, v int64, by string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := sub(h.files, lib)
	if prev, ok := f[key]; !ok || v >= prev.version {
		f[key] = ack{version: v, data: obj{"md5": md5}, by: by}
	}
	r := sub(h.fileRegs, lib)
	r[key] = append(r[key], fileReg{version: v, md5: md5, base: base, by: by})
}

func (h *history) recordMaybeFile(lib, key, md5 string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := sub(h.maybeFiles, lib)
	if m[key] == nil {
		m[key] = map[string]bool{}
	}
	m[key][md5] = true
}
