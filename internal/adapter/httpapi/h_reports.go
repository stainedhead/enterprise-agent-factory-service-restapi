package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

// ReportsAPI serves GetUsage, GetTeamReport, GetAgentReport and GetServiceReport (M3-H, RA-6). Each
// also answers Accept: text/csv with the same rows, the next page's token in X-Afs-Next-Token.
type ReportsAPI struct {
	Svc   *reports.Service
	Pages PageTokens
}

// BindReports binds the report handlers as the reports area.
func BindReports(a *ReportsAPI) { w2SetArea("reports", a.Register) }

// Register binds the report operations.
func (a *ReportsAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{
		"GetUsage": a.usage, "GetTeamReport": a.team, "GetAgentReport": a.agent, "GetServiceReport": a.service,
	} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *ReportsAPI) usage(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, true, func(c reportCall) (domain.Report, error) {
		return a.Svc.Usage(r.Context(), c.caller, r.PathValue("teamId"), c.raw, c.page)
	})
}

func (a *ReportsAPI) team(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, false, func(c reportCall) (domain.Report, error) {
		kind, err := domain.ParseReportKind(r.PathValue("kind"))
		if err != nil {
			return domain.Report{}, err
		}
		return a.Svc.TeamReport(r.Context(), c.caller, r.PathValue("teamId"), kind, c.raw, c.page)
	})
}

func (a *ReportsAPI) agent(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, false, func(c reportCall) (domain.Report, error) {
		kind, err := domain.ParseReportKind(r.PathValue("kind"))
		if err != nil {
			return domain.Report{}, err
		}
		return a.Svc.AgentReport(r.Context(), c.caller, r.PathValue("teamId"), r.PathValue("agentId"), kind, c.raw, c.page)
	})
}

func (a *ReportsAPI) service(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, false, func(c reportCall) (domain.Report, error) {
		kind, err := domain.ParseReportKind(r.PathValue("kind"))
		if err != nil {
			return domain.Report{}, err
		}
		return a.Svc.ServiceReport(r.Context(), c.caller, kind, c.raw, c.page)
	})
}

type reportCall struct {
	caller acting.Caller
	raw    domain.RawReportQuery
	page   acting.Page
}

// serve parses the request, runs fn and writes JSON or CSV. asUsage selects the Usage wire shape.
func (a *ReportsAPI) serve(w http.ResponseWriter, r *http.Request, asUsage bool, fn func(reportCall) (domain.Report, error)) {
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
	rep, err := fn(reportCall{caller: c, raw: rawReportQuery(r), page: page})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	next := w2Next(a.Pages, r, c, rep.NextCursor)
	if WantsCSV(r) {
		header, cells := reportCells(rep)
		if err := WriteCSV(w, header, cells, next); err != nil {
			WriteError(w, r, err)
		}
		return
	}
	if asUsage {
		WriteJSON(w, http.StatusOK, toUsageBody(rep, next))
		return
	}
	WriteJSON(w, http.StatusOK, toReportBody(rep, next))
}
