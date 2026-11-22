package httpapi

import (
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs"
)

// costRowBody is the CostRow of API contract §5. agentId, poolId and modelId are null when the row has
// none; calls is 0 for CUR rows.
type costRowBody struct {
	Day     string       `json:"day"`
	Source  string       `json:"source"`
	AgentID *string      `json:"agentId"`
	PoolID  *string      `json:"poolId"`
	ModelID *string      `json:"modelId"`
	CostUsd domain.Money `json:"costUsd"`
	Calls   int64        `json:"calls"`
}

func toCostRowBody(r costs.Row) costRowBody {
	return costRowBody{Day: r.Day.UTC().Format("2006-01-02"), Source: r.Source, AgentID: r.AgentID, PoolID: r.PoolID,
		ModelID: r.ModelID, CostUsd: r.CostUSD, Calls: r.Calls}
}

// costCSVHeader is the header of the CSV answer of ListCosts.
var costCSVHeader = []string{"day", "source", "agentId", "poolId", "modelId", "costUsd", "calls"}

func costCSVRow(r costs.Row) []string {
	opt := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return []string{r.Day.UTC().Format("2006-01-02"), r.Source, opt(r.AgentID), opt(r.PoolID), opt(r.ModelID),
		r.CostUSD.String(), strconv.FormatInt(r.Calls, 10)}
}

// reconciliationBody is the CostReconciliation of API contract §5. variancePct has at most two decimals
// (the schema's pattern) and reads "0.00" when nothing was billed, where a percentage is undefined: the
// status missing_billing says so.
type reconciliationBody struct {
	TeamID       string       `json:"teamId"`
	Day          string       `json:"day"`
	GatewayUsd   domain.Money `json:"gatewayUsd"`
	BilledUsd    domain.Money `json:"billedUsd"`
	VariancePct  string       `json:"variancePct"`
	Status       string       `json:"status"`
	ReconciledAt string       `json:"reconciledAt"`
}

func toReconciliationBody(r costs.Reconciliation) reconciliationBody {
	return reconciliationBody{TeamID: r.TeamID, Day: r.Day.UTC().Format("2006-01-02"), GatewayUsd: r.GatewayUSD, BilledUsd: r.BilledUSD,
		VariancePct: variancePct(r.VariancePct), Status: r.Status, ReconciledAt: w2Time(r.ReconciledAt)}
}

func variancePct(s *string) string {
	if s == nil {
		return "0.00"
	}
	f, err := strconv.ParseFloat(*s, 64)
	if err != nil {
		return "0.00"
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}
