package zapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/storage"
	"github.com/robertjndw/gotero/zotero"
)

// Notifier tells the streaming server about changes that are not library
// versions. Both methods must be cheap and must not block.
type Notifier interface {
	LoginCompleted(token string)
	AccessChanged(userID int64)
}

// maxFormBytes bounds the form-encoded authorize and register bodies. They
// carry a handful of short fields.
const maxFormBytes = 64 << 10

func (a *API) fileService(c *gin.Context) bool {
	if a.files == nil {
		Fail(c, http.StatusNotImplemented, "File storage is not enabled")
		return false
	}
	return true
}

// attachment is a stored item seen through the fields ZFS cares about.
type attachment struct {
	obj      repo.Object
	data     zotero.OrderedMap
	linkMode string
	md5      string // the item's md5, empty while no file is registered
	filename string
}

func parseAttachment(o repo.Object) (attachment, error) {
	at := attachment{obj: o}
	if err := json.Unmarshal(o.Data, &at.data); err != nil {
		return at, fmt.Errorf("stored item %s is corrupt: %w", o.Key, err)
	}
	at.linkMode, _ = at.data.GetString("linkMode")
	at.md5, _ = at.data.GetString("md5")
	at.filename, _ = at.data.GetString("filename")
	return at, nil
}

// loadAttachment fetches an item that can carry a synced file. Items that
// are not attachments, and attachment kinds that never have a file, are 404;
// linked files are a client error like on zotero.org.
func loadAttachment(ctx context.Context, r repo.LibraryReader, key string) (attachment, error) {
	if !zotero.ValidKey(key) {
		return attachment{}, errStatus(http.StatusNotFound, "Item not found")
	}
	objs, err := r.Get(ctx, repo.TypeItem, []string{key})
	if err != nil {
		return attachment{}, err
	}
	o, ok := objs[key]
	if !ok || o.ItemType != "attachment" {
		return attachment{}, errStatus(http.StatusNotFound, "Item not found")
	}
	at, err := parseAttachment(o)
	if err != nil {
		return at, err
	}
	switch at.linkMode {
	case "imported_file", "imported_url":
		return at, nil
	case "linked_file", "linked_url":
		return at, errStatus(http.StatusBadRequest, "Cannot upload a file for a linked attachment")
	}
	return at, errStatus(http.StatusNotFound, "Item not found")
}

// checkFilePreconditions enforces If-Match / If-None-Match. Zotero refuses
// blind uploads, so one of them is required. A 412 reports the attachment's
// own version, which is what the client compares against.
func checkFilePreconditions(c *gin.Context, at attachment, hasFile bool) error {
	none := strings.TrimSpace(c.GetHeader("If-None-Match"))
	match := strings.Trim(strings.TrimSpace(c.GetHeader("If-Match")), `"`)
	if none == "" && match == "" {
		return errStatus(http.StatusPreconditionRequired, "If-Match or If-None-Match must be provided")
	}
	failed := func(msg string) error {
		return &httpError{status: http.StatusPreconditionFailed, version: at.obj.Version, msg: msg}
	}
	if none == "*" && hasFile {
		return failed("If-None-Match: * set but file exists")
	}
	if match != "" && !strings.EqualFold(match, at.md5) {
		return failed("ETag does not match current version of file")
	}
	return nil
}

// fileForm is the validated authorize request.
type fileForm struct {
	md5      string // md5 of the file as the item will show it
	blobMD5  string // md5 of the stored bytes: zipMD5 for snapshots
	filename string
	size     int64 // size of the stored bytes
	mtime    int64
	zipped   bool
	params   bool
}

func (a *API) parseFileForm(form url.Values) (fileForm, error) {
	var f fileForm
	f.md5 = strings.ToLower(form.Get("md5"))
	if !files.ValidMD5(f.md5) {
		return f, errStatus(http.StatusBadRequest, "Invalid md5")
	}
	f.filename = form.Get("filename")
	if f.filename == "" || len(f.filename) > 1024 {
		return f, errStatus(http.StatusBadRequest, "Invalid filename")
	}
	size, err := strconv.ParseInt(form.Get("filesize"), 10, 64)
	if err != nil || size < 0 {
		return f, errStatus(http.StatusBadRequest, "Invalid filesize")
	}
	if a.maxUpload > 0 && size > a.maxUpload {
		return f, errStatus(http.StatusRequestEntityTooLarge, "File is too large")
	}
	f.size = size
	if f.mtime, err = strconv.ParseInt(form.Get("mtime"), 10, 64); err != nil || f.mtime < 0 {
		return f, errStatus(http.StatusBadRequest, "Invalid mtime")
	}
	f.blobMD5 = f.md5
	if z := strings.ToLower(form.Get("zipMD5")); z != "" {
		if !files.ValidMD5(z) {
			return f, errStatus(http.StatusBadRequest, "Invalid zipMD5")
		}
		f.blobMD5, f.zipped = z, true
	}
	f.params = form.Get("params") == "1"
	return f, nil
}

func readForm(c *gin.Context) (url.Values, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxFormBytes)
	if err := c.Request.ParseForm(); err != nil {
		if httpx.IsTooLarge(err) {
			return nil, errStatus(http.StatusRequestEntityTooLarge, "Request body too large")
		}
		return nil, errStatus(http.StatusBadRequest, "Invalid form body")
	}
	return c.Request.PostForm, nil
}

func (a *API) requireFileWrite(c *gin.Context) (*LibraryContext, bool) {
	l := lib(c)
	if !l.Access.FileWrite {
		Fail(c, http.StatusForbidden, "Forbidden")
		return l, false
	}
	return l, true
}

// itemFile handles POST items/:key/file, which is both the authorize and the
// register step of ZFS; the form tells them apart.
func (a *API) itemFile(c *gin.Context) {
	if !a.fileService(c) {
		return
	}
	l, ok := a.requireFileWrite(c)
	if !ok {
		return
	}
	key := c.Param("key")
	at, err := loadAttachment(c.Request.Context(), l.Reader, key)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	form, err := readForm(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	if up := form.Get("upload"); up != "" {
		a.registerFile(c, l, key, up)
		return
	}
	a.authorizeFile(c, l, at, form)
}

func (a *API) authorizeFile(c *gin.Context, l *LibraryContext, at attachment, form url.Values) {
	ctx := c.Request.Context()
	key := at.obj.Key
	cur, curErr := l.Reader.AttachmentFile(ctx, key)
	if curErr != nil && !errors.Is(curErr, repo.ErrNotFound) {
		a.FailErr(c, curErr)
		return
	}
	hasFile := curErr == nil
	if err := checkFilePreconditions(c, at, hasFile); err != nil {
		a.FailErr(c, err)
		return
	}
	f, err := a.parseFileForm(form)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	if err := a.checkQuota(c, l, l.Reader, f.size-cur.Size); err != nil {
		a.FailErr(c, err)
		return
	}

	if size, ok, err := a.knownBlobSize(ctx, l, f.blobMD5); err != nil {
		a.FailErr(c, err)
		return
	} else if ok {
		a.attachExisting(c, l, key, f, size)
		return
	}

	// 32 hex characters, like zotero.org's upload keys.
	uploadKey := zotero.NewToken(32)
	tok := repo.UploadToken{
		UploadKey: uploadKey, LibraryID: l.LibraryID, ItemKey: key, MD5: f.blobMD5,
		Filename: f.filename, Size: f.size, MtimeMS: f.mtime, Compressed: f.zipped,
		ExpiresAt: a.now().Add(files.UploadTTL),
	}
	if f.zipped {
		tok.ItemMD5 = f.md5
	}
	if err := a.store.Files().CreateUploadToken(ctx, tok); err != nil {
		a.FailErr(c, err)
		return
	}
	target := httpx.BaseURL(ctx) + "/files/upload/" + uploadKey
	if f.params {
		c.JSON(http.StatusOK, gin.H{"url": target, "params": gin.H{"key": uploadKey}, "uploadKey": uploadKey})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"url": target, "contentType": "application/octet-stream", "prefix": "", "suffix": "", "uploadKey": uploadKey,
	})
}

// readableLibraries lists the libraries the principal could read a file
// from. The exists shortcut only counts blobs referenced there, otherwise
// "exists" would confirm other people's files by their checksum.
func (a *API) readableLibraries(ctx context.Context, l *LibraryContext) ([]int64, error) {
	ids := []int64{l.LibraryID}
	p := l.Principal
	if p.Key.Access.Library && l.LibraryID != p.User.LibraryID {
		ids = append(ids, p.User.LibraryID)
	}
	if g := p.Key.Access.Groups; g == "all" || g == "read" {
		groups, err := a.store.Groups().ListForUser(ctx, p.User.ID)
		if err != nil {
			return nil, err
		}
		for _, gr := range groups {
			if gr.LibraryID != l.LibraryID {
				ids = append(ids, gr.LibraryID)
			}
		}
	}
	return ids, nil
}

// knownBlobSize reports whether the blob is already stored and referenced
// in a library the caller may read, and how big it really is. The size comes
// from the store rather than the request so a client cannot understate it.
func (a *API) knownBlobSize(ctx context.Context, l *LibraryContext, blobMD5 string) (int64, bool, error) {
	ids, err := a.readableLibraries(ctx, l)
	if err != nil {
		return 0, false, err
	}
	ok, err := a.store.Files().BlobReferencedIn(ctx, blobMD5, ids)
	if err != nil || !ok {
		return 0, false, err
	}
	info, err := a.files.Backend().Stat(ctx, files.BlobKey(blobMD5))
	if errors.Is(err, storage.ErrNotFound) {
		return 0, false, nil
	}
	return info.Size, err == nil, err
}

// attachExisting is the {"exists":1} path: no bytes travel, the attachment
// just points at a blob that is already there.
func (a *API) attachExisting(c *gin.Context, l *LibraryContext, key string, f fileForm, size int64) {
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		at, err := loadAttachment(ctx, tx, key)
		if err != nil {
			return withVersion(err, tx.Version())
		}
		old, ferr := tx.AttachmentFile(ctx, key)
		if ferr != nil && !errors.Is(ferr, repo.ErrNotFound) {
			return ferr
		}
		if err := checkFilePreconditions(c, at, ferr == nil); err != nil {
			return err
		}
		// The authorize step looked at the size the client announced, and
		// other uploads may have landed since. Charge the real blob size.
		if err := a.checkQuota(c, l, tx, size-old.Size); err != nil {
			return err
		}
		return applyFile(ctx, tx, at, repo.AttachmentFile{
			ItemKey: key, MD5: f.blobMD5, Filename: f.filename, MtimeMS: f.mtime, Size: size, Compressed: f.zipped,
		}, f.md5, old.Size)
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.JSON(http.StatusOK, gin.H{"exists": 1})
}

func withVersion(err error, v int64) error {
	var he *httpError
	if errors.As(err, &he) && he.version == 0 {
		he.version = v
	}
	return err
}

// applyFile writes a file registration into the library: the item's
// md5/mtime/filename, the attachment_files row and the storage counter.
// Callers hold the library lock and pass the size of the file being replaced
// (0 if none). The item version only moves if its data actually changed, so
// re-registering the same file is a no-op for clients.
func applyFile(ctx context.Context, tx repo.LibraryTx, at attachment, f repo.AttachmentFile, itemMD5 string, oldSize int64) error {
	m := at.data.Clone()
	if err := m.SetValue("md5", itemMD5); err != nil {
		return err
	}
	if err := m.SetValue("mtime", f.MtimeMS); err != nil {
		return err
	}
	if at.filename != f.Filename {
		if err := m.SetValue("filename", f.Filename); err != nil {
			return err
		}
	}
	obj, err := ObjectFromData(repo.TypeItem, f.ItemKey, m)
	if err != nil {
		return err
	}
	if string(obj.ContentHash) != string(at.obj.ContentHash) {
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		obj.Version = ver
		if err := tx.PutObjects(ctx, repo.TypeItem, []repo.Object{obj}); err != nil {
			return err
		}
	}
	if err := tx.PutAttachmentFile(ctx, f); err != nil {
		return err
	}
	if delta := f.Size - oldSize; delta != 0 {
		return tx.AddStorageUsed(ctx, delta)
	}
	return nil
}

// tokenItemMD5 is the md5 the item shows once the token is registered. It
// differs from the blob md5 for zipped snapshots.
func tokenItemMD5(tok repo.UploadToken) string {
	if tok.ItemMD5 != "" {
		return tok.ItemMD5
	}
	return tok.MD5
}

// registerFile is the last ZFS step: the bytes are committed, now the item
// learns about them.
func (a *API) registerFile(c *gin.Context, l *LibraryContext, key, uploadKey string) {
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		at, err := loadAttachment(ctx, tx, key)
		if err != nil {
			return withVersion(err, tx.Version())
		}
		old, ferr := tx.AttachmentFile(ctx, key)
		if ferr != nil && !errors.Is(ferr, repo.ErrNotFound) {
			return ferr
		}
		// Read under the library lock: of two concurrent registrations of
		// one token, the second one sees "registered".
		tok, terr := a.store.Files().GetUploadToken(ctx, uploadKey)
		if terr != nil && !errors.Is(terr, repo.ErrNotFound) {
			return terr
		}
		// A repeat after a lost 204 would fail the preconditions, because the
		// first call already changed the file. If the item shows exactly what
		// this token registered, answer as the first call did and change
		// nothing.
		if terr == nil && ferr == nil && tok.State == "registered" &&
			tok.LibraryID == l.LibraryID && tok.ItemKey == key &&
			old.MD5 == tok.MD5 && at.md5 == tokenItemMD5(tok) {
			return nil
		}
		if err := checkFilePreconditions(c, at, ferr == nil); err != nil {
			return err
		}
		switch {
		case errors.Is(terr, repo.ErrNotFound):
			return errStatus(http.StatusBadRequest, "Unknown upload key")
		case tok.LibraryID != l.LibraryID || tok.ItemKey != key:
			return errStatus(http.StatusBadRequest, "Upload key does not belong to this item")
		case !tok.ExpiresAt.After(a.now()):
			return errStatus(http.StatusBadRequest, "Upload key has expired")
		case tok.State == "registered":
			return errStatus(http.StatusBadRequest, "Upload has already been registered")
		case tok.State != "uploaded":
			return errStatus(http.StatusBadRequest, "File has not been uploaded")
		}
		itemMD5 := tokenItemMD5(tok)
		// Parallel authorizations each passed the check against the same
		// usage; registering is where the bytes actually get charged.
		if err := a.checkQuota(c, l, tx, tok.Size-old.Size); err != nil {
			return err
		}
		if err := applyFile(ctx, tx, at, repo.AttachmentFile{
			ItemKey: key, MD5: tok.MD5, Filename: tok.Filename, MtimeMS: tok.MtimeMS,
			Size: tok.Size, Compressed: tok.Compressed,
		}, itemMD5, old.Size); err != nil {
			return err
		}
		moved, err := a.store.Files().SetUploadTokenStateFrom(ctx, uploadKey, "uploaded", "registered")
		if err != nil {
			return err
		}
		if !moved {
			return errStatus(http.StatusBadRequest, "Upload has already been registered")
		}
		return nil
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.Status(http.StatusNoContent)
}

// getItemFile answers GET items/:key/file with a redirect to a
// self-authenticating URL; the client follows it without its API key.
func (a *API) getItemFile(c *gin.Context) {
	if !a.fileService(c) {
		return
	}
	l := lib(c)
	if !l.Access.FileRead {
		Fail(c, http.StatusForbidden, "Forbidden")
		return
	}
	ctx := c.Request.Context()
	key := c.Param("key")
	at, af, err := a.consistentFile(ctx, l.Reader, key)
	if err != nil && !isStatus(err, http.StatusBadRequest) {
		a.FailErr(c, err)
		return
	}
	if err != nil {
		// Linked attachments never have a file to hand out.
		Fail(c, http.StatusNotFound, "File not found")
		return
	}
	// Plain files are served from exactly this blob, so its md5 is the one
	// to announce. Only a zipped snapshot shows the item's md5, which is
	// that of the file inside.
	itemMD5 := af.MD5
	if af.Compressed && at.md5 != "" {
		itemMD5 = at.md5
	}
	compressed := "No"
	if af.Compressed {
		compressed = "Yes"
	}
	c.Header("Location", httpx.BaseURL(ctx)+"/files/download/"+a.files.SignDownload(l.LibraryID, key, af.MD5))
	c.Header("Zotero-File-MD5", itemMD5)
	c.Header("Zotero-File-Size", strconv.FormatInt(af.Size, 10))
	c.Header("Zotero-File-Modification-Time", strconv.FormatInt(af.MtimeMS, 10))
	c.Header("Zotero-File-Compressed", compressed)
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusFound)
}

// consistentFile reads an attachment and its file row as of the same
// registration. The reads are not locked, so a new upload registering
// between them would pair the old item md5 with the new blob, and Zotero
// rejects a download whose md5 does not match. Reading the file row before
// and after the item and comparing catches that.
func (a *API) consistentFile(ctx context.Context, r repo.LibraryReader, key string) (attachment, repo.AttachmentFile, error) {
	for attempt := 0; ; attempt++ {
		before, err := r.AttachmentFile(ctx, key)
		if errors.Is(err, repo.ErrNotFound) {
			if _, aerr := loadAttachment(ctx, r, key); aerr != nil {
				return attachment{}, before, aerr
			}
			return attachment{}, before, errStatus(http.StatusNotFound, "File not found")
		}
		if err != nil {
			return attachment{}, before, err
		}
		at, err := loadAttachment(ctx, r, key)
		if err != nil {
			return at, before, err
		}
		after, err := r.AttachmentFile(ctx, key)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			return at, after, errStatus(http.StatusNotFound, "File not found")
		case err != nil:
			return at, after, err
		case after.MD5 == before.MD5 || attempt == 4:
			return at, after, nil
		}
	}
}

func isStatus(err error, status int) bool {
	var he *httpError
	return errors.As(err, &he) && he.status == status
}

// removeStorageFiles handles POST /users/:id/removestoragefiles: the files
// go, the items stay. The blobs become orphans for GC to collect.
func (a *API) removeStorageFiles(c *gin.Context) {
	l, ok := a.requireFileWrite(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		removed, err := tx.DeleteAttachmentFiles(ctx)
		if err != nil {
			return err
		}
		if err := tx.AddStorageUsed(ctx, -tx.StorageUsed()); err != nil {
			return err
		}
		if len(removed) == 0 {
			return nil
		}
		keys := make([]string, len(removed))
		for i, f := range removed {
			keys[i] = f.ItemKey
		}
		items, err := tx.Get(ctx, repo.TypeItem, keys)
		if err != nil {
			return err
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		objs := make([]repo.Object, 0, len(items))
		for _, k := range keys {
			o, ok := items[k]
			if !ok {
				continue
			}
			var m zotero.OrderedMap
			if err := json.Unmarshal(o.Data, &m); err != nil {
				return err
			}
			m.Set("md5", json.RawMessage("null"))
			m.Set("mtime", json.RawMessage("null"))
			no, err := rebuildItem(k, m, ver)
			if err != nil {
				return err
			}
			objs = append(objs, no)
		}
		return tx.PutObjects(ctx, repo.TypeItem, objs)
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.Status(http.StatusNoContent)
}

// checkQuota rejects an upload that would push the library owner over quota.
// delta is the change in stored bytes the upload causes. A group library is
// charged to the group owner, together with everything else they own.
//
// cur is the reader for l's own library: the locked transaction when called
// while registering a file, so its counter is exact. The owner's other
// libraries are read without a lock, so two uploads into different libraries
// of one owner can still both pass and overshoot the quota by up to one
// file each. Closing that gap would mean locking every library of the owner
// in a fixed order for each upload, which is not worth the contention.
func (a *API) checkQuota(c *gin.Context, l *LibraryContext, cur repo.LibraryReader, delta int64) error {
	ctx := c.Request.Context()
	ownerID := l.Principal.User.ID
	if l.Group != nil {
		ownerID = l.Group.OwnerID
	}
	// Not the cached principal: an admin changing a quota must apply at once.
	owner, err := a.store.Users().Get(ctx, ownerID)
	if err != nil {
		return err
	}
	quota := owner.QuotaBytes
	if quota == nil && a.defQuota != nil {
		var err error
		if quota, err = a.defQuota(ctx); err != nil {
			return err
		}
	}
	if quota == nil {
		return nil
	}
	usage, err := files.OwnerUsage(ctx, a.store, owner, cur)
	if err != nil {
		return err
	}
	if usage+delta > *quota {
		c.Header("Zotero-Storage-Usage", strconv.FormatInt(files.QuotaMB(usage), 10))
		c.Header("Zotero-Storage-Quota", strconv.FormatInt(files.QuotaMB(*quota), 10))
		return errStatus(http.StatusRequestEntityTooLarge, "File would exceed quota")
	}
	return nil
}

// RegisterFileRoutes mounts the self-authenticating upload and download
// targets. The server mounts it at /files, outside the API key middleware:
// the token in the URL is the credential.
func (a *API) RegisterFileRoutes(r gin.IRouter) {
	r.POST("/upload/:uploadKey", a.uploadTarget)
	r.GET("/download/:token", a.downloadTarget)
}

// rfc5987 percent-encodes s for a Content-Disposition filename* value.
func rfc5987(s string) string {
	const attr = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', strings.IndexByte(attr, ch) >= 0:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func (a *API) downloadTarget(c *gin.Context) {
	if !a.fileService(c) {
		return
	}
	ctx := c.Request.Context()
	claims, err := a.files.VerifyDownload(c.Param("token"))
	if err != nil {
		Fail(c, http.StatusNotFound, "File not found")
		return
	}
	r, err := a.store.Library(ctx, claims.LibraryID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			Fail(c, http.StatusNotFound, "File not found")
			return
		}
		a.FailErr(c, err)
		return
	}
	// The token only proves the file was shared once. Make sure the
	// attachment still points at that blob.
	af, err := r.AttachmentFile(ctx, claims.ItemKey)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && af.MD5 != claims.MD5) {
		Fail(c, http.StatusNotFound, "File not found")
		return
	}
	if err != nil {
		a.FailErr(c, err)
		return
	}
	ctype := "application/octet-stream"
	if af.Compressed {
		ctype = "application/zip"
	} else if at, err := loadAttachment(ctx, r, claims.ItemKey); err == nil {
		if ct, _ := at.data.GetString("contentType"); ct != "" && !strings.ContainsAny(ct, "\r\n") {
			ctype = ct
		}
	}
	name := af.Filename
	if af.Compressed {
		name += ".zip"
	}
	rd, info, err := a.files.Open(ctx, af.MD5)
	if errors.Is(err, storage.ErrNotFound) {
		Fail(c, http.StatusNotFound, "File not found")
		return
	}
	if err != nil {
		a.FailErr(c, err)
		return
	}
	defer func() { _ = rd.Close() }()

	h := c.Writer.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+rfc5987(name))
	h.Set("Cache-Control", "no-store")
	if rs, ok := rd.(io.ReadSeeker); ok {
		// A zero modtime keeps ServeContent from emitting Last-Modified.
		http.ServeContent(c.Writer, c.Request, "", time.Time{}, rs)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	c.Status(http.StatusOK)
	if _, err := io.CopyBuffer(c.Writer, rd, make([]byte, 256<<10)); err != nil {
		a.log.WarnContext(ctx, "zapi: download interrupted", "err", err)
	}
}
