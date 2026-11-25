package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	me "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets/secretstest"
)

type mepRig struct {
	svc    *me.Service
	db     *pgtest.DB
	writer *secretstest.Writer
}

func newMEPRig(t *testing.T) *mepRig {
	t.Helper()
	db := pgtest.Get(t)
	runner := postgres.NewTxRunner(appPool(t, db))
	w := secretstest.NewWriter()
	return &mepRig{db: db, writer: w, svc: &me.Service{
		Deps: acting.Deps{Tx: runner, Audit: postgres.NewAuditWriter(runner), Outbox: postgres.NewOutboxWriter(),
			Idem: postgres.NewIdempotencyStore(), Clock: &fakes.Clock{T: time.Now().UTC()}, IDs: billingIDs{}},
		Repo: postgres.NewModelEndpointsRepo(), Secrets: w,
		Config: me.Config{OIDCIssuer: "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE", Partition: "aws", AccountID: "111122223333"},
	}}
}

func teamAdminOf(team string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}, RequestID: "req_mep_team"}
}

func mepSlug(prefix string) string { return prefix + "-" + strings.ToLower(pgtest.NewID("x")[2:12]) }

func price(in, out string) *me.Price {
	return &me.Price{Input: domain.MustMoney(in), Output: domain.MustMoney(out)}
}

// MEG-API-1..4 against the real schema: a service endpoint is created off, priced, switched on, selected by
// a team, given a per-team credential at the database's path, and shown to the team as callable.
func TestModelEndpointLifecycleAgainstTheSchema(t *testing.T) {
	r := newMEPRig(t)
	ctx := t.Context()
	admin := svcAdmin()
	tm := r.db.SeedTeam(t)
	slug := mepSlug("oai")
	in := me.NewEndpoint{Endpoint: domain.ModelEndpoint{Slug: slug, DisplayName: "OpenAI", Protocols: []domain.Protocol{domain.ProtocolChat},
		Location: domain.LocationExternal, BaseURL: "https://api.openai.com/v1", AuthKind: domain.AuthBearer,
		CredentialMode: domain.CredentialPerTeam, PricingMode: domain.PricingPerToken},
		Models: []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "gpt-x", MaxOutputTokens: 4000, Enabled: true}},
			{EndpointModel: domain.EndpointModel{ModelName: "gpt-y", MaxOutputTokens: 1000, Enabled: true}}}}
	created, err := r.svc.CreateEndpoint(ctx, admin, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.Value
	if ep.Provider != "openai" || ep.Enabled || !ep.ProviderOn || len(ep.Models) != 2 {
		t.Fatalf("created %+v", ep)
	}
	if _, err := r.svc.CreatePrice(ctx, admin, ep.ID, me.PriceRow{ModelName: "gpt-x", EffectiveFrom: time.Now().UTC().Add(-time.Hour),
		Price: *price("1.25", "10"), Source: "manual"}, nil); err != nil {
		t.Fatal(err)
	}
	if ep, err = r.svc.GetEndpoint(ctx, admin, ep.ID); err != nil {
		t.Fatal(err)
	}
	if ep.Models[0].PriceState != me.PriceApproved || ep.Models[1].PriceState != me.PriceNone {
		t.Fatalf("price states %+v", ep.Models)
	}
	// The slug is unique among service endpoints.
	_, err = r.svc.CreateEndpoint(ctx, admin, in, nil)
	if de := asDomain(err); de == nil || de.Kind != domain.KindConflict || de.Reason != domain.ReasonSlugTaken {
		t.Fatalf("second slug: %v", err)
	}
	on := true
	ep, err = r.svc.UpdateEndpoint(ctx, admin, ep.ID, me.Patch{Enabled: &on}, ep.Version())
	if err != nil || !ep.Enabled {
		t.Fatalf("enable %+v %v", ep, err)
	}
	if _, err := r.svc.UpdateEndpoint(ctx, admin, ep.ID, me.Patch{Enabled: &on}, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	cp, err := r.svc.CreatePrice(ctx, admin, ep.ID, me.PriceRow{ModelName: "gpt-y", EffectiveFrom: time.Now().UTC().Add(-time.Minute),
		Price: *price("0.5", "2"), Source: "manual"}, nil)
	if err != nil || cp.Value.ApprovedBy == "" {
		t.Fatalf("price %+v %v", cp, err)
	}
	got, err := r.svc.GetEndpoint(ctx, admin, ep.ID)
	if err != nil || got.Models[1].PriceState != me.PriceApproved || got.Models[1].Price.Input.Cmp(domain.MustMoney("0.5")) != 0 {
		t.Fatalf("get %+v %v", got.Models, err)
	}
	v := got.Version()
	ml, err := r.svc.PutModels(ctx, admin, ep.ID, []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "gpt-x", MaxOutputTokens: 8000, Enabled: true}}}, &v)
	if err != nil || len(ml.Models) != 1 || ml.Version <= v {
		t.Fatalf("put models %+v %v", ml, err)
	}
	list, _, err := r.svc.ListEndpoints(ctx, admin, me.Filter{Provider: "openai"}, acting.Page{Limit: 100})
	if err != nil || !containsEndpoint(list, ep.ID) {
		t.Fatalf("list %v", err)
	}

	// The team opts in and stores its credential; the team sees the route as selected with a credential.
	team := teamAdminOf(tm.ID)
	sel, err := r.svc.PutSelections(ctx, team, tm.ID, []string{ep.ID}, nil)
	if err != nil || len(sel.Selections) != 1 || sel.Selections[0].SelectedBy != "team_admin" {
		t.Fatalf("select %+v %v", sel, err)
	}
	val, _ := domain.NewSecretValue("value", "sk-test-not-real")
	cred, err := r.svc.PutTeamCredential(ctx, team, tm.ID, ep.ID, val, nil)
	if err != nil || cred.State != me.StateSet || !strings.Contains(cred.ARN, "/_gateway/"+slug+"-") {
		t.Fatalf("credential %+v %v", cred, err)
	}
	tl, err := r.svc.ListTeamEndpoints(ctx, team, tm.ID, me.RelationSelected, acting.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var te *me.TeamEndpoint
	for i := range tl.Items {
		if tl.Items[i].ID == ep.ID {
			te = &tl.Items[i]
		}
	}
	if te == nil || te.CredentialState != me.StateSet || te.OffReason != "" || te.SelectableBy != domain.SelectionTeamAdmin {
		t.Fatalf("team view %+v", te)
	}
	teams, _, err := r.svc.ListEndpointTeams(ctx, admin, ep.ID, acting.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, et := range teams {
		if et.TeamID == tm.ID {
			found = et.Selected && et.CredentialState == me.StateSet && et.TeamSlug == tm.Slug
		}
	}
	if !found {
		t.Fatalf("endpoint teams %+v", teams)
	}
	// A team cannot delete an endpoint that is on; a service admin switches it off first.
	if err := r.svc.DeleteEndpoint(ctx, admin, ep.ID, ml.Version); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("delete enabled: %v", err)
	}
	off := false
	ep, err = r.svc.UpdateEndpoint(ctx, admin, ep.ID, me.Patch{Enabled: &off}, ml.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.PutSelections(ctx, team, tm.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.DeleteEndpoint(ctx, admin, ep.ID, ep.Version()); err != nil {
		t.Fatal(err)
	}
}

// MEG-API-6: a team declares its own in-namespace endpoint only once a service admin allows it.
func TestTeamEndpointNeedsMayAddAgainstTheSchema(t *testing.T) {
	r := newMEPRig(t)
	ctx := t.Context()
	tm := r.db.SeedTeam(t)
	team := teamAdminOf(tm.ID)
	in := me.NewEndpoint{Endpoint: domain.ModelEndpoint{Slug: mepSlug("vllm"), DisplayName: "vLLM", Owner: "ml", Purpose: "tests",
		Protocols: []domain.Protocol{domain.ProtocolChat}, Location: domain.LocationInNamespace, BaseURL: "http://vllm:8000/v1",
		AuthKind: domain.AuthNone, PricingMode: domain.PricingFree},
		Models: []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "llama", MaxOutputTokens: 1000, Enabled: true}}}}
	_, err := r.svc.CreateTeamEndpoint(ctx, team, tm.ID, in, nil)
	if de := asDomain(err); de == nil || de.Kind != domain.KindAccessDenied {
		t.Fatalf("not allowed: %v", err)
	}
	may := true
	g, err := r.svc.PutTeamGateway(ctx, team, tm.ID, me.TeamGatewayPatch{MayAdd: &may}, nil)
	if domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team admin set mayAdd: %+v %v", g, err)
	}
	dep := domain.GatewayPerNamespace
	g, err = r.svc.PutTeamGateway(ctx, svcAdmin(), tm.ID, me.TeamGatewayPatch{MayAdd: &may, Deployment: &dep}, nil)
	if err != nil || !g.MayAdd || g.Deployment != dep || g.GatewayURL != "https://afs-gateway.afs-t-"+tm.Slug+".svc:8443" {
		t.Fatalf("gateway %+v %v", g, err)
	}
	created, err := r.svc.CreateTeamEndpoint(ctx, team, tm.ID, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Value.Provider != domain.ProviderInternal || !created.Value.Enabled || created.Value.TeamID != tm.ID {
		t.Fatalf("own %+v", created.Value)
	}
	te, err := r.svc.ListTeamEndpoints(ctx, team, tm.ID, me.RelationOwn, acting.Page{Limit: 10})
	if err != nil || len(te.Items) != 1 || te.Items[0].Models[0].PriceState != me.PriceNotRequired {
		t.Fatalf("own list %+v %v", te, err)
	}
	// Another team cannot see it.
	other := r.db.SeedTeam(t)
	if _, err := r.svc.GetEndpoint(ctx, teamAdminOf(other.ID), created.Value.ID); domain.KindOf(err) != domain.KindAccessDenied && domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team: %v", err)
	}
	off := false
	upd, err := r.svc.UpdateTeamEndpoint(ctx, team, tm.ID, created.Value.ID, me.Patch{Enabled: &off}, created.Value.Version())
	if err != nil || upd.Enabled {
		t.Fatalf("switch off %+v %v", upd, err)
	}
	if err := r.svc.DeleteTeamEndpoint(ctx, team, tm.ID, upd.ID, upd.Version()); err != nil {
		t.Fatal(err)
	}
}

// MEG-API-7, MEG-BL-3: providers, the Bedrock-off guard, proposals and statements.
func TestProvidersProposalsAndStatementsAgainstTheSchema(t *testing.T) {
	r := newMEPRig(t)
	ctx := t.Context()
	admin := svcAdmin()
	ps, err := r.svc.ListProviders(ctx, admin)
	if err != nil || len(ps) < 8 {
		t.Fatalf("providers %d %v", len(ps), err)
	}
	var bedrock me.Provider
	for _, p := range ps {
		if p.Provider == domain.ProviderBedrock {
			bedrock = p
		}
	}
	off := false
	_, err = r.svc.UpdateProvider(ctx, admin, domain.ProviderBedrock, me.ProviderPatch{Enabled: &off}, false, domain.Version(bedrock.Version))
	if de := asDomain(err); de == nil || de.Reason != domain.ReasonNoFallbackModel {
		t.Fatalf("bedrock off: %v", err)
	}
	dry, err := r.svc.UpdateProvider(ctx, admin, "openai", me.ProviderPatch{Enabled: &off}, true, 1)
	if err != nil || dry.Impact == nil {
		t.Fatalf("dry run %+v %v", dry, err)
	}

	// A Nous endpoint with a shared credential and a monthly statement.
	tm := r.db.SeedTeam(t)
	in := me.NewEndpoint{Endpoint: domain.ModelEndpoint{Slug: mepSlug("nous"), DisplayName: "Nous", Protocols: []domain.Protocol{domain.ProtocolChat},
		Location: domain.LocationExternal, BaseURL: "https://inference-api.nousresearch.com/v1", AuthKind: domain.AuthBearer,
		CredentialMode: domain.CredentialShared, PricingMode: domain.PricingPerToken},
		Models: []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "hermes", MaxOutputTokens: 1000, Enabled: true}}}}
	created, err := r.svc.CreateEndpoint(ctx, admin, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep := created.Value
	val, _ := domain.NewSecretValue("value", "nous-test-not-real")
	cred, err := r.svc.PutCredential(ctx, admin, ep.ID, domain.SharedCredentialScope, val, nil)
	if err != nil || !strings.Contains(cred.ARN, "/_service/gateway/"+ep.Slug+"-") {
		t.Fatalf("shared credential %+v %v", cred, err)
	}
	tokens := int64(1000)
	st, err := r.svc.ImportStatement(ctx, admin, ep.ID, me.Statement{Period: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: me.SourceNousStatement,
		Rows: []me.StatementRow{{TeamID: tm.ID, Model: "hermes", AmountUSD: domain.MustMoney("12.5"), Tokens: &tokens}}}, nil)
	if err != nil || st.Value.Total().Cmp(domain.MustMoney("12.5")) != 0 {
		t.Fatalf("statement %+v %v", st, err)
	}
	_, err = r.svc.ImportStatement(ctx, admin, ep.ID, me.Statement{Period: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Source: me.SourceNousStatement,
		Rows: []me.StatementRow{{TeamID: tm.ID, Model: "hermes", AmountUSD: domain.MustMoney("1")}}}, nil)
	if domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("second statement: %v", err)
	}

	// The billing job proposes a price; a service admin approves it and the price is in force.
	pid := pgtest.NewID("epp")
	if _, err := r.db.Admin(t).Exec(ctx, `INSERT INTO afs.endpoint_price_proposals (id, endpoint_id, model_name, effective_from,
  input_per_mtok_usd, output_per_mtok_usd) VALUES ($1, $2, 'hermes', now() - interval '1 hour', 0.4, 1.6)`, pid, ep.ID); err != nil {
		t.Fatal(err)
	}
	props, _, err := r.svc.ListProposals(ctx, admin, me.ProposalPending, ep.ID, acting.Page{Limit: 10})
	if err != nil || len(props) != 1 || props[0].Slug != ep.Slug {
		t.Fatalf("proposals %+v %v", props, err)
	}
	if _, err := r.svc.Approve(ctx, teamAdminOf(tm.ID), pid, "", nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team approve: %v", err)
	}
	dec, err := r.svc.Approve(ctx, admin, pid, "list price", nil)
	if err != nil || dec.Value.State != me.ProposalApproved || dec.Value.DecidedBy == nil {
		t.Fatalf("approve %+v %v", dec, err)
	}
	if _, err := r.svc.Reject(ctx, admin, pid, "", nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("decided twice: %v", err)
	}
	got, err := r.svc.GetEndpoint(ctx, admin, ep.ID)
	if err != nil || got.Models[0].PriceState != me.PriceApproved {
		t.Fatalf("approved price %+v %v", got.Models, err)
	}
}

// MEG-API-8: the inference report groups route calls by route and model.
func TestInferenceReportAgainstTheSchema(t *testing.T) {
	r := newMEPRig(t)
	ctx := t.Context()
	tm := r.db.SeedTeam(t)
	adm := r.db.Admin(t)
	for i, q := range []string{
		`INSERT INTO afs.usage_events (occurred_at, team_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd, latency_ms)
VALUES (now() - interval '1 hour', $1, 'anthropic.claude-x', 'req_r1', 'ok', 100, 50, 0.01, 800)`,
		`INSERT INTO afs.usage_events (occurred_at, team_id, request_id, outcome, input_tokens, output_tokens, cost_usd, latency_ms, http_status)
VALUES (now() - interval '1 hour', $1, 'req_r2', 'error', 0, 0, 0, 30, 429)`,
	} {
		if _, err := adm.Exec(ctx, q, tm.ID); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	rep, err := r.svc.InferenceReport(ctx, teamAdminOf(tm.ID), tm.ID, me.RawReport{Range: "24h", GroupBy: "route,model"}, acting.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Calls != 2 || rep.Totals.Errors != 1 || rep.Totals.Throttled != 1 || len(rep.Rows) != 2 || rep.Rows[0].Route != "bedrock" {
		t.Fatalf("report %+v", rep)
	}
}

func asDomain(err error) *domain.Error {
	var de *domain.Error
	if err == nil {
		return nil
	}
	if e, ok := err.(*domain.Error); ok { //nolint:errorlint // the use case returns *domain.Error unwrapped
		de = e
	}
	return de
}

func containsEndpoint(es []me.Endpoint, id string) bool {
	for _, e := range es {
		if e.ID == id {
			return true
		}
	}
	return false
}

var _ = context.Background
