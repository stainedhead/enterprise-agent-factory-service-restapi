package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// modelState reads, for an agent ("agent") or a pool ("pool"), why its default model is refused (empty when the
// default is among the effective models) and the ModelUnavailable status the last reconcile wrote (nil when
// there is none). The reason comes from afs.model_admission, the same first-failing-level the gateway reports.
func modelState(ctx context.Context, t *Tx, subjectKind, id, defaultModel string, effective []string) (reason string, prev *bool, err error) {
	if route, model, ok := domain.ParseRouteModel(defaultModel); ok && !slices.Contains(effective, defaultModel) {
		var r *string
		err := t.QueryRow(ctx, `SELECT reason FROM afs.route_admission($1, $2) WHERE slug = $3 AND model_name = $4`, subjectKind, id, route, model).Scan(&r)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			reason = domain.ReasonModelNotOnRoute
		case err != nil:
			return "", nil, MapError(err, OpRead, "route admission")
		case r != nil && *r != "":
			reason = *r
		default:
			reason = domain.ReasonModelNotEnabledForAgent
		}
	} else if !slices.Contains(effective, defaultModel) {
		var r *string
		err := t.QueryRow(ctx, `SELECT reason FROM afs.model_admission($1, $2) WHERE model_id = $3`, subjectKind, id, defaultModel).Scan(&r)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			reason = domain.ReasonModelNotEnabledForService // the model is not in the catalog
		case err != nil:
			return "", nil, MapError(err, OpRead, "model admission")
		case r != nil && *r != "":
			reason = *r
		default:
			reason = domain.ReasonModelNotEnabledForAgent // admitted but outside the view: a change racing the read
		}
	}
	kind := "agent"
	if subjectKind == "pool" {
		kind = "pool"
	}
	var conds []byte
	err = t.QueryRow(ctx, `SELECT conditions FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, kind, id).Scan(&conds)
	if errors.Is(err, pgx.ErrNoRows) {
		return reason, nil, nil
	}
	if err != nil {
		return "", nil, MapError(err, OpRead, "resource status")
	}
	var cs []domain.Condition
	if json.Unmarshal(conds, &cs) == nil {
		for _, c := range cs {
			if c.Type == "ModelUnavailable" {
				on := c.Status == domain.ConditionTrue
				prev = &on
			}
		}
	}
	return reason, prev, nil
}

// loadRouteSQL lists a subject's effective route models with what llm-routes.json shows of each route.
const loadRouteSQL = `SELECT r.slug, e.protocols, e.pricing_mode, r.model_name, m.max_output_tokens
  FROM afs.effective_agent_routes r
  JOIN afs.model_endpoints e ON e.id = r.endpoint_id
  JOIN afs.endpoint_models m ON m.endpoint_id = r.endpoint_id AND m.model_name = r.model_name
 WHERE r.subject_kind = $1 AND r.subject_id = $2 ORDER BY r.slug, r.model_name`

// loadRoutes fills the subject's effective routes and the Bedrock switch (gateway contract §12.4). When
// routes.DefaultRoute is set, model is the route's model name (CD-42); the returned default is then
// <route>/<model>, the AFS_DEFAULT_MODEL value, else model unchanged.
func loadRoutes(ctx context.Context, t *Tx, kind, id, model string, routes *agent.LLMRoutes) (string, error) {
	if err := t.QueryRow(ctx, `SELECT afs.provider_enabled('bedrock')`).Scan(&routes.BedrockOn); err != nil {
		return "", MapError(err, OpRead, "provider")
	}
	idx := map[string]int{}
	err := collect(ctx, t, "routes", loadRouteSQL, []any{kind, id}, func(rows pgx.Rows) error {
		var slug, pricing, name string
		var protocols []string
		var maxOut int
		if err := rows.Scan(&slug, &protocols, &pricing, &name, &maxOut); err != nil {
			return err
		}
		i, ok := idx[slug]
		if !ok {
			i = len(routes.Routes)
			idx[slug] = i
			routes.Routes = append(routes.Routes, agent.RouteSpec{Slug: slug, Protocols: protocols, Pricing: pricing})
		}
		routes.Routes[i].Models = append(routes.Routes[i].Models, agent.RouteModel{Name: name, MaxOutputTokens: maxOut})
		return nil
	})
	if err != nil || routes.DefaultRoute == "" {
		return model, err
	}
	routes.DefaultModel = model
	return routes.DefaultRoute + "/" + model, nil
}

// loadMcp fills the subject's effective MCP servers and mode (gateway contract §13.5): the servers of
// afs.effective_agent_mcp, which runs with the reconciler's rights, and the subject's mode or the service default.
// Nothing while mcp.enabled is off.
func loadMcp(ctx context.Context, t *Tx, kind, id string, m *agent.McpAccess) error {
	table := "agents"
	if kind == "pool" {
		table = "agent_pools"
	}
	var on bool
	err := t.QueryRow(ctx, `SELECT coalesce((afs.mcp_setting('mcp.enabled'))::boolean, false),
  coalesce((SELECT x.mcp_mode FROM afs.`+table+` x WHERE x.id = $1), afs.mcp_setting('mcp.default_agent_mode') #>> '{}', 'search')`, id).
		Scan(&on, &m.Mode)
	if err != nil || !on {
		return MapError(err, OpRead, "MCP access")
	}
	return collect(ctx, t, "MCP access", `SELECT DISTINCT e.slug, e.server_kind, e.mode = 'read_only' FROM afs.effective_agent_mcp e
 WHERE e.subject_kind = $1 AND e.subject_id = $2 ORDER BY 1`, []any{kind, id}, func(rows pgx.Rows) error {
		var s agent.McpServerRef
		err := rows.Scan(&s.Slug, &s.Kind, &s.ReadOnly)
		m.Servers = append(m.Servers, s)
		return err
	})
}
