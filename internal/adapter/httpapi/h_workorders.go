package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/workorders"
)

// WorkOrdersAPI serves the work order operations of API contract §4.10a (M6-C): ListWorkOrders (also
// as text/csv), CreateWorkOrder, GetWorkOrder, UpdateWorkOrder, CancelWorkOrder, ListWorkOrderRepos,
// UpdateWorkOrderRepo and ListTeamWorkOrders.
type WorkOrdersAPI struct {
	Svc   *workorders.Service
	Pages PageTokens
}

// BindWorkOrders binds the work order handlers as the workorders area.
func BindWorkOrders(a *WorkOrdersAPI) { w2SetArea("workorders", a.Register) }

// Register binds the work order operations.
func (a *WorkOrdersAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListWorkOrders": a.list, "CreateWorkOrder": a.create, "GetWorkOrder": a.get, "UpdateWorkOrder": a.update,
		"CancelWorkOrder": a.cancel, "ListWorkOrderRepos": a.listRepos, "UpdateWorkOrderRepo": a.updateRepo,
		"ListTeamWorkOrders": a.listTeam,
	})
}

func workOrderListBody(w domain.WorkOrder) workOrderBody { return toWorkOrderBody(w, false) }

func (a *WorkOrdersAPI) list(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := workorders.Filter{Status: q.Get("status"), Severity: q.Get("severity"), SLAState: q.Get("slaState"), TeamID: q.Get("teamId")}
	if f.TeamID != "" && !domain.ValidTeamID(f.TeamID) {
		WriteError(w, r, bad("teamId", "must be a team id (team_…)"))
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, next, err := a.Svc.List(r.Context(), c, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	token := w2Next(a.Pages, r, c, next)
	if WantsCSV(r) {
		cells := make([][]string, 0, len(rows))
		for _, x := range rows {
			cells = append(cells, []string{x.ID, x.CVEID, string(x.Severity), x.Title, string(x.Status), string(x.SLAState), x.TeamID, w2Time(x.SLADueAt)})
		}
		if err := WriteCSV(w, []string{"id", "cveId", "severity", "title", "status", "slaState", "teamId", "slaDueAt"}, cells, token); err != nil {
			WriteError(w, r, err)
		}
		return
	}
	out := w2ListBody[workOrderBody]{Items: make([]workOrderBody, 0, len(rows)), NextToken: token}
	for _, x := range rows {
		out.Items = append(out.Items, workOrderListBody(x))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *WorkOrdersAPI) listTeam(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.WorkOrder, string, error) {
		return a.Svc.ListForTeam(r.Context(), c, r.PathValue("teamId"), p)
	}, workOrderListBody)
}

func (a *WorkOrdersAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	x, err := a.Svc.Get(r.Context(), c, r.PathValue("workOrderId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, x.Version)
	WriteJSON(w, http.StatusOK, toWorkOrderBody(x, true))
}

func (a *WorkOrdersAPI) create(w http.ResponseWriter, r *http.Request) {
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
	var req createWorkOrderRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	var sla time.Time
	if req.SLADueAt != "" {
		if sla, err = w2ParseTime("slaDueAt", req.SLADueAt); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	idem, err := w2Idem(r, "CreateWorkOrder", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toWorkOrderBody(v.(domain.WorkOrder), true))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, domain.NewWorkOrderParams{Kind: req.Kind, CVEID: req.CVEID, Severity: req.Severity, Title: req.Title,
		SLADueAt: sla, TemplateID: req.TemplateID, BudgetUSD: req.BudgetUSD}, toRepoInputs(req.Repos), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.WorkOrder.Version)
	w.Header().Set("Location", "/v1/work-orders/"+res.WorkOrder.ID)
	WriteJSON(w, http.StatusCreated, toWorkOrderBody(res.WorkOrder, true))
}

func (a *WorkOrdersAPI) update(w http.ResponseWriter, r *http.Request) {
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
	var req updateWorkOrderRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	patch, err := req.patch()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	x, err := a.Svc.Update(r.Context(), c, r.PathValue("workOrderId"), expect, patch, toRepoInputs(req.AddRepos))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, x.Version)
	WriteJSON(w, http.StatusOK, toWorkOrderBody(x, false))
}

func (a *WorkOrdersAPI) cancel(w http.ResponseWriter, r *http.Request) {
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
		Reason string `json:"reason"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CancelWorkOrder", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toWorkOrderBody(v.(domain.WorkOrder), false))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Cancel(r.Context(), c, r.PathValue("workOrderId"), req.Reason, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.WorkOrder.Version)
	WriteJSON(w, http.StatusOK, toWorkOrderBody(res.WorkOrder, false))
}

func (a *WorkOrdersAPI) listRepos(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.WorkOrderRepo, string, error) {
		return a.Svc.ListRepos(r.Context(), c, r.PathValue("workOrderId"), p)
	}, toWorkOrderRepoBody)
}

func (a *WorkOrdersAPI) updateRepo(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// A workload principal may omit If-Match (CLI-CD-1): expect 0 means the current version, and only the
	// use case honours it, for a workload.
	var expect domain.Version
	if r.Header.Get("If-Match") != "" || c.Principal.Workload == nil {
		if expect, err = w2IfMatch(r); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	var req updateWorkOrderRepoRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	repo, err := a.Svc.UpdateRepo(r.Context(), c, r.PathValue("workOrderId"), r.PathValue("host"), r.PathValue("owner")+"/"+r.PathValue("repo"), expect,
		domain.RepoReport{Status: req.Status, PRURL: req.PRURL, PRNumber: req.PRNumber, Detail: req.Detail})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, repo.Version)
	WriteJSON(w, http.StatusOK, toWorkOrderRepoBody(repo))
}
