package pg_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/robertjndw/gotero/internal/repo"
)

func TestConsumeAPIKeyIsOneShot(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	hash := []byte("session-hash")
	require.NoError(t, s.LoginSessions().Create(ctx(), repo.LoginSession{TokenHash: hash, ExpiresAt: time.Now().Add(time.Hour)}))

	key, err := s.LoginSessions().ConsumeAPIKey(ctx(), hash)
	require.NoError(t, err)
	assert.Empty(t, key, "pending sessions have no key to give")

	require.NoError(t, s.LoginSessions().Complete(ctx(), hash, u.ID, "KEY123"))
	var wg sync.WaitGroup
	got := make(chan string, 6)
	for range 6 {
		wg.Go(func() {
			k, err := s.LoginSessions().ConsumeAPIKey(ctx(), hash)
			assert.NoError(t, err)
			got <- k
		})
	}
	wg.Wait()
	close(got)
	var keys []string
	for k := range got {
		if k != "" {
			keys = append(keys, k)
		}
	}
	assert.Equal(t, []string{"KEY123"}, keys)

	sess, err := s.LoginSessions().Get(ctx(), hash)
	require.NoError(t, err)
	assert.Equal(t, "completed", sess.Status)
	assert.Empty(t, sess.APIKey)
}

func TestAttachmentFilesSizeAndItemMD5(t *testing.T) {
	s, _ := newStore(t)
	u := mustUser(t, s, "alice")
	const md5a, md5b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	require.NoError(t, s.Files().PutBlob(ctx(), repo.Blob{MD5: md5a, Size: 10}))
	require.NoError(t, s.Files().PutBlob(ctx(), repo.Blob{MD5: md5b, Size: 20}))
	write(t, s, u.LibraryID, func(tx repo.LibraryTx) error {
		require.NoError(t, tx.PutObjects(ctx(), repo.TypeItem, []repo.Object{item("AAAAAAAA", 1, nil), item("BBBBBBBB", 1, nil), item("CCCCCCCC", 1, nil)}))
		require.NoError(t, tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "AAAAAAAA", MD5: md5a, Filename: "a", Size: 10}))
		return tx.PutAttachmentFile(ctx(), repo.AttachmentFile{ItemKey: "BBBBBBBB", MD5: md5b, Filename: "b", Size: 20})
	})
	r, err := s.Library(ctx(), u.LibraryID)
	require.NoError(t, err)
	n, err := r.AttachmentFilesSize(ctx(), []string{"AAAAAAAA", "BBBBBBBB", "CCCCCCCC", "ZZZZZZZZ"})
	require.NoError(t, err)
	assert.EqualValues(t, 30, n)
	n, err = r.AttachmentFilesSize(ctx(), []string{"BBBBBBBB"})
	require.NoError(t, err)
	assert.EqualValues(t, 20, n)
	n, err = r.AttachmentFilesSize(ctx(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)

	require.NoError(t, s.Files().CreateUploadToken(ctx(), repo.UploadToken{
		UploadKey: "zipkey", LibraryID: u.LibraryID, ItemKey: "AAAAAAAA", MD5: md5a, ItemMD5: md5b,
		Filename: "i.html", Size: 10, Compressed: true, ExpiresAt: time.Now().Add(time.Hour),
	}))
	tok, err := s.Files().GetUploadToken(ctx(), "zipkey")
	require.NoError(t, err)
	assert.Equal(t, md5b, tok.ItemMD5)
}
