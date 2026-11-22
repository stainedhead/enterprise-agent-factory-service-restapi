package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/exports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
)

// ExportsAPI serves CreateExport, GetExport, ListExports and CreateImport (M4-F, RA-12). The routes
// require admin:export, which only service admins hold; the middleware answers 403 for everyone else.
type ExportsAPI struct {
	Exports *exports.Service
	Imports *imports.Service
	Pages   PageTokens
}

// BindExports binds the export and import handlers as the exports area.
func BindExports(a *ExportsAPI) { w2SetArea("exports", a.Register) }

// Register binds the export and import operations.
func (a *ExportsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"CreateExport": a.create, "GetExport": a.get, "ListExports": a.list, "CreateImport": a.importBundle})
}

type exportBody struct {
	ID                string   `json:"id"`
	Status            string   `json:"status"`
	SchemaVersion     int      `json:"schemaVersion"`
	TeamIDs           []string `json:"teamIds"`
	SHA256            *string  `json:"sha256"`
	SizeBytes         *int64   `json:"sizeBytes"`
	DownloadURL       *string  `json:"downloadUrl"`
	DownloadExpiresAt *string  `json:"downloadExpiresAt"`
	Error             *string  `json:"error"`
	CreatedAt         string   `json:"createdAt"`
	CreatedBy         string   `json:"createdBy"`
	FinishedAt        *string  `json:"finishedAt"`
}

func toExportBody(e domain.Export) exportBody {
	ids := e.TeamIDs
	if ids == nil {
		ids = []string{}
	}
	return exportBody{ID: e.ID, Status: string(e.Status), SchemaVersion: e.SchemaVersion, TeamIDs: ids, SHA256: e.SHA256, SizeBytes: e.SizeBytes,
		DownloadURL: e.DownloadURL, DownloadExpiresAt: w2TimePtr(e.DownloadExpiresAt), Error: e.Error, CreatedAt: w2Time(e.CreatedAt),
		CreatedBy: e.CreatedBy, FinishedAt: w2TimePtr(e.FinishedAt)}
}

func (a *ExportsAPI) create(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		TeamIDs []string `json:"teamIds"`
	}
	if len(raw) > 0 {
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	idem, err := w2Idem(r, "CreateExport", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusAccepted, toExportBody(v.(domain.Export)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Exports.Create(r.Context(), c, req.TeamIDs, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w.Header().Set("Location", "/v1/exports/"+res.Export.ID)
	WriteJSON(w, http.StatusAccepted, toExportBody(res.Export))
}

func (a *ExportsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	e, err := a.Exports.Get(r.Context(), c, r.PathValue("exportId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toExportBody(e))
}

func (a *ExportsAPI) list(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, next, err := a.Exports.List(r.Context(), c, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[exportBody]{Items: make([]exportBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, e := range rows {
		out.Items = append(out.Items, toExportBody(e))
	}
	WriteJSON(w, http.StatusOK, out)
}

type importBody struct {
	ID     string           `json:"id"`
	DryRun bool             `json:"dryRun"`
	Teams  []importTeamBody `json:"teams"`
}

type importTeamBody struct {
	Slug    string   `json:"slug"`
	Result  string   `json:"result"`
	Changes []string `json:"changes"`
	Error   *string  `json:"error"`
}

func toImportBody(r imports.Report) importBody {
	out := importBody{ID: r.ID, DryRun: r.DryRun, Teams: make([]importTeamBody, 0, len(r.Teams))}
	for _, t := range r.Teams {
		out.Teams = append(out.Teams, importTeamBody{Slug: t.Slug, Result: t.Result, Changes: t.Changes, Error: t.Error})
	}
	return out
}

// importBundle serves CreateImport: dryRun defaults to true; the request body (up to 100 MB, enforced by
// the route's body limit) carries the bundle and the account map.
func (a *ExportsAPI) importBundle(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	dry := true
	if v := r.URL.Query().Get("dryRun"); v != "" {
		if dry, err = strconv.ParseBool(v); err != nil {
			WriteError(w, r, bad("dryRun", "must be true or false"))
			return
		}
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Bundle     json.RawMessage   `json:"bundle"`
		AccountMap map[string]string `json:"accountMap"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if len(req.Bundle) == 0 || string(req.Bundle) == "null" {
		WriteError(w, r, bad("bundle", "is required"))
		return
	}
	idem, err := w2Idem(r, "CreateImport", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toImportBody(v.(imports.Report)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Imports.Run(r.Context(), c, imports.Input{Bundle: req.Bundle, AccountMap: req.AccountMap, DryRun: dry}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusOK, toImportBody(res.Report))
}
