package importer

import (
	"context"
	"crypto/md5" //nolint:gosec // md5 is Zotero's file identity, not a security control
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/zotero"
	"github.com/robertjndw/gotero/zoteroclient"
)

var (
	errQuota  = errors.New("importer: storage quota reached")
	errNoFile = errors.New("importer: zotero.org has no file for this item")
)

// Per-library messages. The job-level summary already explains these in more
// detail, so it leaves them out.
const (
	libErrFilesFailed = "Some files could not be downloaded."
	libErrQuota       = "Storage quota reached, remaining files were not imported."
)

// fileCand is an attachment whose file should be fetched.
type fileCand struct {
	key      string
	md5      string
	filename string
	mtime    int64 // from the item data, 0 when unknown
}

// candidates lists the stored attachments that carry a file on zotero.org.
func (r *run) candidates(ctx context.Context, libID int64) ([]fileCand, error) {
	rd, err := r.w.Store.Library(ctx, libID)
	if err != nil {
		return nil, err
	}
	seq, err := rd.Objects(ctx, repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem, IncludeTrashed: true}})
	if err != nil {
		return nil, err
	}
	var out []fileCand
	for o, err := range seq {
		if err != nil {
			return nil, err
		}
		if o.ItemType != "attachment" {
			continue
		}
		var m zotero.OrderedMap
		if m.UnmarshalJSON(o.Data) != nil {
			continue
		}
		mode, _ := m.GetString("linkMode")
		sum, _ := m.GetString("md5")
		if (mode != "imported_file" && mode != "imported_url") || !files.ValidMD5(sum) {
			continue
		}
		name, _ := m.GetString("filename")
		if name == "" {
			name = o.Key
		}
		mtime, _ := m.GetInt("mtime")
		out = append(out, fileCand{key: o.Key, md5: sum, filename: name, mtime: mtime})
	}
	return out, nil
}

func (r *run) syncFiles(ctx context.Context, l *lib) error {
	if !r.fileAccess {
		return nil
	}
	cands, err := r.candidates(ctx, l.id)
	if err != nil {
		return err
	}
	r.update(func() { l.st.FilesTotal = int64(len(cands)); l.st.Files = 0; l.st.Bytes = 0 })

	err = parallel(ctx, fileWorkers, len(cands), func(ctx context.Context, i int) error {
		if r.quotaHit.Load() {
			return nil
		}
		c := cands[i]
		err := r.fetchFile(ctx, l, c)
		if err != nil && !isStop(ctx, err) {
			// One more try: a dropped connection or a rate limit hiccup
			// usually clears up.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.w.retryDelay):
			}
			err = r.fetchFile(ctx, l, c)
		}
		switch {
		case err == nil:
		case errors.Is(err, errQuota):
			return nil
		case errors.Is(err, errNoFile):
			r.update(func() { l.st.Files++ })
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			r.log.WarnContext(ctx, "importer: file failed", "item", c.key, "err", err)
			r.update(func() {
				r.failed = append(r.failed, c.key)
				l.st.Files++
				l.st.Error = libErrFilesFailed
			})
		}
		return r.checkpoint(ctx, false)
	})
	if err != nil {
		return err
	}
	if r.quotaHit.Load() {
		r.update(func() { l.st.Error = libErrQuota })
		r.warn("Storage quota reached: %d of %d files were imported so far. Free up space or ask an admin for a larger quota, then run the import again.",
			r.filesDone(), r.filesAll())
	}
	return nil
}

func (r *run) filesDone() (n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.cur.Libs {
		n += s.Files
	}
	return n
}

func (r *run) filesAll() (n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.cur.Libs {
		n += s.FilesTotal
	}
	return n
}

// isStop reports errors that a second attempt cannot fix.
func isStop(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, errQuota) || errors.Is(err, errNoFile)
}

// fetchFile downloads one attachment and registers it.
func (r *run) fetchFile(ctx context.Context, l *lib, c fileCand) error {
	rd, err := r.w.Store.Library(ctx, l.id)
	if err != nil {
		return err
	}
	if af, err := rd.AttachmentFile(ctx, c.key); err == nil {
		// A zipped snapshot's row holds the zip's md5, so the item's mtime
		// is what tells a stale row from a current one.
		current := (!af.Compressed && af.MD5 == c.md5) || (af.Compressed && (c.mtime == 0 || af.MtimeMS == c.mtime))
		if current {
			r.update(func() { l.st.Files++; l.st.Bytes += af.Size })
			return nil
		}
	} else if !errors.Is(err, repo.ErrNotFound) {
		return err
	}

	d, err := l.src.DownloadFile(ctx, c.key)
	if zoteroclient.IsNotFound(err) {
		return errNoFile
	}
	if err != nil {
		return err
	}
	defer func() { _ = d.Body.Close() }()
	if d.Size < 0 {
		return errors.New("zotero.org did not announce the file size")
	}
	if err := r.reserve(ctx, d.Size); err != nil {
		return err
	}
	defer r.release(d.Size)

	blobMD5, size := c.md5, d.Size
	if d.Compressed {
		// The blob is the zip, addressed by the zip's md5, which only the
		// finished stream reveals. Receive needs it up front, so the zip
		// goes through a temp file first.
		blobMD5, size, err = r.spool(ctx, d)
		if err != nil {
			return err
		}
	} else {
		if d.MD5 != "" && d.MD5 != c.md5 {
			return fmt.Errorf("file of %s changed on zotero.org (md5 %s, item says %s)", c.key, d.MD5, c.md5)
		}
		if known, err := r.knownBlob(ctx, l.id, c.md5); err != nil {
			return err
		} else if known > 0 {
			// Same bytes are already stored for this library: skip the transfer.
			size = known
			return r.register(ctx, l, c, d, repo.AttachmentFile{ItemKey: c.key, MD5: blobMD5, Size: size})
		}
		if err := r.w.Files.Receive(ctx, d.Body, c.md5, d.Size); err != nil {
			return fmt.Errorf("store file: %w", err)
		}
	}
	return r.register(ctx, l, c, d, repo.AttachmentFile{ItemKey: c.key, MD5: blobMD5, Size: size, Compressed: d.Compressed})
}

// spool writes a zipped snapshot to a temp file while hashing it, then hands
// it to Receive. It returns the zip's md5 and size.
func (r *run) spool(ctx context.Context, d *zoteroclient.Download) (string, int64, error) {
	tmp, err := os.CreateTemp("", "gotero-import-*.zip")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	h := md5.New() //nolint:gosec // see import
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(d.Body, d.Size+1))
	if err != nil {
		return "", 0, fmt.Errorf("download snapshot: %w", err)
	}
	if n != d.Size {
		return "", 0, fmt.Errorf("download snapshot: got %d of %d bytes", n, d.Size)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	if err := r.w.Files.Receive(ctx, tmp, sum, n); err != nil {
		return "", 0, fmt.Errorf("store snapshot: %w", err)
	}
	return sum, n, nil
}

// knownBlob returns the size of a blob that is stored and already referenced
// in this library, or 0.
func (r *run) knownBlob(ctx context.Context, libID int64, sum string) (int64, error) {
	ok, err := r.w.Store.Files().BlobReferencedIn(ctx, sum, []int64{libID})
	if err != nil || !ok {
		return 0, err
	}
	info, err := r.w.Files.Backend().Stat(ctx, files.BlobKey(sum))
	if errors.Is(err, storage.ErrNotFound) {
		return 0, nil
	}
	return info.Size, err
}

// register points the attachment at its blob. The item itself is untouched:
// zotero.org already sent md5 and mtime in its data, and rewriting it would
// move the version that has to match zotero.org's.
func (r *run) register(ctx context.Context, l *lib, c fileCand, d *zoteroclient.Download, f repo.AttachmentFile) error {
	f.Filename = c.filename
	f.MtimeMS = d.MtimeMS
	if f.MtimeMS == 0 {
		f.MtimeMS = c.mtime
	}
	registered := false
	_, err := r.w.Store.InLibrary(ctx, l.id, func(tx repo.LibraryTx) error {
		// The download took a while. If a client replaced the file since the
		// candidates were listed, the blob in hand is stale and registering it
		// would make downloads disagree with the item's md5.
		items, err := tx.Get(ctx, repo.TypeItem, []string{c.key})
		if err != nil {
			return err
		}
		if it, ok := items[c.key]; !ok || !itemFileIs(it.Data, c) {
			return nil
		}
		registered = true
		old, err := tx.AttachmentFile(ctx, c.key)
		if err != nil && !errors.Is(err, repo.ErrNotFound) {
			return err
		}
		if err := tx.PutAttachmentFile(ctx, f); err != nil {
			return err
		}
		if delta := f.Size - old.Size; delta != 0 {
			return tx.AddStorageUsed(ctx, delta)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("register file: %w", err)
	}
	if !registered {
		r.addConflicts(l, "files", 1)
		r.update(func() { l.st.Files++ })
		return nil
	}
	r.update(func() { l.st.Files++; l.st.Bytes += f.Size })
	return nil
}

// itemFileIs reports whether the item data still describes the file the
// importer is about to register.
func itemFileIs(data json.RawMessage, c fileCand) bool {
	var m zotero.OrderedMap
	if m.UnmarshalJSON(data) != nil {
		return false
	}
	sum, _ := m.GetString("md5")
	mtime, _ := m.GetInt("mtime")
	return sum == c.md5 && mtime == c.mtime
}

// quotaBytes resolves the importing user's quota, nil for unlimited.
func (r *run) quotaBytes(ctx context.Context) (*int64, error) {
	if r.quotaSet {
		return r.quota, nil
	}
	q := r.user.QuotaBytes
	if q == nil && r.w.DefaultQuota != nil {
		var err error
		if q, err = r.w.DefaultQuota(ctx); err != nil {
			return nil, err
		}
	}
	r.quota, r.quotaSet = q, true
	return q, nil
}

// reserve claims room for a download that is about to start. Concurrent
// downloads count against each other, so four large files cannot together
// overshoot the quota.
func (r *run) reserve(ctx context.Context, size int64) error {
	r.qmu.Lock()
	defer r.qmu.Unlock()
	quota, err := r.quotaBytes(ctx)
	if err != nil {
		return err
	}
	if quota != nil {
		used, err := files.OwnerUsage(ctx, r.w.Store, r.user, 0)
		if err != nil {
			return err
		}
		if used+r.inflight+size > *quota {
			r.quotaHit.Store(true)
			return errQuota
		}
	}
	r.inflight += size
	return nil
}

func (r *run) release(size int64) {
	r.qmu.Lock()
	r.inflight -= size
	r.qmu.Unlock()
}
