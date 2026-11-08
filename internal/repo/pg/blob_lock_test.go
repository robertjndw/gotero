package pg_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestLockBlobRollsBackOnError(t *testing.T) {
	s, _ := newStore(t)
	f := s.Files()
	const md5 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	require.NoError(t, f.PutBlob(ctx(), repo.Blob{MD5: md5, Size: 3}))

	boom := errors.New("byte delete failed")
	err := f.LockBlob(ctx(), md5, func(tx repo.BlobTx) error {
		ok, err := tx.DeleteIfOrphan(ctx(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.True(t, ok)
		return boom
	})
	require.ErrorIs(t, err, boom)
	ok, err := f.BlobExists(ctx(), md5)
	require.NoError(t, err)
	assert.True(t, ok, "a failed fn must not commit the row delete")

	require.NoError(t, f.LockBlob(ctx(), md5, func(tx repo.BlobTx) error {
		ok, err := tx.DeleteIfOrphan(ctx(), time.Now().Add(time.Hour))
		require.True(t, ok)
		return err
	}))
	ok, _ = f.BlobExists(ctx(), md5)
	assert.False(t, ok)

	require.NoError(t, f.LockBlob(ctx(), md5, func(tx repo.BlobTx) error { return tx.Put(ctx(), 3) }))
	ok, _ = f.BlobExists(ctx(), md5)
	assert.True(t, ok)
}

func TestLockBlobSerializesSameBlob(t *testing.T) {
	s, _ := newStore(t)
	f := s.Files()
	const md5 = "abababababababababababababababab"

	var inside atomic.Int32
	held := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- f.LockBlob(ctx(), md5, func(repo.BlobTx) error {
			inside.Add(1)
			close(held)
			<-release
			inside.Add(-1)
			return nil
		})
	}()
	<-held

	second := make(chan error, 1)
	go func() {
		second <- f.LockBlob(ctx(), md5, func(repo.BlobTx) error {
			if inside.Load() != 0 {
				return errors.New("entered while the first holder was inside")
			}
			return nil
		})
	}()
	select {
	case err := <-second:
		t.Fatalf("second holder got in while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
}
