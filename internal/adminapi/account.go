package adminapi

import (
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
)

func (a *API) getSetup(c *gin.Context) error {
	open, err := a.Accounts.OpenRegistration(c.Request.Context())
	if err != nil {
		return err
	}
	providers := []gin.H{}
	for _, p := range a.SSO.List() {
		providers = append(providers, gin.H{
			"id": p.Name(), "kind": p.Kind(), "displayName": p.DisplayName(),
			"startUrl": "/auth/" + string(p.Kind()) + "/" + url.PathEscape(p.Name()) + "/start",
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"openRegistration": open,
		"publicUrl":        httpx.BaseURL(c.Request.Context()),
		"version":          a.Version,
		"localLogin":       a.Config.LocalLogin,
		"providers":        providers,
	})
	return nil
}

func (a *API) getInvite(c *gin.Context) error {
	inv, valid, err := a.Accounts.LookupInvite(c.Request.Context(), c.Param("token"))
	if err != nil {
		return err
	}
	var email *string
	if valid && inv.Email != "" {
		email = &inv.Email
	}
	c.JSON(http.StatusOK, gin.H{"valid": valid, "email": email})
	return nil
}

type registerRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	InviteToken string `json:"inviteToken"`
}

func (a *API) register(c *gin.Context) error {
	var in registerRequest
	if err := bind(c, &in); err != nil {
		return err
	}
	ctx := c.Request.Context()
	u, err := a.Accounts.Register(ctx, accounts.CreateUserInput{
		Username: in.Username, Password: in.Password, Email: in.Email, DisplayName: in.DisplayName,
	}, in.InviteToken)
	if err != nil {
		return err
	}
	csrf, err := a.startSession(c, u)
	if err != nil {
		return err
	}
	me, err := a.meDTO(ctx, u, csrf)
	if err != nil {
		return err
	}
	c.JSON(http.StatusCreated, me)
	return nil
}

func (a *API) patchMe(c *gin.Context) error {
	var in struct {
		DisplayName *string `json:"displayName"`
		Email       *string `json:"email"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	u, err := a.Accounts.UpdateUser(c.Request.Context(), currentUser(c).ID,
		accounts.UserPatch{DisplayName: in.DisplayName, Email: in.Email}, 0)
	if err != nil {
		return err
	}
	me, err := a.meDTO(c.Request.Context(), u, currentSession(c).CSRFToken)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, me)
	return nil
}

func (a *API) changePassword(c *gin.Context) error {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	ctx := c.Request.Context()
	u := currentUser(c)
	if !a.Accounts.LocalLoginAllowed(u) {
		return accounts.ErrLocalLoginDisabled
	}
	// SSO-only accounts have no password to confirm; they are already
	// authenticated by their session.
	if u.PasswordHash != "" {
		if ok, err := auth.VerifyPassword(u.PasswordHash, in.CurrentPassword); err != nil || !ok {
			return fieldError("currentPassword", "Current password is incorrect.")
		}
	}
	if err := accounts.ValidatePassword(in.NewPassword); err != nil {
		return renameFields(err, "password", "newPassword")
	}
	if err := a.Accounts.SetPassword(ctx, u.ID, in.NewPassword); err != nil {
		return renameFields(err, "password", "newPassword")
	}
	// SetPassword logs every session out, including this one. Hand the caller
	// a fresh one so changing your own password does not bounce you to login.
	if _, err := a.startSession(c, u); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

func (a *API) listKeys(c *gin.Context) error {
	keys, err := a.Store.Keys().ListByUser(c.Request.Context(), currentUser(c).ID)
	if err != nil {
		return err
	}
	items := make([]keyDTO, 0, len(keys))
	for _, k := range keys {
		items = append(items, toKeyDTO(k))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

func (a *API) createKey(c *gin.Context) error {
	var in struct {
		Name   string         `json:"name"`
		Access repo.KeyAccess `json:"access"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	k, raw, err := a.Accounts.CreateKey(c.Request.Context(), currentUser(c).ID, in.Name, in.Access)
	if err != nil {
		return err
	}
	c.JSON(http.StatusCreated, struct {
		keyDTO
		Key string `json:"key"`
	}{toKeyDTO(k), raw})
	return nil
}

func (a *API) deleteKey(c *gin.Context) error {
	id, err := pathID(c, "id")
	if err != nil {
		return err
	}
	if err := a.Accounts.RevokeKey(c.Request.Context(), currentUser(c).ID, id); err != nil {
		return err
	}
	// Streaming connections using that key must stop receiving updates.
	a.accessChanged(currentUser(c).ID)
	c.Status(http.StatusNoContent)
	return nil
}

func (a *API) deleteMyIdentity(c *gin.Context) error {
	if err := a.Accounts.UnlinkIdentity(c.Request.Context(), currentUser(c).ID, c.Param("provider")); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}
