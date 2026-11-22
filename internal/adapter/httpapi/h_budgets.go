package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/budgets"
)

// BudgetsAPI serves GetBudget and PutBudget (M3-E, RA-14). Route permissions are enforced by the
// authorization middleware; the use case checks them again and adds the field permission of
// onExhaust=notify (teams:set-guardrails).
type BudgetsAPI struct {
	Svc *budgets.Service
}

// BindBudgets binds the budget handlers as the budgets area.
func BindBudgets(a *BudgetsAPI) Registrar { return a.Register }

// Register binds the budget operations.
func (a *BudgetsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"GetBudget": a.get, "PutBudget": a.put})
}

func (a *BudgetsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	pos, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, pos.Version)
	WriteJSON(w, http.StatusOK, toBudgetBody(pos))
}

// put creates or replaces the budget: 200 either way (the contract has no 201 for it); If-Match is
// optional (CD-2).
func (a *BudgetsAPI) put(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req putBudgetRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := req.params()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	pos, err := a.Svc.Put(r.Context(), c, r.PathValue("teamId"), p, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, pos.Version)
	WriteJSON(w, http.StatusOK, toBudgetBody(pos))
}
