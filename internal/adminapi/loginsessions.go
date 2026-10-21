package adminapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
)

type loginSessionDTO struct {
	Token          string `json:"token"`
	Status         string `json:"status"`
	RequiredUserID *int64 `json:"requiredUserId"`
	ClientName     string `json:"clientName"`
	CreatedAt      string `json:"createdAt"`
	ExpiresAt      string `json:"expiresAt"`
}

// loginSessionDTO reports expiry as a status; the table only ever stores
// pending, completed and cancelled.
func (a *API) toLoginSessionDTO(token string, s repo.LoginSession) loginSessionDTO {
	status := s.Status
	if status == "pending" && !s.ExpiresAt.After(a.Now()) {
		status = "expired"
	}
	return loginSessionDTO{
		Token: token, Status: status, RequiredUserID: s.RequiredUserID,
		ClientName: s.ClientName, CreatedAt: ts(s.CreatedAt), ExpiresAt: ts(s.ExpiresAt),
	}
}

func (a *API) loadLoginSession(c *gin.Context) (repo.LoginSession, error) {
	return a.Store.LoginSessions().Get(c.Request.Context(), auth.HashToken(c.Param("token")))
}

func (a *API) getLoginSession(c *gin.Context) error {
	s, err := a.loadLoginSession(c)
	if err != nil {
		return notFoundAs(err, "Unknown login session.")
	}
	c.JSON(http.StatusOK, a.toLoginSessionDTO(c.Param("token"), s))
	return nil
}

func notFoundAs(err error, msg string) error {
	if errors.Is(err, repo.ErrNotFound) {
		return newErr(http.StatusNotFound, "not_found", msg)
	}
	return err
}

func (a *API) approveLoginSession(c *gin.Context) error {
	ctx := c.Request.Context()
	token := c.Param("token")
	hash := auth.HashToken(token)
	s, err := a.Store.LoginSessions().Get(ctx, hash)
	if err != nil {
		return notFoundAs(err, "Unknown login session.")
	}
	dto := a.toLoginSessionDTO(token, s)
	switch dto.Status {
	case "expired":
		return newErr(http.StatusGone, "expired", "This login request has expired. Start again from Zotero.")
	case "pending":
	default:
		return newErr(http.StatusConflict, "conflict", "This request is "+dto.Status+".")
	}
	u := currentUser(c)
	if s.RequiredUserID != nil && *s.RequiredUserID != u.ID {
		return newErr(http.StatusForbidden, "user_mismatch", "This Zotero client is linked to a different account.")
	}

	name := s.ClientName
	if name == "" {
		name = "Zotero"
	}
	key, raw, err := a.Accounts.CreateKey(ctx, u.ID, name, accounts.FullAccess())
	if err != nil {
		return err
	}
	if err := a.Store.LoginSessions().Complete(ctx, hash, u.ID, raw); err != nil {
		// Lost a race with another approval or a cancel: don't leave an
		// unreachable key behind.
		if rerr := a.Store.Keys().DeleteByHash(ctx, key.Hash); rerr != nil {
			a.Log.WarnContext(ctx, "could not remove key of failed approval", "err", rerr)
		}
		if errors.Is(err, repo.ErrConflict) {
			return newErr(http.StatusConflict, "conflict", "This request is no longer pending.")
		}
		return err
	}
	if a.Notifier != nil {
		a.Notifier.LoginCompleted(token)
	}
	s.Status = "completed"
	c.JSON(http.StatusOK, a.toLoginSessionDTO(token, s))
	return nil
}

func (a *API) cancelLoginSession(c *gin.Context) error {
	ctx := c.Request.Context()
	hash := auth.HashToken(c.Param("token"))
	if err := a.Store.LoginSessions().Cancel(ctx, hash); err != nil {
		return notFoundAs(err, "Unknown login session.")
	}
	s, err := a.Store.LoginSessions().Get(ctx, hash)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, a.toLoginSessionDTO(c.Param("token"), s))
	return nil
}
