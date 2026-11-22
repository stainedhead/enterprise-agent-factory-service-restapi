package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs"
)

// CostsAPI serves ListCosts and ListCostReconciliations (M4-C5, RA-1). ListCosts answers text/csv as
// well, with the same rows and the next page's token in X-Afs-Next-Token.
type CostsAPI struct {
	Svc   *costs.Service
	Pages PageTokens
}

// BindCosts binds the cost handlers as the costs area.
func BindCosts(a *CostsAPI) Registrar { return a.Register }

// Register binds the cost operations.
func (a *CostsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"ListCosts": a.listCosts, "ListCostReconciliations": a.listReconciliations})
}

func (a *CostsAPI) listCosts(w http.ResponseWriter, r *http.Request) {
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
	q := r.URL.Query()
	rows, next, err := a.Svc.ListCosts(r.Context(), c, r.PathValue("teamId"), q.Get("from"), q.Get("to"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	token := w2Next(a.Pages, r, c, next)
	if WantsCSV(r) {
		cells := make([][]string, 0, len(rows))
		for _, row := range rows {
			cells = append(cells, costCSVRow(row))
		}
		if err := WriteCSV(w, costCSVHeader, cells, token); err != nil {
			WriteError(w, r, err)
		}
		return
	}
	out := w2ListBody[costRowBody]{Items: make([]costRowBody, 0, len(rows)), NextToken: token}
	for _, row := range rows {
		out.Items = append(out.Items, toCostRowBody(row))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *CostsAPI) listReconciliations(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]costs.Reconciliation, string, error) {
		return a.Svc.ListReconciliations(r.Context(), c, status, p)
	}, toReconciliationBody)
}
