package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	me "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
)

const team1 = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

func TestParseStatementCSVAndJSON(t *testing.T) {
	csvBody := "\ufeffteamId,model,amountUsd,tokens\n" + team1 + ",Hermes-4,212.40,48100000\n" + team1 + ",Hermes-3,1.00,\n"
	r := httptest.NewRequest("POST", "/x?period=2026-11&source=nous_statement", strings.NewReader(csvBody))
	r.Header.Set("Content-Type", "text/csv")
	st, err := parseStatement(r, []byte(csvBody))
	if err != nil || len(st.Rows) != 2 || st.Source != me.SourceNousStatement || *st.Rows[0].Tokens != 48100000 || st.Rows[1].Tokens != nil {
		t.Fatalf("csv %+v %v", st, err)
	}
	if st.Total().Cmp(domain.MustMoney("213.40")) != 0 || st.Period.Format("2006-01") != "2026-11" {
		t.Fatalf("total %s period %s", st.Total(), st.Period)
	}
	js := `{"period":"2026-10","source":"opencode_csv","rows":[{"teamId":"` + team1 + `","model":"m","amountUsd":"3"}]}`
	r = httptest.NewRequest("POST", "/x", strings.NewReader(js))
	r.Header.Set("Content-Type", "application/json")
	if st, err = parseStatement(r, []byte(js)); err != nil || len(st.Rows) != 1 {
		t.Fatalf("json %+v %v", st, err)
	}
	for name, c := range map[string]struct{ ct, q, body string }{
		"no header":      {"text/csv", "?period=2026-11&source=nous_statement", ""},
		"missing column": {"text/csv", "?period=2026-11&source=nous_statement", "teamId,model\n"},
		"bad amount":     {"text/csv", "?period=2026-11&source=nous_statement", "teamId,model,amountUsd\n" + team1 + ",m,abc\n"},
		"bad tokens":     {"text/csv", "?period=2026-11&source=nous_statement", "teamId,model,amountUsd,tokens\n" + team1 + ",m,1,1.5\n"},
		"short row":      {"text/csv", "?period=2026-11&source=nous_statement", "teamId,model,amountUsd\n" + team1 + ",m\n"},
		"bad period":     {"text/csv", "?period=2026-13&source=nous_statement", "teamId,model,amountUsd\n"},
		"query on json":  {"application/json", "?period=2026-11", `{}`},
		"unknown field":  {"application/json", "", `{"period":"2026-11","x":1}`},
	} {
		r := httptest.NewRequest("POST", "/x"+c.q, strings.NewReader(c.body))
		r.Header.Set("Content-Type", c.ct)
		if _, err := parseStatement(r, []byte(c.body)); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInferenceRowBody(t *testing.T) {
	row := toInferenceRowBody(me.ReportRow{Route: "bedrock", PricingMode: "per_token", Calls: 4, Errors: 1, InputTokens: 600_000, OutputTokens: 400_000,
		CostUSD: domain.MustMoney("3")})
	if row.PricingMode != "bedrock" || row.ErrorRatePct != 25 || row.EffectiveUsdPerMtok == nil || row.EffectiveUsdPerMtok.Cmp(domain.MustMoney("3")) != 0 {
		t.Fatalf("%+v", row)
	}
	if toInferenceRowBody(me.ReportRow{}).EffectiveUsdPerMtok != nil {
		t.Fatal("no tokens, no effective price")
	}
}

func TestEndpointBodiesHideEmptyOptionals(t *testing.T) {
	e := me.Endpoint{ModelEndpoint: domain.ModelEndpoint{ID: "mep_1", Slug: "openai", PricingMode: domain.PricingFree, Version: 3}, ProviderOn: true,
		Models: []me.Model{{EndpointModel: domain.EndpointModel{ModelName: "m", Enabled: true}, PriceState: me.PriceNotRequired}}}
	b := toModelEndpointBody(e)
	if b.TeamID != nil || b.CABundle != nil || b.ProviderState != "on" || b.Health.State != "Unknown" || !b.Models[0].Callable || b.Version != 3 {
		t.Fatalf("%+v", b)
	}
	te := toTeamModelEndpointBody(me.TeamEndpoint{Endpoint: e, Relation: me.RelationSelected})
	if te.BaseURL != nil || te.BaseURLEnv != "AFS_LLM_OPENAI_BASE_URL" || te.ProviderState != "on" {
		t.Fatalf("%+v", te)
	}
	if got := toInferenceProviderBody(me.Provider{Impact: &me.Impact{Routes: 2}}); got.Hosts == nil || got.Impact.Routes != 2 || got.ChangedAt != nil {
		t.Fatalf("%+v", got)
	}
}
