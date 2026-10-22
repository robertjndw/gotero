// Package repo defines gotero's persistence boundary. The service layer only
// talks to these interfaces; internal/repo/pg is the PostgreSQL
// implementation. Object data crosses this boundary as raw JSON so the
// repository never needs to understand Zotero semantics.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"time"
)

var (
	ErrNotFound = errors.New("repo: not found")
	ErrConflict = errors.New("repo: conflict") // unique violation, e.g. username taken
	// ErrBusy means a lock could not be acquired in time. The server maps it
	// to 503 with Retry-After so clients back off instead of piling up.
	ErrBusy = errors.New("repo: busy")
	// ErrUnsupportedSort is a client error: the API maps it to 400.
	ErrUnsupportedSort = errors.New("repo: unsupported sort")
)

// Store is the root of all repositories.
type Store interface {
	Users() UserRepo
	Keys() KeyRepo
	Groups() GroupRepo
	LoginSessions() LoginSessionRepo
	WebSessions() WebSessionRepo
	Invites() InviteRepo
	Identities() IdentityRepo
	SSOStates() SSOStateRepo
	Settings() InstanceSettingsRepo
	Files() FileRepo
	Imports() ImportRepo

	// Library returns read access to a library without locking. Reads may
	// observe a version that is concurrently being superseded; that is fine
	// because clients restart a download when Last-Modified-Version moves.
	Library(ctx context.Context, libraryID int64) (LibraryReader, error)

	// InLibrary runs fn in a transaction holding the library's row lock.
	// The library version is bumped at most once, lazily, the first time fn
	// calls NextVersion. It returns the library version after commit.
	// Callers must publish stream events only after InLibrary returns.
	InLibrary(ctx context.Context, libraryID int64, fn func(tx LibraryTx) error) (int64, error)

	// Prune removes expired sessions, tokens and invites and delete-log
	// entries older than keepDeletedFor. It returns the number of rows removed.
	Prune(ctx context.Context, now time.Time, keepDeletedFor time.Duration) (int64, error)

	// Stats returns cheap COUNT-based totals for the admin dashboard.
	Stats(ctx context.Context) (Stats, error)

	Ping(ctx context.Context) error
}

// Stats are instance-wide row counts.
type Stats struct {
	Users       int64
	Groups      int64
	Items       int64
	Attachments int64
}

// ObjectType mirrors zotero.ObjectType for collections, searches and items.
type ObjectType string

const (
	TypeCollection ObjectType = "collection"
	TypeSearch     ObjectType = "search"
	TypeItem       ObjectType = "item"
	TypeTag        ObjectType = "tag"
	TypeSetting    ObjectType = "setting"
)

// Object is a stored collection, search or item. Data excludes key and
// version, which live in their own fields.
type Object struct {
	Key         string
	Version     int64
	Data        json.RawMessage
	ContentHash []byte

	// Item-only denormalized columns, maintained by the service layer.
	ItemType  string
	ParentKey string
	InTrash   bool
	Tags      []ItemTag
}

type ItemTag struct {
	Tag  string
	Type int
	// NumItems is only filled in by LibraryReader.Tags.
	NumItems int
}

type KeyVersion struct {
	Key     string
	Version int64
}

// VersionsQuery selects objects for format=versions and key listings.
type VersionsQuery struct {
	Type           ObjectType
	Since          int64    // strictly greater than
	IncludeTrashed bool     // items only
	OnlyTrashed    bool     // items only: just the trash; wins over IncludeTrashed
	TopOnly        bool     // items: no parent item; collections: no parent collection
	ExcludeNotes   bool     // items only: leave out itemType note (keys without notes access)
	ParentKey      string   // items: parent item; collections: parent collection
	Keys           []string // restrict to these keys (itemKey=...)
}

// ListQuery is used by the read API for full objects.
type ListQuery struct {
	VersionsQuery
	Limit      int // 0 means no limit
	Start      int
	Sort       string // "dateModified", "dateAdded", "version"; empty = version asc
	Desc       bool
	Tag        []string // AND of tags
	Collection string
}

type Setting struct {
	Key     string
	Value   json.RawMessage
	Version int64
}

type Fulltext struct {
	ItemKey      string
	Content      string
	IndexedChars *int
	TotalChars   *int
	IndexedPages *int
	TotalPages   *int
	Version      int64
}

// AttachmentFile is the stored file belonging to an attachment item.
type AttachmentFile struct {
	ItemKey    string
	MD5        string
	Filename   string
	MtimeMS    int64
	Size       int64
	Compressed bool
}

// LibraryReader provides reads scoped to one library.
type LibraryReader interface {
	ID() int64
	Version() int64
	DeleteLogFloor() int64

	Versions(ctx context.Context, q VersionsQuery) (iter.Seq2[KeyVersion, error], error)
	Objects(ctx context.Context, q ListQuery) (iter.Seq2[Object, error], error)
	Count(ctx context.Context, q ListQuery) (int64, error)
	Get(ctx context.Context, t ObjectType, keys []string) (map[string]Object, error)
	Deleted(ctx context.Context, since int64) (map[ObjectType][]string, error)
	Settings(ctx context.Context, since int64) ([]Setting, error)
	// Tags lists the distinct (tag, type) pairs on the matching items with
	// the number of matching items carrying each.
	Tags(ctx context.Context, q ListQuery) ([]ItemTag, error)
	// ChildCounts returns the number of non-trashed child items for each of
	// the given parent keys. Parents without children are absent.
	// excludeNotes skips child notes, for keys that may not see notes.
	ChildCounts(ctx context.Context, parentKeys []string, excludeNotes bool) (map[string]int, error)
	FulltextVersions(ctx context.Context, since int64) (map[string]int64, error)
	Fulltext(ctx context.Context, itemKey string) (Fulltext, error)
	AttachmentFile(ctx context.Context, itemKey string) (AttachmentFile, error)
	// ImportedObjects lists what the zotero.org importer last wrote for the
	// given type, keyed by object key.
	ImportedObjects(ctx context.Context, t ObjectType) (map[string]ImportedObject, error)
	StorageUsed() int64
	// AttachmentFilesSize sums the sizes of the stored files of the given
	// items. Items without a file count as zero.
	AttachmentFilesSize(ctx context.Context, itemKeys []string) (int64, error)
}

// LibraryTx is a locked write transaction on one library.
type LibraryTx interface {
	LibraryReader

	// NextVersion returns the version all writes in this transaction use,
	// bumping the library version on first call.
	NextVersion(ctx context.Context) (int64, error)

	// RaiseVersion lifts the library version to at least min and never
	// lowers it. Importers use it to adopt versions assigned elsewhere
	// instead of allocating new ones with NextVersion.
	RaiseVersion(ctx context.Context, min int64) error

	// ClaimWriteToken records a Zotero-Write-Token. It returns false if the
	// token was already used for this library.
	ClaimWriteToken(ctx context.Context, token string) (bool, error)

	PutObjects(ctx context.Context, t ObjectType, objs []Object) error
	// DeleteObjects removes objects and records them in the delete log at
	// version. Missing keys are ignored. It returns the keys actually removed.
	DeleteObjects(ctx context.Context, t ObjectType, keys []string, version int64) ([]string, error)
	// LogDeleted records a deletion without removing a row (used for tags).
	LogDeleted(ctx context.Context, t ObjectType, keys []string, version int64) error
	// ClearDeleted removes delete-log entries, e.g. when a key is re-created.
	ClearDeleted(ctx context.Context, t ObjectType, keys []string) error

	PutSettings(ctx context.Context, s []Setting) error
	DeleteSettings(ctx context.Context, keys []string, version int64) ([]string, error)

	PutFulltext(ctx context.Context, f []Fulltext) error

	PutAttachmentFile(ctx context.Context, f AttachmentFile) error
	DeleteAttachmentFiles(ctx context.Context) ([]AttachmentFile, error)
	AddStorageUsed(ctx context.Context, delta int64) error

	// RecordImported remembers what the importer wrote, replacing earlier
	// records of the same keys.
	RecordImported(ctx context.Context, t ObjectType, objs []ImportedObject) error
}

// ImportedObject is the importer's record of one object it wrote: the
// content hash it stored and the version the object had on the source.
type ImportedObject struct {
	Key           string
	SourceVersion int64
	ContentHash   []byte
}

// Users

type User struct {
	ID           int64
	LibraryID    int64
	Username     string
	DisplayName  string
	Email        string
	PasswordHash string
	IsAdmin      bool
	Disabled     bool
	QuotaBytes   *int64
	CreatedAt    time.Time
	LastSyncAt   *time.Time
}

type UserRepo interface {
	// Create allocates a library and inserts the user. A zero ID allocates
	// the next free ID.
	Create(ctx context.Context, u User) (User, error)
	// CreateWithInvite creates the user and marks the invite as used in one
	// transaction, so a failed signup never burns an invite and two signups
	// cannot share one. ErrConflict covers both a taken username and an
	// invite that is unknown, used or expired.
	CreateWithInvite(ctx context.Context, u User, inviteID int64, at time.Time) (User, error)
	Get(ctx context.Context, id int64) (User, error)
	GetByUsername(ctx context.Context, username string) (User, error)
	// GetByEmail matches case-insensitively. ErrNotFound if no user has it.
	GetByEmail(ctx context.Context, email string) (User, error)
	// CreateExternal creates a user and its external identity in one
	// transaction, claiming the invite too when inviteID is not zero. A
	// failure of any part rolls back all of it. ErrConflict covers a taken
	// username or email, an identity that is already linked, and an invite
	// that is unknown, used or expired.
	CreateExternal(ctx context.Context, u User, ident Identity, inviteID int64, at time.Time) (User, error)
	List(ctx context.Context, q string, limit int, cursor int64) ([]User, error)
	Update(ctx context.Context, u User) (User, error)
	Delete(ctx context.Context, id int64) error
	CountAdmins(ctx context.Context) (int64, error)
	TouchSync(ctx context.Context, id int64, at time.Time) error
}

// API keys

type KeyAccess struct {
	Library bool `json:"library"`
	Notes   bool `json:"notes"`
	Write   bool `json:"write"`
	Files   bool `json:"files"`
	// Groups is "all", "read" or "none".
	Groups string `json:"groups"`
}

type APIKey struct {
	ID         int64
	UserID     int64
	Hash       []byte
	Prefix     string
	Name       string
	Access     KeyAccess
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

type KeyRepo interface {
	Create(ctx context.Context, k APIKey) (APIKey, error)
	GetByHash(ctx context.Context, hash []byte) (APIKey, error)
	ListByUser(ctx context.Context, userID int64) ([]APIKey, error)
	Delete(ctx context.Context, userID, id int64) error
	DeleteByHash(ctx context.Context, hash []byte) error
	// Touch updates last_used_at; implementations may coalesce writes.
	Touch(ctx context.Context, id int64, at time.Time) error
}

// Groups

type Group struct {
	ID             int64
	LibraryID      int64
	OwnerID        int64
	Name           string
	Description    string
	URL            string
	LibraryReading string
	LibraryEditing string
	FileEditing    string
	Version        int64
	CreatedAt      time.Time
}

type GroupMember struct {
	GroupID     int64
	UserID      int64
	Username    string
	DisplayName string
	Role        string
}

type GroupRepo interface {
	// Create allocates a library, inserts the group and adds the owner as
	// member with role owner.
	Create(ctx context.Context, g Group) (Group, error)
	Get(ctx context.Context, id int64) (Group, error)
	List(ctx context.Context, limit int, cursor int64) ([]Group, error)
	ListForUser(ctx context.Context, userID int64) ([]Group, error)
	// Update bumps the group version.
	Update(ctx context.Context, g Group) (Group, error)
	Delete(ctx context.Context, id int64) error
	// SetOwner hands the group to an existing member in one transaction: the
	// new owner gets role owner, the previous owner becomes admin and the
	// group version is bumped. ErrNotFound if newOwnerID is not a member.
	SetOwner(ctx context.Context, groupID, newOwnerID int64) error
	Members(ctx context.Context, groupID int64) ([]GroupMember, error)
	// SetMember inserts or updates a membership and bumps the group version.
	SetMember(ctx context.Context, groupID, userID int64, role string) error
	// RemoveMember deletes a membership and bumps the group version.
	RemoveMember(ctx context.Context, groupID, userID int64) error
}

// Login sessions (browser login flow of Zotero clients)

type LoginSession struct {
	TokenHash      []byte
	RequiredUserID *int64
	ClientName     string
	Status         string
	UserID         *int64
	APIKey         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

type LoginSessionRepo interface {
	Create(ctx context.Context, s LoginSession) error
	Get(ctx context.Context, tokenHash []byte) (LoginSession, error)
	// Complete moves a pending session to completed. It returns ErrConflict
	// if the session is not pending.
	Complete(ctx context.Context, tokenHash []byte, userID int64, apiKey string) error
	// ConsumeAPIKey returns the API key of a completed session and clears it
	// in the same statement, so the key is handed out at most once. It
	// returns "" if the session has no key left.
	ConsumeAPIKey(ctx context.Context, tokenHash []byte) (string, error)
	Cancel(ctx context.Context, tokenHash []byte) error
}

// Web sessions (admin UI)

type WebSession struct {
	IDHash    []byte
	UserID    int64
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type WebSessionRepo interface {
	Create(ctx context.Context, s WebSession) error
	Get(ctx context.Context, idHash []byte) (WebSession, error)
	// Extend moves the expiry of a live session (sliding lifetime).
	Extend(ctx context.Context, idHash []byte, expiresAt time.Time) error
	Delete(ctx context.Context, idHash []byte) error
	DeleteForUser(ctx context.Context, userID int64) error
}

// Invites

type Invite struct {
	ID        int64
	TokenHash []byte
	Email     string
	IsAdmin   bool
	CreatedBy *int64
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedBy    *int64
	UsedAt    *time.Time
}

type InviteRepo interface {
	Create(ctx context.Context, i Invite) (Invite, error)
	GetByHash(ctx context.Context, tokenHash []byte) (Invite, error)
	List(ctx context.Context, limit int, cursor int64) ([]Invite, error)
	// Use marks an unused, unexpired invite as used. ErrConflict otherwise.
	Use(ctx context.Context, id, userID int64, at time.Time) error
	Delete(ctx context.Context, id int64) error
}

// Single sign-on

// Identity links a user to an account at an external identity provider.
type Identity struct {
	ID          int64
	UserID      int64
	Provider    string
	Subject     string
	Email       string // empty when the provider did not release one
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

type IdentityRepo interface {
	// Get returns ErrNotFound when (provider, subject) is not linked.
	Get(ctx context.Context, provider, subject string) (Identity, error)
	ListByUser(ctx context.Context, userID int64) ([]Identity, error)
	// ProvidersByUsers maps each user ID to the providers it is linked to.
	// Users without identities are absent from the result.
	ProvidersByUsers(ctx context.Context, ids []int64) (map[int64][]string, error)
	// Create returns ErrConflict if the external account is linked already or
	// the user already has an identity for the provider.
	Create(ctx context.Context, i Identity) (Identity, error)
	// Touch records a login and refreshes the stored email.
	Touch(ctx context.Context, id int64, email string, at time.Time) error
	// Delete unlinks the provider from the user. ErrNotFound if not linked.
	Delete(ctx context.Context, userID int64, provider string) error
}

// SSOState is the server-side state of one SSO round trip. IDHash and
// BindingHash are hashes of secrets that only the browser holds.
type SSOState struct {
	IDHash       []byte
	BindingHash  []byte
	Provider     string
	Nonce        string
	PKCEVerifier string
	RequestID    string
	InviteHash   []byte
	LinkUserID   *int64
	ReturnTo     string
	Identity     []byte // raw JSON stashed by the SAML ACS, nil until then
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type SSOStateRepo interface {
	Create(ctx context.Context, s SSOState) error
	// Get returns ErrNotFound for unknown or expired states.
	Get(ctx context.Context, idHash []byte, now time.Time) (SSOState, error)
	// Take deletes and returns an unexpired state in one statement, so a
	// state can be redeemed only once. ErrNotFound otherwise.
	Take(ctx context.Context, idHash []byte, now time.Time) (SSOState, error)
	// Stash stores the identity and moves the expiry, but only while no
	// identity is stored yet and the state is unexpired. ErrNotFound otherwise.
	Stash(ctx context.Context, idHash []byte, identity []byte, newExpiry, now time.Time) error
}

type InstanceSettingsRepo interface {
	Get(ctx context.Context, key string, dst any) (bool, error)
	Set(ctx context.Context, key string, value any) error
}

// Files

type UploadToken struct {
	UploadKey  string
	LibraryID  int64
	ItemKey    string
	MD5        string
	Filename   string
	Size       int64
	MtimeMS    int64
	Compressed bool
	State      string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// ItemMD5 is the md5 stored in the item when it differs from MD5, i.e.
	// for zipped snapshots. Empty means MD5.
	ItemMD5 string
}

type Blob struct {
	MD5       string
	Size      int64
	CreatedAt time.Time
}

type FileRepo interface {
	BlobExists(ctx context.Context, md5 string) (bool, error)
	// BlobReferencedIn reports whether md5 is attached anywhere in the given
	// libraries. Used so {exists:1} never leaks other users' files.
	BlobReferencedIn(ctx context.Context, md5 string, libraryIDs []int64) (bool, error)
	// PutBlob records a blob. Re-recording an existing blob refreshes its
	// created_at, which restarts the garbage collection grace period.
	PutBlob(ctx context.Context, b Blob) error

	CreateUploadToken(ctx context.Context, t UploadToken) error
	GetUploadToken(ctx context.Context, uploadKey string) (UploadToken, error)
	SetUploadTokenState(ctx context.Context, uploadKey, state string) error

	// OrphanBlobs streams blobs that no attachment references, that are older
	// than olderThan and that no live upload token points at.
	OrphanBlobs(ctx context.Context, olderThan time.Time) (iter.Seq2[Blob, error], error)
	// DeleteOrphanBlob removes the blob row only if it is still an orphan by
	// the same rules as OrphanBlobs, checked atomically. It reports whether
	// the row was removed; only then may the caller delete the bytes.
	DeleteOrphanBlob(ctx context.Context, md5 string, olderThan time.Time) (bool, error)
	Stats(ctx context.Context) (FileStats, error)
}

type FileStats struct {
	BlobCount   int64
	BlobBytes   int64
	OrphanBytes int64
}

// Imports

type ImportJob struct {
	ID            int64
	UserID        int64
	Status        string
	SourceAPIKey  string
	IncludeGroups bool
	IncludeFiles  bool
	Cursor        json.RawMessage
	Progress      json.RawMessage
	Error         string
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

type ImportRepo interface {
	Create(ctx context.Context, j ImportJob) (ImportJob, error)
	Get(ctx context.Context, id int64) (ImportJob, error)
	ListByUser(ctx context.Context, userID int64) ([]ImportJob, error)
	// ClaimNext atomically moves the oldest queued job to running.
	ClaimNext(ctx context.Context) (ImportJob, error)
	Update(ctx context.Context, j ImportJob) error

	// SaveProgress stores cursor and progress of a running job. It reports
	// false when the job is no longer running (cancelled meanwhile), so a
	// worker never overwrites the status the user set.
	SaveProgress(ctx context.Context, id int64, cursor, progress json.RawMessage) (bool, error)
	// Finish ends a running job with status completed or failed, wipes the
	// source key and sets finished_at. It reports false if the job was not
	// running anymore.
	Finish(ctx context.Context, id int64, status, errMsg string, at time.Time) (bool, error)
	// Requeue puts a running job back in the queue, keeping its cursor.
	Requeue(ctx context.Context, id int64) error
	// RequeueRunning does the same for every running job. A worker calls it
	// on startup, when jobs left running by a crashed process are orphans.
	RequeueRunning(ctx context.Context) (int64, error)
}
