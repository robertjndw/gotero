package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/accounts"
	"github.com/robertjndw/gotero/internal/httpx"
	"github.com/robertjndw/gotero/internal/repo"
)

const maxBodyBytes = 1 << 20

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func tsPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

type userDTO struct {
	ID          int64    `json:"id"`
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName"`
	Email       string   `json:"email"`
	IsAdmin     bool     `json:"isAdmin"`
	Disabled    bool     `json:"disabled"`
	QuotaBytes  *int64   `json:"quotaBytes"`
	UsedBytes   int64    `json:"usedBytes"`
	CreatedAt   string   `json:"createdAt"`
	LastSyncAt  *string  `json:"lastSyncAt"`
	HasPassword bool     `json:"hasPassword"`
	Providers   []string `json:"providers"`
}

type identityDTO struct {
	Provider     string  `json:"provider"`
	ProviderName string  `json:"providerName"`
	Kind         string  `json:"kind"`
	Email        string  `json:"email"`
	CreatedAt    string  `json:"createdAt"`
	LastLoginAt  *string `json:"lastLoginAt"`
}

type meDTO struct {
	userDTO
	CSRFToken  string        `json:"csrfToken"`
	Identities []identityDTO `json:"identities"`
}

func (a *API) userDTO(ctx context.Context, u repo.User) (userDTO, error) {
	idents, err := a.Store.Identities().ListByUser(ctx, u.ID)
	if err != nil {
		return userDTO{}, err
	}
	return a.userDTOWith(ctx, u, providerIDs(idents))
}

// userDTOs builds DTOs for a list with one identity query instead of one
// per user.
func (a *API) userDTOs(ctx context.Context, users []repo.User) ([]userDTO, error) {
	ids := make([]int64, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	byUser, err := a.Store.Identities().ProvidersByUsers(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		d, err := a.userDTOWith(ctx, u, byUser[u.ID])
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func providerIDs(idents []repo.Identity) []string {
	out := make([]string, 0, len(idents))
	for _, i := range idents {
		out = append(out, i.Provider)
	}
	return out
}

func (a *API) userDTOWith(ctx context.Context, u repo.User, providers []string) (userDTO, error) {
	lib, err := a.Store.Library(ctx, u.LibraryID)
	if err != nil {
		return userDTO{}, err
	}
	if providers == nil {
		providers = []string{}
	}
	return userDTO{
		ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		IsAdmin: u.IsAdmin, Disabled: u.Disabled, QuotaBytes: u.QuotaBytes, UsedBytes: lib.StorageUsed(),
		CreatedAt: ts(u.CreatedAt), LastSyncAt: tsPtr(u.LastSyncAt),
		HasPassword: u.PasswordHash != "", Providers: providers,
	}, nil
}

func (a *API) meDTO(ctx context.Context, u repo.User, csrf string) (meDTO, error) {
	idents, err := a.Store.Identities().ListByUser(ctx, u.ID)
	if err != nil {
		return meDTO{}, err
	}
	d, err := a.userDTOWith(ctx, u, providerIDs(idents))
	if err != nil {
		return meDTO{}, err
	}
	out := make([]identityDTO, 0, len(idents))
	for _, i := range idents {
		// A provider removed from the config still shows up so it can be
		// unlinked; its name falls back to the id.
		dto := identityDTO{
			Provider: i.Provider, ProviderName: i.Provider, Email: i.Email,
			CreatedAt: ts(i.CreatedAt), LastLoginAt: tsPtr(i.LastLoginAt),
		}
		if p, ok := a.SSO.Get(i.Provider); ok {
			dto.ProviderName, dto.Kind = p.DisplayName(), string(p.Kind())
		}
		out = append(out, dto)
	}
	return meDTO{userDTO: d, CSRFToken: csrf, Identities: out}, nil
}

type keyDTO struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Prefix     string         `json:"prefix"`
	Access     repo.KeyAccess `json:"access"`
	CreatedAt  string         `json:"createdAt"`
	LastUsedAt *string        `json:"lastUsedAt"`
}

func toKeyDTO(k repo.APIKey) keyDTO {
	return keyDTO{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Access: k.Access, CreatedAt: ts(k.CreatedAt), LastUsedAt: tsPtr(k.LastUsedAt)}
}

// page is the list envelope.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

// pageParams reads ?limit and ?cursor. Cursors are the last seen ID.
func pageParams(c *gin.Context) (limit int, cursor int64, err error) {
	limit = defaultLimit
	if v := c.Query("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n < 1 {
			return 0, 0, fieldError("limit", "Must be a positive integer.")
		}
		limit = min(n, maxLimit)
	}
	if v := c.Query("cursor"); v != "" {
		cursor, err = strconv.ParseInt(v, 10, 64)
		if err != nil || cursor < 0 {
			return 0, 0, fieldError("cursor", "Invalid cursor.")
		}
	}
	return limit, cursor, nil
}

// paginate trims the limit+1 rows fetched to detect a next page.
func paginate[T any](rows []T, limit int, id func(T) int64) page[T] {
	p := page[T]{Items: rows}
	if len(rows) > limit {
		p.Items = rows[:limit]
		next := strconv.FormatInt(id(rows[limit-1]), 10)
		p.NextCursor = &next
	}
	if p.Items == nil {
		p.Items = []T{}
	}
	return p
}

// bind decodes a JSON body with a size cap. An empty body leaves dst as is.
func bind(c *gin.Context, dst any) error {
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	err := json.NewDecoder(body).Decode(dst)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return newErr(http.StatusBadRequest, "bad_request", "The request body is not valid JSON.")
	}
	return nil
}

func pathID(c *gin.Context, name string) (int64, error) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, errNotFound
	}
	return id, nil
}

// optInt64 tells an absent field from an explicit null.
type optInt64 struct {
	set bool
	val *int64
}

func (o *optInt64) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		o.val = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.val = &v
	return nil
}

func (o optInt64) opt() accounts.Optional[int64] {
	return accounts.Optional[int64]{Set: o.set, Value: o.val}
}

// renameFields lets shared validators speak the field names of this form.
func renameFields(err error, from, to string) error {
	var ve *accounts.ValidationError
	if errors.As(err, &ve) {
		if m, ok := ve.Fields[from]; ok {
			delete(ve.Fields, from)
			ve.Fields[to] = m
		}
	}
	return err
}

func containsFold(s, q string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(q))
}

func baseURL(c *gin.Context) string { return httpx.BaseURL(c.Request.Context()) }
