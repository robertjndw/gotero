package zoteroclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/robertjndw/gotero/zotero"
)

// CurrentKey returns what the client's API key is and what it may access. A
// bad key is a 403 (IsForbidden).
func (c *Client) CurrentKey(ctx context.Context) (*zotero.KeyInfo, error) {
	var ki zotero.KeyInfo
	if _, err := c.get(ctx, "keys/current", nil, nil, &ki); err != nil {
		return nil, err
	}
	return &ki, nil
}

// DeleteCurrentKey revokes the client's API key on the server.
func (c *Client) DeleteCurrentKey(ctx context.Context) error {
	_, err := c.exec(ctx, c.newRequest(http.MethodDelete, "keys/current", nil, nil), nil)
	return err
}

// CreateKeyWithPassword is the legacy login: it trades a username and password
// for a new API key with the given access. Prefer the browser based login
// session where the server offers it, this sends the password to the API.
// Build the client with an empty API key for this call.
func (c *Client) CreateKeyWithPassword(ctx context.Context, username, password, name string, access zotero.Access) (*zotero.KeyInfo, error) {
	// The legacy login API takes the password in the body.
	body, err := json.Marshal(struct { //nolint:gosec
		Username string        `json:"username"`
		Password string        `json:"password"`
		Name     string        `json:"name"`
		Access   zotero.Access `json:"access"`
	}{username, password, name, access})
	if err != nil {
		return nil, err
	}
	var ki zotero.KeyInfo
	if _, err := c.sendJSON(ctx, http.MethodPost, "keys", nil, nil, body, &ki); err != nil {
		return nil, err
	}
	return &ki, nil
}

// LoginSession is a started browser login. Send the user to LoginURL and poll
// GetLoginSession with Token.
type LoginSession struct {
	Token    string `json:"sessionToken"`
	LoginURL string `json:"loginURL"`
}

// LoginSessionStatus is the state of a login session.
type LoginSessionStatus struct {
	// Status is "pending", "completed" or "cancelled".
	Status string `json:"status"`
	// APIKey and the fields after it are set once Status is "completed".
	APIKey      string `json:"apiKey,omitempty"`
	UserID      int64  `json:"userID,omitempty"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// CreateLoginSession starts a browser login. userID, if not nil, restricts the
// session to that account.
func (c *Client) CreateLoginSession(ctx context.Context, userID *int64) (*LoginSession, error) {
	body := []byte("{}")
	if userID != nil {
		body = []byte(`{"userID":` + strconv.FormatInt(*userID, 10) + `}`)
	}
	var s LoginSession
	if _, err := c.sendJSON(ctx, http.MethodPost, "keys/sessions", nil, nil, body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetLoginSession polls a login session. An unknown token is a 404 and an
// expired session a 410.
func (c *Client) GetLoginSession(ctx context.Context, token string) (*LoginSessionStatus, error) {
	var s LoginSessionStatus
	if _, err := c.get(ctx, "keys/sessions/"+url.PathEscape(token), nil, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// CancelLoginSession cancels a pending login session.
func (c *Client) CancelLoginSession(ctx context.Context, token string) error {
	_, err := c.exec(ctx, c.newRequest(http.MethodDelete, "keys/sessions/"+url.PathEscape(token), nil, nil), nil)
	return err
}

// UserGroupVersions lists the groups a user belongs to as group ID -> group
// version, which is how a sync notices new, changed and removed groups.
func (c *Client) UserGroupVersions(ctx context.Context, userID int64) (map[int64]int64, error) {
	var raw map[string]int64
	rel := "users/" + strconv.FormatInt(userID, 10) + "/groups"
	if _, err := c.get(ctx, rel, url.Values{"format": {"versions"}}, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(raw))
	for k, v := range raw {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("zoteroclient: bad group ID %q in group versions", k)
		}
		out[id] = v
	}
	return out, nil
}

// GetGroup fetches a group's metadata and membership.
func (c *Client) GetGroup(ctx context.Context, id int64) (*zotero.Group, error) {
	var g zotero.Group
	if _, err := c.get(ctx, "groups/"+strconv.FormatInt(id, 10), nil, nil, &g); err != nil {
		return nil, err
	}
	return &g, nil
}
