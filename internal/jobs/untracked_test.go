package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// backdateFiles makes every stored blob look older than the orphan grace
// period, as it would on a server that has been running for a while.
func (e *env) backdateFiles(t *testing.T) {
	t.Helper()
	old := time.Now().Add(-3 * OrphanGrace)
	require.NoError(t, filepath.WalkDir(filepath.Join(e.root, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, old, old)
	}))
}

// A few bytes leaked by crashes are collected even though no row points at
// them.
func TestGCCollectsAFewUntrackedBlobs(t *testing.T) {
	e := setup(t)
	var kept []string
	for i := range 30 {
		m := e.upload(t, fmt.Appendf(nil, "kept %d", i))
		e.attach(t, fmt.Sprintf("KEPT%04d", i), m)
		kept = append(kept, m)
	}
	leaked := e.upload(t, []byte("leaked by a crash"))
	e.exec(`DELETE FROM blobs WHERE md5 = $1`, leaked)
	e.backdateFiles(t)

	res, err := e.run.GC(context.Background(), false)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.OrphanBlobs)
	require.False(t, e.exists(t, leaked))
	for _, m := range kept {
		require.True(t, e.exists(t, m))
	}
}

// The failure the safety limit exists for: the store is fine but the
// database is empty or from somewhere else. GC must not read that as
// "every file is garbage".
func TestGCRefusesToWipeStoreTheDatabaseDoesNotKnow(t *testing.T) {
	e := setup(t)
	var all []string
	for i := range 50 {
		all = append(all, e.upload(t, fmt.Appendf(nil, "file %d", i)))
	}
	e.exec(`DELETE FROM blobs`)
	e.backdateFiles(t)

	res, err := e.run.GC(context.Background(), false)
	require.ErrorIs(t, err, ErrUntrackedSuspicious)
	require.Zero(t, res.OrphanBlobs)
	for _, m := range all {
		require.True(t, e.exists(t, m), "GC deleted %s from a store its database does not match", m)
	}
}
