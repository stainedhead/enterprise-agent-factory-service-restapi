package gatewayhttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// RouteUpstream sends a route call to the endpoint's base URL (adapter routeup). It returns the raw
// response; a transport failure is an error, a timeout one that satisfies IsTimeout.
type RouteUpstream interface {
	Do(ctx context.Context, ep gateway.RouteEndpoint, req *http.Request) (*http.Response, error)
}

// ErrUpstreamTimeout marks an upstream call that ran out of time (504 UpstreamTimeout).
var ErrUpstreamTimeout = errors.New("gatewayhttp: upstream timeout")

// routeOp is a parsed route path.
type routeOp struct {
	route    string // "" for the body form, the bedrock route for /r/bedrock
	protocol domain.Protocol
	suffix   string // path below /v1
	models   bool
	bodyForm bool
}

var routeSuffixes = map[string]domain.Protocol{
	"/chat/completions": domain.ProtocolChat,
	"/responses":        domain.ProtocolResponses,
	"/embeddings":       domain.ProtocolEmbeddings,
}

// parseRoutePath reads /r/{route}/v1/... and /openai/v1/... (gateway contract §12.2).
func parseRoutePath(method, escaped string) (routeOp, bool) {
	if rest, ok := strings.CutPrefix(escaped, "/openai/v1"); ok {
		p, ok := routeSuffixes[rest]
		return routeOp{protocol: p, suffix: rest, bodyForm: true}, ok && method == http.MethodPost
	}
	rest, ok := strings.CutPrefix(escaped, "/r/")
	if !ok {
		return routeOp{}, false
	}
	slug, sub, ok := strings.Cut(rest, "/v1")
	if !ok || (slug != domain.RouteBedrock && domain.ValidRouteSlug(slug) != nil) {
		return routeOp{}, false
	}
	if sub == "/models" {
		return routeOp{route: slug, models: true}, method == http.MethodGet
	}
	p, ok := routeSuffixes[sub]
	if !ok || method != http.MethodPost {
		return routeOp{}, false
	}
	if slug == domain.RouteBedrock && p != domain.ProtocolChat {
		return routeOp{}, false // Responses and Embeddings on bedrock: 404 in 1.2.0
	}
	return routeOp{route: slug, protocol: p, suffix: sub}, true
}

// RoutesFamily serves the OpenAI-compatible route paths of a team gateway.
func (s *Server) RoutesFamily() Family {
	return FamilyFunc(func(m *Mux) {
		h := s.wrap(s.serveRoute)
		m.Handle("/r/", h)
		m.Handle("/openai/", h)
	})
}

func (s *Server) serveRoute(rw *respWriter, r *http.Request) {
	op, ok := parseRoutePath(r.Method, r.URL.EscapedPath())
	if !ok {
		rw.writeProblem(domain.NotFound("operation"), 0, false)
		return
	}
	if op.route == domain.RouteBedrock && !op.models {
		s.call(rw, r, route{chat: true})
		return
	}
	start := time.Now()
	ip, err := sourceIP(r)
	if err != nil {
		rw.writeProblem(domain.Unauthenticated(), 0, false)
		return
	}
	token := r.Header.Get(TokenHeader)
	base := gateway.Request{SourceIP: ip, Token: token, RequestID: rw.requestID}
	if op.models {
		s.listRouteModels(rw, r, base, op.route)
		return
	}
	if _, err := s.svc.Authenticate(r.Context(), ip, token); err != nil {
		s.routeDone(rw, op, err, start, 0)
		return
	}
	body, ok := s.readBody(rw, r, false)
	if !ok {
		return
	}
	call, err := parseOpenAI(op, body)
	if err != nil {
		rw.writeProblem(err, 0, false)
		return
	}
	base.MaxTokens, base.InputBytes, base.Op = call.maxTokens, len(body), gateway.Operation("route:"+string(op.protocol))
	ctx, span := startRouteSpan(r, op.protocol, call.route, call.model)
	defer span.End()
	var upstream time.Duration
	err = s.svc.DoRoute(ctx, gateway.RouteRequest{Request: base, Route: call.route, Model: call.model, Protocol: op.protocol},
		func(ctx context.Context, a gateway.RouteAuthorized) (gateway.RouteResult, error) {
			annotateRouteSpan(span, a)
			res, err := s.forward(ctx, rw, r, op, call, a)
			upstream = res.Upstream
			endRouteSpan(span, res, err)
			return res, err
		})
	if err != nil {
		span.SetStatus(codes.Error, string(domain.KindOf(err)))
	}
	s.routeDone(rw, op, err, start, upstream)
}

func (s *Server) routeDone(rw *respWriter, op routeOp, err error, start time.Time, upstream time.Duration) {
	rt := route{op: gateway.Operation("route:" + string(op.protocol))}
	if err == nil {
		s.done(rt, rw.status, start, upstream)
		return
	}
	s.finish(rw, rt, err, start, upstream)
}

// listRouteModels answers GET /r/{route}/v1/models (OpenAI List models).
func (s *Server) listRouteModels(rw *respWriter, r *http.Request, base gateway.Request, slug string) {
	if slug == domain.RouteBedrock {
		rw.writeProblem(domain.NotFound("operation"), 0, false)
		return
	}
	ms, err := s.svc.RouteModels(r.Context(), base, slug)
	if err != nil {
		rw.writeProblem(err, 0, false)
		return
	}
	data := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		data = append(data, map[string]any{"id": m.Name, "object": "model", "created": 0, "owned_by": slug, "max_output_tokens": m.MaxOutputTokens})
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]any{"object": "list", "data": data})
}

// openAICall is a parsed route request body.
type openAICall struct {
	route, model string
	maxTokens    int
	stream       bool
	includeUsage bool
	doc          map[string]json.RawMessage
}

// capField is where each protocol carries its output limit.
var capField = map[domain.Protocol]string{domain.ProtocolChat: "max_completion_tokens", domain.ProtocolResponses: "max_output_tokens"}

func parseOpenAI(op routeOp, body []byte) (openAICall, error) {
	doc, err := parseDoc(body)
	if err != nil {
		return openAICall{}, err
	}
	c := openAICall{route: op.route, doc: doc}
	var model string
	if json.Unmarshal(doc["model"], &model) != nil || model == "" {
		return c, badRequest("model", "is required")
	}
	if op.bodyForm {
		r, m, ok := domain.ParseRouteModel(model)
		if !ok {
			return c, badRequest("model", "must be <route>/<model> on /openai/v1")
		}
		c.route, model = r, m
		if c.route == domain.RouteBedrock {
			return c, badRequest("model", "the bedrock route is served at /r/bedrock/v1/chat/completions")
		}
	}
	c.model = model
	if raw, ok := doc["stream"]; ok {
		_ = json.Unmarshal(raw, &c.stream)
	}
	if raw, ok := doc["stream_options"]; ok {
		var so struct {
			IncludeUsage bool `json:"include_usage"`
		}
		_ = json.Unmarshal(raw, &so)
		c.includeUsage = so.IncludeUsage
	}
	switch op.protocol {
	case domain.ProtocolChat:
		for _, k := range []string{"max_completion_tokens", "max_tokens"} {
			if n, ok := pathInt(doc, k); ok && c.maxTokens == 0 {
				c.maxTokens = n
			}
		}
	case domain.ProtocolResponses:
		c.maxTokens, _ = pathInt(doc, "max_output_tokens")
	}
	return c, nil
}

// upstreamBody is the agent's body with the route's model name, the reserved cap and, for a chat
// stream, include_usage (gateway contract §12.2).
func (c openAICall) upstreamBody(p domain.Protocol, a gateway.RouteAuthorized) ([]byte, error) {
	doc := make(map[string]json.RawMessage, len(c.doc)+2)
	for k, v := range c.doc {
		doc[k] = v
	}
	mb, _ := json.Marshal(a.Model)
	doc["model"] = mb
	if f := capField[p]; f != "" && a.MaxOutputTokens > 0 {
		doc[f] = json.RawMessage(strconv.Itoa(a.MaxOutputTokens))
		if p == domain.ProtocolChat {
			delete(doc, "max_tokens") // the reserved cap replaces it; sending both is refused by OpenAI
		}
	}
	if c.stream && p == domain.ProtocolChat {
		so := map[string]json.RawMessage{}
		if raw, ok := doc["stream_options"]; ok {
			_ = json.Unmarshal(raw, &so)
		}
		so["include_usage"] = json.RawMessage("true")
		b, _ := json.Marshal(so)
		doc["stream_options"] = b
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, domain.Internal(err)
	}
	return b, nil
}

// propagatesTrace reports whether traceparent may leave the cluster towards ep (gateway contract §12.7).
func propagatesTrace(ep gateway.RouteEndpoint) bool {
	return ep.Location == domain.LocationCorporate || ep.Location == domain.LocationInNamespace
}

// forward sends the call upstream and relays the answer.
func (s *Server) forward(ctx context.Context, rw *respWriter, in *http.Request, op routeOp, c openAICall, a gateway.RouteAuthorized) (gateway.RouteResult, error) {
	body, err := c.upstreamBody(op.protocol, a)
	if err != nil {
		return gateway.RouteResult{}, err
	}
	if a.Endpoint.MaxBodyBytes > 0 && int64(len(body)) > a.Endpoint.MaxBodyBytes {
		return gateway.RouteResult{}, badRequest("body", "is larger than the route's limit")
	}
	url := strings.TrimSuffix(a.Endpoint.BaseURL, "/") + op.suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return gateway.RouteResult{}, domain.Internal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", firstNonEmpty(in.Header.Get("Accept"), "application/json"))
	if a.Credential.Header != "" {
		req.Header.Set(a.Credential.Header, a.Credential.Value)
	}
	if propagatesTrace(a.Endpoint) {
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
		if tp := in.Header.Get("Traceparent"); tp != "" && req.Header.Get("Traceparent") == "" {
			req.Header.Set("Traceparent", tp) // no tracer configured: continue the caller's trace as is
		}
	}
	t0 := time.Now()
	resp, err := s.routes.Do(ctx, a.Endpoint, req)
	if err != nil {
		res := gateway.RouteResult{Upstream: time.Since(t0)}
		switch {
		case ctx.Err() != nil:
			return res, context.Canceled
		case errors.Is(err, ErrUpstreamTimeout):
			res.HTTPStatus = http.StatusGatewayTimeout
			return res, &domain.Error{Kind: domain.KindUpstreamTimeout, Message: "the model endpoint did not answer in time"}
		}
		res.HTTPStatus = http.StatusBadGateway
		return res, &domain.Error{Kind: domain.KindUpstream, Message: "the model endpoint could not be reached", Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		res := gateway.RouteResult{Upstream: time.Since(t0), HTTPStatus: resp.StatusCode}
		return res, routeStatusError(resp)
	}
	if c.stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return relaySSE(ctx, rw, resp, c, op.protocol, t0)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	res := gateway.RouteResult{Started: true, Upstream: time.Since(t0), HTTPStatus: http.StatusOK}
	res.TTFT = res.Upstream
	if err != nil {
		return res, &domain.Error{Kind: domain.KindUpstream, Message: "the model answer was cut", Cause: err}
	}
	res.Usage, res.UsageReported = routeUsageOf(b)
	rw.Header().Set("Content-Type", firstNonEmpty(resp.Header.Get("Content-Type"), "application/json"))
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(b)
	return res, nil
}

// relaySSE passes an upstream event stream through, meters its usage and drops the usage-only chunk a
// client that did not ask for include_usage would not expect.
func relaySSE(ctx context.Context, rw *respWriter, resp *http.Response, c openAICall, p domain.Protocol, t0 time.Time) (gateway.RouteResult, error) {
	res := gateway.RouteResult{HTTPStatus: http.StatusOK}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("X-Accel-Buffering", "no")
	rw.WriteHeader(http.StatusOK)
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	var event bytes.Buffer
	skip := false
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if data, ok := bytes.CutPrefix(trimmed, []byte("data:")); ok {
				data = bytes.TrimSpace(data)
				if !res.Started {
					res.Started, res.TTFT = true, time.Since(t0)
				}
				if u, ok := routeUsageOf(data); ok {
					res.Usage, res.UsageReported = u, true
					if p == domain.ProtocolChat && !c.includeUsage && usageOnlyChunk(data) {
						skip = true
					}
				}
			}
			event.Write(line)
			if len(trimmed) == 0 {
				if !skip {
					if _, werr := rw.Write(event.Bytes()); werr != nil {
						res.Upstream = time.Since(t0)
						return res, errClientWrite
					}
					rw.flush()
				}
				event.Reset()
				skip = false
			}
		}
		if err != nil {
			res.Upstream = time.Since(t0)
			if event.Len() > 0 && !skip {
				_, _ = rw.Write(event.Bytes())
				rw.flush()
			}
			if errors.Is(err, io.EOF) {
				return res, nil
			}
			if ctx.Err() != nil {
				return res, context.Canceled
			}
			return res, &domain.Error{Kind: domain.KindUpstream, Message: "the model stream was cut", Cause: err}
		}
	}
}

func usageOnlyChunk(data []byte) bool {
	var c struct {
		Choices []json.RawMessage `json:"choices"`
	}
	return json.Unmarshal(data, &c) == nil && len(c.Choices) == 0
}

type openAIUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	InputTokens      *int `json:"input_tokens"`
	OutputTokens     *int `json:"output_tokens"`
	PromptDetails    *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	Cost *json.Number `json:"cost"`
}

// routeUsageOf reads the usage of a Chat, Responses or Embeddings answer (or stream event): top-level
// usage, or response.usage of a Responses event.
func routeUsageOf(b []byte) (gateway.RouteUsage, bool) {
	var d struct {
		Usage    *openAIUsage `json:"usage"`
		Response *struct {
			Usage *openAIUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(b, &d) != nil {
		return gateway.RouteUsage{}, false
	}
	u := d.Usage
	if u == nil && d.Response != nil {
		u = d.Response.Usage
	}
	if u == nil {
		return gateway.RouteUsage{}, false
	}
	var out gateway.RouteUsage
	out.Input = val(firstPtr(u.PromptTokens, u.InputTokens))
	out.Output = val(firstPtr(u.CompletionTokens, u.OutputTokens))
	if u.PromptTokens == nil && u.InputTokens == nil && u.CompletionTokens == nil && u.OutputTokens == nil {
		return gateway.RouteUsage{}, false
	}
	switch {
	case u.PromptDetails != nil:
		out.CachedInput = u.PromptDetails.CachedTokens
	case u.InputDetails != nil:
		out.CachedInput = u.InputDetails.CachedTokens
	}
	switch {
	case u.CompletionDetails != nil:
		out.Reasoning = u.CompletionDetails.ReasoningTokens
	case u.OutputDetails != nil:
		out.Reasoning = u.OutputDetails.ReasoningTokens
	}
	if u.Cost != nil {
		if m, err := domain.ParseMoney("usage.cost", roundUSD(u.Cost.String())); err == nil && !m.IsNegative() {
			out.ProviderCost = &m
		}
	}
	return out, true
}

func firstPtr(a, b *int) *int {
	if a != nil {
		return a
	}
	return b
}

// roundUSD rounds a decimal string up to six decimal places (micro-dollars), the precision of Money.
func roundUSD(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	micros := int64(f * 1e6)
	if float64(micros) < f*1e6 {
		micros++
	}
	return domain.MoneyFromMicros(micros).String()
}

// routeStatusError maps an upstream error status (gateway contract §12.5): 402 is the provider's empty
// prepaid balance, a 429 that names a budget or quota is the provider's own spending limit, other 429s
// pass through as Throttled, 5xx and authentication failures are 502, and other 4xx keep their status
// with the provider's message as a ValidationError.
func routeStatusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &body)
	low := strings.ToLower(body.Error.Type + " " + toString(body.Error.Code) + " " + body.Error.Message)
	switch {
	case resp.StatusCode == http.StatusPaymentRequired:
		return &domain.Error{Kind: domain.KindUnavailable, Message: "the provider's prepaid balance is empty", Reason: domain.ReasonProviderBalanceExhausted,
			Details: map[string]any{"reason": domain.ReasonProviderBalanceExhausted}, RetryAfterSeconds: 300}
	case resp.StatusCode == http.StatusTooManyRequests && (strings.Contains(low, "budget") || strings.Contains(low, "insufficient_quota") || strings.Contains(low, "spend")):
		return &domain.Error{Kind: domain.KindThrottled, Message: "the provider's spending limit refused the call", Reason: domain.ReasonProviderBudgetExhausted,
			Details: map[string]any{"reason": domain.ReasonProviderBudgetExhausted}, RetryAfterSeconds: retryAfter(resp, 300)}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &domain.Error{Kind: domain.KindThrottled, Message: "the model endpoint is throttling requests", RetryAfterSeconds: retryAfter(resp, 1)}
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusGatewayTimeout:
		return &domain.Error{Kind: domain.KindUpstreamTimeout, Message: "the model endpoint did not answer in time"}
	case resp.StatusCode >= 500, resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return &domain.Error{Kind: domain.KindUpstream, Message: "the model endpoint returned an error",
			Cause: errors.New("route upstream status " + strconv.Itoa(resp.StatusCode))}
	}
	msg := body.Error.Message
	if msg == "" {
		msg = "the model endpoint refused the request"
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return &domain.Error{Kind: domain.KindValidation, Message: msg, Fields: []domain.FieldError{{Field: "body", Reason: msg}}}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func retryAfter(resp *http.Response, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		return n
	}
	return def
}

// startRouteSpan starts the GenAI client span of a route call, continuing the caller's traceparent
// (gateway contract §12.7). No prompt or completion content is ever put on it.
func startRouteSpan(r *http.Request, p domain.Protocol, route, model string) (context.Context, trace.Span) {
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	op := "chat"
	if p == domain.ProtocolEmbeddings {
		op = "embeddings"
	}
	return otel.Tracer("afs-gateway").Start(ctx, op+" "+model, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("gen_ai.operation.name", op), attribute.String("gen_ai.request.model", model),
			attribute.String("afs.route", route)))
}

// annotateRouteSpan adds the caller and the endpoint; agent and pool ids go on spans, never on metrics.
func annotateRouteSpan(span trace.Span, a gateway.RouteAuthorized) {
	span.SetAttributes(attribute.String("afs.team_id", a.Caller.TeamID), attribute.String("afs.provider", a.Endpoint.Provider),
		attribute.String("gen_ai.provider.name", a.Endpoint.Provider))
	if a.Caller.AgentID != "" {
		span.SetAttributes(attribute.String("afs.agent_id", a.Caller.AgentID))
	}
	if a.Caller.PoolID != "" {
		span.SetAttributes(attribute.String("afs.pool_id", a.Caller.PoolID))
	}
}

func endRouteSpan(span trace.Span, res gateway.RouteResult, err error) {
	source := "estimated"
	if res.UsageReported {
		source = "response"
	}
	span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", res.Usage.Input), attribute.Int("gen_ai.usage.output_tokens", res.Usage.Output),
		attribute.Int("http.response.status_code", res.HTTPStatus), attribute.String("afs.usage_source", source))
	if err != nil {
		span.SetStatus(codes.Error, string(domain.KindOf(err)))
	}
}
