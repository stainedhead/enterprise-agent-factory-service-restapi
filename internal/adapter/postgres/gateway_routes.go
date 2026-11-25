package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/routeauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

var _ gateway.RouteStore = (*GatewayStore)(nil)

var _ routeauth.FederationStore = (*GatewayStore)(nil)

// RouteAdmission implements gateway.RouteStore with afs.route_admission (database contract §7.4). A subject
// that does not exist answers no rows; a live one with no route model also answers none, which the use case
// reads as "no route enabled".
func (g *GatewayStore) RouteAdmission(ctx context.Context, kind, subjectID string) ([]gateway.RouteGrant, error) {
	rows, err := g.pool.Query(ctx, `SELECT endpoint_id, slug, model_name, coalesce(reason, '') FROM afs.route_admission($1, $2)`, kind, subjectID)
	if err != nil {
		return nil, fmt.Errorf("read route admission: %w", err)
	}
	defer rows.Close()
	var out []gateway.RouteGrant
	for rows.Next() {
		var r gateway.RouteGrant
		if err := rows.Scan(&r.EndpointID, &r.Slug, &r.Model, &r.Reason); err != nil {
			return nil, fmt.Errorf("read route admission: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read route admission: %w", err)
	}
	if out == nil {
		out = []gateway.RouteGrant{}
	}
	return out, nil
}

// RouteEndpoint implements gateway.RouteStore: the team's own endpoint under slug wins over a service one
// (the slugs are unique per scope; a team endpoint cannot take a service slug, database contract §7.4).
func (g *GatewayStore) RouteEndpoint(ctx context.Context, teamID, slug string) (gateway.RouteEndpoint, error) {
	var e gateway.RouteEndpoint
	var team, ca, header *string
	var protocols []string
	var loc, auth, cred, pricing string
	var timeout, maxBody int32
	err := g.pool.QueryRow(ctx, `SELECT id, team_id, slug, provider, protocols, location, base_url, ca_bundle, auth_kind, auth_header,
  credential_mode, pricing_mode, use_provider_cost, timeout_seconds, max_body_mib
FROM afs.model_endpoints WHERE slug = $2 AND (team_id = $1 OR team_id IS NULL)
ORDER BY team_id NULLS LAST LIMIT 1`, teamID, slug).Scan(&e.ID, &team, &e.Slug, &e.Provider, &protocols, &loc, &e.BaseURL, &ca, &auth, &header,
		&cred, &pricing, &e.UseProviderCost, &timeout, &maxBody)
	if err != nil {
		return gateway.RouteEndpoint{}, notFound(fmt.Errorf("read model endpoint: %w", err))
	}
	if team != nil {
		e.TeamID = *team
	}
	if ca != nil {
		e.CABundle = *ca
	}
	if header != nil {
		e.AuthHeader = *header
	}
	for _, p := range protocols {
		e.Protocols = append(e.Protocols, domain.Protocol(p))
	}
	e.Location, e.AuthKind, e.CredentialMode, e.Pricing = domain.EndpointLocation(loc), domain.AuthKind(auth), domain.CredentialMode(cred), domain.PricingMode(pricing)
	e.Timeout, e.MaxBodyBytes = time.Duration(timeout)*time.Second, int64(maxBody)<<20
	rows, err := g.pool.Query(ctx, `SELECT model_name, max_output_tokens FROM afs.endpoint_models WHERE endpoint_id = $1 AND enabled`, e.ID)
	if err != nil {
		return gateway.RouteEndpoint{}, fmt.Errorf("read endpoint models: %w", err)
	}
	defer rows.Close()
	e.Models = map[string]int{}
	for rows.Next() {
		var name string
		var max int32
		if err := rows.Scan(&name, &max); err != nil {
			return gateway.RouteEndpoint{}, fmt.Errorf("read endpoint models: %w", err)
		}
		e.Models[name] = int(max)
	}
	return e, rows.Err()
}

// RoutePrices implements gateway.RouteStore, newest first.
func (g *GatewayStore) RoutePrices(ctx context.Context, endpointID, model string) ([]gateway.RoutePrice, error) {
	rows, err := g.pool.Query(ctx, `SELECT effective_from, input_per_mtok_usd::text, output_per_mtok_usd::text,
  coalesce(cached_input_per_mtok_usd::text, ''), request_fee_usd::text
FROM afs.endpoint_model_prices WHERE endpoint_id = $1 AND model_name = $2 ORDER BY effective_from DESC`, endpointID, model)
	if err != nil {
		return nil, fmt.Errorf("read route prices: %w", err)
	}
	defer rows.Close()
	var out []gateway.RoutePrice
	for rows.Next() {
		var p gateway.RoutePrice
		var in, outp, cached, fee string
		if err := rows.Scan(&p.EffectiveFrom, &in, &outp, &cached, &fee); err != nil {
			return nil, fmt.Errorf("read route prices: %w", err)
		}
		var perr error
		parse := func(s string) domain.Money {
			if s == "" {
				return domain.MoneyFromMicros(0)
			}
			m, err := domain.ParseMoney("price", s)
			if err != nil {
				perr = err
			}
			return m
		}
		p.Input, p.Output, p.CachedInput, p.RequestFee = parse(in), parse(outp), parse(cached), parse(roundFee(fee))
		if perr != nil {
			return nil, fmt.Errorf("read route prices: %w", perr)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// roundFee rounds a request fee (numeric(14, 8)) up to micro-dollars, the precision the gateway counts in.
func roundFee(s string) string {
	var n pgtype.Numeric
	if n.Scan(s) != nil || !n.Valid {
		return s
	}
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return s
	}
	micros := int64(f.Float64 * 1e6)
	if float64(micros) < f.Float64*1e6 {
		micros++
	}
	return domain.MoneyFromMicros(micros).String()
}

// Federation implements routeauth.FederationStore.
func (g *GatewayStore) Federation(ctx context.Context, endpointID, teamID string) (routeauth.Federation, error) {
	var f routeauth.Federation
	var tenant, client, num, pool, prov, proj, loc *string
	err := g.pool.QueryRow(ctx, `SELECT kind, tenant_id, client_id, project_number, pool_id, provider_id, project_id, location, status
FROM afs.endpoint_federations WHERE endpoint_id = $1 AND team_id = $2`, endpointID, teamID).Scan(&f.Kind, &tenant, &client, &num, &pool, &prov, &proj, &loc, &f.Status)
	if err != nil {
		return routeauth.Federation{}, notFound(fmt.Errorf("read federation: %w", err))
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	f.TenantID, f.ClientID, f.ProjectNumber, f.PoolID, f.ProviderID, f.ProjectID, f.Location =
		deref(tenant), deref(client), deref(num), deref(pool), deref(prov), deref(proj), deref(loc)
	return f, nil
}

// ReportFederation implements routeauth.FederationStore (afs_gateway may update status, last_error, checked_at).
func (g *GatewayStore) ReportFederation(ctx context.Context, endpointID, teamID, status, lastError string) error {
	_, err := g.pool.Exec(ctx, `UPDATE afs.endpoint_federations SET status = $3, last_error = nullif($4, ''), checked_at = now()
WHERE endpoint_id = $1 AND team_id = $2`, endpointID, teamID, status, lastError)
	if err != nil {
		return fmt.Errorf("report federation: %w", err)
	}
	return nil
}
