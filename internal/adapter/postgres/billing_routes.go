package postgres

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// RouteBillStore implements routebill.Store for afs-billing's login (database contract §7.4): it reads routes,
// route usage, statements, capacity costs and route prices, writes cost_daily rows of source route_capacity and
// inserts and supersedes endpoint_price_proposals.
type RouteBillStore struct{}

// NewRouteBillStore returns a RouteBillStore.
func NewRouteBillStore() *RouteBillStore { return &RouteBillStore{} }

var _ routebill.Store = (*RouteBillStore)(nil)

// statementProviders are the providers reconciled from imported statements, not pulled.
var statementProviders = []string{"nous", "opencode"}

// routeCalls are the usage events that are calls (as RollupGateway).
const routeCalls = `outcome IN ('ok', 'error')`

// Threshold implements routebill.Store. An unset setting is the default, 5.
func (s *RouteBillStore) Threshold(ctx context.Context, tx port.Tx) (float64, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return 0, err
	}
	var pct float64
	err = t.QueryRow(ctx, `SELECT coalesce((SELECT (value #>> '{}')::float8 FROM afs.service_settings WHERE key = $1), 5)`,
		domain.SettingBillingVarianceAlertPct).Scan(&pct)
	return pct, MapError(err, OpRead, domain.SettingBillingVarianceAlertPct)
}

func (s *RouteBillStore) endpoints(ctx context.Context, tx port.Tx, providers []string) ([]routebill.Endpoint, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT id, slug, provider FROM afs.model_endpoints
 WHERE enabled AND provider = ANY($1) AND afs.provider_enabled(provider) ORDER BY slug, id`, providers)
	if err != nil {
		return nil, MapError(err, OpRead, "model endpoints")
	}
	defer rows.Close()
	var out []routebill.Endpoint
	for rows.Next() {
		var e routebill.Endpoint
		if err := rows.Scan(&e.ID, &e.Slug, &e.Provider); err != nil {
			return nil, MapError(err, OpRead, "model endpoints")
		}
		out = append(out, e)
	}
	return out, MapError(rows.Err(), OpRead, "model endpoints")
}

// Endpoints implements routebill.Store: the enabled routes of the providers, while the provider is on.
func (s *RouteBillStore) Endpoints(ctx context.Context, tx port.Tx, providers []string) ([]routebill.Endpoint, error) {
	return s.endpoints(ctx, tx, providers)
}

// StatementRoutes implements routebill.Store.
func (s *RouteBillStore) StatementRoutes(ctx context.Context, tx port.Tx) ([]routebill.Endpoint, error) {
	return s.endpoints(ctx, tx, statementProviders)
}

func money(field, v string) (domain.Money, error) { return domain.ParseMoney(field, v) }

// GatewayCost implements routebill.Store: the settled cost of the route's calls per team.
func (s *RouteBillStore) GatewayCost(ctx context.Context, tx port.Tx, endpointID string, from, to time.Time) (map[string]domain.Money, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT team_id, round(sum(cost_usd), 6)::text FROM afs.usage_events
 WHERE endpoint_id = $1 AND occurred_at >= $2 AND occurred_at < $3 AND `+routeCalls+` GROUP BY team_id`, endpointID, from.UTC(), to.UTC())
	if err != nil {
		return nil, MapError(err, OpRead, "usage events")
	}
	defer rows.Close()
	out := map[string]domain.Money{}
	for rows.Next() {
		var team, v string
		if err := rows.Scan(&team, &v); err != nil {
			return nil, MapError(err, OpRead, "usage events")
		}
		if out[team], err = money("cost", v); err != nil {
			return nil, err
		}
	}
	return out, MapError(rows.Err(), OpRead, "usage events")
}

// Statements implements routebill.Store: each statement of the month with its rows summed per team. A
// statement with no rows is returned with no figures, so its route counts as reconciled.
func (s *RouteBillStore) Statements(ctx context.Context, tx port.Tx, period time.Time) ([]routebill.Statement, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT s.id, e.id, e.slug, e.provider, s.source, coalesce(r ->> 'teamId', ''),
       round(coalesce(sum((r ->> 'amountUsd')::numeric), 0), 6)::text
  FROM afs.provider_statements s
  JOIN afs.model_endpoints e ON e.id = s.endpoint_id
  LEFT JOIN LATERAL jsonb_array_elements(s.rows) r ON true
 WHERE s.period = $1::date
 GROUP BY 1, 2, 3, 4, 5, 6 ORDER BY e.slug, s.source, 6`, dayArg(period))
	if err != nil {
		return nil, MapError(err, OpRead, "provider statements")
	}
	defer rows.Close()
	var out []routebill.Statement
	last := ""
	for rows.Next() {
		var id, team, amount string
		var st routebill.Statement
		if err := rows.Scan(&id, &st.Endpoint.ID, &st.Endpoint.Slug, &st.Endpoint.Provider, &st.Source, &team, &amount); err != nil {
			return nil, MapError(err, OpRead, "provider statements")
		}
		if id != last {
			out = append(out, st)
			last = id
		}
		if team == "" {
			continue
		}
		m, err := money("amountUsd", amount)
		if err != nil {
			return nil, err
		}
		cur := &out[len(out)-1]
		cur.Figures = append(cur.Figures, routebill.Figure{TeamID: team, AmountUSD: m})
	}
	return out, MapError(rows.Err(), OpRead, "provider statements")
}

// Capacities implements routebill.Store: per capacity route and team, the cost row in force on day.
func (s *RouteBillStore) Capacities(ctx context.Context, tx port.Tx, day time.Time) ([]routebill.Capacity, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT DISTINCT ON (c.endpoint_id, c.team_id)
       e.id, e.slug, e.provider, c.team_id, c.monthly_usd::text, c.hourly_usd::text, c.allocation
  FROM afs.endpoint_capacity_costs c
  JOIN afs.model_endpoints e ON e.id = c.endpoint_id
 WHERE c.effective_from <= $1::date AND e.pricing_mode = 'capacity'
 ORDER BY c.endpoint_id, c.team_id, c.effective_from DESC`, dayArg(day))
	if err != nil {
		return nil, MapError(err, OpRead, "capacity costs")
	}
	defer rows.Close()
	var out []routebill.Capacity
	for rows.Next() {
		var c routebill.Capacity
		var monthly, hourly *string
		if err := rows.Scan(&c.Endpoint.ID, &c.Endpoint.Slug, &c.Endpoint.Provider, &c.TeamID, &monthly, &hourly, &c.Allocation); err != nil {
			return nil, MapError(err, OpRead, "capacity costs")
		}
		for _, p := range []struct {
			v   *string
			dst **domain.Money
		}{{monthly, &c.MonthlyUSD}, {hourly, &c.HourlyUSD}} {
			if p.v == nil {
				continue
			}
			m, err := money("capacity", *p.v)
			if err != nil {
				return nil, err
			}
			*p.dst = &m
		}
		out = append(out, c)
	}
	return out, MapError(rows.Err(), OpRead, "capacity costs")
}

// Shares implements routebill.Store: the tokens each agent and pool of the team used on the route that day.
func (s *RouteBillStore) Shares(ctx context.Context, tx port.Tx, endpointID, teamID string, day time.Time) ([]routebill.Share, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT coalesce(agent_id, ''), coalesce(pool_id, ''), sum(input_tokens + output_tokens)
  FROM afs.usage_events
 WHERE endpoint_id = $1 AND team_id = $2 AND occurred_at >= $3::timestamptz AND occurred_at < $3::timestamptz + interval '1 day'
   AND `+routeCalls+`
 GROUP BY 1, 2 ORDER BY 1, 2`, endpointID, teamID, startArg(day))
	if err != nil {
		return nil, MapError(err, OpRead, "usage events")
	}
	defer rows.Close()
	var out []routebill.Share
	for rows.Next() {
		var sh routebill.Share
		if err := rows.Scan(&sh.AgentID, &sh.PoolID, &sh.Tokens); err != nil {
			return nil, MapError(err, OpRead, "usage events")
		}
		out = append(out, sh)
	}
	return out, MapError(rows.Err(), OpRead, "usage events")
}

// ReplaceCapacity implements routebill.Store. The rows carry cost only: the route's tokens are already in
// its source route rows, so counting them again would double the reports' token totals.
func (s *RouteBillStore) ReplaceCapacity(ctx context.Context, tx port.Tx, day time.Time, rows []routebill.CostRow) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if _, err := t.Exec(ctx, `DELETE FROM afs.cost_daily WHERE day = $1::date AND source = 'route_capacity'`, dayArg(day)); err != nil {
		return MapError(err, OpWrite, "cost_daily")
	}
	for _, r := range rows {
		if _, err := t.Exec(ctx, `INSERT INTO afs.cost_daily (team_id, day, source, agent_id, pool_id, model_id, cost_usd)
VALUES ($1, $2::date, 'route_capacity', nullif($3, ''), nullif($4, ''), $5, $6::numeric)`,
			r.TeamID, dayArg(day), r.AgentID, r.PoolID, r.ModelID, r.AmountUSD.String()); err != nil {
			return MapError(err, OpWrite, "cost_daily")
		}
	}
	return nil
}

// RouteModels implements routebill.Store: the enabled models of enabled per_token routes, with the price in
// force now and the pending proposal.
func (s *RouteBillStore) RouteModels(ctx context.Context, tx port.Tx) ([]routebill.RouteModel, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT e.id, e.slug, e.provider, m.model_name,
       c.input_per_mtok_usd::text, c.output_per_mtok_usd::text,
       coalesce(p.id, ''), coalesce(p.input_per_mtok_usd, 0)::text, coalesce(p.output_per_mtok_usd, 0)::text
  FROM afs.model_endpoints e
  JOIN afs.endpoint_models m ON m.endpoint_id = e.id AND m.enabled
  LEFT JOIN LATERAL (SELECT * FROM afs.endpoint_model_prices x
                      WHERE x.endpoint_id = m.endpoint_id AND x.model_name = m.model_name AND x.effective_from <= now()
                      ORDER BY x.effective_from DESC LIMIT 1) c ON true
  LEFT JOIN afs.endpoint_price_proposals p ON p.endpoint_id = m.endpoint_id AND p.model_name = m.model_name AND p.state = 'pending'
 WHERE e.enabled AND e.pricing_mode = 'per_token'
 ORDER BY e.slug, m.model_name`)
	if err != nil {
		return nil, MapError(err, OpRead, "route models")
	}
	defer rows.Close()
	var out []routebill.RouteModel
	for rows.Next() {
		var rm routebill.RouteModel
		var in, outp *string
		var pin, pout string
		if err := rows.Scan(&rm.Endpoint.ID, &rm.Endpoint.Slug, &rm.Endpoint.Provider, &rm.Model, &in, &outp, &rm.PendingID, &pin, &pout); err != nil {
			return nil, MapError(err, OpRead, "route models")
		}
		if in != nil && outp != nil {
			var cur [2]domain.Money
			if cur[0], err = money("input", *in); err != nil {
				return nil, err
			}
			if cur[1], err = money("output", *outp); err != nil {
				return nil, err
			}
			rm.Current = &cur
		}
		if rm.Pending[0], err = money("input", pin); err != nil {
			return nil, err
		}
		if rm.Pending[1], err = money("output", pout); err != nil {
			return nil, err
		}
		out = append(out, rm)
	}
	return out, MapError(rows.Err(), OpRead, "route models")
}

// Supersede implements routebill.Store.
func (s *RouteBillStore) Supersede(ctx context.Context, tx port.Tx, proposalID string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `UPDATE afs.endpoint_price_proposals SET state = 'superseded' WHERE id = $1 AND state = 'pending'`, proposalID)
	return MapError(err, OpWrite, "route price proposal")
}

// Insert implements routebill.Store. team_id is copied from the endpoint (NULL for a service endpoint).
func (s *RouteBillStore) Insert(ctx context.Context, tx port.Tx, p routebill.Proposal) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `INSERT INTO afs.endpoint_price_proposals
(id, endpoint_id, model_name, team_id, effective_from, input_per_mtok_usd, output_per_mtok_usd, source, note)
SELECT $1, e.id, $3, e.team_id, $4, $5::numeric, $6::numeric, 'price_list', $7 FROM afs.model_endpoints e WHERE e.id = $2`,
		p.ID, p.EndpointID, p.Model, p.EffectiveFrom, p.Input.String(), p.Output.String(), p.Note)
	return MapError(err, OpWrite, "route price proposal")
}

// SecretPrefix is service setting secrets.path_prefix (default afs), the root of the billing key names.
func (s *RouteBillStore) SecretPrefix(ctx context.Context, tx port.Tx) (string, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var p string
	err = t.QueryRow(ctx, `SELECT coalesce((SELECT value #>> '{}' FROM afs.service_settings WHERE key = 'secrets.path_prefix'), 'afs')`).Scan(&p)
	return p, MapError(err, OpRead, "secrets.path_prefix")
}
