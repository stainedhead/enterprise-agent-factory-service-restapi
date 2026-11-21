package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// pipeline is what wirePipeline builds for the authentication wiring and for the use cases.
type pipeline struct {
	Limiter *middleware.Limiter
	Auditor *middleware.DeniedAuditor
}

// routeInfo adapts the route table to the middleware package.
func routeInfo(r *http.Request) (middleware.RouteInfo, bool) {
	rt, ok := httpapi.RouteFrom(r)
	if !ok {
		return middleware.RouteInfo{}, false
	}
	return middleware.RouteInfo{OperationID: rt.OperationID, Method: rt.Method, Public: rt.Scope == authz.ScopePublic,
		Idempotent: rt.Idempotent, Mutation: rt.Mutation}, true
}

// sessionFromContext is the database session of the authenticated caller of a request.
func sessionFromContext(ctx context.Context) (port.Session, bool) {
	p, ok := httpapi.PrincipalFrom(ctx)
	if !ok {
		return port.Session{}, false
	}
	return sessionOf(p, httpapi.RequestIDFrom(ctx)), true
}

// wirePipeline installs the request pipeline of M1-D into cfg: request metadata and CORS around the
// router, the public-route source limit before authentication, and after authorization the denied
// audit, If-Match and ETag rules and Idempotency-Key handling (architecture.md §1). It sets c.Tx, the
// transaction runner every use case of a creating POST or :verb action must be built on, because it
// claims the Idempotency-Key inside the use case's own transaction.
func wirePipeline(c *components, cfg *httpapi.Config, s settings, log *slog.Logger) (*pipeline, error) {
	required, err := middleware.IfMatchRules(api.OpenAPI)
	if err != nil {
		return nil, err
	}
	p := &pipeline{
		Limiter: middleware.NewLimiter(middleware.LimiterConfig{Replicas: s.APIReplicas}),
		Auditor: &middleware.DeniedAuditor{Audit: c.Audit, Session: auth.SessionOf, RequestID: httpapi.RequestIDFrom, Log: log},
	}
	idem := &middleware.Idempotency{Store: c.Idem, Runner: c.Runner, Session: sessionFromContext,
		Routes: routeInfo, Errors: httpapi.WriteError, Log: log}
	c.Tx = idem.WrapRunner(c.Runner)

	origins := func(ctx context.Context) ([]string, error) {
		raw, err := c.Settings.Setting(ctx, domain.SettingAPICORSOrigins)
		if err != nil {
			return nil, err
		}
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		return list, nil
	}
	cfg.Outer = append(cfg.Outer, middleware.WithRequestMeta(s.TrustedProxyHops), middleware.CORS(origins, nil, log))
	cfg.PreAuth = append(cfg.PreAuth, middleware.PublicLimit(p.Limiter, routeInfo, httpapi.WriteError))
	cfg.PostAuth = append(cfg.PostAuth,
		p.Auditor.Stage(routeInfo, httpapi.PrincipalFrom),
		middleware.RequireIfMatch(required, routeInfo, httpapi.WriteError),
		middleware.AutoETag(),
		idem.Middleware(),
	)
	cfg.Hooks.Denied = func(ctx context.Context, d httpapi.Denial) {
		p.Auditor.Record(ctx, middleware.Denied{Principal: d.Principal, OperationID: d.OperationID, TeamID: d.TeamID, Required: d.RequiredPermission})
	}
	cfg.Hooks.CrossTeamNotFound = p.Auditor.CrossTeamNotFound
	return p, nil
}
