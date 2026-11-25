package billingpull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

// azureAPIVersion is the Cost Management query API version.
const azureAPIVersion = "2023-11-01"

// Azure pulls Azure Cost Management (POST <scope>/providers/Microsoft.CostManagement/query) for the route's
// scope (a subscription or resource group holding the teams' Azure OpenAI or Foundry resources), the day's
// actual cost grouped by resource group and the afs-team-id tag. Token is an Azure Resource Manager bearer
// token (live federation is deferred: see implementation notes).
type Azure struct {
	// BaseURL defaults to https://management.azure.com.
	BaseURL string
	Client  Doer
	Token   Credential
	// Scopes maps a route slug to its Cost Management scope (/subscriptions/<id>[/resourceGroups/<name>]).
	Scopes map[string]string
}

// Provider implements routebill.Puller.
func (a *Azure) Provider() string { return "azure" }

type azureQuery struct {
	Properties struct {
		NextLink *string `json:"nextLink"`
		Columns  []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]any `json:"rows"`
	} `json:"properties"`
}

// Pull implements routebill.Puller.
func (a *Azure) Pull(ctx context.Context, ep routebill.Endpoint, day time.Time) ([]routebill.Figure, domain.Money, error) {
	scope, ok := a.Scopes[ep.Slug]
	if !ok || !strings.HasPrefix(scope, "/subscriptions/") {
		return nil, domain.Money{}, fmt.Errorf("billingpull: azure: no Cost Management scope for route %s", ep.Slug)
	}
	token, err := a.Token(ctx, ep)
	if err != nil {
		return nil, domain.Money{}, err
	}
	base := a.BaseURL
	if base == "" {
		base = "https://management.azure.com"
	}
	start := day.UTC().Truncate(24 * time.Hour)
	body, _ := json.Marshal(map[string]any{ // plain maps always marshal
		"type": "ActualCost", "timeframe": "Custom",
		"timePeriod": map[string]string{"from": start.Format(time.RFC3339), "to": start.Add(24*time.Hour - time.Second).Format(time.RFC3339)},
		"dataset": map[string]any{
			"granularity": "None",
			"aggregation": map[string]any{"totalCost": map[string]string{"name": "CostUSD", "function": "Sum"}},
			"grouping":    []map[string]string{{"type": "Dimension", "name": "ResourceGroupName"}, {"type": "TagKey", "name": TeamTag}},
		},
	})
	next := strings.TrimRight(base, "/") + scope + "/providers/Microsoft.CostManagement/query?api-version=" + azureAPIVersion
	var att attribution
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, next, bytes.NewReader(body))
		if err != nil {
			return nil, domain.Money{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		var res azureQuery
		if err := call(a.Client, req, token, &res); err != nil {
			return nil, domain.Money{}, err
		}
		col := map[string]int{}
		for i, c := range res.Properties.Columns {
			col[c.Name] = i
		}
		ci, ok := col["CostUSD"]
		if !ok {
			return nil, domain.Money{}, fmt.Errorf("%w: azure: no CostUSD column", ErrProvider)
		}
		for _, r := range res.Properties.Rows {
			cost, _ := cell(r, ci).(float64)
			team, found := "", false
			if s, _ := cell(r, col["TagKey"]).(string); s == TeamTag {
				v, _ := cell(r, col["TagValue"]).(string)
				team, found = TeamOfFolded(v)
			}
			if !found {
				rg, _ := cell(r, col["ResourceGroupName"]).(string)
				team, found = TeamOfFolded(rg)
			}
			att.add(team, found, usd(cost))
		}
		next = ""
		if res.Properties.NextLink != nil {
			next = *res.Properties.NextLink
		}
	}
	figs, un := att.result()
	return figs, un, nil
}

func cell(row []any, i int) any {
	if i < 0 || i >= len(row) {
		return nil
	}
	return row[i]
}
