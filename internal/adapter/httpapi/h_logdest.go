package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/logdest"
)

// logDestAreaName is the area bound by BindLogDestinations. It has its own area, added here, because
// the "delivery" area is shared with the notification channels of M3-G.
const logDestAreaName = "logdest"

func init() { Areas = append(Areas, Area{logDestAreaName, none}) }

// LogDestinationsAPI serves the five log destination operations (M2-I1, RC-18).
type LogDestinationsAPI struct {
	Svc   *logdest.Service
	Pages PageTokens
}

// BindLogDestinations binds the log destination handlers as the logdest area.
func BindLogDestinations(a *LogDestinationsAPI) { w2SetArea(logDestAreaName, a.Register) }

// Register binds the log destination operations.
func (a *LogDestinationsAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{
		"ListLogDestinations": a.list, "CreateLogDestination": a.create, "GetLogDestination": a.get,
		"UpdateLogDestination": a.update, "DeleteLogDestination": a.del,
	} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *LogDestinationsAPI) list(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[logDestinationBody]{Items: make([]logDestinationBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, d := range rows {
		out.Items = append(out.Items, toLogDestinationBody(d))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *LogDestinationsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, d.Version)
	WriteJSON(w, http.StatusOK, toLogDestinationBody(d))
}

func (a *LogDestinationsAPI) create(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := refuseInlineCredential(raw); err != nil {
		WriteError(w, r, err)
		return
	}
	var req createLogDestinationRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	idem, err := w2Idem(r, "CreateLogDestination", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toLogDestinationBody(v.(domain.LogDestination)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, teamID, domain.NewLogDestinationParams{Name: req.Name,
		Kind: domain.LogDestinationKind(req.Kind), Config: req.Config, SecretID: req.SecretID, Enabled: enabled}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Destination.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/log-destinations/"+res.Destination.ID)
	WriteJSON(w, http.StatusCreated, toLogDestinationBody(res.Destination))
}

func (a *LogDestinationsAPI) update(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := refuseInlineCredential(raw); err != nil {
		WriteError(w, r, err)
		return
	}
	var req updateLogDestinationRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := domain.LogDestinationPatch{Name: req.Name, Config: req.Config, Enabled: req.Enabled}
	if req.SecretID.Set {
		if req.SecretID.Null {
			p.ClearSecret = true
		} else {
			p.SecretID = &req.SecretID.Value
		}
	}
	d, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("id"), p, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, d.Version)
	WriteJSON(w, http.StatusOK, toLogDestinationBody(d))
}

func (a *LogDestinationsAPI) del(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("id"), expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, d.Version)
	WriteJSON(w, http.StatusAccepted, toLogDestinationBody(d))
}
