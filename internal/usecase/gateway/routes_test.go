package gateway_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

const (
	epOpenAI = "mep_01JD000000000000000000000A"
	epLocal  = "mep_01JD000000000000000000000B"
)

type routeStore struct {
	grants    map[string][]gateway.RouteGrant
	endpoints map[string]gateway.RouteEndpoint
	prices    map[string][]gateway.RoutePrice
	err       error
	calls     int
}

func (s *routeStore) RouteAdmission(_ context.Context, kind, id string) ([]gateway.RouteGrant, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	g, ok := s.grants[kind+"/"+id]
	if !ok {
		return nil, gateway.ErrNotFound
	}
	return g, nil
}

func (s *routeStore) RouteEndpoint(_ context.Context, _, slug string) (gateway.RouteEndpoint, error) {
	e, ok := s.endpoints[slug]
	if !ok {
		return e, gateway.ErrNotFound
	}
	return e, nil
}

func (s *routeStore) RoutePrices(_ context.Context, id, model string) ([]gateway.RoutePrice, error) {
	return s.prices[id+"/"+model], nil
}

type upstreamAuth struct{ err error }

func (u upstreamAuth) Credential(_ context.Context, ep gateway.RouteEndpoint, _ string) (gateway.UpstreamCredential, error) {
	if u.err != nil {
		return gateway.UpstreamCredential{}, u.err
	}
	if ep.AuthKind == domain.AuthNone {
		return gateway.UpstreamCredential{}, nil
	}
	return gateway.UpstreamCredential{Header: "Authorization", Value: "Bearer sk-test"}, nil
}

type routeRig struct {
	*rig
	routes *routeStore
	auth   *upstreamAuth
}

func newRouteRig(t *testing.T) *routeRig {
	t.Helper()
	r := newRig(t)
	rs := &routeStore{
		grants: map[string][]gateway.RouteGrant{"agent/agt_01": {
			{EndpointID: epOpenAI, Slug: "openai", Model: "gpt-5"},
			{EndpointID: epOpenAI, Slug: "openai", Model: "gpt-5-mini", Reason: domain.ReasonModelNotPriced},
			{EndpointID: epLocal, Slug: "local", Model: "llama", Reason: ""},
			{EndpointID: "mep_off", Slug: "nous", Model: "hermes", Reason: domain.ReasonProviderDisabled},
		}},
		endpoints: map[string]gateway.RouteEndpoint{
			"openai": {ID: epOpenAI, Slug: "openai", Provider: "openai", Protocols: []domain.Protocol{domain.ProtocolChat, domain.ProtocolEmbeddings},
				AuthKind: domain.AuthBearer, Pricing: domain.PricingPerToken, Models: map[string]int{"gpt-5": 1000, "gpt-5-mini": 500}},
			"local": {ID: epLocal, Slug: "local", Provider: "internal", Protocols: []domain.Protocol{domain.ProtocolChat},
				AuthKind: domain.AuthNone, Pricing: domain.PricingCapacity, Models: map[string]int{"llama": 2000}},
		},
		prices: map[string][]gateway.RoutePrice{epOpenAI + "/gpt-5": {{EffectiveFrom: now.Add(-time.Hour), Input: domain.MustMoney("1"),
			Output: domain.MustMoney("10"), CachedInput: domain.MustMoney("0.1"), RequestFee: domain.MustMoney("0.0001")}}},
	}
	auth := &upstreamAuth{}
	r.svc = gateway.NewService(gateway.Deps{Pods: r.dir, Tokens: r.tokens, Store: r.store, Spend: r.spend, Thresholds: r.thr, Usage: r.usage,
		Routes: rs, Upstreams: auth},
		gateway.Config{Partition: "aws", AccountID: "111122223333", FailClosedTimeout: 200 * time.Millisecond, TeamNamespace: "afs-t-alpha",
			Now: func() time.Time { return now }})
	return &routeRig{rig: r, routes: rs, auth: auth}
}

func routeReq(route, model string) gateway.RouteRequest {
	return gateway.RouteRequest{Request: gateway.Request{SourceIP: netip.MustParseAddr("10.0.0.1"), MaxTokens: 100, InputBytes: 300, RequestID: "req-r"},
		Route: route, Model: model, Protocol: domain.ProtocolChat}
}

func routeOK(in, out int) gateway.RouteResult {
	return gateway.RouteResult{Usage: gateway.RouteUsage{Input: in, Output: out}, UsageReported: true, Started: true, HTTPStatus: 200}
}

func (r *routeRig) invokeRoute(res gateway.RouteResult, err error, seen *gateway.RouteAuthorized) gateway.RouteInvokeFunc {
	return func(_ context.Context, a gateway.RouteAuthorized) (gateway.RouteResult, error) {
		r.invokes.Add(1)
		if seen != nil {
			*seen = a
		}
		return res, err
	}
}

func TestAdmitRouteOrder(t *testing.T) {
	g := []gateway.RouteGrant{
		{Slug: "a", Model: "m1"},
		{Slug: "a", Model: "m2", Reason: domain.ReasonModelNotPriced},
		{Slug: "b", Model: "m1", Reason: domain.ReasonRouteNotEnabledForAgent},
		{Slug: "c", Model: "m1", Reason: domain.ReasonProviderDisabled},
	}
	cases := []struct{ route, model, want string }{
		{"a", "m1", ""},
		{"a", "m2", domain.ReasonModelNotPriced},
		{"a", "zz", domain.ReasonModelNotOnRoute},
		{"b", "zz", domain.ReasonRouteNotEnabledForAgent},
		{"c", "m1", domain.ReasonProviderDisabled},
		{"nope", "m1", domain.ReasonRouteNotEnabledForTeam},
	}
	for _, c := range cases {
		if got := gateway.AdmitRoute(g, c.route, c.model); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.route, c.model, got, c.want)
		}
	}
}

func TestRouteCallReservesSettlesAndRecords(t *testing.T) {
	r := newRouteRig(t)
	var seen gateway.RouteAuthorized
	res := routeOK(1000, 200)
	res.Usage.CachedInput = 400
	res.Usage.Reasoning = 50
	res.TTFT, res.Upstream = 120*time.Millisecond, 900*time.Millisecond
	if err := r.svc.DoRoute(t.Context(), routeReq("openai", "gpt-5"), r.invokeRoute(res, nil, &seen)); err != nil {
		t.Fatal(err)
	}
	if seen.MaxOutputTokens != 100 || seen.Credential.Value != "Bearer sk-test" || seen.Endpoint.ID != epOpenAI {
		t.Fatalf("authorized = %+v", seen)
	}
	// 600 x 1 + 400 x 0.1 + 200 x 10 per million + 0.0001 = 0.00264 + 0.0001.
	if len(r.spend.Settlements) != 1 || r.spend.Settlements[0].Actual.String() != "0.00274" {
		t.Fatalf("settlements = %+v", r.spend.Settlements)
	}
	rows := r.usage.All()
	if len(rows) != 1 || rows[0].ModelID != "" || rows[0].Route == nil {
		t.Fatalf("usage = %+v", rows)
	}
	rf := rows[0].Route
	if rf.EndpointID != epOpenAI || rf.Model != "gpt-5" || rf.Source != gateway.UsageFromResponse || rf.Reasoning != 50 || rf.TTFTMS != 120 ||
		rf.UpstreamMS != 900 || rf.HTTPStatus != 200 {
		t.Fatalf("route fields = %+v", rf)
	}
}

func TestRouteProviderCostSettlesToTheLarger(t *testing.T) {
	r := newRouteRig(t)
	res := routeOK(100, 10)
	pc := domain.MustMoney("0.5")
	res.Usage.ProviderCost = &pc
	if err := r.svc.DoRoute(t.Context(), routeReq("openai", "gpt-5"), r.invokeRoute(res, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if r.spend.Settlements[0].Actual.String() != "0.50" || r.usage.All()[0].Route.Source != gateway.UsageFromProviderCost {
		t.Fatalf("settlement = %+v row %+v", r.spend.Settlements, r.usage.All()[0].Route)
	}
	small := domain.MustMoney("0.000001")
	res.Usage.ProviderCost = &small
	got, src := gateway.RouteSettleAmount(res, nil, gateway.RoutePrice{Input: domain.MustMoney("1"), Output: domain.MustMoney("10")}, domain.MustMoney("1"))
	if got.String() != "0.0002" || src != gateway.UsageFromResponse {
		t.Fatalf("got %s %s", got, src)
	}
}

func TestRouteWithoutUsageSettlesToTheReservation(t *testing.T) {
	r := newRouteRig(t)
	if err := r.svc.DoRoute(t.Context(), routeReq("openai", "gpt-5"), r.invokeRoute(gateway.RouteResult{Started: true, HTTPStatus: 200}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	// 100 x 10 + 100 x 1 per million + 0.0001 fee.
	if r.spend.Settlements[0].Actual.String() != "0.0012" || r.usage.All()[0].Route.Source != gateway.UsageEstimated {
		t.Fatalf("settlement = %+v", r.spend.Settlements)
	}
}

func TestRouteUpstreamFailureReleasesTheReservation(t *testing.T) {
	r := newRouteRig(t)
	upErr := &domain.Error{Kind: domain.KindUpstream, Message: "5xx"}
	err := r.svc.DoRoute(t.Context(), routeReq("openai", "gpt-5"), r.invokeRoute(gateway.RouteResult{HTTPStatus: 502}, upErr, nil))
	if kind(err) != domain.KindUpstream || r.spend.Settlements[0].Actual.Micros() != 0 || r.spend.Held() != 0 {
		t.Fatalf("err %v settlements %+v", err, r.spend.Settlements)
	}
	if rows := r.usage.All(); rows[0].Outcome != gateway.OutcomeError {
		t.Fatalf("usage = %+v", rows)
	}
}

func TestRouteRefusals(t *testing.T) {
	cases := map[string]struct {
		route, model string
		mutate       func(*routeRig)
		kind         domain.ErrorKind
		reason       string
	}{
		"model not on route":   {"openai", "gpt-4", nil, domain.KindAccessDenied, domain.ReasonModelNotOnRoute},
		"not priced":           {"openai", "gpt-5-mini", nil, domain.KindAccessDenied, domain.ReasonModelNotPriced},
		"provider off":         {"nous", "hermes", nil, domain.KindAccessDenied, domain.ReasonProviderDisabled},
		"unknown route":        {"secret", "x", nil, domain.KindAccessDenied, domain.ReasonRouteNotEnabledForTeam},
		"credential missing":   {"openai", "gpt-5", func(r *routeRig) { r.auth.err = gateway.ErrCredentialMissing }, domain.KindUnavailable, domain.ReasonCredentialMissing},
		"federation not ready": {"openai", "gpt-5", func(r *routeRig) { r.auth.err = gateway.ErrFederationNotReady }, domain.KindUnavailable, domain.ReasonFederationNotReady},
		"auth broken":          {"openai", "gpt-5", func(r *routeRig) { r.auth.err = errors.New("io") }, domain.KindUnavailable, ""},
		"database down":        {"openai", "gpt-5", func(r *routeRig) { r.routes.err = errors.New("down") }, domain.KindUnavailable, ""},
		"no price row":         {"openai", "gpt-5", func(r *routeRig) { r.routes.prices = nil }, domain.KindAccessDenied, domain.ReasonModelNotPriced},
		"no budget":            {"openai", "gpt-5", func(r *routeRig) { delete(r.store.Budgets, teamA) }, domain.KindBudgetExceeded, ""},
		"endpoint gone": {"openai", "gpt-5", func(r *routeRig) { delete(r.routes.endpoints, "openai") }, domain.KindAccessDenied,
			domain.ReasonRouteNotEnabledForTeam},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRouteRig(t)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			err := r.svc.DoRoute(t.Context(), routeReq(tc.route, tc.model), r.invokeRoute(routeOK(1, 1), nil, nil))
			var de *domain.Error
			if !errors.As(err, &de) || de.Kind != tc.kind || (tc.reason != "" && de.Details["reason"] != tc.reason) {
				t.Fatalf("err = %#v", err)
			}
			if r.invokes.Load() != 0 || r.spend.Held() != 0 {
				t.Fatal("a refused call was forwarded or left a hold")
			}
		})
	}
}

func TestRouteProtocolNotServedIs404(t *testing.T) {
	r := newRouteRig(t)
	q := routeReq("local", "llama")
	q.Protocol = domain.ProtocolResponses
	if err := r.svc.DoRoute(t.Context(), q, r.invokeRoute(routeOK(1, 1), nil, nil)); kind(err) != domain.KindNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestCapacityRouteNeitherReservesNorCharges(t *testing.T) {
	r := newRouteRig(t)
	var seen gateway.RouteAuthorized
	if err := r.svc.DoRoute(t.Context(), routeReq("local", "llama"), r.invokeRoute(routeOK(10, 10), nil, &seen)); err != nil {
		t.Fatal(err)
	}
	if len(r.spend.Settlements) != 0 || seen.Credential.Value != "" || seen.MaxOutputTokens != 100 {
		t.Fatalf("settlements %+v seen %+v", r.spend.Settlements, seen)
	}
	if rows := r.usage.All(); rows[0].Cost.Micros() != 0 || rows[0].Route.Source != gateway.UsageFromResponse {
		t.Fatalf("usage = %+v", rows[0])
	}
}

func TestEmbeddingsReserveInputOnly(t *testing.T) {
	r := newRouteRig(t)
	q := routeReq("openai", "gpt-5")
	q.Protocol = domain.ProtocolEmbeddings
	var seen gateway.RouteAuthorized
	if err := r.svc.DoRoute(t.Context(), q, r.invokeRoute(gateway.RouteResult{Started: true}, nil, &seen)); err != nil {
		t.Fatal(err)
	}
	// 100 input tokens x 1 per million + fee.
	if seen.MaxOutputTokens != 0 || r.spend.Settlements[0].Actual.String() != "0.0002" {
		t.Fatalf("seen %+v settlement %+v", seen, r.spend.Settlements)
	}
}

func TestRoutesOnlyOnATeamGatewayForItsNamespace(t *testing.T) {
	r := newRig(t) // the shared gateway: no team namespace, no route store
	if err := r.svc.DoRoute(t.Context(), routeReq("openai", "gpt-5"), nil); kind(err) != domain.KindNotFound {
		t.Fatalf("shared gateway err = %v", err)
	}
	if _, err := r.svc.RouteModels(t.Context(), routeReq("", "").Request, "openai"); kind(err) != domain.KindNotFound {
		t.Fatalf("shared gateway models err = %v", err)
	}
	rr := newRouteRig(t)
	q := routeReq("openai", "gpt-5")
	q.SourceIP = netip.MustParseAddr("10.0.1.1") // a pod of team B's namespace
	if err := rr.svc.DoRoute(t.Context(), q, rr.invokeRoute(routeOK(1, 1), nil, nil)); kind(err) != domain.KindUnauthenticated {
		t.Fatalf("other namespace err = %v", err)
	}
	if err := rr.svc.Do(t.Context(), req("10.0.1.1"), rr.invoke(ok(1, 1), nil)); kind(err) != domain.KindUnauthenticated {
		t.Fatalf("other namespace on a Bedrock path err = %v", err)
	}
}

func TestRouteModels(t *testing.T) {
	r := newRouteRig(t)
	ms, err := r.svc.RouteModels(t.Context(), routeReq("", "").Request, "openai")
	if err != nil || len(ms) != 1 || ms[0].Name != "gpt-5" || ms[0].MaxOutputTokens != 1000 {
		t.Fatalf("models = %+v err %v", ms, err)
	}
	if _, err := r.svc.RouteModels(t.Context(), routeReq("", "").Request, "nous"); kind(err) != domain.KindAccessDenied {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.svc.RouteModels(t.Context(), routeReq("", "").Request, "other"); kind(err) != domain.KindAccessDenied {
		t.Fatalf("err = %v", err)
	}
	r.routes.err = errors.New("down")
	if _, err := r.svc.RouteModels(t.Context(), routeReq("", "").Request, "openai"); kind(err) != domain.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestCachedRoutesInvalidate(t *testing.T) {
	rs := &routeStore{grants: map[string][]gateway.RouteGrant{"agent/a": {{Slug: "x", Model: "m"}}},
		endpoints: map[string]gateway.RouteEndpoint{"x": {ID: "e"}}, prices: map[string][]gateway.RoutePrice{}}
	c := gateway.NewCachedRoutes(rs, 0, nil)
	for range 3 {
		if _, err := c.RouteAdmission(t.Context(), "agent", "a"); err != nil {
			t.Fatal(err)
		}
	}
	if rs.calls != 1 {
		t.Fatalf("calls = %d", rs.calls)
	}
	for _, table := range []string{"endpoint_models", "agents", "endpoint_model_prices", "inference_providers"} {
		c.Invalidate(table, "")
		_, _ = c.RouteAdmission(t.Context(), "agent", "a")
	}
	c.Invalidate("unrelated", "")
	_, _ = c.RouteAdmission(t.Context(), "agent", "a")
	if rs.calls != 5 {
		t.Fatalf("calls = %d", rs.calls)
	}
	c.InvalidateAll()
	if e, err := c.RouteEndpoint(t.Context(), "t", "x"); err != nil || e.ID != "e" {
		t.Fatal(err)
	}
	if _, err := c.RoutePrices(t.Context(), "e", "m"); err != nil {
		t.Fatal(err)
	}
}
