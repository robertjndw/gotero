package adminapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/jobs"
	"github.com/robertjndw/gotero/internal/repo"
)

func (a *API) stats(c *gin.Context) error {
	ctx := c.Request.Context()
	st, err := a.Store.Stats(ctx)
	if err != nil {
		return err
	}
	fs, err := a.Store.Files().Stats(ctx)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, gin.H{
		"users": st.Users, "groups": st.Groups, "items": st.Items, "attachments": st.Attachments,
		"storage": gin.H{
			"backend": a.Config.Storage.Backend, "usedBytes": fs.BlobBytes,
			"blobCount": fs.BlobCount, "orphanBytes": fs.OrphanBytes,
		},
		"version": a.Version,
	})
	return nil
}

func (a *API) adminListUsers(c *gin.Context) error {
	limit, cursor, err := pageParams(c)
	if err != nil {
		return err
	}
	rows, err := a.Store.Users().List(c.Request.Context(), c.Query("q"), limit+1, cursor)
	if err != nil {
		return err
	}
	p := paginate(rows, limit, func(u repo.User) int64 { return u.ID })
	items, err := a.userDTOs(c.Request.Context(), p.Items)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, page[userDTO]{Items: items, NextCursor: p.NextCursor})
	return nil
}

func (a *API) adminCreateUser(c *gin.Context) error {
	var in struct {
		Username    string   `json:"username"`
		Password    string   `json:"password"`
		Email       string   `json:"email"`
		DisplayName string   `json:"displayName"`
		IsAdmin     bool     `json:"isAdmin"`
		QuotaBytes  optInt64 `json:"quotaBytes"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	u, err := a.Accounts.CreateUser(c.Request.Context(), accounts.CreateUserInput{
		Username: in.Username, Password: in.Password, Email: in.Email, DisplayName: in.DisplayName,
		IsAdmin: in.IsAdmin, Quota: in.QuotaBytes.opt(),
	})
	if err != nil {
		return err
	}
	d, err := a.userDTO(c.Request.Context(), u)
	if err != nil {
		return err
	}
	c.JSON(http.StatusCreated, d)
	return nil
}

func (a *API) adminPatchUser(c *gin.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	var in struct {
		DisplayName *string  `json:"displayName"`
		Email       *string  `json:"email"`
		IsAdmin     *bool    `json:"isAdmin"`
		Disabled    *bool    `json:"disabled"`
		QuotaBytes  optInt64 `json:"quotaBytes"`
		Password    *string  `json:"password"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	u, err := a.Accounts.UpdateUser(c.Request.Context(), id, accounts.UserPatch{
		DisplayName: in.DisplayName, Email: in.Email, IsAdmin: in.IsAdmin, Disabled: in.Disabled,
		Quota: in.QuotaBytes.opt(), Password: in.Password,
	}, currentUser(c).ID)
	if err != nil {
		return err
	}
	if in.Disabled != nil && *in.Disabled {
		a.accessChanged(u.ID)
	}
	d, err := a.userDTO(c.Request.Context(), u)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, d)
	return nil
}

func (a *API) adminDeleteUser(c *gin.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	if err := a.Accounts.DeleteUser(c.Request.Context(), id, currentUser(c).ID); err != nil {
		return err
	}
	a.accessChanged(id)
	c.Status(http.StatusNoContent)
	return nil
}

// adminDeleteIdentity goes through UnlinkIdentity like the self-service
// endpoint, so an admin cannot strand a user without any way to sign in
// either; set a password first if the identity really has to go.
func (a *API) adminDeleteIdentity(c *gin.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	if err := a.Accounts.UnlinkIdentity(c.Request.Context(), id, c.Param("provider")); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

type inviteDTO struct {
	ID        int64   `json:"id"`
	URL       string  `json:"url"`
	Email     *string `json:"email"`
	IsAdmin   bool    `json:"isAdmin"`
	CreatedBy string  `json:"createdBy"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt string  `json:"expiresAt"`
	UsedBy    *string `json:"usedBy"`
}

// toInviteDTO resolves usernames through names, a per-request cache.
func (a *API) toInviteDTO(c *gin.Context, inv repo.Invite, names map[int64]string) inviteDTO {
	lookup := func(id *int64) string {
		if id == nil {
			return ""
		}
		if n, ok := names[*id]; ok {
			return n
		}
		n := ""
		if u, err := a.Store.Users().Get(c.Request.Context(), *id); err == nil {
			n = u.Username
		}
		names[*id] = n
		return n
	}
	d := inviteDTO{
		ID: inv.ID, URL: baseURL(c) + "/register?invite=" + a.Accounts.InviteToken(inv),
		IsAdmin: inv.IsAdmin, CreatedBy: lookup(inv.CreatedBy), CreatedAt: ts(inv.CreatedAt), ExpiresAt: ts(inv.ExpiresAt),
	}
	if inv.Email != "" {
		d.Email = &inv.Email
	}
	if inv.UsedBy != nil {
		if n := lookup(inv.UsedBy); n != "" {
			d.UsedBy = &n
		}
	}
	return d
}

func (a *API) adminListInvites(c *gin.Context) error {
	limit, cursor, err := pageParams(c)
	if err != nil {
		return err
	}
	rows, err := a.Store.Invites().List(c.Request.Context(), limit+1, cursor)
	if err != nil {
		return err
	}
	p := paginate(rows, limit, func(i repo.Invite) int64 { return i.ID })
	names := map[int64]string{}
	items := make([]inviteDTO, 0, len(p.Items))
	for _, inv := range p.Items {
		items = append(items, a.toInviteDTO(c, inv, names))
	}
	c.JSON(http.StatusOK, page[inviteDTO]{Items: items, NextCursor: p.NextCursor})
	return nil
}

func (a *API) adminCreateInvite(c *gin.Context) error {
	var in struct {
		Email          string `json:"email"`
		IsAdmin        bool   `json:"isAdmin"`
		ExpiresInHours int    `json:"expiresInHours"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	if in.ExpiresInHours == 0 {
		in.ExpiresInHours = 72
	}
	if in.ExpiresInHours < 0 || in.ExpiresInHours > int(accounts.MaxInviteTTL/time.Hour) {
		return fieldError("expiresInHours", "Choose between 1 hour and 365 days.")
	}
	by := currentUser(c).ID
	inv, _, err := a.Accounts.CreateInvite(c.Request.Context(), accounts.CreateInviteInput{
		CreatedBy: &by, Email: in.Email, IsAdmin: in.IsAdmin, TTL: time.Duration(in.ExpiresInHours) * time.Hour,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusCreated, a.toInviteDTO(c, inv, map[int64]string{}))
	return nil
}

func (a *API) adminDeleteInvite(c *gin.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	if err := a.Store.Invites().Delete(c.Request.Context(), id); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

type settingsDTO struct {
	OpenRegistration  bool   `json:"openRegistration"`
	DefaultQuotaBytes *int64 `json:"defaultQuotaBytes"`
}

func (a *API) loadSettings(c *gin.Context) (settingsDTO, error) {
	open, err := a.Accounts.OpenRegistration(c.Request.Context())
	if err != nil {
		return settingsDTO{}, err
	}
	q, err := a.Accounts.DefaultQuota(c.Request.Context())
	return settingsDTO{OpenRegistration: open, DefaultQuotaBytes: q}, err
}

func (a *API) getSettings(c *gin.Context) error {
	s, err := a.loadSettings(c)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, s)
	return nil
}

func (a *API) patchSettings(c *gin.Context) error {
	var in struct {
		OpenRegistration  *bool    `json:"openRegistration"`
		DefaultQuotaBytes optInt64 `json:"defaultQuotaBytes"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	ctx := c.Request.Context()
	if in.DefaultQuotaBytes.set {
		if err := a.Accounts.SetDefaultQuota(ctx, in.DefaultQuotaBytes.val); err != nil {
			return err
		}
	}
	if in.OpenRegistration != nil {
		if err := a.Accounts.SetOpenRegistration(ctx, *in.OpenRegistration); err != nil {
			return err
		}
	}
	return a.getSettings(c)
}

func (a *API) runGC(c *gin.Context) error {
	var in struct {
		DryRun bool `json:"dryRun"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	var res jobs.GCResult
	if a.GC != nil {
		var err error
		if res, err = a.GC.GC(c.Request.Context(), in.DryRun); err != nil {
			return err
		}
	}
	c.JSON(http.StatusOK, gin.H{"orphanBlobs": res.OrphanBlobs, "orphanBytes": res.OrphanBytes, "deleted": res.Deleted})
	return nil
}
