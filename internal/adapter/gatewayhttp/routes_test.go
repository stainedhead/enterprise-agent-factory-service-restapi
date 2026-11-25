package gatewayhttp_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp/gwtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/routeup"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway/gatewaytest"
)

const epID = "mep_01JD000000000000000000000A"

// fakeOpenAI is an OpenAI-compatible upstream that records what reached it.
type fakeOpenAI struct {
	mu      sync.Mutex
	reqs    []*http.Request
	bodies  []map[string]any
	status  int
	body    string
	stream  string
	delay   time.Duration
	headers map[string]string
}

func (f *fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.bodies = append(f.bodies, doc)
	status, body, stream, delay := f.status, f.body, f.stream, f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	for k, v := range f.headers {
		w.Header().Set(k, v)
	}
	if stream != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeOpenAI) last(t *testing.T) (*http.Request, map[string]any) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("nothing reached the upstream")
	}
	return f.reqs[len(f.reqs)-1], f.bodies[len(f.bodies)-1]
}

const chatAnswer = `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cached_tokens":400},"completion_tokens_details":{"reasoning_tokens":50}}}`

func teamRig(t *testing.T, up *fakeOpenAI, mutate ...func(*gwtest.Rig, *gatewaytest.Routes)) (*gwtest.Rig, *gatewaytest.Routes) {
	t.Helper()
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	routes := gatewaytest.NewRoutes()
	routes.Grants["agent/agt_01"] = []gateway.RouteGrant{{EndpointID: epID, Slug: "openai", Model: "gpt-5"},
		{EndpointID: "mep_x", Slug: "nous", Model: "hermes", Reason: domain.ReasonProviderDisabled}}
	routes.Endpoints["openai"] = gateway.RouteEndpoint{ID: epID, Slug: "openai", Provider: "openai", BaseURL: srv.URL + "/v1",
		Protocols: []domain.Protocol{domain.ProtocolChat, domain.ProtocolResponses, domain.ProtocolEmbeddings}, Location: domain.LocationExternal,
		AuthKind: domain.AuthBearer, Pricing: domain.PricingPerToken, Timeout: 2 * time.Second, Models: map[string]int{"gpt-5": 1000}}
	routes.PriceRows[epID+"/gpt-5"] = []gateway.RoutePrice{{EffectiveFrom: gwtest.Now.Add(-time.Hour), Input: domain.MustMoney("1"),
		Output: domain.MustMoney("10"), CachedInput: domain.MustMoney("0.1")}}
	auth := &gatewaytest.Auth{Creds: map[string]gateway.UpstreamCredential{"openai": {Header: "Authorization", Value: "Bearer sk-route-secret"}},
		Errs: map[string]error{}}
	r := gwtest.New(t, func(r *gwtest.Rig) {
		r.TeamNamespace, r.Routes, r.Auth, r.RouteUp = "afs-t-alpha", routes, auth, routeup.New()
		for _, m := range mutate {
			m(r, routes)
		}
	})
	return r, routes
}

func TestRouteChatPassThrough(t *testing.T) {
	up := &fakeOpenAI{body: chatAnswer}
	r, _ := teamRig(t, up)
	resp := post(t, r, "/r/openai/v1/chat/completions", `{"model":"gpt-5","max_tokens":5000,"messages":[{"role":"user","content":"hi"}],"seed":7}`, //nolint:bodyclose // closed by post's cleanup
		map[string]string{"Authorization": "Bearer agent-placeholder", "Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	if !strings.Contains(string(readAll(t, resp)), `"reasoning_tokens":50`) {
		t.Error("the answer was not passed through")
	}
	req, body := up.last(t)
	if req.URL.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer sk-route-secret" {
		t.Errorf("upstream got %s auth %q", req.URL.Path, req.Header.Get("Authorization"))
	}
	if req.Header.Get("Traceparent") != "" {
		t.Error("traceparent was propagated to an external endpoint")
	}
	if body["max_completion_tokens"] != float64(1000) || body["max_tokens"] != nil || body["seed"] != float64(7) || body["model"] != "gpt-5" {
		t.Errorf("upstream body %v", body)
	}
	r.Svc.Wait()
	rows := r.Usage.All()
	if len(rows) != 1 || rows[0].Route == nil || rows[0].Route.Reasoning != 50 || rows[0].Route.HTTPStatus != 200 || rows[0].Usage.CacheRead != 400 {
		t.Fatalf("usage = %+v", rows)
	}
	// 600 x 1 + 400 x 0.1 + 200 x 10 per million.
	if got := r.Spend.Settlements[0].Actual.String(); got != "0.00264" {
		t.Fatalf("settled %s", got)
	}
}

func TestRouteBodyFormAndTraceparentToCorporate(t *testing.T) {
	up := &fakeOpenAI{body: chatAnswer}
	r, _ := teamRig(t, up, func(_ *gwtest.Rig, rs *gatewaytest.Routes) {
		e := rs.Endpoints["openai"]
		e.Location = domain.LocationCorporate
		rs.Endpoints["openai"] = e
	})
	resp := post(t, r, "/openai/v1/chat/completions", `{"model":"openai/gpt-5","messages":[]}`, //nolint:bodyclose // closed by post's cleanup
		map[string]string{"Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	req, body := up.last(t)
	if body["model"] != "gpt-5" || req.Header.Get("Traceparent") == "" {
		t.Errorf("body %v traceparent %q", body, req.Header.Get("Traceparent"))
	}
	for _, m := range []string{`"gpt-5"`, `"bedrock/x"`} {
		resp := post(t, r, "/openai/v1/chat/completions", `{"model":`+m+`}`, nil) //nolint:bodyclose // closed by post's cleanup
		if resp.StatusCode != 400 {
			t.Errorf("model %s: status %d", m, resp.StatusCode)
		}
	}
}

const streamUpstream = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"cost\":0.75}}\n\n" +
	"data: [DONE]\n\n"

func TestRouteStreamIncludeUsage(t *testing.T) {
	for _, asked := range []bool{false, true} {
		up := &fakeOpenAI{stream: streamUpstream}
		r, _ := teamRig(t, up)
		opts := ""
		if asked {
			opts = `,"stream_options":{"include_usage":true}`
		}
		resp := post(t, r, "/r/openai/v1/chat/completions", `{"model":"gpt-5","stream":true`+opts+`}`, nil) //nolint:bodyclose // closed by post's cleanup
		out := string(readAll(t, resp))
		if resp.StatusCode != 200 || !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, "[DONE]") {
			t.Fatalf("status %d: %s", resp.StatusCode, out)
		}
		if got := strings.Contains(out, `"usage"`); got != asked {
			t.Errorf("asked=%v: usage chunk relayed=%v", asked, got)
		}
		_, body := up.last(t)
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Errorf("include_usage not injected: %v", body)
		}
		r.Svc.Wait()
		// usage.cost 0.75 is larger than the token cost: the settled cost is the provider's.
		if got := r.Spend.Settlements[0].Actual.String(); got != "0.75" {
			t.Errorf("settled %s", got)
		}
		if rows := r.Usage.All(); rows[0].Route.Source != gateway.UsageFromProviderCost {
			t.Errorf("source %q", rows[0].Route.Source)
		}
	}
}

func TestRouteUpstreamErrors(t *testing.T) {
	cases := map[string]struct {
		up     *fakeOpenAI
		status int
		reason string
	}{
		"balance":   {&fakeOpenAI{status: 402, body: `{"error":{"message":"no credits"}}`}, 503, domain.ReasonProviderBalanceExhausted},
		"budget":    {&fakeOpenAI{status: 429, body: `{"error":{"message":"budget exceeded"}}`}, 429, domain.ReasonProviderBudgetExhausted},
		"throttled": {&fakeOpenAI{status: 429, body: `{}`, headers: map[string]string{"Retry-After": "7"}}, 429, ""},
		"5xx":       {&fakeOpenAI{status: 500, body: `{}`}, 502, ""},
		"401":       {&fakeOpenAI{status: 401, body: `{}`}, 502, ""},
		"400":       {&fakeOpenAI{status: 400, body: `{"error":{"message":"bad tool"}}`}, 400, ""},
		"504":       {&fakeOpenAI{status: 504, body: `{}`}, 504, ""},
		"timeout":   {&fakeOpenAI{delay: 400 * time.Millisecond, body: chatAnswer}, 504, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, _ := teamRig(t, c.up, func(_ *gwtest.Rig, rs *gatewaytest.Routes) {
				e := rs.Endpoints["openai"]
				e.Timeout = 100 * time.Millisecond
				rs.Endpoints["openai"] = e
			})
			resp := post(t, r, "/r/openai/v1/chat/completions", `{"model":"gpt-5"}`, nil) //nolint:bodyclose // closed by post's cleanup
			if resp.StatusCode != c.status {
				t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
			}
			if c.reason != "" {
				if d, _ := errBody(t, resp)["details"].(map[string]any); d["reason"] != c.reason {
					t.Errorf("details %v", d)
				}
			}
			if name == "throttled" && resp.Header.Get("Retry-After") != "7" {
				t.Errorf("retry-after %q", resp.Header.Get("Retry-After"))
			}
			r.Svc.Wait()
			if r.Spend.Held() != 0 || r.Spend.Settlements[0].Actual.Micros() != 0 {
				t.Errorf("reservation not released: %+v", r.Spend.Settlements)
			}
		})
	}
}

func TestRouteRefusalsAndPaths(t *testing.T) {
	up := &fakeOpenAI{body: chatAnswer}
	r, _ := teamRig(t, up, func(r *gwtest.Rig, _ *gatewaytest.Routes) {
		r.Auth.(*gatewaytest.Auth).Errs["openai"] = nil
	})
	cases := []struct {
		path, body string
		status     int
		reason     string
	}{
		{"/r/nous/v1/chat/completions", `{"model":"hermes"}`, 403, domain.ReasonProviderDisabled},
		{"/r/openai/v1/chat/completions", `{"model":"gpt-4"}`, 403, domain.ReasonModelNotOnRoute},
		{"/r/openai/v1/audio/speech", `{}`, 404, ""},
		{"/r/Bad_Slug/v1/chat/completions", `{}`, 404, ""},
		{"/r/bedrock/v1/embeddings", `{}`, 404, ""},
		{"/r/openai/v1/chat/completions", `not json`, 400, ""},
	}
	for _, c := range cases {
		resp := post(t, r, c.path, c.body, nil) //nolint:bodyclose // closed by post's cleanup
		if resp.StatusCode != c.status {
			t.Errorf("%s: status %d", c.path, resp.StatusCode)
			continue
		}
		if c.reason != "" {
			if d, _ := errBody(t, resp)["details"].(map[string]any); d["reason"] != c.reason || d["route"] == nil {
				t.Errorf("%s: details %v", c.path, d)
			}
		}
	}
}

func TestRouteCredentialErrors(t *testing.T) {
	for err, reason := range map[error]string{gateway.ErrCredentialMissing: domain.ReasonCredentialMissing, gateway.ErrFederationNotReady: domain.ReasonFederationNotReady} {
		up := &fakeOpenAI{body: chatAnswer}
		r, _ := teamRig(t, up, func(r *gwtest.Rig, _ *gatewaytest.Routes) { r.Auth.(*gatewaytest.Auth).Errs["openai"] = err })
		resp := post(t, r, "/r/openai/v1/chat/completions", `{"model":"gpt-5"}`, nil) //nolint:bodyclose // closed by post's cleanup
		if d, _ := errBody(t, resp)["details"].(map[string]any); resp.StatusCode != 503 || d["reason"] != reason {
			t.Errorf("%v: %d %v", err, resp.StatusCode, d)
		}
		if r.Spend.Held() != 0 || len(r.Spend.Settlements) != 0 {
			t.Error("reserved before the credential check")
		}
	}
}

func TestRouteModelsList(t *testing.T) {
	r, _ := teamRig(t, &fakeOpenAI{})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, r.HTTP.URL+"/r/openai/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || resp.StatusCode != 200 || len(doc.Data) != 1 || doc.Data[0].ID != "gpt-5" {
		t.Fatalf("status %d doc %+v err %v", resp.StatusCode, doc, err)
	}
}

func TestSharedGatewayServesNoRoutes(t *testing.T) {
	r := gwtest.New(t)
	resp := post(t, r, "/r/openai/v1/chat/completions", `{"model":"gpt-5"}`, nil) //nolint:bodyclose // closed by post's cleanup
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBedrockRouteTranslatesChatToConverse(t *testing.T) {
	r, _ := teamRig(t, &fakeOpenAI{})
	resp := post(t, r, "/r/bedrock/v1/chat/completions", //nolint:bodyclose // closed by post's cleanup
		`{"model":"`+gwtest.Model+`","max_tokens":50,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, readAll(t, resp))
	}
	var doc map[string]any
	if err := json.Unmarshal(readAll(t, resp), &doc); err != nil || doc["object"] != "chat.completion" || doc["usage"] == nil {
		t.Fatalf("doc %v err %v", doc, err)
	}
	r.Svc.Wait()
	calls := r.Fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
	var ub map[string]any
	_ = json.Unmarshal(calls[0].Body, &ub)
	if ub["system"] == nil || ub["requestMetadata"] == nil {
		t.Errorf("converse body %v", ub)
	}
	for _, bad := range []string{`"n":2`, `"logprobs":true`, `"audio":{}`, `"response_format":{}`} {
		resp := post(t, r, "/r/bedrock/v1/chat/completions", `{"model":"`+gwtest.Model+`",`+bad+`,"messages":[{"role":"user","content":"x"}]}`, nil) //nolint:bodyclose // closed by post's cleanup
		d, _ := errBody(t, resp)["details"].(map[string]any)
		if resp.StatusCode != 400 || d["reason"] != domain.ReasonUnsupportedParameter || d["field"] == nil {
			t.Errorf("%s: %d %v", bad, resp.StatusCode, d)
		}
	}
}

func TestMuxComposesFamilies(t *testing.T) {
	r := gwtest.New(t)
	m := r.Server.Mux()
	m.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(299) }))
	m.Handle("/mcp/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(298) }))
	for path, want := range map[string]int{"/mcp": 299, "/mcp/x": 298, "/nope": 404, "/healthz": 200} {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
	defer func() {
		if recover() == nil {
			t.Error("a second registration of a path did not panic")
		}
	}()
	m.Handle("/mcp", http.NotFoundHandler())
}

func TestFamilyFromConfig(t *testing.T) {
	f := gatewayhttp.FamilyFunc(func(m *gatewayhttp.Mux) {
		m.Handle("/x/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(297) }))
	})
	s := gatewayhttp.New(gatewayhttp.Config{Families: []gatewayhttp.Family{f}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x/y", nil))
	if rec.Code != 297 {
		t.Fatalf("code %d", rec.Code)
	}
}
