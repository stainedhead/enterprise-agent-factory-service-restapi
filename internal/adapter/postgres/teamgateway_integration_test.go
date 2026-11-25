package postgres_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// MEG-RC-1, MEG-RC-6: the reconciler reads a per-namespace team's route set with the credential reference its
// calls use, writes route health and the team gateway status, and publishes degraded and recovered once per
// transition.
func TestTeamGatewaySourceAndRouteStatusAgainstTheSchema(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	mustExec(t, admin, `UPDATE afs.teams SET gateway_deployment = 'per_namespace' WHERE id = $1`, tm.ID)
	slug, ep := mepSlug("gw"), pgtest.NewID("mep")
	mustExec(t, admin, `INSERT INTO afs.model_endpoints (id, slug, display_name, location, base_url, auth_kind, pricing_mode, enabled)
VALUES ($1, $2, 'Gateway', 'external', 'https://llm.example.com/v1', 'bearer', 'free', true)`, ep, slug)
	mustExec(t, admin, `INSERT INTO afs.team_model_endpoints (team_id, endpoint_id) VALUES ($1, $2)`, tm.ID, ep)
	off := pgtest.NewID("mep")
	mustExec(t, admin, `INSERT INTO afs.model_endpoints (id, slug, display_name, location, base_url, auth_kind, pricing_mode, enabled)
VALUES ($1, $2, 'Off', 'external', 'https://llm.example.com/v1', 'none', 'free', false)`, off, mepSlug("off"))
	mustExec(t, admin, `INSERT INTO afs.team_model_endpoints (team_id, endpoint_id) VALUES ($1, $2)`, tm.ID, off)

	runner := reconcilerPool(t, db)
	src := postgres.NewTeamSource(runner)
	got, err := src.LoadTeam(ctx, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	g := got.Gateway
	if !g.PerNamespace() || len(g.Endpoints) != 1 || g.Endpoints[0].Slug != slug || g.Endpoints[0].CredentialARN != "" {
		t.Fatalf("gateway = %+v", g)
	}
	health := g.RouteHealth()
	if health[0].Health != domain.RouteHealthUnavailable || health[0].Reason != team.ReasonCredentialMissing {
		t.Fatalf("health = %+v", health)
	}

	// A per-team credential at the database's path makes the route Ready.
	mustExec(t, admin, `INSERT INTO afs.endpoint_credentials (endpoint_id, scope, team_id, secret_arn)
VALUES ($1, $2, $2, 'arn:aws:secretsmanager:us-east-1:111122223333:secret:' || afs.secret_path($2, '_gateway', $3) || '-AbCdEf')`, ep, tm.ID, slug)

	w := postgres.NewRouteStatusWriter(runner, postgres.NewOutboxWriter(), billingIDs{}, &fakes.Clock{T: time.Now().UTC()})
	events := func() int {
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type IN ('model_endpoint.degraded', 'model_endpoint.recovered')`, ep).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	phase := func() string {
		var p string
		if err := admin.QueryRow(ctx, `SELECT phase FROM afs.resource_status WHERE resource_kind = 'team_route' AND resource_id = $1`, tm.ID+"/"+ep).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := w.ReportRoutes(ctx, tm.ID, true, health); err != nil {
		t.Fatal(err)
	}
	if phase() != "Failed" || events() != 1 {
		t.Errorf("first report: %s, %d events", phase(), events())
	}
	if err := w.ReportRoutes(ctx, tm.ID, true, health); err != nil {
		t.Fatal(err)
	}
	if events() != 1 {
		t.Errorf("unchanged health published again: %d", events())
	}
	got, err = src.LoadTeam(ctx, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := got.Gateway.RouteHealth()
	if ready[0].Health != domain.RouteHealthReady {
		t.Fatalf("with credential = %+v", ready)
	}
	if err := w.ReportRoutes(ctx, tm.ID, true, ready); err != nil {
		t.Fatal(err)
	}
	if phase() != "Ready" || events() != 2 {
		t.Errorf("recovered: %s, %d events", phase(), events())
	}
	var gw string
	if err := admin.QueryRow(ctx, `SELECT phase FROM afs.resource_status WHERE resource_kind = 'team_gateway' AND resource_id = $1`, tm.ID).Scan(&gw); err != nil || gw != "Ready" {
		t.Errorf("team gateway status = %q %v", gw, err)
	}
	// Back to shared with no routes: the rows go.
	if err := w.ReportRoutes(ctx, tm.ID, false, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.resource_status WHERE team_id = $1 AND resource_kind IN ('team_route', 'team_gateway')`, tm.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows left = %d %v", n, err)
	}
}
