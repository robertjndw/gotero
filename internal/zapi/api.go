// Package zapi implements the Zotero Web API v3 sync subset: keys, groups,
// library reads, writes, settings, deleted objects, tags and full text.
//
// File sync and browser login sessions live here too. RegisterFileRoutes
// mounts the self-authenticating upload and download targets separately from
// Register, because they sit outside the API key middleware. The streaming
// server plugs in through Publisher and Notifier.
package zapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/files"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero/schema"
)

// Event announces that a library moved to a new version. It is published only
// after the transaction that caused it has committed.
type Event struct {
	// Topic is "/users/{id}" or "/groups/{id}".
	Topic   string
	Version int64
}

// Publisher receives library change events, typically to feed the streaming
// server. Publish must not block.
type Publisher interface {
	Publish(ev Event)
}

// NopPublisher drops every event.
type NopPublisher struct{}

func (NopPublisher) Publish(Event) {}

// Deps are the collaborators of the API.
type Deps struct {
	Store     repo.Store
	Auth      *auth.Authenticator
	Schema    *schema.Schema
	Publisher Publisher
	Log       *slog.Logger
	// MaxRequestBytes caps JSON write bodies, compressed and decompressed.
	MaxRequestBytes int64
	Now             func() time.Time

	// Files stores attachment bytes. Without it the file endpoints answer 501.
	Files *files.Service
	// MaxUploadBytes caps the filesize of one upload; 0 means unlimited.
	MaxUploadBytes int64
	// DefaultQuota returns the instance default quota in bytes, nil for
	// unlimited. When unset there is no instance default.
	DefaultQuota func(ctx context.Context) (*int64, error)
	// Notifier receives stream notifications that are not library versions.
	// May be nil.
	Notifier Notifier
	// Login checks a username and password for the legacy POST /keys flow. It
	// must be the same guarded entry point the admin UI uses
	// (accounts.Service.Login) so both draw from one rate limit and one
	// argon2 concurrency budget. Without it POST /keys refuses every login.
	Login func(ctx context.Context, username, password, ip string) (repo.User, error)
}

type nopNotifier struct{}

func (nopNotifier) LoginCompleted(string) {}
func (nopNotifier) AccessChanged(int64)   {}

// API serves the Zotero Web API v3.
type API struct {
	store  repo.Store
	auth   *auth.Authenticator
	schema *schema.Schema
	pub    Publisher
	log    *slog.Logger
	maxReq int64
	now    func() time.Time

	files        *files.Service
	maxUpload    int64
	defQuota     func(ctx context.Context) (*int64, error)
	notify       Notifier
	login        func(ctx context.Context, username, password, ip string) (repo.User, error)
	sessionLimit *rateLimiter
}

const defaultMaxRequestBytes = 32 << 20

// New builds an API. Optional dependencies fall back to sane defaults.
func New(d Deps) *API {
	a := &API{
		store: d.Store, auth: d.Auth, schema: d.Schema, pub: d.Publisher,
		log: d.Log, maxReq: d.MaxRequestBytes, now: d.Now,
		files: d.Files, maxUpload: d.MaxUploadBytes, defQuota: d.DefaultQuota, notify: d.Notifier, login: d.Login,
		sessionLimit: newRateLimiter(sessionsPerHour, sessionWindow),
	}
	if a.notify == nil {
		a.notify = nopNotifier{}
	}
	if a.schema == nil {
		a.schema = schema.Default()
	}
	if a.pub == nil {
		a.pub = NopPublisher{}
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.maxReq <= 0 {
		a.maxReq = defaultMaxRequestBytes
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a
}

// Register mounts every route on r. The server mounts it at /api.
func (a *API) Register(r gin.IRouter) {
	g := r.Group("", a.apiVersion())
	a.registerKeys(g)
	a.registerSessions(g)
	a.registerGroups(g)
	a.registerLibrary(g.Group("/users/:id", a.Authenticated(), a.Library(kindUser)), "user")
	a.registerLibrary(g.Group("/groups/:id", a.Authenticated(), a.Library(kindGroup)), "group")
}

// registerLibrary registers everything that exists under both /users/:id and
// /groups/:id.
func (a *API) registerLibrary(g *gin.RouterGroup, kind string) {
	body := a.body()

	// Reads.
	g.GET("/items", a.listObjects(repo.TypeItem, itemsAll))
	g.GET("/items/top", a.listObjects(repo.TypeItem, itemsTop))
	g.GET("/items/trash", a.listObjects(repo.TypeItem, itemsTrash))
	g.GET("/items/tags", a.listTags(tagsAllItems))
	g.GET("/items/top/tags", a.listTags(tagsTopItems))
	g.GET("/items/:key", a.getObject(repo.TypeItem))
	g.GET("/items/:key/children", a.listObjects(repo.TypeItem, itemsChildren))
	g.GET("/items/:key/tags", a.listTags(tagsOfItem))
	g.GET("/collections", a.listObjects(repo.TypeCollection, collectionsAll))
	g.GET("/collections/top", a.listObjects(repo.TypeCollection, collectionsTop))
	g.GET("/collections/:key", a.getObject(repo.TypeCollection))
	g.GET("/collections/:key/collections", a.listObjects(repo.TypeCollection, collectionsChildren))
	g.GET("/collections/:key/items", a.listObjects(repo.TypeItem, itemsInCollection))
	g.GET("/collections/:key/items/top", a.listObjects(repo.TypeItem, itemsTopInCollection))
	g.GET("/collections/:key/tags", a.listTags(tagsInCollection))
	g.GET("/searches", a.listObjects(repo.TypeSearch, searchesAll))
	g.GET("/searches/:key", a.getObject(repo.TypeSearch))
	g.GET("/tags", a.listTags(tagsAllItems))
	g.GET("/tags/:name", a.getTag)
	g.GET("/settings", a.getSettings)
	g.GET("/settings/:key", a.getSetting)
	g.GET("/deleted", a.getDeleted)
	g.GET("/fulltext", a.getFulltextVersions)
	g.GET("/items/:key/fulltext", a.getFulltext)
	g.GET("/items/:key/file", a.getItemFile)

	// Writes. The body middleware only wraps routes that read JSON.
	for _, t := range []repo.ObjectType{repo.TypeItem, repo.TypeCollection, repo.TypeSearch} {
		plural := pluralOf(t)
		g.POST("/"+plural, body, a.postObjects(t))
		g.PUT("/"+plural+"/:key", body, a.putObject(t, true))
		g.PATCH("/"+plural+"/:key", body, a.putObject(t, false))
		g.DELETE("/"+plural, a.deleteObjects(t))
		g.DELETE("/"+plural+"/:key", a.deleteObject(t))
	}
	g.POST("/settings", body, a.postSettings)
	g.PUT("/settings/:key", body, a.putSetting)
	g.DELETE("/settings", a.deleteSettings)
	g.DELETE("/settings/:key", a.deleteSetting)
	g.DELETE("/tags", a.deleteTags)
	g.POST("/fulltext", body, a.postFulltext)
	g.PUT("/items/:key/fulltext", body, a.putFulltext)
	g.POST("/items/:key/file", a.itemFile)
	if kind == "user" {
		g.POST("/removestoragefiles", a.removeStorageFiles)
	}
}

func pluralOf(t repo.ObjectType) string {
	if t == repo.TypeSearch {
		return "searches"
	}
	return string(t) + "s"
}
