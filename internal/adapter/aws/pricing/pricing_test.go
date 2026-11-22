package pricing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	awspricing "github.com/aws/aws-sdk-go-v2/service/pricing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
)

// product builds a Price List product document as the API returns it (a JSON string).
func product(model, inference, region, unit, usd, effective, extra string) string {
	doc := map[string]any{
		"product": map[string]any{"productFamily": "Amazon Bedrock", "attributes": map[string]string{
			"servicecode": "AmazonBedrock", "model": model, "inferenceType": inference, "regionCode": region,
			"usagetype": "USE1-" + strings.ReplaceAll(model, " ", "") + "-" + inference + extra}},
		"terms": map[string]any{"OnDemand": map[string]any{"SKU.TERM": map[string]any{
			"effectiveDate": effective,
			"priceDimensions": map[string]any{"SKU.TERM.DIM": map[string]any{
				"unit": unit, "pricePerUnit": map[string]string{"USD": usd}}}}}}}
	b, _ := json.Marshal(doc)
	return string(b)
}

func TestFetchParsesPagesRegionUnitsAndSkipsOtherProducts(t *testing.T) {
	srv := awstest.New(t)
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Action != "GetProducts" {
			return nil
		}
		if strings.Contains(string(r.Body), `"NextToken"`) {
			resp := awstest.JSONOK(map[string]any{"PriceList": []string{
				product("Claude Sonnet 4.5", "Output tokens", "us-east-1", "1K tokens", "0.015", "2026-12-01T00:00:00Z", ""),
				product("Claude Sonnet 4.5", "Input tokens cache read", "us-east-1", "1M tokens", "0.30", "2026-11-01T00:00:00Z", ""),
			}})
			return &resp
		}
		resp := awstest.JSONOK(map[string]any{"NextToken": "p2", "PriceList": []string{
			product("Claude Sonnet 4.5", "Input tokens", "us-east-1", "1K tokens", "0.003", "2026-12-01T00:00:00Z", ""),
			product("Claude Sonnet 4.5", "Input tokens", "eu-west-1", "1K tokens", "0.009", "2026-12-01T00:00:00Z", ""),
			product("Claude Sonnet 4.5", "Input tokens", "us-east-1", "1K tokens", "0.0015", "2026-12-01T00:00:00Z", "-batch"),
			product("Nova Pro", "Output tokens", "us-east-1", "tokens", "0.0000032", "2026-10-01T00:00:00Z", ""),
			product("Nova Pro", "Hours", "us-east-1", "Hour", "1", "2026-10-01T00:00:00Z", ""),
			`{not json`,
		}})
		return &resp
	})
	got, err := newList(awsClient(t, srv), Config{}, nil).Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		product string
		kind    prices.Kind
		usd     string
		eff     string
	}
	var rows []row
	for _, p := range got {
		rows = append(rows, row{p.Product, p.Kind, p.USDPerMtok.String(), p.EffectiveFrom.Format(time.DateOnly)})
	}
	want := []row{
		{"Claude Sonnet 4.5", prices.Input, "3.00", "2026-12-01"},
		{"Claude Sonnet 4.5", prices.Output, "15.00", "2026-12-01"},
		{"Claude Sonnet 4.5", prices.CacheRead, "0.30", "2026-11-01"},
		{"Nova Pro", prices.Output, "3.20", "2026-10-01"},
	}
	sortRows := func(r []row) { // Fetch orders by product then kind name
		for i := range r {
			for j := i + 1; j < len(r); j++ {
				if r[j].product < r[i].product || (r[j].product == r[i].product && r[j].kind < r[i].kind) {
					r[i], r[j] = r[j], r[i]
				}
			}
		}
	}
	sortRows(want)
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
	first, _ := srv.Find("GetProducts")
	var in struct {
		ServiceCode string
		Filters     []struct{ Type, Field, Value string }
	}
	if err := first.JSON(&in); err != nil {
		t.Fatal(err)
	}
	if in.ServiceCode != "AmazonBedrock" || len(in.Filters) != 1 || in.Filters[0].Field != "regionCode" || in.Filters[0].Value != "us-east-1" || in.Filters[0].Type != "TERM_MATCH" {
		t.Fatalf("request = %+v", in)
	}
	if srv.Count("GetProducts") != 2 {
		t.Fatalf("calls = %d", srv.Count("GetProducts"))
	}
}

func awsClient(t *testing.T, srv *awstest.Server) API {
	t.Helper()
	return awspricing.NewFromConfig(srv.Config())
}

func TestFetchErrors(t *testing.T) {
	srv := awstest.New(t)
	srv.OnAction("GetProducts", awstest.JSONError(400, "AccessDeniedException", "not allowed"))
	if _, err := newList(awsClient(t, srv), Config{}, nil).Fetch(t.Context()); err == nil || !strings.Contains(err.Error(), "GetProducts") {
		t.Fatalf("err = %v", err)
	}

	loop := awstest.New(t)
	loop.OnAction("GetProducts", awstest.JSONOK(map[string]any{"NextToken": "again", "PriceList": []string{}}))
	if _, err := newList(awsClient(t, loop), Config{MaxPages: 3}, nil).Fetch(t.Context()); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("err = %v", err)
	}
}

func TestPerMtok(t *testing.T) {
	for _, tc := range []struct{ unit, usd, want string }{
		{"1K tokens", "0.003", "3.00"}, {"1M tokens", "3", "3.00"}, {"tokens", "0.000003", "3.00"},
		{"1K tokens", "0.0000000004", "0.00"}, {"1K tokens", "0.00000123456789", "0.001235"},
	} {
		got, err := perMtok(tc.unit, tc.usd)
		if err != nil {
			t.Fatalf("%v: %v", tc, err)
		}
		if got.String() != tc.want {
			t.Fatalf("%v: got %s", tc, got)
		}
	}
	for _, tc := range []struct{ unit, usd string }{{"Hour", "1"}, {"1K tokens", "abc"}, {"1K tokens", "-1"}} {
		if _, err := perMtok(tc.unit, tc.usd); err == nil {
			t.Fatalf("%v accepted", tc)
		}
	}
}
