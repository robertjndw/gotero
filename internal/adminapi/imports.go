package adminapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/robertjndw/gotero/internal/repo"
)

// ImportProgress is the shape the importer worker writes into
// import_jobs.progress. All fields are optional so a queued job with an
// empty object renders as zeros.
type ImportProgress struct {
	Libraries []ImportLibrary `json:"libraries"`
	Objects   int64           `json:"objects"`
	Total     int64           `json:"objectsTotal"`
	Files     int64           `json:"files"`
	FilesAll  int64           `json:"filesTotal"`
	Bytes     int64           `json:"bytes"`
	// FileAccess records whether the key may download files, as found when the
	// job was queued. The worker keeps it when it rewrites progress.
	FileAccess *bool `json:"fileAccess,omitempty"`
}

type ImportLibrary struct {
	Name  string `json:"name"`
	Items int64  `json:"items"`
	Files int64  `json:"files"`
	Done  bool   `json:"done"`
	// Conflicts counts objects a re-import left alone because they were
	// changed in gotero since the previous import.
	Conflicts int64 `json:"conflicts"`
	// Error is set when the importer skipped the library or could not finish it.
	Error string `json:"error,omitempty"`
}

type importJobDTO struct {
	ID        int64           `json:"id"`
	Status    string          `json:"status"`
	Source    string          `json:"source"`
	Libraries []ImportLibrary `json:"libraries"`
	Progress  struct {
		Objects      int64 `json:"objects"`
		ObjectsTotal int64 `json:"objectsTotal"`
		Files        int64 `json:"files"`
		FilesTotal   int64 `json:"filesTotal"`
		Bytes        int64 `json:"bytes"`
	} `json:"progress"`
	Error      *string `json:"error"`
	CreatedAt  string  `json:"createdAt"`
	FinishedAt *string `json:"finishedAt"`
}

func toImportDTO(j repo.ImportJob) importJobDTO {
	var p ImportProgress
	// A malformed progress blob must not hide the job itself.
	if len(j.Progress) > 0 {
		_ = json.Unmarshal(j.Progress, &p)
	}
	d := importJobDTO{
		ID: j.ID, Status: j.Status, Source: "zotero.org", Libraries: p.Libraries,
		CreatedAt: ts(j.CreatedAt), FinishedAt: tsPtr(j.FinishedAt),
	}
	if d.Libraries == nil {
		d.Libraries = []ImportLibrary{}
	}
	d.Progress.Objects, d.Progress.ObjectsTotal = p.Objects, p.Total
	d.Progress.Files, d.Progress.FilesTotal, d.Progress.Bytes = p.Files, p.FilesAll, p.Bytes
	if j.Error != "" {
		d.Error = &j.Error
	}
	return d
}

func (a *API) listImports(c *gin.Context) error {
	jobs, err := a.Store.Imports().ListByUser(c.Request.Context(), currentUser(c).ID)
	if err != nil {
		return err
	}
	items := make([]importJobDTO, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, toImportDTO(j))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

// zoteroKeyRe is deliberately loose: zotero.org's key format has changed
// before, and the real check is the first request the importer makes.
var zoteroKeyRe = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)

func (a *API) createImport(c *gin.Context) error {
	var in struct {
		APIKey        string `json:"apiKey"`
		IncludeGroups bool   `json:"includeGroups"`
		IncludeFiles  bool   `json:"includeFiles"`
	}
	if err := bind(c, &in); err != nil {
		return err
	}
	key := strings.TrimSpace(in.APIKey)
	if key == "" {
		return fieldError("apiKey", "Enter your zotero.org API key.")
	}
	if !zoteroKeyRe.MatchString(key) {
		return newErr(http.StatusUnprocessableEntity, "invalid_key", "That does not look like a zotero.org API key.").
			withFields(map[string]string{"apiKey": "That does not look like a zotero.org API key."})
	}
	var progress json.RawMessage
	if a.ImportValidator != nil {
		check, err := a.ImportValidator.ValidateImportKey(c.Request.Context(), key)
		if err != nil {
			a.Log.WarnContext(c.Request.Context(), "adminapi: zotero.org key check failed", "err", err)
			return newErr(http.StatusBadGateway, "upstream_error", "Could not reach zotero.org. Try again in a moment.")
		}
		if !check.Valid {
			return invalidImportKey("zotero.org rejected that API key.")
		}
		if !check.Library {
			return invalidImportKey("That API key cannot read your library. Allow library access when you create it.")
		}
		progress, _ = json.Marshal(ImportProgress{Libraries: []ImportLibrary{}, FileAccess: &check.Files})
	}
	job, err := a.Store.Imports().Create(c.Request.Context(), repo.ImportJob{
		UserID: currentUser(c).ID, Status: "queued", SourceAPIKey: key,
		IncludeGroups: in.IncludeGroups, IncludeFiles: in.IncludeFiles, Progress: progress,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusCreated, toImportDTO(job))
	return nil
}

func invalidImportKey(msg string) error {
	return newErr(http.StatusUnprocessableEntity, "invalid_key", msg).withFields(map[string]string{"apiKey": msg})
}

// ownJob hides other users' jobs behind a 404.
func (a *API) ownJob(c *gin.Context) (repo.ImportJob, error) {
	id, err := pathID(c, "id")
	if err != nil {
		return repo.ImportJob{}, newErr(http.StatusNotFound, "not_found", "Import not found.")
	}
	j, err := a.Store.Imports().Get(c.Request.Context(), id)
	if err != nil {
		return repo.ImportJob{}, notFoundAs(err, "Import not found.")
	}
	if j.UserID != currentUser(c).ID {
		return repo.ImportJob{}, newErr(http.StatusNotFound, "not_found", "Import not found.")
	}
	return j, nil
}

func (a *API) getImport(c *gin.Context) error {
	j, err := a.ownJob(c)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toImportDTO(j))
	return nil
}

func (a *API) cancelImport(c *gin.Context) error {
	j, err := a.ownJob(c)
	if err != nil {
		return err
	}
	if j.Status == "queued" || j.Status == "running" {
		now := a.Now()
		j.Status, j.FinishedAt = "cancelled", &now
		// The zotero.org key is only needed while the job runs.
		j.SourceAPIKey = ""
		if err := a.Store.Imports().Update(c.Request.Context(), j); err != nil {
			return err
		}
	}
	c.JSON(http.StatusOK, toImportDTO(j))
	return nil
}
