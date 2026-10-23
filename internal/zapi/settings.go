package zapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

// maxSettingsPerWrite is the most settings one request may carry. Zotero
// uploads all changed settings in one request, so it needs more room than
// the per-object limit; 270 is what zotero.org allows.
const maxSettingsPerWrite = 270

var settingKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,100}$`)

type setting struct {
	Value   json.RawMessage `json:"value"`
	Version int64           `json:"version"`
}

// settingIn is a setting as uploaded. The version stays a pointer because an
// explicit 0 on an existing setting is a stale write, not "no version".
type settingIn struct {
	Value   json.RawMessage `json:"value"`
	Version *int64          `json:"version"`
}

func (a *API) getSettings(c *gin.Context) {
	l := lib(c)
	since, err := parseSince(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	list, err := l.Reader.Settings(c.Request.Context(), since)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	out := make(map[string]setting, len(list))
	for _, s := range list {
		out[s.Key] = setting{Value: s.Value, Version: s.Version}
	}
	c.JSON(http.StatusOK, out)
}

func (a *API) getSetting(c *gin.Context) {
	l := lib(c)
	list, err := l.Reader.Settings(c.Request.Context(), 0)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	for _, s := range list {
		if s.Key == c.Param("key") {
			c.JSON(http.StatusOK, setting{Value: s.Value, Version: s.Version})
			return
		}
	}
	Fail(c, http.StatusNotFound, "Setting not found")
}

func (a *API) postSettings(c *gin.Context) {
	l, ok := a.requireWrite(c)
	if !ok {
		return
	}
	b, err := readObjectBody(c, maxSettingsPerWrite)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	var in map[string]settingIn
	if err := json.Unmarshal(b, &in); err != nil || in == nil {
		Fail(c, http.StatusBadRequest, "Uploaded data must be a JSON object")
		return
	}
	a.writeSettings(c, l, in)
}

func (a *API) putSetting(c *gin.Context) {
	l, ok := a.requireWrite(c)
	if !ok {
		return
	}
	b, err := readObjectBody(c, 8)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	var in settingIn
	if err := json.Unmarshal(b, &in); err != nil {
		Fail(c, http.StatusBadRequest, "Uploaded data must be a JSON object")
		return
	}
	a.writeSettings(c, l, map[string]settingIn{c.Param("key"): in})
}

// writeSettings applies a batch of settings. Unchanged values keep their
// version and do not move the library, so clients that re-upload settings on
// every sync stay quiet.
func (a *API) writeSettings(c *gin.Context, l *LibraryContext, in map[string]settingIn) {
	if len(in) > maxSettingsPerWrite {
		Fail(c, http.StatusRequestEntityTooLarge, fmt.Sprintf("Too many settings (max %d)", maxSettingsPerWrite))
		return
	}
	for k, s := range in {
		if !settingKeyRe.MatchString(k) {
			Fail(c, http.StatusBadRequest, fmt.Sprintf("Invalid setting key '%s'", k))
			return
		}
		if len(s.Value) == 0 {
			Fail(c, http.StatusBadRequest, fmt.Sprintf("'value' not provided for setting '%s'", k))
			return
		}
		if len(s.Value) > maxSettingValueBytes {
			Fail(c, http.StatusRequestEntityTooLarge, fmt.Sprintf("Value of setting '%s' is too large", k))
			return
		}
	}
	pre, err := parsePrecondition(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		if err := checkLibraryVersion(tx, pre); err != nil {
			return err
		}
		stored, err := tx.Settings(ctx, 0)
		if err != nil {
			return err
		}
		byKey := make(map[string]repo.Setting, len(stored))
		for _, s := range stored {
			byKey[s.Key] = s
		}
		var changed []repo.Setting
		for k, s := range in {
			cur, exists := byKey[k]
			same := exists && zotero.EqualJSON(cur.Value, s.Value)
			if exists && s.Version != nil && versionConflict(cur.Version, *s.Version, same) {
				return &httpError{
					status: http.StatusPreconditionFailed, version: tx.Version(),
					msg: fmt.Sprintf("Setting '%s' has been modified since specified version (expected %d, found %d)",
						k, *s.Version, cur.Version),
				}
			}
			if same {
				continue
			}
			changed = append(changed, repo.Setting{Key: k, Value: s.Value})
		}
		if len(changed) == 0 {
			return nil
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		keys := make([]string, len(changed))
		for i := range changed {
			changed[i].Version = ver
			keys[i] = changed[i].Key
		}
		if err := tx.PutSettings(ctx, changed); err != nil {
			return err
		}
		return tx.ClearDeleted(ctx, repo.TypeSetting, keys)
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.Status(http.StatusNoContent)
}

func (a *API) deleteSettings(c *gin.Context) {
	var keys []string
	for _, k := range strings.Split(c.Query("settingKey"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	a.removeSettings(c, keys, false)
}

func (a *API) deleteSetting(c *gin.Context) {
	a.removeSettings(c, []string{c.Param("key")}, true)
}

func (a *API) removeSettings(c *gin.Context, keys []string, mustExist bool) {
	l, ok := a.requireWrite(c)
	if !ok {
		return
	}
	pre, err := a.preconditionRequired(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	if len(keys) == 0 {
		Fail(c, http.StatusBadRequest, "settingKey not provided")
		return
	}
	if len(keys) > maxObjectsPerWrite {
		Fail(c, http.StatusRequestEntityTooLarge, fmt.Sprintf("Too many keys (max %d)", maxObjectsPerWrite))
		return
	}
	ctx := c.Request.Context()
	var before int64
	v, err := a.store.InLibrary(ctx, l.LibraryID, func(tx repo.LibraryTx) error {
		before = tx.Version()
		if err := checkLibraryVersion(tx, pre); err != nil {
			return err
		}
		stored, err := tx.Settings(ctx, 0)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, s := range stored {
			have[s.Key] = true
		}
		var present []string
		for _, k := range keys {
			if have[k] {
				present = append(present, k)
			}
		}
		if len(present) == 0 {
			if mustExist {
				return &httpError{status: http.StatusNotFound, version: tx.Version(), msg: "Setting not found"}
			}
			return nil
		}
		ver, err := tx.NextVersion(ctx)
		if err != nil {
			return err
		}
		_, err = tx.DeleteSettings(ctx, present, ver)
		return err
	})
	if err != nil {
		a.FailErr(c, err)
		return
	}
	a.finishWrite(c, l, before, v)
	c.Status(http.StatusNoContent)
}
