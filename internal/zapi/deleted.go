package zapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/zotero"
)

func (a *API) getDeleted(c *gin.Context) {
	l := lib(c)
	since, err := parseSince(c)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	// Entries at or below the floor have been pruned, so a client older than
	// that cannot learn what was deleted and must resync from scratch.
	if since > 0 && since < l.Reader.DeleteLogFloor() {
		Fail(c, http.StatusConflict, "'since' is older than the oldest retained deletion; a full sync is required")
		return
	}
	del, err := l.Reader.Deleted(c.Request.Context(), since)
	if err != nil {
		a.FailErr(c, err)
		return
	}
	c.JSON(http.StatusOK, zotero.DeletedResponse{
		Collections: del[repo.TypeCollection],
		Searches:    del[repo.TypeSearch],
		Items:       del[repo.TypeItem],
		Tags:        del[repo.TypeTag],
		Settings:    del[repo.TypeSetting],
	})
}
