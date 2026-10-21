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
	"sync"
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
type Service struct {
	backend storage.Backend
	files   repo.FileRepo
	secret  []byte
	now     func() time.Time

	// locks serialize committing a blob against garbage collecting the same
	// md5. Without it GC could delete bytes right after a re-upload of the
	// same content committed (Commit is a no-op when the key still exists).
	// Striped by md5 so unrelated uploads never wait on each other.
	locks [64]sync.Mutex
}

func (s *Service) lock(md5hex string) *sync.Mutex {
	b, _ := hex.DecodeString(md5hex[:2])
	return &s.locks[int(b[0])%len(s.locks)]
}

// RemoveOrphan deletes a blob if, and only if, it is still an orphan. The
// row goes first and atomically re-checks every condition; the bytes only
// after that. A crash in between leaks a file but never loses one that is
// referenced.
func (s *Service) RemoveOrphan(ctx context.Context, md5hex string, olderThan time.Time) (bool, error) {
	if !ValidMD5(md5hex) {
		return false, nil
	}
	mu := s.lock(md5hex)
	mu.Lock()
	defer mu.Unlock()
	ok, err := s.files.DeleteOrphanBlob(ctx, md5hex, olderThan)
	if err != nil || !ok {
		return false, err
	}
	if err := s.backend.Delete(ctx, BlobKey(md5hex)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return true, fmt.Errorf("files: delete orphan bytes: %w", err)
	}
	return true, nil
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
	mu := s.lock(md5hex)
	mu.Lock()
	defer mu.Unlock()
	if err := up.Commit(ctx, BlobKey(md5hex)); err != nil {
		return fmt.Errorf("files: commit upload: %w", err)
	}
	if err := s.files.PutBlob(ctx, repo.Blob{MD5: md5hex, Size: size, CreatedAt: s.now()}); err != nil {
		return fmt.Errorf("files: record blob: %w", err)
	}
	return nil
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
// library plus every group they own. cur, if non-nil, is used for its own
// library instead of a fresh read, so a caller holding that library's lock
// sees its exact counter.
func OwnerUsage(ctx context.Context, store repo.Store, owner repo.User, cur repo.LibraryReader) (int64, error) {
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
		if cur != nil && id == cur.ID() {
			total += cur.StorageUsed()
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
