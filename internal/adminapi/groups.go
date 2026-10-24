package adminapi

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
)

type groupDTO struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Owner          groupRef `json:"owner"`
	LibraryReading string   `json:"libraryReading"`
	LibraryEditing string   `json:"libraryEditing"`
	FileEditing    string   `json:"fileEditing"`
	MemberCount    int      `json:"memberCount"`
	UsedBytes      int64    `json:"usedBytes"`
	Version        int64    `json:"version"`
	CreatedAt      string   `json:"createdAt"`
}

type groupRef struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type memberDTO struct {
	UserID      int64  `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

func toMemberDTO(m repo.GroupMember) memberDTO {
	return memberDTO{UserID: m.UserID, Username: m.Username, DisplayName: m.DisplayName, Role: m.Role}
}

func (a *API) toGroupDTO(ctx context.Context, g repo.Group, owners map[int64]string) (groupDTO, error) {
	members, err := a.Store.Groups().Members(ctx, g.ID)
	if err != nil {
		return groupDTO{}, err
	}
	return a.groupDTOWith(ctx, g, owners, members)
}

// groupDTOWith is toGroupDTO for callers that already loaded the members.
func (a *API) groupDTOWith(ctx context.Context, g repo.Group, owners map[int64]string, members []repo.GroupMember) (groupDTO, error) {
	name, ok := owners[g.OwnerID]
	if !ok {
		u, err := a.Store.Users().Get(ctx, g.OwnerID)
		if err != nil {
			return groupDTO{}, err
		}
		name = u.Username
		owners[g.OwnerID] = name
	}
	lib, err := a.Store.Library(ctx, g.LibraryID)
	if err != nil {
		return groupDTO{}, err
	}
	return groupDTO{
		ID: g.ID, Name: g.Name, Description: g.Description, Owner: groupRef{ID: g.OwnerID, Username: name},
		LibraryReading: g.LibraryReading, LibraryEditing: g.LibraryEditing, FileEditing: g.FileEditing,
		MemberCount: len(members), UsedBytes: lib.StorageUsed(), Version: g.Version, CreatedAt: ts(g.CreatedAt),
	}, nil
}

func (a *API) groupList(ctx context.Context, gs []repo.Group) ([]groupDTO, error) {
	owners := map[int64]string{}
	out := make([]groupDTO, 0, len(gs))
	for _, g := range gs {
		d, err := a.toGroupDTO(ctx, g, owners)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// groupCtx is what a handler needs to decide whether the caller may act.
type groupCtx struct {
	group   repo.Group
	members []repo.GroupMember
	role    string // caller's role, empty for non-members
	site    bool   // caller is an instance admin
}

func (g groupCtx) canManage() bool { return g.site || g.role == "owner" || g.role == "admin" }
func (g groupCtx) canDelete() bool { return g.site || g.role == "owner" }

func (g groupCtx) member(userID int64) (repo.GroupMember, bool) {
	for _, m := range g.members {
		if m.UserID == userID {
			return m, true
		}
	}
	return repo.GroupMember{}, false
}

// loadGroup answers 404 to outsiders so group IDs cannot be probed.
func (a *API) loadGroup(c *gin.Context) (groupCtx, error) {
	id, err := pathID(c, "id")
	if err != nil {
		return groupCtx{}, newErr(http.StatusNotFound, "not_found", "Group not found.")
	}
	ctx := c.Request.Context()
	g, err := a.Store.Groups().Get(ctx, id)
	if err != nil {
		return groupCtx{}, notFoundAs(err, "Group not found.")
	}
	members, err := a.Store.Groups().Members(ctx, id)
	if err != nil {
		return groupCtx{}, err
	}
	u := currentUser(c)
	gc := groupCtx{group: g, members: members, site: u.IsAdmin}
	if m, ok := gc.member(u.ID); ok {
		gc.role = m.Role
	}
	if !gc.site && gc.role == "" {
		return groupCtx{}, newErr(http.StatusNotFound, "not_found", "Group not found.")
	}
	return gc, nil
}

func (a *API) listGroups(c *gin.Context) error {
	limit, cursor, err := pageParams(c)
	if err != nil {
		return err
	}
	ctx := c.Request.Context()
	u := currentUser(c)
	var rows []repo.Group
	if u.IsAdmin {
		rows, err = a.Store.Groups().List(ctx, limit+1, cursor)
	} else {
		var all []repo.Group
		if all, err = a.Store.Groups().ListForUser(ctx, u.ID); err == nil {
			for _, g := range all {
				if g.ID > cursor && len(rows) <= limit {
					rows = append(rows, g)
				}
			}
		}
	}
	if err != nil {
		return err
	}
	p := paginate(rows, limit, func(g repo.Group) int64 { return g.ID })
	items, err := a.groupList(ctx, p.Items)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, page[groupDTO]{Items: items, NextCursor: p.NextCursor})
	return nil
}

func (a *API) myGroups(c *gin.Context) error {
	ctx := c.Request.Context()
	u := currentUser(c)
	gs, err := a.Store.Groups().ListForUser(ctx, u.ID)
	if err != nil {
		return err
	}
	owners := map[int64]string{}
	type myGroup struct {
		groupDTO
		Role string `json:"role"`
	}
	items := make([]myGroup, 0, len(gs))
	for _, g := range gs {
		members, err := a.Store.Groups().Members(ctx, g.ID)
		if err != nil {
			return err
		}
		d, err := a.groupDTOWith(ctx, g, owners, members)
		if err != nil {
			return err
		}
		role := ""
		for _, m := range members {
			if m.UserID == u.ID {
				role = m.Role
			}
		}
		items = append(items, myGroup{d, role})
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

type groupInput struct {
	Name           *string `json:"name"`
	Description    *string `json:"description"`
	LibraryReading *string `json:"libraryReading"`
	LibraryEditing *string `json:"libraryEditing"`
	FileEditing    *string `json:"fileEditing"`
	OwnerID        *int64  `json:"ownerId"`
}

// apply validates the supplied fields and copies them onto g.
func (in groupInput) apply(g *repo.Group) error {
	fields := map[string]string{}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" || len(n) > 255 {
			fields["name"] = "Enter a name of up to 255 characters."
		}
		g.Name = n
	}
	if in.Description != nil {
		if len(*in.Description) > 5000 {
			fields["description"] = "Description is too long."
		}
		g.Description = *in.Description
	}
	check := func(field string, v *string, allowed []string, dst *string) {
		if v == nil {
			return
		}
		if !slices.Contains(allowed, *v) {
			fields[field] = "Must be one of: " + strings.Join(allowed, ", ") + "."
			return
		}
		*dst = *v
	}
	check("libraryReading", in.LibraryReading, []string{"all", "members"}, &g.LibraryReading)
	check("libraryEditing", in.LibraryEditing, []string{"admins", "members"}, &g.LibraryEditing)
	check("fileEditing", in.FileEditing, []string{"none", "admins", "members"}, &g.FileEditing)
	if len(fields) > 0 {
		return newErr(http.StatusUnprocessableEntity, "validation", "Validation failed.").withFields(fields)
	}
	return nil
}

func (a *API) createGroup(c *gin.Context) error {
	var in groupInput
	if err := bind(c, &in); err != nil {
		return err
	}
	if in.Name == nil {
		return fieldError("name", "Enter a name of up to 255 characters.")
	}
	g := repo.Group{OwnerID: currentUser(c).ID, LibraryReading: "members", LibraryEditing: "members", FileEditing: "members"}
	if err := in.apply(&g); err != nil {
		return err
	}
	ctx := c.Request.Context()
	created, err := a.Store.Groups().Create(ctx, g)
	if err != nil {
		return err
	}
	a.accessChanged(created.OwnerID)
	d, err := a.toGroupDTO(ctx, created, map[int64]string{})
	if err != nil {
		return err
	}
	c.JSON(http.StatusCreated, d)
	return nil
}

func (a *API) getGroup(c *gin.Context) error {
	gc, err := a.loadGroup(c)
	if err != nil {
		return err
	}
	d, err := a.groupDTOWith(c.Request.Context(), gc.group, map[int64]string{}, gc.members)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, d)
	return nil
}

func (a *API) patchGroup(c *gin.Context) error {
	gc, err := a.loadGroup(c)
	if err != nil {
		return err
	}
	if !gc.canManage() {
		return errForbidden
	}
	var in groupInput
	if err := bind(c, &in); err != nil {
		return err
	}
	g := gc.group
	if err := in.apply(&g); err != nil {
		return err
	}
	changeOwner := in.OwnerID != nil && *in.OwnerID != g.OwnerID
	if changeOwner {
		if !gc.canDelete() { // same people who may delete may hand the group over
			return newErr(http.StatusForbidden, "forbidden", "Only the owner can transfer a group.")
		}
		if _, ok := gc.member(*in.OwnerID); !ok {
			return fieldError("ownerId", "The new owner must already be a member of the group.")
		}
	}
	ctx := c.Request.Context()
	updated, err := a.Store.Groups().Update(ctx, g)
	if err != nil {
		return err
	}
	if changeOwner {
		if err := a.Store.Groups().SetOwner(ctx, g.ID, *in.OwnerID); err != nil {
			return err
		}
		if updated, err = a.Store.Groups().Get(ctx, g.ID); err != nil {
			return err
		}
	}
	d, err := a.toGroupDTO(ctx, updated, map[int64]string{})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, d)
	return nil
}

func (a *API) deleteGroup(c *gin.Context) error {
	gc, err := a.loadGroup(c)
	if err != nil {
		return err
	}
	if !gc.canDelete() {
		return newErr(http.StatusForbidden, "forbidden", "Only the owner can delete a group.")
	}
	// Collect members first; after the delete nobody knows who lost access.
	members, err := a.Store.Groups().Members(c.Request.Context(), gc.group.ID)
	if err != nil {
		return err
	}
	if err := a.Store.Groups().Delete(c.Request.Context(), gc.group.ID); err != nil {
		return err
	}
	for _, m := range members {
		a.accessChanged(m.UserID)
	}
	c.Status(http.StatusNoContent)
	return nil
}

func (a *API) listMembers(c *gin.Context) error {
	gc, err := a.loadGroup(c)
	if err != nil {
		return err
	}
	items := make([]memberDTO, 0, len(gc.members))
	for _, m := range gc.members {
		items = append(items, toMemberDTO(m))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

func (a *API) putMember(c *gin.Context) error {
	gc, err := a.loadGroup(c)
	if err != nil {
		return err
	}
	if !gc.canManage() {
		return errForbidden
	}
	uid, err := pathID(c, "userId")
	if err != nil {
		return newErr(http.StatusNotFound, "not_found", "User not found.")
	}
	var in struct {
		Role string `json:"role"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	switch in.Role {
	case "member", "admin":
	case "owner":
		return fieldError("role", "Transfer ownership by setting ownerId on the group.")
	default:
		return fieldError("role", "Must be member or admin.")
	}
	ctx := c.Request.Context()
	if _, err := a.Store.Users().Get(ctx, uid); err != nil {
		return notFoundAs(err, "User not found.")
	}
	existing, isMember := gc.member(uid)
	if isMember && existing.Role == "owner" {
		return newErr(http.StatusConflict, "owner_role", "The owner's role cannot be changed. Transfer ownership first.")
	}
	// Plain group admins run day to day membership, but only the owner (or an
	// instance admin) decides who else holds admin rights.
	if !gc.canDelete() && uid != currentUser(c).ID {
		if in.Role == "admin" || (isMember && existing.Role == "admin") {
			return newErr(http.StatusForbidden, "forbidden", "Only the owner can grant or change admin rights.")
		}
	}
	if err := a.Store.Groups().SetMember(ctx, gc.group.ID, uid, in.Role); err != nil {
		return err
	}
	a.accessChanged(uid)
	members, err := a.Store.Groups().Members(ctx, gc.group.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.UserID == uid {
			c.JSON(http.StatusOK, toMemberDTO(m))
			return nil
		}
	}
	return errNotFound
}

func (a *API) removeMember(c *gin.Context) error {
	gc, err := a.loadGroup(c)
	if err != nil {
		return err
	}
	uid, err := pathID(c, "userId")
	if err != nil {
		return newErr(http.StatusNotFound, "not_found", "Member not found.")
	}
	target, ok := gc.member(uid)
	if !ok {
		return newErr(http.StatusNotFound, "not_found", "Member not found.")
	}
	// Anyone may leave a group, otherwise management rights are needed.
	self := uid == currentUser(c).ID
	if !self && !gc.canManage() {
		return errForbidden
	}
	if target.Role == "owner" {
		return newErr(http.StatusConflict, "owner_role", "The owner cannot be removed. Transfer ownership or delete the group.")
	}
	if !self && target.Role == "admin" && !gc.canDelete() {
		return newErr(http.StatusForbidden, "forbidden", "Only the owner can remove an admin.")
	}
	if err := a.Store.Groups().RemoveMember(c.Request.Context(), gc.group.ID, uid); err != nil {
		return err
	}
	a.accessChanged(uid)
	c.Status(http.StatusNoContent)
	return nil
}

func (a *API) lookupUsers(c *gin.Context) error {
	q := strings.TrimSpace(c.Query("q"))
	if len([]rune(q)) < 2 {
		return fieldError("q", "Enter at least 2 characters.")
	}
	rows, err := a.Store.Users().List(c.Request.Context(), q, 50, 0)
	if err != nil {
		return err
	}
	type hit struct {
		ID          int64  `json:"id"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
	}
	items := []hit{}
	for _, u := range rows {
		// The repo search also matches email; only names are public here, or
		// any user could test whether an address is registered.
		if u.Disabled || (!containsFold(u.Username, q) && !containsFold(u.DisplayName, q)) {
			continue
		}
		items = append(items, hit{u.ID, u.Username, u.DisplayName})
		if len(items) == 10 {
			break
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}
