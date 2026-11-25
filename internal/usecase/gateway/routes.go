package gateway

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// RouteGrant is one row of afs.route_admission (database contract §7.4): a model on a route and why the
// caller may not use it, empty when it may.
type RouteGrant struct {
	EndpointID string
	Slug       string
	Model      string
	Reason     string
}

// RouteEndpoint is the part of a model endpoint a team gateway needs to forward a call. It never holds a
// credential: UpstreamAuth supplies that per call.
type RouteEndpoint struct {
	ID              string
	TeamID          string // empty for a service endpoint
	Slug            string
	Provider        string
	Protocols       []domain.Protocol
	Location        domain.EndpointLocation
	BaseURL         string
	CABundle        string
	AuthKind        domain.AuthKind
	AuthHeader      string
	CredentialMode  domain.CredentialMode
	Pricing         domain.PricingMode
	UseProviderCost bool
	Timeout         time.Duration
	MaxBodyBytes    int64
	// Models maps each enabled model name to its maxOutputTokens.
	Models map[string]int
}

// Serves reports whether the endpoint lists the protocol.
func (e RouteEndpoint) Serves(p domain.Protocol) bool {
	for _, x := range e.Protocols {
		if x == p {
			return true
		}
	}
	return false
}

// RoutePrice is an endpoint_model_prices row: USD per million tokens plus a fee per request.
type RoutePrice struct {
	EffectiveFrom time.Time
	Input         domain.Money
	Output        domain.Money
	CachedInput   domain.Money
	RequestFee    domain.Money
}

// RouteStore reads the route state a team gateway needs (afs_gateway role, afs.team_ids set to the
// gateway's team, database contract §7.4).
type RouteStore interface {
	// RouteAdmission returns afs.route_admission for an agent or a pool; ErrNotFound when it does not exist.
	RouteAdmission(ctx context.Context, kind, subjectID string) ([]RouteGrant, error)
	// RouteEndpoint returns the endpoint a team reaches under slug (its own or a service one); ErrNotFound.
	RouteEndpoint(ctx context.Context, teamID, slug string) (RouteEndpoint, error)
	// RoutePrices returns the prices of a model on an endpoint, newest effective_from first.
	RoutePrices(ctx context.Context, endpointID, model string) ([]RoutePrice, error)
}

// Upstream authentication failures (gateway contract §12.5), returned by UpstreamAuth.
var (
	// ErrCredentialMissing means the route has no credential for this team.
	ErrCredentialMissing = errors.New("gateway: route credential missing") //nolint:gosec // an error, not a credential
	// ErrFederationNotReady means the Azure or Google token exchange is not set up or is failing.
	ErrFederationNotReady = errors.New("gateway: federation not ready")
)

// UpstreamCredential is the header the gateway sends upstream in place of the agent's Authorization.
// Value is a secret: it lives in memory for the call and is never logged, stored or returned.
type UpstreamCredential struct {
	Header string
	Value  string
}

// UpstreamAuth supplies the upstream credential of a route for a team: a mounted secret for bearer and
// header routes, an exchanged token for federated ones, nothing for auth kind none.
type UpstreamAuth interface {
	Credential(ctx context.Context, ep RouteEndpoint, teamID string) (UpstreamCredential, error)
}

// RouteUsage is the token usage an OpenAI-compatible response reports.
type RouteUsage struct {
	Input, Output, CachedInput, Reasoning int
	// ProviderCost is usage.cost when the provider reported one (OpenRouter, OpenCode).
	ProviderCost *domain.Money
}

// RouteResult is what a RouteInvokeFunc reports.
type RouteResult struct {
	Usage         RouteUsage
	UsageReported bool
	Started       bool
	HTTPStatus    int
	TTFT          time.Duration
	Upstream      time.Duration
}

// RouteRequest is a parsed route call.
type RouteRequest struct {
	Request
	Route    string
	Model    string
	Protocol domain.Protocol
}

// RouteAuthorized is what a RouteInvokeFunc needs to forward the call.
type RouteAuthorized struct {
	Caller   Caller
	Endpoint RouteEndpoint
	Model    string
	// MaxOutputTokens is the cap to write into the forwarded request; zero for embeddings.
	MaxOutputTokens int
	Credential      UpstreamCredential
	RequestID       string
}

// RouteInvokeFunc forwards a route call and reports what it used.
type RouteInvokeFunc func(ctx context.Context, a RouteAuthorized) (RouteResult, error)

// Usage sources of a route call's row (gateway contract §12.6).
const (
	UsageFromResponse     = "response"
	UsageEstimated        = "estimated"
	UsageFromProviderCost = "provider_cost"
)

// RouteModel is a model the caller may call on a route (GET /r/{route}/v1/models).
type RouteModel struct {
	Name            string
	MaxOutputTokens int
}

func subjectOf(c Caller) (string, string) {
	if c.PoolID != "" {
		return "pool", c.PoolID
	}
	return "agent", c.AgentID
}

// routeLevel are the reasons that hold for every model of a route.
var routeLevel = map[string]bool{
	domain.ReasonProviderDisabled:          true,
	domain.ReasonRouteNotEnabledForService: true,
	domain.ReasonRouteNotEnabledForTeam:    true,
	domain.ReasonRouteNotEnabledForAgent:   true,
}

// AdmitRoute applies gateway contract §12.3 to the admission rows of one caller: the reason the model
// on the route is refused, in the order of CD-41, or "" when it is admitted. A route the caller's team
// cannot see at all is route_not_enabled_for_team, so its existence is not revealed.
func AdmitRoute(grants []RouteGrant, route, model string) string {
	seen := false
	routeReason := ""
	for _, g := range grants {
		if g.Slug != route {
			continue
		}
		seen = true
		if g.Model == model {
			return g.Reason
		}
		if routeLevel[g.Reason] && routeReason == "" {
			routeReason = g.Reason
		}
	}
	switch {
	case !seen:
		return domain.ReasonRouteNotEnabledForTeam
	case routeReason != "":
		return routeReason
	}
	return domain.ReasonModelNotOnRoute
}

var routeMessage = map[string]string{
	domain.ReasonProviderDisabled:          "the route's provider is switched off",
	domain.ReasonRouteNotEnabledForService: "the route is not enabled for the service",
	domain.ReasonRouteNotEnabledForTeam:    "the route is not enabled for the team",
	domain.ReasonRouteNotEnabledForAgent:   "the route is not enabled for the agent",
	domain.ReasonModelNotOnRoute:           "the model is not listed on the route",
	domain.ReasonModelNotPriced:            "the model has no approved price on the route",
}

func (s *Service) refuseRoute(c Caller, route, model, reason string) error {
	if s.ModelDenials != nil {
		s.ModelDenials.ModelDenied(c.TeamID, route+"/"+model, reason)
	}
	s.log.Info("route refused", "team", c.TeamID, "agent", c.AgentID, "pool", c.PoolID, "route", route, "model", model, "reason", reason)
	e := denied(reason, routeMessage[reason])
	e.Details["route"] = route
	if model != "" {
		e.Details["model"] = model
	}
	return e
}

// identifyRoute identifies the caller of a route call on a team gateway: route paths are served only by
// a team gateway (gateway contract §12.1), and it accepts calls only from its own namespace.
func (s *Service) identifyRoute(ctx context.Context, req Request) (Caller, error) {
	if s.Routes == nil || s.cfg.TeamNamespace == "" {
		return Caller{}, domain.NotFound("operation")
	}
	return s.identify(ctx, req)
}

// RouteModels answers GET /r/{route}/v1/models from the caller's effective models on the route.
func (s *Service) RouteModels(ctx context.Context, req Request, route string) ([]RouteModel, error) {
	caller, err := s.identifyRoute(ctx, req)
	if err != nil {
		return nil, err
	}
	pre, cancel := context.WithTimeout(ctx, s.cfg.FailClosedTimeout)
	defer cancel()
	if _, err := s.checkTeam(pre, caller); err != nil {
		return nil, err
	}
	kind, id := subjectOf(caller)
	grants, err := s.Routes.RouteAdmission(pre, kind, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, s.refuseRoute(caller, route, "", domain.ReasonRouteNotEnabledForAgent)
	case err != nil:
		return nil, unavailable("database", err)
	}
	if r := AdmitRoute(grants, route, "\x00"); r != domain.ReasonModelNotOnRoute && r != domain.ReasonModelNotPriced {
		return nil, s.refuseRoute(caller, route, "", r)
	}
	ep, err := s.Routes.RouteEndpoint(pre, caller.TeamID, route)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, s.refuseRoute(caller, route, "", domain.ReasonRouteNotEnabledForTeam)
	case err != nil:
		return nil, unavailable("database", err)
	}
	var out []RouteModel
	for _, g := range grants {
		if g.Slug == route && g.Reason == "" {
			out = append(out, RouteModel{Name: g.Model, MaxOutputTokens: ep.Models[g.Model]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DoRoute runs one route call (gateway contract §12): identify, admit, authenticate upstream, reserve on
// a per-token route, forward, settle and record. It returns nil when the call succeeded and otherwise a
// *domain.Error the adapter maps to §7 and §12.5.
func (s *Service) DoRoute(ctx context.Context, req RouteRequest, invoke RouteInvokeFunc) error {
	started := s.cfg.Now()
	caller, err := s.identifyRoute(ctx, req.Request)
	if err != nil {
		return err
	}
	pre, cancel := context.WithTimeout(ctx, s.cfg.FailClosedTimeout)
	defer cancel()
	tagSet, _ := s.store.TagSetID(pre, caller.TeamID)
	rr := routeRecorder{s: s, c: caller, req: req, started: started, tagSet: tagSet}

	if _, err := s.checkTeam(pre, caller); err != nil {
		if domain.KindOf(err) == domain.KindAccessDenied {
			rr.rec(OutcomeError, RouteResult{HTTPStatus: 403}, domain.Money{}, "", "")
		}
		return err
	}
	kind, id := subjectOf(caller)
	grants, err := s.Routes.RouteAdmission(pre, kind, id)
	switch {
	case errors.Is(err, ErrNotFound):
		grants = nil
	case err != nil:
		return unavailable("database", err)
	}
	if reason := AdmitRoute(grants, req.Route, req.Model); reason != "" {
		rr.rec(OutcomeRejectedModel, RouteResult{HTTPStatus: 403}, domain.Money{}, endpointOf(grants, req.Route), "")
		return s.refuseRoute(caller, req.Route, req.Model, reason)
	}
	ep, err := s.Routes.RouteEndpoint(pre, caller.TeamID, req.Route)
	switch {
	case errors.Is(err, ErrNotFound):
		return s.refuseRoute(caller, req.Route, req.Model, domain.ReasonRouteNotEnabledForTeam)
	case err != nil:
		return unavailable("database", err)
	}
	if !ep.Serves(req.Protocol) {
		return domain.NotFound("operation")
	}
	rr.provider = ep.Provider
	cred, err := s.Upstreams.Credential(pre, ep, caller.TeamID)
	switch {
	case errors.Is(err, ErrCredentialMissing):
		return &domain.Error{Kind: domain.KindUnavailable, Message: "the route has no credential for this team",
			Reason: domain.ReasonCredentialMissing, Details: map[string]any{"reason": domain.ReasonCredentialMissing, "route": ep.Slug}, RetryAfterSeconds: 30}
	case errors.Is(err, ErrFederationNotReady):
		return &domain.Error{Kind: domain.KindUnavailable, Message: "the route's identity federation is not ready",
			Reason: domain.ReasonFederationNotReady, Details: map[string]any{"reason": domain.ReasonFederationNotReady, "route": ep.Slug}, RetryAfterSeconds: 30}
	case err != nil:
		return unavailable("upstream credential", err)
	}

	cap := 0
	if req.Protocol != domain.ProtocolEmbeddings {
		cap = OutputCap(req.MaxTokens, ep.Models[req.Model])
	}
	var price RoutePrice
	var amount domain.Money
	var budget BudgetRecord
	var hold Hold
	var period time.Time
	if ep.Pricing.Capped() {
		prices, err := s.Routes.RoutePrices(pre, ep.ID, req.Model)
		if err != nil {
			return unavailable("database", err)
		}
		p, ok := EffectiveRoutePrice(prices, started)
		if !ok {
			rr.rec(OutcomeRejectedModel, RouteResult{HTTPStatus: 403}, domain.Money{}, ep.ID, "")
			return s.refuseRoute(caller, req.Route, req.Model, domain.ReasonModelNotPriced)
		}
		price = p
		amount = RouteReserveAmount(p, cap, req.InputBytes)
		rec := func(o Outcome, _ Usage, c domain.Money, _ string) {
			rr.rec(o, RouteResult{HTTPStatus: 402}, c, ep.ID, "")
		}
		budget, hold, period, err = s.reserve(pre, caller, req.Request, amount, rec, tagSet)
		if err != nil {
			return err
		}
	}

	res, callErr := invoke(ctx, RouteAuthorized{Caller: caller, Endpoint: ep, Model: req.Model, MaxOutputTokens: cap,
		Credential: cred, RequestID: req.RequestID})

	closeBooks := func() {
		sctx := ctx
		if s.cfg.AsyncSettle {
			sctx = context.WithoutCancel(ctx)
		}
		actual, source := domain.MoneyFromMicros(0), UsageEstimated
		if res.UsageReported {
			source = UsageFromResponse
		}
		if ep.Pricing.Capped() {
			actual, source = RouteSettleAmount(res, callErr, price, amount)
		}
		ctr := s.settle(sctx, caller, budget, period, hold, actual)
		outcome := OutcomeOK
		if callErr != nil {
			outcome = OutcomeError
		}
		rr.rec(outcome, res, actual, ep.ID, source)
		if ctr != nil {
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SettleTimeout)
			defer cancel()
			s.recordThresholds(tctx, caller, budget, period, *ctr)
		}
	}
	if s.cfg.AsyncSettle {
		s.wg.Go(closeBooks)
		return asCallError(callErr)
	}
	closeBooks()
	return asCallError(callErr)
}

func endpointOf(grants []RouteGrant, route string) string {
	for _, g := range grants {
		if g.Slug == route {
			return g.EndpointID
		}
	}
	return ""
}

// routeRecorder writes the usage row of a route call.
type routeRecorder struct {
	s       *Service
	c       Caller
	req     RouteRequest
	started time.Time
	tagSet  string
	// provider is set once the endpoint is known.
	provider string
}

func (r routeRecorder) rec(o Outcome, res RouteResult, cost domain.Money, endpointID, source string) {
	if r.s.Usage == nil {
		return
	}
	now := r.s.cfg.Now()
	row := record(r.c, r.req.Request, o, Usage{Input: res.Usage.Input, Output: res.Usage.Output, CacheRead: res.Usage.CachedInput},
		cost, r.started, now, r.tagSet)
	row.ModelID = ""
	row.Route = &RouteFields{EndpointID: endpointID, Model: r.req.Model, Slug: r.req.Route, Provider: r.provider, Protocol: r.req.Protocol,
		Source: source, Reasoning: res.Usage.Reasoning,
		TTFTMS: ms(res.TTFT), UpstreamMS: ms(res.Upstream), HTTPStatus: res.HTTPStatus}
	total := now.Sub(r.started)
	if g := total - res.Upstream; g > 0 {
		row.Route.GatewayMS = ms(g)
	}
	if source == "" && o == OutcomeOK {
		row.Route.Source = UsageEstimated
	}
	r.s.Usage.Record(row)
}

func ms(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Millisecond)
}

// RouteFields are the usage_events columns of a route call (gateway contract §12.6).
type RouteFields struct {
	EndpointID string
	Slug       string
	Model      string
	// Provider and Protocol label telemetry only; usage_events does not store them.
	Provider   string
	Protocol   domain.Protocol
	Source     string
	Reasoning  int
	TTFTMS     int
	UpstreamMS int
	GatewayMS  int
	HTTPStatus int
}

// EffectiveRoutePrice picks the price in effect at t from rows newest first.
func EffectiveRoutePrice(rows []RoutePrice, t time.Time) (RoutePrice, bool) {
	for _, r := range rows {
		if !r.EffectiveFrom.After(t) {
			return r, true
		}
	}
	return RoutePrice{}, false
}

func asPrice(p RoutePrice) Price {
	return Price{EffectiveFrom: p.EffectiveFrom, Input: p.Input, Output: p.Output, CacheRead: p.CachedInput}
}

// RouteReserveAmount is the worst case of a per-token route call: the output cap at the output price,
// the body estimate at the input price, plus the request fee.
func RouteReserveAmount(p RoutePrice, outputCap, bodyBytes int) domain.Money {
	return ReserveAmount(asPrice(p), outputCap, bodyBytes).Add(p.RequestFee)
}

// RouteActualCost is the token-price cost of reported usage: cached input at the cached price (the
// input price when none is set), the rest at the input and output prices, plus the request fee.
func RouteActualCost(p RoutePrice, u RouteUsage) domain.Money {
	cached := u.CachedInput
	if cached > u.Input {
		cached = u.Input
	}
	cp := p.CachedInput
	if cp.Micros() == 0 {
		cp = p.Input
	}
	return ActualCost(Price{Input: p.Input, Output: p.Output, CacheRead: cp},
		Usage{Input: u.Input - cached, Output: u.Output, CacheRead: cached}).Add(p.RequestFee)
}

// RouteSettleAmount is the settled cost of a per-token route call and its usage_source (§12.6): the
// rules of SettleAmount on the route price; when the provider reported usage.cost, the larger of that
// and the token-price cost. Every upstream failure before the answer began releases the reservation.
func RouteSettleAmount(res RouteResult, callErr error, p RoutePrice, reserved domain.Money) (domain.Money, string) {
	switch {
	case callErr != nil && !res.Started:
		return domain.MoneyFromMicros(0), UsageEstimated
	case !res.UsageReported:
		return reserved, UsageEstimated
	}
	cost := RouteActualCost(p, res.Usage)
	if pc := res.Usage.ProviderCost; pc != nil && pc.Cmp(cost) > 0 {
		return *pc, UsageFromProviderCost
	}
	return cost, UsageFromResponse
}
