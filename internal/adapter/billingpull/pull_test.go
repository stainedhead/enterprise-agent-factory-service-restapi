package billingpull_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/billingpull"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
)

const (
	teamA  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	secret = "sk-admin-not-a-real-key"
)

var day = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func key(context.Context, routebill.Endpoint) (string, error) { return secret, nil }

// replay serves recorded fixtures by "METHOD path?query" (query "" matches any) and records the requests.
type replay struct {
	t      *testing.T
	routes map[string]string
	status int
	seen   []*http.Request
	bodies []string
}

func (r *replay) server() *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.seen = append(r.seen, req)
		r.bodies = append(r.bodies, string(b))
		if got := req.Header.Get("Authorization"); req.URL.Path != "/prices.json" && got != "Bearer "+secret {
			r.t.Errorf("%s: Authorization = %q", req.URL.Path, got)
		}
		if r.status != 0 {
			w.WriteHeader(r.status)
			_, _ = w.Write([]byte(`{"error":"` + secret + `"}`))
			return
		}
		f, ok := r.routes[req.Method+" "+req.URL.Path+"?"+req.URL.Query().Get("page")]
		if !ok {
			f, ok = r.routes[req.Method+" "+req.URL.Path]
		}
		if !ok {
			r.t.Errorf("unexpected %s %s", req.Method, req.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			r.t.Fatal(err)
		}
		_, _ = w.Write(b)
	}))
	r.t.Cleanup(s.Close)
	return s
}

func figures(t *testing.T, f []routebill.Figure) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, x := range f {
		out[x.TeamID] = x.AmountUSD.String()
	}
	return out
}

func TestTeamOf(t *testing.T) {
	for _, c := range []struct {
		in, want string
		folded   bool
	}{{"afs " + teamA, teamA, false}, {"Default", "", false}, {"rg-" + strings.ToLower(teamB), teamB, true}, {"team_short", "", true}} {
		got, _ := billingpull.TeamOf(c.in)
		if c.folded {
			got, _ = billingpull.TeamOfFolded(c.in)
		}
		if got != c.want {
			t.Errorf("TeamOf(%q) = %q", c.in, got)
		}
	}
}

// TC-BL-01: OpenAI costs per project, paged, named by project; null and unnamed projects unattributed.
func TestOpenAI(t *testing.T) {
	r := &replay{t: t, routes: map[string]string{
		"GET /v1/organization/projects": "openai_projects.json",
		"GET /v1/organization/costs?":   "openai_costs_1.json", "GET /v1/organization/costs?page_2": "openai_costs_2.json"}}
	s := r.server()
	p := &billingpull.OpenAI{BaseURL: s.URL, Client: s.Client(), Key: key}
	if p.Provider() != "openai" {
		t.Fatal(p.Provider())
	}
	figs, un, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "openai"}, day.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := figures(t, figs); got[teamA] != "10.20" || got[teamB] != "8.00" || len(got) != 2 || un.String() != "0.45" {
		t.Fatalf("figures = %v, unattributed %v", got, un)
	}
	q := r.seen[1].URL.Query()
	if q.Get("start_time") != "1795132800" || q.Get("end_time") != "1795219200" || q.Get("group_by") != "project_id" || q.Get("bucket_width") != "1d" {
		t.Errorf("query = %v", q)
	}
}

// A provider failure names the status, never the key or the body.
func TestProviderErrorsHideTheKey(t *testing.T) {
	r := &replay{t: t, status: http.StatusUnauthorized}
	s := r.server()
	_, _, err := (&billingpull.OpenAI{BaseURL: s.URL, Client: s.Client(), Key: key}).Pull(t.Context(), routebill.Endpoint{}, day)
	if !errors.Is(err, billingpull.ErrProvider) || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
	keyErr := errors.New("no key")
	_, _, err = (&billingpull.OpenAI{BaseURL: s.URL, Client: s.Client(),
		Key: func(context.Context, routebill.Endpoint) (string, error) { return "", keyErr }}).Pull(t.Context(), routebill.Endpoint{}, day)
	if !errors.Is(err, keyErr) {
		t.Fatalf("key error = %v", err)
	}
	// A transport error that echoes the key is redacted.
	_, _, err = (&billingpull.OpenAI{BaseURL: "http://127.0.0.1:1", Client: echoDoer{}, Key: key}).Pull(t.Context(), routebill.Endpoint{}, day)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("transport error = %v", err)
	}
	// A body that is not JSON.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer bad.Close()
	if _, _, err := (&billingpull.OpenAI{BaseURL: bad.URL, Client: bad.Client(), Key: key}).Pull(t.Context(), routebill.Endpoint{}, day); !errors.Is(err, billingpull.ErrProvider) {
		t.Fatalf("decode error = %v", err)
	}
}

type echoDoer struct{}

func (echoDoer) Do(req *http.Request) (*http.Response, error) {
	return nil, errors.New("refused: " + req.Header.Get("Authorization"))
}

// TC-BL-01: OpenRouter key usage for today only, named by key name or label.
func TestOpenRouter(t *testing.T) {
	r := &replay{t: t, routes: map[string]string{"GET /api/v1/keys": "openrouter_keys.json"}}
	s := r.server()
	p := &billingpull.OpenRouter{BaseURL: s.URL, Client: s.Client(), Key: key, Now: func() time.Time { return day.Add(23 * time.Hour) }}
	if p.Provider() != "openrouter" {
		t.Fatal(p.Provider())
	}
	figs, un, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "openrouter"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if got := figures(t, figs); got[teamA] != "3.25" || got[teamB] != "1.50" || un.String() != "0.75" {
		t.Fatalf("figures = %v, unattributed %v", got, un)
	}
	if _, _, err := p.Pull(t.Context(), routebill.Endpoint{}, day.AddDate(0, 0, -1)); !errors.Is(err, billingpull.ErrDayNotAvailable) {
		t.Fatalf("yesterday: %v", err)
	}
}

// TC-BL-01: Azure Cost Management by afs-team-id tag, else by resource group name.
func TestAzure(t *testing.T) {
	scope := "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/afs"
	r := &replay{t: t, routes: map[string]string{"POST " + scope + "/providers/Microsoft.CostManagement/query": "azure_query.json"}}
	s := r.server()
	p := &billingpull.Azure{BaseURL: s.URL, Client: s.Client(), Token: key, Scopes: map[string]string{"azure-prod": scope}}
	if p.Provider() != "azure" {
		t.Fatal(p.Provider())
	}
	figs, un, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "azure-prod"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if got := figures(t, figs); got[teamA] != "15.00" || len(got) != 1 || un.String() != "1.25" {
		t.Fatalf("figures = %v, unattributed %v", got, un)
	}
	var body struct {
		Type       string            `json:"type"`
		TimePeriod map[string]string `json:"timePeriod"`
	}
	if err := json.Unmarshal([]byte(r.bodies[0]), &body); err != nil || body.Type != "ActualCost" || body.TimePeriod["from"] != "2026-11-20T00:00:00Z" ||
		r.seen[0].URL.Query().Get("api-version") == "" {
		t.Errorf("request = %s %v", r.bodies[0], err)
	}
	if _, _, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "unknown"}, day); err == nil {
		t.Error("route without a scope")
	}
}

// TC-BL-01: the Google billing export by afs-team-id label.
func TestGoogle(t *testing.T) {
	r := &replay{t: t, routes: map[string]string{"POST /bigquery/v2/projects/afs-billing-prj/queries": "google_query.json"}}
	s := r.server()
	p := &billingpull.Google{BaseURL: s.URL, Client: s.Client(), Token: key,
		Exports: map[string]string{"vertex": "afs-billing-prj:billing.gcp_billing_export_v1_0000"}}
	if p.Provider() != "google" {
		t.Fatal(p.Provider())
	}
	figs, un, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "vertex"}, day)
	if err != nil {
		t.Fatal(err)
	}
	if got := figures(t, figs); got[teamA] != "7.125" || un.String() != "0.50" {
		t.Fatalf("figures = %v, unattributed %v", got, un)
	}
	if !strings.Contains(r.bodies[0], "billing.gcp_billing_export_v1_0000") || !strings.Contains(r.bodies[0], `"useLegacySql":false`) {
		t.Errorf("body = %s", r.bodies[0])
	}
	if _, _, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "unknown"}, day); err == nil {
		t.Error("route without an export")
	}
	incomplete := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"jobComplete":false}`)) }))
	defer incomplete.Close()
	p.BaseURL, p.Client = incomplete.URL, incomplete.Client()
	if _, _, err := p.Pull(t.Context(), routebill.Endpoint{Slug: "vertex"}, day); !errors.Is(err, billingpull.ErrProvider) {
		t.Errorf("incomplete job: %v", err)
	}
}

// TC-BL-03: LiteLLM's list as route list prices per million tokens, provider prefixes dropped.
func TestLiteLLM(t *testing.T) {
	r := &replay{t: t, routes: map[string]string{"GET /prices.json": "litellm_prices.json"}}
	s := r.server()
	list, err := (&billingpull.LiteLLM{URL: s.URL + "/prices.json", Client: s.Client()}).Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range list {
		got[p.Provider+"|"+p.Model] = p.Input.String() + "/" + p.Output.String()
	}
	want := map[string]string{"openai|gpt-5": "1.25/10.00", "openrouter|anthropic/claude-sonnet-4.5": "3.00/15.00",
		"azure|gpt-4.1": "2.00/8.00", "google|gemini-2.5-pro": "1.25/10.00"}
	if len(got) != len(want) {
		t.Fatalf("list = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
