// Package files is the storage side of Zotero file sync (ZFS): verified
// streaming uploads into a content-addressed blob store, and short-lived
// download tokens. The HTTP endpoints live in internal/zapi.
package files

import (
	"context"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // md5 is Zotero's file identity, not a security control
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/storage"
)

var (
	// ErrMismatch means the uploaded bytes do not match the announced size
	// or md5.
	ErrMismatch = errors.New("files: uploaded file does not match its md5 or size")
	// ErrBadToken means a download token is malformed, forged or expired.
	ErrBadToken = errors.New("files: invalid or expired download token")
)

var md5Re = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidMD5 reports whether s is a lowercase hex md5.
func ValidMD5(s string) bool { return md5Re.MatchString(s) }

// BlobKey is where a blob lives in the backend. Two levels of fan-out keep
// directories small for the local and WebDAV backends.
func BlobKey(md5hex string) string {
	return "blobs/" + md5hex[:2] + "/" + md5hex
}

// Service stores and serves attachment blobs.
//
// A blobs row must never outlive its bytes: uploads skip sending content
// that already has a row, and attachments are registered against rows. GC
// and uploads of the same md5 can run in different processes (the import
// CLI, or two servers during an upgrade), so they serialize on a database
// lock (repo.FileRepo.LockBlob) rather than a mutex.
type Service struct {
	backend storage.Backend
	files   repo.FileRepo
	secret  []byte
	now     func() time.Time
}

// ErrBlobCollected means garbage collection removed the bytes of a blob
// between an upload committing them and recording the row. The upload is
// fine to repeat, so it is reported as busy and clients retry.
var ErrBlobCollected = fmt.Errorf("files: blob was garbage collected during upload: %w", repo.ErrBusy)

// statTimeout bounds the existence check Receive makes while it holds the
// blob lock and a database connection.
const statTimeout = 30 * time.Second

// RemoveOrphan deletes a blob if, and only if, it is still an orphan.
//
// The row delete and the byte delete happen under the blob lock and commit
// together, so a failed byte delete rolls the row back and the next pass
// retries. A crash after the bytes are gone leaves an orphan row nothing can
// attach to (Receive re-checks the bytes), which the next pass cleans up.
//
// This holds a pool connection across the backend delete, which is only
// acceptable because GC is a single goroutine and ctx has a deadline.
func (s *Service) RemoveOrphan(ctx context.Context, md5hex string, olderThan time.Time) (bool, error) {
	if !ValidMD5(md5hex) {
		return false, nil
	}
	var removed bool
	err := s.files.LockBlob(ctx, md5hex, func(tx repo.BlobTx) error {
		ok, err := tx.DeleteIfOrphan(ctx, olderThan)
		if err != nil || !ok {
			return err
		}
		if err := s.backend.Delete(ctx, BlobKey(md5hex)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("files: delete orphan bytes: %w", err)
		}
		removed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

func NewService(backend storage.Backend, files repo.FileRepo, secret string) *Service {
	// Derive a dedicated key so the configured secret can be reused for
	// other purposes without cross-protocol token confusion.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("gotero download token v1"))
	return &Service{backend: backend, files: files, secret: mac.Sum(nil), now: time.Now}
}

// Backend exposes the blob store, e.g. for garbage collection.
func (s *Service) Backend() storage.Backend { return s.backend }

// copyBufSize trades syscalls against memory per concurrent upload.
const copyBufSize = 1 << 20

// Receive streams body into the blob store and verifies it against md5hex
// and size. On success the blob is committed under BlobKey and recorded in
// the blobs table; if an identical blob already exists the upload still
// succeeds. Nothing is ever kept in memory beyond the copy buffer.
func (s *Service) Receive(ctx context.Context, body io.Reader, md5hex string, size int64) error {
	if !ValidMD5(md5hex) || size < 0 {
		return ErrMismatch
	}
	up, err := s.backend.Create(ctx)
	if err != nil {
		return fmt.Errorf("files: start upload: %w", err)
	}
	defer func() { _ = up.Abort(context.WithoutCancel(ctx)) }()

	h := md5.New() //nolint:gosec // see import
	// Read one byte more than announced so oversized bodies are detected
	// without buffering them.
	n, err := io.CopyBuffer(io.MultiWriter(up, h), io.LimitReader(body, size+1), make([]byte, copyBufSize))
	if err != nil {
		return fmt.Errorf("files: receive upload: %w", err)
	}
	if n != size || hex.EncodeToString(h.Sum(nil)) != md5hex {
		return ErrMismatch
	}
	// Commit stays outside the lock because on S3 or WebDAV it can be a slow
	// server-side copy, and holding a pool connection across that is how the
	// pool deadlocked before. GC may delete the bytes Commit deduplicated
	// against in that gap, so check they are still there once we hold the lock.
	if err := up.Commit(ctx, BlobKey(md5hex)); err != nil {
		return fmt.Errorf("files: commit upload: %w", err)
	}
	return s.files.LockBlob(ctx, md5hex, func(tx repo.BlobTx) error {
		sctx, cancel := context.WithTimeout(ctx, statTimeout)
		defer cancel()
		if _, err := s.backend.Stat(sctx, BlobKey(md5hex)); errors.Is(err, storage.ErrNotFound) {
			return ErrBlobCollected
		} else if err != nil {
			return fmt.Errorf("files: check committed blob: %w", err)
		}
		if err := tx.Put(ctx, size); err != nil {
			return fmt.Errorf("files: record blob: %w", err)
		}
		return nil
	})
}

// Open returns the blob for md5hex. The reader may implement io.Seeker.
func (s *Service) Open(ctx context.Context, md5hex string) (storage.Reader, storage.Info, error) {
	if !ValidMD5(md5hex) {
		return nil, storage.Info{}, storage.ErrNotFound
	}
	return s.backend.Open(ctx, BlobKey(md5hex))
}

// DownloadClaims is what a download token grants: one file of one
// attachment, as it was when the token was issued.
type DownloadClaims struct {
	LibraryID int64
	ItemKey   string
	MD5       string
	Expires   time.Time
}

// DownloadTTL is how long a download link stays valid. Zotero follows the
// redirect immediately; the margin covers slow starts and retries.
const DownloadTTL = 5 * time.Minute

// SignDownload issues a self-authenticating token. Zotero follows download
// redirects without its API key, so the URL itself must carry the
// authorization, much like an S3 presigned URL.
func (s *Service) SignDownload(libraryID int64, itemKey, md5hex string) string {
	exp := s.now().Add(DownloadTTL).Unix()
	payload := strconv.FormatInt(libraryID, 10) + "." + itemKey + "." + md5hex + "." + strconv.FormatInt(exp, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + s.mac(payload)
}

// VerifyDownload checks a token and returns its claims.
func (s *Service) VerifyDownload(token string) (DownloadClaims, error) {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 512 {
		return DownloadClaims{}, ErrBadToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return DownloadClaims{}, ErrBadToken
	}
	payload := string(raw)
	if !hmac.Equal([]byte(sig), []byte(s.mac(payload))) {
		return DownloadClaims{}, ErrBadToken
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 {
		return DownloadClaims{}, ErrBadToken
	}
	lib, err1 := strconv.ParseInt(parts[0], 10, 64)
	exp, err2 := strconv.ParseInt(parts[3], 10, 64)
	if err1 != nil || err2 != nil || !ValidMD5(parts[2]) {
		return DownloadClaims{}, ErrBadToken
	}
	c := DownloadClaims{LibraryID: lib, ItemKey: parts[1], MD5: parts[2], Expires: time.Unix(exp, 0)}
	if !s.now().Before(c.Expires) {
		return DownloadClaims{}, ErrBadToken
	}
	return c, nil
}

func (s *Service) mac(payload string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// UploadTTL bounds how long an authorized upload may take to start and
// finish registering.
const UploadTTL = 6 * time.Hour

// OwnerUsage adds up the stored bytes a user is charged for: their own
// library plus every group they own. Library except (0 for none) is
// skipped, so a caller can add that one's counter itself once it holds
// the lock.
func OwnerUsage(ctx context.Context, store repo.Store, owner repo.User, except int64) (int64, error) {
	ids := []int64{owner.LibraryID}
	groups, err := store.Groups().ListForUser(ctx, owner.ID)
	if err != nil {
		return 0, err
	}
	for _, g := range groups {
		if g.OwnerID == owner.ID {
			ids = append(ids, g.LibraryID)
		}
	}
	var total int64
	for _, id := range ids {
		if id == except {
			continue
		}
		r, err := store.Library(ctx, id)
		if err != nil {
			return 0, err
		}
		total += r.StorageUsed()
	}
	return total, nil
}

// QuotaMB formats bytes the way Zotero-Storage-Usage/-Quota expect: whole
// megabytes, rounded up so a nearly full quota never reads as free space.
func QuotaMB(bytes int64) int64 {
	const mb = 1 << 20
	return (bytes + mb - 1) / mb
}
