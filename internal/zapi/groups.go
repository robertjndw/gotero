package zapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

func (a *API) registerGroups(g *gin.RouterGroup) {
	g.GET("/users/:id/groups", a.Authenticated(), a.userGroups)
	g.GET("/groups/:id", a.Authenticated(), a.Library(kindGroup), a.getGroup)
}

func (a *API) userGroups(c *gin.Context) {
	p, _ := PrincipalFrom(c)
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		Fail(c, http.StatusBadRequest, "Invalid user ID")
		return
	}
	if id != p.User.ID {
		Fail(c, http.StatusForbidden, "Forbidden")
		return
	}
	ctx := c.Request.Context()
	var groups []repo.Group
	// A key without group access sees no groups at all.
	if g := p.Key.Access.Groups; g == "all" || g == "read" {
		groups, err = a.store.Groups().ListForUser(ctx, id)
		if err != nil {
			a.FailErr(c, err)
			return
		}
	}

	switch c.DefaultQuery("format", "json") {
	case "versions":
		out := make(map[string]int64, len(groups))
		for _, g := range groups {
			out[strconv.FormatInt(g.ID, 10)] = g.Version
		}
		c.JSON(http.StatusOK, out)
	case "json":
		out := make([]zotero.Group, 0, len(groups))
		for _, g := range groups {
			obj, err := a.groupObject(c, g)
			if err != nil {
				a.FailErr(c, err)
				return
			}
			out = append(out, obj)
		}
		c.Header("Total-Results", strconv.Itoa(len(out)))
		// Zotero's getGroups parses Link unconditionally and throws when it
		// is missing, so always send at least rel="last", even when empty.
		c.Header("Link", linkHeader(c, 0, maxLimit, max(int64(len(out)), 1)))
		c.JSON(http.StatusOK, out)
	default:
		Fail(c, http.StatusBadRequest, "Invalid 'format' value")
	}
}

func (a *API) getGroup(c *gin.Context) {
	l := lib(c)
	obj, err := a.groupObject(c, *l.Group)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	// The group carries its own version, separate from its library's.
	setVersion(c, l.Group.Version)
	c.JSON(http.StatusOK, obj)
}

// groupObject builds the group JSON. admins excludes the owner and members
// excludes owner and admins, which is how zotero.org lists them and what the
// client relies on to decide whether a member may edit.
func (a *API) groupObject(c *gin.Context, g repo.Group) (zotero.Group, error) {
	ctx := c.Request.Context()
	members, err := a.store.Groups().Members(ctx, g.ID)
	if err != nil {
		return zotero.Group{}, err
	}
	admins, plain := []int64{}, []int64{}
	for _, m := range members {
		switch m.Role {
		case "admin":
			admins = append(admins, m.UserID)
		case "member":
			plain = append(plain, m.UserID)
		}
	}
	numItems := 0
	if r, err := a.store.Library(ctx, g.LibraryID); err == nil {
		n, err := r.Count(ctx, repo.ListQuery{VersionsQuery: repo.VersionsQuery{Type: repo.TypeItem}})
		if err != nil {
			return zotero.Group{}, err
		}
		numItems = int(n)
	} else {
		return zotero.Group{}, err
	}

	base := apiRoot(c) + "/groups/" + strconv.FormatInt(g.ID, 10)
	return zotero.Group{
		ID:      g.ID,
		Version: g.Version,
		Links: zotero.Links{
			"self":      {Href: base, Type: "application/json"},
			"alternate": {Href: base, Type: "text/html"},
		},
		Meta: zotero.GroupMeta{
			Created:  g.CreatedAt.UTC().Format(time.RFC3339),
			NumItems: numItems,
		},
		Data: zotero.GroupData{
			ID: g.ID, Version: g.Version, Name: g.Name, Owner: g.OwnerID, Type: "Private",
			Description: g.Description, URL: g.URL,
			LibraryEditing: g.LibraryEditing, LibraryReading: g.LibraryReading, FileEditing: g.FileEditing,
			Admins: admins, Members: plain,
		},
	}, nil
}
