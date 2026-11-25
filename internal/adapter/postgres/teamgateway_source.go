package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// The team's route set as the team gateway serves it (MEG-RC-1..6): enabled endpoints on providers that are
// on, the team's own or opted in, with the credential reference the team's calls use (per_team: the team's,
// shared: the service's) and the team's federation. No credential value is read: only ARNs and statuses.
const loadTeamGatewaySQL = `
SELECT e.id, e.slug, e.location, e.base_url, e.auth_kind,
       coalesce(c.secret_arn, ''), coalesce(c.status = 'missing', false),
       f.kind, coalesce(f.tenant_id, ''), coalesce(f.client_id, ''), coalesce(f.project_number, ''),
       coalesce(f.pool_id, ''), coalesce(f.provider_id, ''), coalesce(f.status, '')
  FROM afs.model_endpoints e
  LEFT JOIN afs.endpoint_credentials c ON c.endpoint_id = e.id
       AND c.scope = CASE WHEN e.credential_mode = 'shared' THEN '_shared' ELSE $1 END
  LEFT JOIN afs.endpoint_federations f ON f.endpoint_id = e.id AND f.team_id = $1
 WHERE e.enabled AND afs.provider_enabled(e.provider)
   AND (e.team_id = $1 OR (e.team_id IS NULL AND EXISTS (
        SELECT 1 FROM afs.team_model_endpoints tm WHERE tm.team_id = $1 AND tm.endpoint_id = e.id)))
 ORDER BY e.slug`

func loadTeamGateway(ctx context.Context, t *Tx, teamID string) (team.TeamGateway, error) {
	var g team.TeamGateway
	err := t.QueryRow(ctx, `SELECT t.gateway_deployment, afs.provider_enabled('bedrock') FROM afs.teams t WHERE t.id = $1`, teamID).
		Scan(&g.Deployment, &g.BedrockOn)
	if err != nil {
		return g, MapError(err, OpRead, "team gateway")
	}
	rows, err := t.Query(ctx, loadTeamGatewaySQL, teamID)
	if err != nil {
		return g, MapError(err, OpRead, "team routes")
	}
	defer rows.Close()
	for rows.Next() {
		var e team.GatewayEndpoint
		var kind *string
		var f team.Federation
		if err := rows.Scan(&e.ID, &e.Slug, &e.Location, &e.BaseURL, &e.AuthKind, &e.CredentialARN, &e.CredentialMissing,
			&kind, &f.TenantID, &f.ClientID, &f.ProjectNumber, &f.PoolID, &f.ProviderID, &f.Status); err != nil {
			return g, MapError(err, OpRead, "team routes")
		}
		if kind != nil {
			f.Kind = *kind
			e.Federation = &f
		}
		g.Endpoints = append(g.Endpoints, e)
	}
	if err := rows.Err(); err != nil {
		return g, MapError(err, OpRead, "team routes")
	}
	rows.Close()
	g.Mcp, err = loadTeamMcp(ctx, t, teamID)
	return g, err
}

// RouteStatusWriter implements team.RouteReporter on resource_status: kind team_route (id <teamId>/<endpointId>)
// with the RouteHealth condition, and kind team_gateway (id <teamId>) for a per-namespace gateway. A health
// transition publishes model_endpoint.degraded or .recovered in the same transaction.
type RouteStatusWriter struct {
	runner *TxRunner
	outbox port.OutboxWriter
	ids    port.IDGen
	clock  port.Clock
}

// NewRouteStatusWriter returns a RouteStatusWriter.
func NewRouteStatusWriter(runner *TxRunner, outbox port.OutboxWriter, ids port.IDGen, clock port.Clock) *RouteStatusWriter {
	return &RouteStatusWriter{runner: runner, outbox: outbox, ids: ids, clock: clock}
}

var _ team.RouteReporter = (*RouteStatusWriter)(nil)

// routePhase maps a route health to the stored phase (read back by ModelEndpointsRepo).
func routePhase(h domain.RouteHealth) domain.Phase {
	switch h {
	case domain.RouteHealthReady:
		return domain.PhaseReady
	case domain.RouteHealthDegraded:
		return domain.PhaseDegraded
	case domain.RouteHealthUnavailable:
		return domain.PhaseFailed
	}
	return domain.PhasePending
}

func healthOfPhase(p string) domain.RouteHealth {
	switch domain.Phase(p) {
	case domain.PhaseReady:
		return domain.RouteHealthReady
	case domain.PhaseDegraded:
		return domain.RouteHealthDegraded
	case domain.PhaseFailed:
		return domain.RouteHealthUnavailable
	}
	return domain.RouteHealthUnknown
}

// ReportRoutes implements team.RouteReporter.
func (w *RouteStatusWriter) ReportRoutes(ctx context.Context, teamID string, gatewayReady bool, routes []team.RouteStatus) error {
	return w.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		now := w.clock.Now().UTC()
		keep := make([]string, 0, len(routes))
		for _, r := range routes {
			id := teamID + "/" + r.EndpointID
			keep = append(keep, id)
			if err := w.reportRoute(ctx, tx, t, teamID, id, r, now); err != nil {
				return err
			}
		}
		if _, err := t.Exec(ctx, `DELETE FROM afs.resource_status WHERE resource_kind = $1 AND team_id = $2 AND NOT (resource_id = ANY($3))`,
			StatusKindTeamRoute, teamID, keep); err != nil {
			return MapError(err, OpWrite, "route status")
		}
		if !gatewayReady {
			_, err := t.Exec(ctx, `DELETE FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, StatusKindTeamGateway, teamID)
			return MapError(err, OpWrite, "team gateway status")
		}
		conds, _ := json.Marshal([]domain.Condition{{Type: "Ready", Status: domain.ConditionTrue, Reason: "Applied", LastTransitionTime: now}})
		_, err = t.Exec(ctx, upsertStatusSQL, StatusKindTeamGateway, teamID, teamID, int64(0), string(domain.PhaseReady), conds, nil, now)
		return MapError(err, OpWrite, "team gateway status")
	})
}

func (w *RouteStatusWriter) reportRoute(ctx context.Context, tx port.Tx, t *Tx, teamID, id string, r team.RouteStatus, now time.Time) error {
	var prevPhase string
	var prevConds []byte
	err := t.QueryRow(ctx, `SELECT phase, conditions FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2 FOR UPDATE`,
		StatusKindTeamRoute, id).Scan(&prevPhase, &prevConds)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return MapError(err, OpRead, "route status")
	}
	prev := healthOfPhase(prevPhase)
	since := now
	var cs []domain.Condition
	if len(prevConds) > 0 && json.Unmarshal(prevConds, &cs) == nil {
		for _, c := range cs {
			if c.Type == routeHealthCondition && prev == r.Health && c.Reason == r.Reason && !c.LastTransitionTime.IsZero() {
				since = c.LastTransitionTime // unchanged: the transition time stays
			}
		}
	}
	status := domain.ConditionTrue
	if r.Health != domain.RouteHealthReady {
		status = domain.ConditionFalse
	}
	conds, err := json.Marshal([]domain.Condition{{Type: routeHealthCondition, Status: status, Reason: r.Reason, LastTransitionTime: since}})
	if err != nil {
		return fmt.Errorf("postgres: route conditions: %w", err)
	}
	if _, err := t.Exec(ctx, upsertStatusSQL, StatusKindTeamRoute, id, teamID, int64(0), string(routePhase(r.Health)), conds, nil, now); err != nil {
		return MapError(err, OpWrite, "route status")
	}
	typ, ok := domain.RouteHealthEvent(prev, r.Health)
	if !ok {
		return nil
	}
	eid, err := w.ids.NewID(eventIDPrefix)
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, eid.String(), now, tx.Session().Actor, teamID, "model_endpoints", r.EndpointID, map[string]any{
		"endpointId": r.EndpointID, "route": r.Slug, "teamId": teamID, "health": string(r.Health), "previousHealth": string(prev), "reason": r.Reason,
	})
	if err != nil {
		return err
	}
	return w.outbox.Publish(ctx, tx, ev)
}

// loadTeamMcpSQL lists the MCP servers switched on for the team that are approved or suspended, with the credential
// reference the team's calls use (shared: the service's, per_team: the team's), the federation audience, the count
// of primitives held for review and the hosted spec. No credential value is read.
const loadTeamMcpSQL = `
SELECT s.id, s.slug, s.location, coalesce(s.url, ''), s.auth_kind, coalesce(c.secret_arn, ''), coalesce(c.status, ''),
       coalesce(s.federation->>'audience', ''), coalesce(s.federation->>'tokenEndpoint', ''), s.review_status = 'suspended',
       (SELECT count(*) FROM afs.mcp_primitives p WHERE p.server_id = s.id AND p.removed_at IS NULL AND p.status = 'pending_review'),
       h.image, coalesce(h.args, '{}'), coalesce(h.env_names, '{}'), coalesce(h.instance_mode, ''), coalesce(h.egress_hosts, '{}'),
       coalesce(h.resources, '{}'), coalesce(h.needs_aws, false)
  FROM afs.team_mcp_servers tm
  JOIN afs.mcp_servers s ON s.id = tm.server_id
  LEFT JOIN afs.mcp_credentials c ON c.server_id = s.id
       AND c.scope = CASE WHEN s.team_id IS NULL AND s.credential_mode = 'shared' THEN '_shared' ELSE $1 END
  LEFT JOIN afs.mcp_hosted_specs h ON h.server_id = s.id
 WHERE tm.team_id = $1 AND tm.enabled AND s.review_status IN ('approved', 'suspended')
 ORDER BY s.slug`

func loadTeamMcp(ctx context.Context, t *Tx, teamID string) (team.McpGateway, error) {
	var m team.McpGateway
	if err := t.QueryRow(ctx, `SELECT coalesce((afs.mcp_setting('mcp.enabled'))::boolean, false)`).Scan(&m.Enabled); err != nil {
		return m, MapError(err, OpRead, "service settings")
	}
	err := collect(ctx, t, "team MCP servers", loadTeamMcpSQL, []any{teamID}, func(rows pgx.Rows) error {
		var (
			s         team.McpServer
			tokenURL  string
			image     *string
			h         team.HostedServer
			resources []byte
		)
		if err := rows.Scan(&s.ID, &s.Slug, &s.Location, &s.URL, &s.AuthKind, &s.CredentialARN, &s.CredentialState, &s.Audience, &tokenURL,
			&s.Suspended, &s.DriftPending, &image, &h.Args, &h.EnvNames, &h.InstanceMode, &h.EgressHosts, &resources, &h.NeedsAWS); err != nil {
			return err
		}
		s.TokenEndpoint = tokenURL
		if image != nil {
			h.Image = *image
			if err := json.Unmarshal(resources, &h.Resources); err != nil {
				return fmt.Errorf("hosted resources of %s: %w", s.ID, err)
			}
			s.Hosted = &h
		}
		m.Servers = append(m.Servers, s)
		return nil
	})
	return m, err
}

// StatusKindTeamMcpServer is the resource_status kind of a team's MCP server (id <teamId>/<serverId>).
const StatusKindTeamMcpServer = "team_mcp_server"

var _ team.McpReporter = (*RouteStatusWriter)(nil)

// ReportMcp implements team.McpReporter: one row per server switched on, with McpServerReady, McpAuthExpired and
// McpDriftPending; rows of servers no longer on are removed. A condition that did not change keeps its time.
func (w *RouteStatusWriter) ReportMcp(ctx context.Context, teamID string, servers []team.McpStatus) error {
	return w.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		now := w.clock.Now().UTC()
		keep := make([]string, 0, len(servers))
		for _, s := range servers {
			id := teamID + "/" + s.ServerID
			keep = append(keep, id)
			var prev []byte
			if err := t.QueryRow(ctx, `SELECT conditions FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`,
				StatusKindTeamMcpServer, id).Scan(&prev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return MapError(err, OpRead, "MCP server status")
			}
			var old []domain.Condition
			_ = json.Unmarshal(prev, &old)
			phase := domain.PhaseReady
			conds := make([]domain.Condition, 0, len(s.Conditions))
			for _, c := range s.Conditions {
				c.LastTransitionTime = now
				for _, o := range old {
					if o.Type == c.Type && o.Status == c.Status && o.Reason == c.Reason && !o.LastTransitionTime.IsZero() {
						c.LastTransitionTime = o.LastTransitionTime
					}
				}
				if c.Type == "McpServerReady" && c.Status != domain.ConditionTrue {
					phase = domain.PhaseDegraded
				}
				conds = append(conds, c)
			}
			b, err := json.Marshal(conds)
			if err != nil {
				return fmt.Errorf("postgres: MCP conditions: %w", err)
			}
			if _, err := t.Exec(ctx, upsertStatusSQL, StatusKindTeamMcpServer, id, teamID, int64(0), string(phase), b, nil, now); err != nil {
				return MapError(err, OpWrite, "MCP server status")
			}
		}
		_, err = t.Exec(ctx, `DELETE FROM afs.resource_status WHERE resource_kind = $1 AND team_id = $2 AND NOT (resource_id = ANY($3))`,
			StatusKindTeamMcpServer, teamID, keep)
		return MapError(err, OpWrite, "MCP server status")
	})
}
