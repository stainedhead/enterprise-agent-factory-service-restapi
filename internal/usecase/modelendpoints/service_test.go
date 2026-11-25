package modelendpoints_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	me "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints/modelendpointstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets/secretstest"
)

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	epOAI = "mep_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	epAZ  = "mep_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	epOR  = "mep_01JD2Q3R4S5T6V7W8X9Y0Z0003"
	epOwn = "mep_01JD2Q3R4S5T6V7W8X9Y0Z0004"
	epNou = "mep_01JD2Q3R4S5T6V7W8X9Y0Z0005"
)

type rigT struct {
	svc    *me.Service
	repo   *modelendpointstest.Repo
	audit  *fakes.Audit
	ob     *fakes.Outbox
	writer *secretstest.Writer
}

func svcEndpoint(id, slug, provider, base string) domain.ModelEndpoint {
	return domain.ModelEndpoint{ID: id, Slug: slug, DisplayName: slug, Provider: provider, Protocols: []domain.Protocol{domain.ProtocolChat},
		Location: domain.LocationExternal, BaseURL: base, AuthKind: domain.AuthBearer, CredentialMode: domain.CredentialPerTeam,
		PricingMode: domain.PricingPerToken, Enabled: true, TimeoutSeconds: 300, MaxBodyMiB: 20, Version: 1}
}

func rig() rigT {
	repo := modelendpointstest.New(now)
	oai := svcEndpoint(epOAI, "openai", "openai", "https://api.openai.com/v1")
	az := svcEndpoint(epAZ, "azure-openai", "azure", "https://<resource>.openai.azure.com/openai/v1")
	az.Enabled, az.AuthKind = false, domain.AuthAzureFederated
	or := svcEndpoint(epOR, "openrouter", "openrouter", "https://openrouter.ai/api/v1")
	or.RequiresTerms = true
	own := svcEndpoint(epOwn, "local-llm", "internal", "http://vllm")
	own.TeamID, own.Location, own.Owner, own.Purpose, own.AuthKind, own.PricingMode = teamA, domain.LocationInNamespace, "ml", "tests", domain.AuthNone, domain.PricingFree
	nous := svcEndpoint(epNou, "nous-portal", "nous", "https://inference-api.nousresearch.com/v1")
	nous.CredentialMode = domain.CredentialShared
	for _, e := range []domain.ModelEndpoint{oai, az, or, own, nous} {
		repo.Endpoints[e.ID] = me.Endpoint{ModelEndpoint: e, Models: []me.Model{{EndpointModel: domain.EndpointModel{ModelName: "m1", MaxOutputTokens: 100, Enabled: true}}}}
	}
	repo.Teams[teamA] = me.TeamGateway{Namespace: "afs-t-alpha", Deployment: domain.GatewayShared, Version: 3}
	repo.Teams[teamB] = me.TeamGateway{Namespace: "afs-t-beta", Deployment: domain.GatewayShared, MayAdd: true, Version: 1}
	audit, ob, w := &fakes.Audit{}, &fakes.Outbox{}, secretstest.NewWriter()
	svc := &me.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: ob, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}},
		Repo: repo, Secrets: w, Config: me.Config{OIDCIssuer: "https://oidc.example.com/id/X", Partition: "aws", AccountID: "111122223333", SharedGatewayURL: "https://afs-gateway.afs-system.svc:8443"}}
	return rigT{svc, repo, audit, ob, w}
}

func admin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_root", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func bound(role authz.Role) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: teamA}}}}
}

func kind(t *testing.T, err error, want domain.ErrorKind, reason string) {
	t.Helper()
	if domain.KindOf(err) != want {
		t.Fatalf("got %v, want %s", err, want)
	}
	if reason == "" {
		return
	}
	var de *domain.Error
	if !errors.As(err, &de) || (de.Reason != reason && !hasField(de, reason)) {
		t.Fatalf("got %v, want reason %s", err, reason)
	}
}

func hasField(de *domain.Error, reason string) bool {
	for _, f := range de.Fields {
		if f.Reason == reason || f.Field == reason {
			return true
		}
	}
	return false
}

func events(ob *fakes.Outbox) []string {
	var out []string
	for _, e := range ob.Events {
		out = append(out, string(e.Type))
	}
	return out
}

func newEndpoint() me.NewEndpoint {
	e := svcEndpoint("", "mistral", "other", "https://api.mistral.example/v1")
	e.Enabled = true
	return me.NewEndpoint{Endpoint: e, Models: []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "mistral-large", MaxOutputTokens: 1000, Enabled: true}}}}
}

func TestServiceOperationsDeniedToTeamRoles(t *testing.T) {
	for _, c := range []acting.Caller{bound(authz.RoleTeamAdmin), bound(authz.RoleTeamViewer), {Principal: authz.Principal{ID: "key_1", Type: authz.PrincipalAPIKey}}} {
		r := rig()
		ctx := t.Context()
		v := domain.Version(1)
		val, _ := domain.NewSecretValue("value", "sk-x")
		calls := map[string]error{}
		_, _, calls["list"] = r.svc.ListEndpoints(ctx, c, me.Filter{}, acting.Page{})
		_, calls["get"] = r.svc.GetEndpoint(ctx, c, epOAI)
		_, calls["create"] = r.svc.CreateEndpoint(ctx, c, newEndpoint(), nil)
		_, calls["update"] = r.svc.UpdateEndpoint(ctx, c, epOAI, me.Patch{}, v)
		calls["delete"] = r.svc.DeleteEndpoint(ctx, c, epOAI, v)
		_, calls["models"] = r.svc.PutModels(ctx, c, epOAI, nil, nil)
		_, calls["price"] = r.svc.CreatePrice(ctx, c, epOAI, me.PriceRow{}, nil)
		_, _, calls["proposals"] = r.svc.ListProposals(ctx, c, "", "", acting.Page{})
		_, calls["approve"] = r.svc.Approve(ctx, c, "epp_1", "", nil)
		_, _, calls["teams"] = r.svc.ListEndpointTeams(ctx, c, epOAI, acting.Page{})
		_, calls["cred"] = r.svc.PutCredential(ctx, c, epOAI, teamA, val, nil)
		_, calls["rotate"] = r.svc.RotateCredential(ctx, c, epOAI, teamA, val, nil)
		_, calls["fed"] = r.svc.PutFederation(ctx, c, epAZ, teamA, me.Federation{}, nil)
		_, calls["statement"] = r.svc.ImportStatement(ctx, c, epNou, me.Statement{}, nil)
		_, calls["providers"] = r.svc.ListProviders(ctx, c)
		_, calls["provider"] = r.svc.UpdateProvider(ctx, c, "openai", me.ProviderPatch{}, false, 1)
		_, calls["gateway"] = r.svc.PutTeamGateway(ctx, c, teamA, me.TeamGatewayPatch{}, nil)
		_, calls["report"] = r.svc.InferenceReport(ctx, c, "", me.RawReport{}, acting.Page{})
		unbound := len(c.Principal.Bindings) == 0
		denied := 0
		for name, err := range calls {
			want := domain.KindAccessDenied
			if unbound && name == "gateway" {
				want = domain.KindNotFound // a team the caller cannot see is not found
			}
			if domain.KindOf(err) != want {
				t.Errorf("%s: %v", name, err)
			}
			if want == domain.KindAccessDenied {
				denied++
			}
		}
		if len(r.ob.Events) != 0 || len(r.writer.Calls) != 0 {
			t.Fatal("a refused call changed something")
		}
		if len(r.audit.Entries) != denied {
			t.Fatalf("denied audits %d, want %d", len(r.audit.Entries), denied)
		}
	}
}

func TestTeamOperationsDeniedAndNotFound(t *testing.T) {
	r := rig()
	ctx := t.Context()
	viewer := bound(authz.RoleTeamViewer)
	val, _ := domain.NewSecretValue("value", "sk-x")
	for name, err := range map[string]error{
		"select":  second(r.svc.PutSelections(ctx, viewer, teamA, nil, nil)),
		"create":  second(r.svc.CreateTeamEndpoint(ctx, viewer, teamA, newEndpoint(), nil)),
		"update":  second(r.svc.UpdateTeamEndpoint(ctx, viewer, teamA, epOwn, me.Patch{}, 1)),
		"delete":  r.svc.DeleteTeamEndpoint(ctx, viewer, teamA, epOwn, 1),
		"models":  second(r.svc.PutTeamModels(ctx, viewer, teamA, epOwn, nil, nil)),
		"cred":    second(r.svc.PutTeamCredential(ctx, viewer, teamA, epOAI, val, nil)),
		"gateway": second(r.svc.PutTeamGateway(ctx, bound(authz.RoleTeamAdmin), teamA, me.TeamGatewayPatch{}, nil)),
	} {
		if domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A caller not bound to the team gets 404 for every team operation.
	other := acting.Caller{Principal: authz.Principal{ID: "usr_o", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamB}}}}
	if _, err := r.svc.ListTeamEndpoints(ctx, other, teamA, "", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("list: %v", err)
	}
	if _, err := r.svc.InferenceReport(ctx, other, teamA, me.RawReport{}, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("report: %v", err)
	}
	// The viewer may read the team's routes.
	if _, err := r.svc.ListTeamEndpoints(ctx, viewer, teamA, "", acting.Page{}); err != nil {
		t.Fatal(err)
	}
}

func second[T any](_ T, err error) error { return err }

func TestCreateUpdateDeleteServiceEndpoint(t *testing.T) {
	r := rig()
	ctx := t.Context()
	res, err := r.svc.CreateEndpoint(ctx, admin(), newEndpoint(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := res.Value
	if e.Enabled || e.TeamID != "" || len(e.Models) != 1 || e.Models[0].PriceState != me.PriceNone || !e.Capped() || !e.Metered() {
		t.Fatalf("created %+v", e)
	}
	if got := events(r.ob); len(got) != 1 || got[0] != "model_endpoint.created" {
		t.Fatalf("events %v", got)
	}
	// Enabling flips the switch: enabled only; with another change, updated and enabled.
	on, name := true, "Mistral"
	out, err := r.svc.UpdateEndpoint(ctx, admin(), e.ID, me.Patch{Enabled: &on, DisplayName: &name}, e.Version())
	if err != nil || !out.Enabled || out.DisplayName != "Mistral" {
		t.Fatalf("update %+v %v", out, err)
	}
	if got := events(r.ob); strings.Join(got[1:], ",") != "model_endpoint.updated,model_endpoint.enabled" {
		t.Fatalf("events %v", got)
	}
	if _, err := r.svc.UpdateEndpoint(ctx, admin(), e.ID, me.Patch{}, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	kind(t, r.svc.DeleteEndpoint(ctx, admin(), e.ID, out.Version()), domain.KindConflict, domain.ReasonEndpointEnabled)
	off := false
	out, err = r.svc.UpdateEndpoint(ctx, admin(), e.ID, me.Patch{Enabled: &off}, out.Version())
	if err != nil {
		t.Fatal(err)
	}
	if got := events(r.ob); got[len(got)-1] != "model_endpoint.disabled" {
		t.Fatalf("events %v", got)
	}
	// A no-op update still records one event.
	if out, err = r.svc.UpdateEndpoint(ctx, admin(), e.ID, me.Patch{}, out.Version()); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.DeleteEndpoint(ctx, admin(), e.ID, out.Version()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.repo.Endpoints[e.ID]; ok {
		t.Fatal("not deleted")
	}
	if got := events(r.ob); got[len(got)-1] != "model_endpoint.deleted" {
		t.Fatalf("events %v", got)
	}
	// Team endpoints are not reachable through the service operations.
	if _, err := r.svc.UpdateEndpoint(ctx, admin(), epOwn, me.Patch{}, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("team endpoint: %v", err)
	}
	kind(t, r.svc.DeleteEndpoint(ctx, admin(), epOwn, 1), domain.KindNotFound, "")
	if _, err := r.svc.PutModels(ctx, admin(), epOwn, nil, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("team models: %v", err)
	}
}

func TestCreateEndpointRefusals(t *testing.T) {
	r := rig()
	ctx := t.Context()
	bad := newEndpoint()
	bad.Endpoint.Slug = "bedrock"
	_, err := r.svc.CreateEndpoint(ctx, admin(), bad, nil)
	kind(t, err, domain.KindValidation, domain.ReasonSlugReserved)

	az := newEndpoint()
	az.Endpoint.Provider = "azure"
	_, err = r.svc.CreateEndpoint(ctx, admin(), az, nil)
	kind(t, err, domain.KindValidation, domain.ReasonProviderDisabled)

	unknown := newEndpoint()
	unknown.Endpoint.Provider = "acme"
	_, err = r.svc.CreateEndpoint(ctx, admin(), unknown, nil)
	kind(t, err, domain.KindValidation, "")

	priced := newEndpoint()
	priced.Models[0].Price = &me.Price{}
	_, err = r.svc.CreateEndpoint(ctx, admin(), priced, nil)
	kind(t, err, domain.KindValidation, "")

	dup := newEndpoint()
	dup.Models = append(dup.Models, dup.Models[0])
	_, err = r.svc.CreateEndpoint(ctx, admin(), dup, nil)
	kind(t, err, domain.KindValidation, "")

	taken := newEndpoint()
	taken.Endpoint.Slug = "openai"
	_, err = r.svc.CreateEndpoint(ctx, admin(), taken, nil)
	kind(t, err, domain.KindConflict, domain.ReasonSlugTaken)

	r.repo.Errs["InsertEndpoint"] = errors.New("boom")
	if _, err := r.svc.CreateEndpoint(ctx, admin(), newEndpoint(), nil); err == nil {
		t.Fatal("insert error swallowed")
	}
	if len(r.ob.Events) != 0 {
		t.Fatal("a refused create emitted")
	}
	// Enabling an endpoint whose provider is off, or still holding a placeholder, is refused.
	on := true
	_, err = r.svc.UpdateEndpoint(ctx, admin(), epAZ, me.Patch{Enabled: &on}, 1)
	kind(t, err, domain.KindValidation, domain.ReasonEndpointBaseURLPlaceholder)
	base := "https://acme.openai.azure.com/openai/v1"
	_, err = r.svc.UpdateEndpoint(ctx, admin(), epAZ, me.Patch{Enabled: &on, BaseURL: &base}, 1)
	kind(t, err, domain.KindValidation, domain.ReasonProviderDisabled)
	// Kept off, it may be edited.
	if _, err := r.svc.UpdateEndpoint(ctx, admin(), epAZ, me.Patch{BaseURL: &base}, 1); err != nil {
		t.Fatal(err)
	}
	ns := domain.LocationInNamespace
	_, err = r.svc.UpdateEndpoint(ctx, admin(), epOAI, me.Patch{Location: &ns}, 1)
	kind(t, err, domain.KindValidation, "")
}

func TestIdempotentCreateReplays(t *testing.T) {
	r := rig()
	ctx := t.Context()
	idem := &acting.Idem{Key: port.IdempotencyKey{Key: "k1", Operation: "CreateModelEndpoint"}, Render: func(v any) (int, json.RawMessage, error) {
		return 201, json.RawMessage(`{"id":"x"}`), nil
	}}
	if _, err := r.svc.CreateEndpoint(ctx, admin(), newEndpoint(), idem); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.CreateEndpoint(ctx, admin(), newEndpoint(), idem)
	if err != nil || res.Replay == nil {
		t.Fatalf("replay %+v %v", res, err)
	}
}

func TestListAndGetEndpoints(t *testing.T) {
	r := rig()
	ctx := t.Context()
	rows, next, err := r.svc.ListEndpoints(ctx, admin(), me.Filter{}, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("rows %d next %q err %v", len(rows), next, err)
	}
	all, _, _ := r.svc.ListEndpoints(ctx, admin(), me.Filter{IncludeTeams: true}, acting.Page{})
	svc, _, _ := r.svc.ListEndpoints(ctx, admin(), me.Filter{}, acting.Page{})
	if len(all) != len(svc)+1 {
		t.Fatalf("includeTeams %d vs %d", len(all), len(svc))
	}
	if az, _, _ := r.svc.ListEndpoints(ctx, admin(), me.Filter{Provider: "azure"}, acting.Page{}); len(az) != 1 || az[0].ProviderOn {
		t.Fatalf("azure %+v", az)
	}
	_, _, err = r.svc.ListEndpoints(ctx, admin(), me.Filter{Location: "moon"}, acting.Page{})
	kind(t, err, domain.KindValidation, "")
	_, _, err = r.svc.ListEndpoints(ctx, admin(), me.Filter{PricingMode: "barter"}, acting.Page{})
	kind(t, err, domain.KindValidation, "")
	r.repo.Errs["ListEndpoints"] = errors.New("boom")
	if _, _, err := r.svc.ListEndpoints(ctx, admin(), me.Filter{}, acting.Page{}); err == nil {
		t.Fatal("error swallowed")
	}
	if e, err := r.svc.GetEndpoint(ctx, admin(), epOAI); err != nil || e.Slug != "openai" {
		t.Fatalf("get %+v %v", e, err)
	}
	if _, err := r.svc.GetEndpoint(ctx, admin(), "mep_01JD2Q3R4S5T6V7W8X9Y0Z0099"); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestPutModelsAndPrices(t *testing.T) {
	r := rig()
	ctx := t.Context()
	ms := []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "gpt-5", MaxOutputTokens: 1000, Enabled: true}}}
	// The endpoint already lists models: If-Match is required.
	if _, err := r.svc.PutModels(ctx, admin(), epOAI, ms, nil); domain.KindOf(err) != domain.KindPreconditionRequired {
		t.Fatalf("no if-match: %v", err)
	}
	v := domain.Version(1)
	out, err := r.svc.PutModels(ctx, admin(), epOAI, ms, &v)
	if err != nil || len(out.Models) != 1 || out.Models[0].ModelName != "gpt-5" || out.Version != 2 {
		t.Fatalf("models %+v %v", out, err)
	}
	if _, err := r.svc.PutModels(ctx, admin(), epOAI, ms, &v); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	priced := []me.ModelInput{{EndpointModel: ms[0].EndpointModel, Price: &me.Price{}}}
	kind(t, second(r.svc.PutModels(ctx, admin(), epOAI, priced, nil)), domain.KindValidation, "")
	kind(t, second(r.svc.PutModels(ctx, admin(), epOAI, []me.ModelInput{{}}, nil)), domain.KindValidation, "")

	p := me.PriceRow{ModelName: "gpt-5", EffectiveFrom: now.Add(-time.Hour), Price: me.Price{Input: domain.MustMoney("1.25"), Output: domain.MustMoney("10")}}
	res, err := r.svc.CreatePrice(ctx, admin(), epOAI, p, nil)
	if err != nil || res.Value.Source != "manual" {
		t.Fatalf("price %+v %v", res, err)
	}
	if e, _ := r.svc.GetEndpoint(ctx, admin(), epOAI); e.Models[0].PriceState != me.PriceApproved || !e.Models[0].Callable(e.PricingMode) {
		t.Fatalf("not priced: %+v", e.Models[0])
	}
	p.ModelName = "gpt-9"
	kind(t, second(r.svc.CreatePrice(ctx, admin(), epOAI, p, nil)), domain.KindValidation, domain.ReasonModelNotOnRoute)
	p.ModelName, p.Source = "gpt-5", "price_list"
	kind(t, second(r.svc.CreatePrice(ctx, admin(), epOAI, p, nil)), domain.KindValidation, "")
	p.Source, p.EffectiveFrom = "", time.Time{}
	kind(t, second(r.svc.CreatePrice(ctx, admin(), epOAI, p, nil)), domain.KindValidation, "")
	p.EffectiveFrom, p.Price.Input = now, domain.MustMoney("-1")
	kind(t, second(r.svc.CreatePrice(ctx, admin(), epOAI, p, nil)), domain.KindValidation, "")
	if got := events(r.ob); strings.Join(got, ",") != "model_endpoint.updated,model_endpoint.updated" {
		t.Fatalf("events %v", got)
	}
}

func TestProposals(t *testing.T) {
	r := rig()
	ctx := t.Context()
	for _, id := range []string{"epp_1", "epp_2"} {
		r.repo.Proposals[id] = me.Proposal{ID: id, EndpointID: epOAI, ModelName: "m1", EffectiveFrom: now.Add(time.Hour), State: me.ProposalPending,
			Price: me.Price{Input: domain.MustMoney("1"), Output: domain.MustMoney("2")}, Source: "provider"}
	}
	rows, next, err := r.svc.ListProposals(ctx, admin(), "pending", "", acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || rows[0].ID != "epp_2" || next != "epp_2" {
		t.Fatalf("rows %+v next %q %v", rows, next, err)
	}
	kind(t, func() error { _, _, err := r.svc.ListProposals(ctx, admin(), "maybe", "", acting.Page{}); return err }(), domain.KindValidation, "")
	d, err := r.svc.Approve(ctx, admin(), "epp_1", "ok", nil)
	if err != nil || d.Value.State != me.ProposalApproved || d.Value.Version() != 2 || len(r.repo.Prices) != 1 {
		t.Fatalf("approve %+v %v", d, err)
	}
	kind(t, second(r.svc.Approve(ctx, admin(), "epp_1", "", nil)), domain.KindConflict, "invalid_state")
	if d, err := r.svc.Reject(ctx, admin(), "epp_2", "", nil); err != nil || d.Value.State != me.ProposalRejected {
		t.Fatalf("reject %+v %v", d, err)
	}
	kind(t, second(r.svc.Reject(ctx, admin(), "epp_9", "", nil)), domain.KindNotFound, "")
	kind(t, second(r.svc.Reject(ctx, admin(), "epp_2", strings.Repeat("x", 501), nil)), domain.KindValidation, "")
	if got := events(r.ob); strings.Join(got, ",") != "pricing.proposal_decided,pricing.proposal_decided" {
		t.Fatalf("events %v", got)
	}
	if (me.Proposal{State: me.ProposalPending}).Version() != 1 {
		t.Fatal("pending version")
	}
}

func TestEndpointTeamsAndStatements(t *testing.T) {
	r := rig()
	ctx := t.Context()
	r.repo.Selections[teamA] = []me.Selection{{EndpointID: epOAI, SelectedBy: "team_admin"}}
	rows, _, err := r.svc.ListEndpointTeams(ctx, admin(), epOAI, acting.Page{})
	if err != nil || len(rows) != 2 || !rows[0].Selected || rows[1].Selected {
		t.Fatalf("teams %+v %v", rows, err)
	}
	kind(t, func() error { _, _, err := r.svc.ListEndpointTeams(ctx, admin(), "mep_x", acting.Page{}); return err }(), domain.KindNotFound, "")

	tokens := int64(10)
	st := me.Statement{Source: me.SourceNousStatement, Period: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Rows: []me.StatementRow{{TeamID: teamA, Model: "Hermes-4", AmountUSD: domain.MustMoney("2.50"), Tokens: &tokens}, {TeamID: teamB, Model: "Hermes-4", AmountUSD: domain.MustMoney("1")}}}
	res, err := r.svc.ImportStatement(ctx, admin(), epNou, st, nil)
	if err != nil || res.Value.ID == "" || res.Value.Total().String() != "3.50" {
		t.Fatalf("import %+v %v", res, err)
	}
	kind(t, second(r.svc.ImportStatement(ctx, admin(), epNou, st, nil)), domain.KindConflict, "duplicate")
	st.Source = me.SourceOpenCodeCSV
	kind(t, second(r.svc.ImportStatement(ctx, admin(), epNou, st, nil)), domain.KindValidation, "")
	for name, mut := range map[string]func(*me.Statement){
		"source": func(s *me.Statement) { s.Source = "pdf" },
		"empty":  func(s *me.Statement) { s.Rows = nil },
		"row": func(s *me.Statement) {
			neg := int64(-1)
			s.Rows = []me.StatementRow{{TeamID: "team_x", Model: "", AmountUSD: domain.MustMoney("-1"), Tokens: &neg}}
		},
	} {
		s := st
		s.Source = me.SourceNousStatement
		mut(&s)
		if _, err := r.svc.ImportStatement(ctx, admin(), epNou, s, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
	if p, err := me.ParsePeriod("2026-10"); err != nil || p.Day() != 1 {
		t.Fatalf("period %v %v", p, err)
	}
	for _, bad := range []string{"2026-13", "2026-1", "202610", ""} {
		if _, err := me.ParsePeriod(bad); err == nil {
			t.Errorf("period %q accepted", bad)
		}
	}
}

func TestCredentials(t *testing.T) {
	r := rig()
	ctx := t.Context()
	val, _ := domain.NewSecretValue("value", "sk-team-a")
	c, err := r.svc.PutCredential(ctx, admin(), epOAI, teamA, val, nil)
	if err != nil || c.State != me.StateSet || c.TeamID != teamA || !strings.Contains(c.ARN, "afs/afs-t-alpha/_gateway/openai") {
		t.Fatalf("put %+v %v", c, err)
	}
	// It exists now: If-Match is required for a PUT, and a rotation writes a new version.
	kind(t, second(r.svc.PutCredential(ctx, admin(), epOAI, teamA, val, nil)), domain.KindPreconditionRequired, "")
	v := c.Version
	if c, err = r.svc.PutCredential(ctx, admin(), epOAI, teamA, val, &v); err != nil || c.Version != 2 {
		t.Fatalf("put again %+v %v", c, err)
	}
	if _, err := r.svc.RotateCredential(ctx, admin(), epOAI, teamA, val, nil); err != nil {
		t.Fatal(err)
	}
	kind(t, second(r.svc.PutCredential(ctx, admin(), epOAI, teamA, val, &v)), domain.KindPreconditionFailed, "")
	if strings.Join(r.writer.Calls, ",") != "create,put,put" {
		t.Fatalf("calls %v", r.writer.Calls)
	}
	kind(t, second(r.svc.RotateCredential(ctx, admin(), epOAI, teamB, val, nil)), domain.KindNotFound, "")
	// Scope rules.
	kind(t, second(r.svc.PutCredential(ctx, admin(), epOAI, "_shared", val, nil)), domain.KindValidation, "scope")
	kind(t, second(r.svc.PutCredential(ctx, admin(), epOAI, "bogus", val, nil)), domain.KindValidation, "scope")
	kind(t, second(r.svc.PutCredential(ctx, admin(), epNou, teamA, val, nil)), domain.KindValidation, "scope")
	if _, err := r.svc.PutCredential(ctx, admin(), epNou, "_shared", val, nil); err != nil {
		t.Fatal(err)
	}
	kind(t, second(r.svc.PutCredential(ctx, admin(), epAZ, teamA, val, nil)), domain.KindValidation, "authKind")
	kind(t, second(r.svc.PutCredential(ctx, admin(), epOwn, teamB, val, nil)), domain.KindValidation, "authKind")
	for _, e := range events(r.ob) {
		if e != "model_endpoint.credential_set" {
			t.Fatalf("event %s", e)
		}
	}
	// No value reaches an audit record or event.
	for _, a := range r.audit.Entries {
		if strings.Contains(strings.ToLower(toString(a.Record)), "sk-team-a") {
			t.Fatal("value audited")
		}
	}
	// A failed row write after the secret was created removes the secret again.
	r.repo.Errs["PutCredential"] = errors.New("boom")
	if _, err := r.svc.PutCredential(ctx, admin(), epOAI, teamB, val, nil); err == nil {
		t.Fatal("error swallowed")
	}
	if r.writer.Calls[len(r.writer.Calls)-1] != "force-delete" {
		t.Fatalf("not compensated: %v", r.writer.Calls)
	}
	r.writer.ForceDeleteErr = errors.New("aws down")
	orphans := 0
	r.svc.OnOrphan = func() { orphans++ }
	if _, err := r.svc.PutCredential(ctx, admin(), epOR, teamB, val, nil); err == nil || orphans != 1 {
		t.Fatalf("orphan %d %v", orphans, err)
	}
	delete(r.repo.Errs, "PutCredential")
	r.writer.CreateErr = errors.New("aws down")
	kind(t, second(r.svc.PutCredential(ctx, admin(), epOR, teamA, val, nil)), domain.KindUnavailable, "")
	r.writer.CreateErr = nil
	r.writer.PutErr = errors.New("aws down")
	kind(t, second(r.svc.RotateCredential(ctx, admin(), epOAI, teamA, val, nil)), domain.KindUnavailable, "")
	r.writer.PutErr = nil
	r.repo.Errs["GetCredential"] = errors.New("db down")
	if _, err := r.svc.RotateCredential(ctx, admin(), epOAI, teamA, val, nil); err == nil {
		t.Fatal("error swallowed")
	}
	delete(r.repo.Errs, "GetCredential")
	r.repo.Errs["CredentialPath"] = errors.New("no team")
	if _, err := r.svc.PutCredential(ctx, admin(), epOR, teamA, val, nil); err == nil {
		t.Fatal("error swallowed")
	}
}

func toString(rec port.AuditRecord) string {
	return strings.ToLower(strings.Join([]string{rec.Action, rec.ResourceID, fmtAny(rec.Before), fmtAny(rec.After)}, " "))
}

func fmtAny(v any) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		var b strings.Builder
		for k, x := range m {
			b.WriteString(k)
			if s, ok := x.(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func TestTeamCredential(t *testing.T) {
	r := rig()
	ctx := t.Context()
	val, _ := domain.NewSecretValue("value", "sk-own")
	ta := bound(authz.RoleTeamAdmin)
	if c, err := r.svc.PutTeamCredential(ctx, ta, teamA, epOAI, val, nil); err != nil || c.Scope != teamA {
		t.Fatalf("team cred %+v %v", c, err)
	}
	// The shared credential of a service endpoint is a service admin's.
	kind(t, second(r.svc.PutTeamCredential(ctx, ta, teamA, epNou, val, nil)), domain.KindAccessDenied, domain.ReasonNeedsServiceAdmin)
	kind(t, second(r.svc.PutTeamCredential(ctx, ta, teamA, "mep_01JD2Q3R4S5T6V7W8X9Y0Z0099", val, nil)), domain.KindNotFound, "")
}

func TestFederation(t *testing.T) {
	r := rig()
	ctx := t.Context()
	az := me.Federation{TenantID: "0a1b2c3d-0000-4000-8000-000000000001", ClientID: "0a1b2c3d-0000-4000-8000-000000000002"}
	f, err := r.svc.PutFederation(ctx, admin(), epAZ, teamA, az, nil)
	if err != nil || f.Kind != "azure" || f.Status != me.FedPending || f.ExpectedSubject != "system:serviceaccount:afs-t-alpha:afs-gateway" ||
		f.ExpectedAudience != "api://AzureADTokenExchange" || f.ExpectedIssuer == "" {
		t.Fatalf("azure %+v %v", f, err)
	}
	kind(t, second(r.svc.PutFederation(ctx, admin(), epAZ, teamA, az, nil)), domain.KindPreconditionRequired, "")
	v := f.Version
	if _, err := r.svc.PutFederation(ctx, admin(), epAZ, teamA, az, &v); err != nil {
		t.Fatal(err)
	}
	kind(t, second(r.svc.PutFederation(ctx, admin(), epAZ, teamA, az, &v)), domain.KindPreconditionFailed, "")
	kind(t, second(r.svc.PutFederation(ctx, admin(), epAZ, teamA, me.Federation{TenantID: "nope", ProjectID: "x"}, nil)), domain.KindValidation, "")
	kind(t, second(r.svc.PutFederation(ctx, admin(), epOAI, teamA, az, nil)), domain.KindValidation, "authKind")
	kind(t, second(r.svc.PutFederation(ctx, admin(), epAZ, "team_bad", az, nil)), domain.KindNotFound, "")
	kind(t, second(r.svc.PutFederation(ctx, admin(), epOwn, teamB, az, nil)), domain.KindNotFound, "")
	kind(t, second(r.svc.PutFederation(ctx, admin(), epAZ, "team_01JD2Q3R4S5T6V7W8X9Y0Z0009", az, nil)), domain.KindNotFound, "")

	g := me.Federation{Kind: "google", TeamID: teamA, ProjectNumber: "123456", PoolID: "afs-pool", ProviderID: "afs-eks", ProjectID: "team-alpha-ai", Location: "us-central1"}
	if err := me.ValidateFederation(g); err != nil {
		t.Fatal(err)
	}
	g.TenantID = "x"
	if err := me.ValidateFederation(g); err == nil {
		t.Fatal("tenant accepted for google")
	}
	if me.GoogleAudience(g) != "//iam.googleapis.com/projects/123456/locations/global/workloadIdentityPools/afs-pool/providers/afs-eks" {
		t.Fatal(me.GoogleAudience(g))
	}
	if me.TeamGatewayRoleName(teamA) != "afs-01jd2q3r4s5t6v7w8x9y0z0001-gateway" {
		t.Fatal(me.TeamGatewayRoleName(teamA))
	}
	// A Google endpoint gets the role ARN to map.
	vx := r.repo.Endpoints[epAZ]
	vx.ID, vx.Slug, vx.Provider, vx.AuthKind = "mep_01JD2Q3R4S5T6V7W8X9Y0Z0007", "vertex-ai", "google", domain.AuthGoogleFederated
	r.repo.Endpoints[vx.ID] = vx
	g.TenantID = ""
	gf, err := r.svc.PutFederation(ctx, admin(), vx.ID, teamA, g, nil)
	if err != nil || gf.ExpectedRoleARN != "arn:aws:iam::111122223333:role/afs-01jd2q3r4s5t6v7w8x9y0z0001-gateway" {
		t.Fatalf("google %+v %v", gf, err)
	}
	if got := events(r.ob); len(got) != 3 || got[0] != "model_endpoint.credential_set" {
		t.Fatalf("events %v", got)
	}
}

func TestProviders(t *testing.T) {
	r := rig()
	ctx := t.Context()
	ps, err := r.svc.ListProviders(ctx, admin())
	if err != nil || len(ps) != 7 {
		t.Fatalf("providers %d %v", len(ps), err)
	}
	off, reason := false, "contract ended"
	p, err := r.svc.UpdateProvider(ctx, admin(), "openai", me.ProviderPatch{Enabled: &off, Reason: &reason}, false, 1)
	if err != nil || p.Enabled || p.DisabledReason != reason || p.Version != 2 {
		t.Fatalf("off %+v %v", p, err)
	}
	on := true
	if p, err = r.svc.UpdateProvider(ctx, admin(), "openai", me.ProviderPatch{Enabled: &on}, false, 2); err != nil || p.DisabledReason != "" {
		t.Fatalf("on %+v %v", p, err)
	}
	if got := events(r.ob); strings.Join(got, ",") != "inference_provider.disabled,inference_provider.enabled" {
		t.Fatalf("events %v", got)
	}
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "openai", me.ProviderPatch{}, false, 1)), domain.KindPreconditionFailed, "")
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "acme", me.ProviderPatch{}, false, 1)), domain.KindNotFound, "")
	bad := domain.TeamSelection("anyone")
	long := strings.Repeat("x", 501)
	fb := "openai/m1"
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "openai", me.ProviderPatch{TeamSelection: &bad, Reason: &long, FallbackModel: &fb}, false, 3)), domain.KindValidation, "")
	sa := domain.SelectionServiceAdmin
	if p, err = r.svc.UpdateProvider(ctx, admin(), "openai", me.ProviderPatch{TeamSelection: &sa}, false, 3); err != nil || p.TeamSelection != sa {
		t.Fatalf("selection %+v %v", p, err)
	}

	// Bedrock off: a fallback is required; dryRun writes nothing.
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "bedrock", me.ProviderPatch{Enabled: &off}, false, 1)), domain.KindValidation, domain.ReasonNoFallbackModel)
	badfb := "bedrock-model"
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "bedrock", me.ProviderPatch{Enabled: &off, FallbackModel: &badfb}, false, 1)), domain.KindValidation, "")
	n := len(r.ob.Events)
	r.repo.Impact = me.Impact{Agents: 5, AgentsMovingToFallback: 3, TeamsToSelect: 1}
	dry, err := r.svc.UpdateProvider(ctx, admin(), "bedrock", me.ProviderPatch{Enabled: &off, FallbackModel: &fb}, true, 1)
	if err != nil || dry.Impact == nil || dry.Impact.FallbackModel != fb || !dry.Enabled || len(r.ob.Events) != n || r.repo.Default != "" {
		t.Fatalf("dry %+v %v", dry, err)
	}
	unknownRoute := "nowhere/m1"
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "bedrock", me.ProviderPatch{Enabled: &off, FallbackModel: &unknownRoute}, false, 1)), domain.KindValidation, domain.ReasonNoFallbackModel)
	offRoute := "azure-openai/m1"
	kind(t, second(r.svc.UpdateProvider(ctx, admin(), "bedrock", me.ProviderPatch{Enabled: &off, FallbackModel: &offRoute}, false, 1)), domain.KindValidation, domain.ReasonNoFallbackModel)
	b, err := r.svc.UpdateProvider(ctx, admin(), "bedrock", me.ProviderPatch{Enabled: &off, FallbackModel: &fb, SelectForAllTeams: true}, false, 1)
	if err != nil || b.Enabled || b.FallbackModel != fb || r.repo.Default != fb || len(r.repo.Selections[teamA]) != 1 || len(r.repo.Selections[teamB]) != 1 {
		t.Fatalf("bedrock off %+v %v sel %v", b, err, r.repo.Selections)
	}
}

func TestTeamGatewaySettings(t *testing.T) {
	r := rig()
	ctx := t.Context()
	d, may := domain.GatewayPerNamespace, true
	g, err := r.svc.PutTeamGateway(ctx, admin(), teamA, me.TeamGatewayPatch{Deployment: &d, MayAdd: &may}, nil)
	if err != nil || g.Deployment != d || !g.MayAdd || g.GatewayState != me.GatewayStateProvisioning || g.GatewayURL != "https://afs-gateway.afs-t-alpha.svc:8443" {
		t.Fatalf("gateway %+v %v", g, err)
	}
	if got := events(r.ob); len(got) != 1 || got[0] != "team.updated" {
		t.Fatalf("events %v", got)
	}
	stale := domain.Version(1)
	kind(t, second(r.svc.PutTeamGateway(ctx, admin(), teamA, me.TeamGatewayPatch{}, &stale)), domain.KindPreconditionFailed, "")
	bad := domain.GatewayDeployment("cluster")
	kind(t, second(r.svc.PutTeamGateway(ctx, admin(), teamA, me.TeamGatewayPatch{Deployment: &bad}, nil)), domain.KindValidation, "")
	// v1.6.0: the MCP settings are stored, a placement outside combined and separate is refused.
	sep, odd := "separate", "elsewhere"
	if g, err = r.svc.PutTeamGateway(ctx, admin(), teamA, me.TeamGatewayPatch{MayAddMcp: &may, McpPlacement: &sep}, nil); err != nil || !g.MayAddMcp || g.McpPlacement != "separate" {
		t.Fatalf("mcp settings %+v %v", g, err)
	}
	kind(t, second(r.svc.PutTeamGateway(ctx, admin(), teamA, me.TeamGatewayPatch{McpPlacement: &odd}, nil)), domain.KindValidation, "")
	shared := domain.GatewayShared
	if g, err = r.svc.PutTeamGateway(ctx, admin(), teamB, me.TeamGatewayPatch{Deployment: &shared}, nil); err != nil || g.GatewayState != me.GatewayStateShared || g.GatewayURL != "https://afs-gateway.afs-system.svc:8443" {
		t.Fatalf("shared %+v %v", g, err)
	}
	for phase, want := range map[string]string{"Ready": me.GatewayStateReady, "Degraded": me.GatewayStateDegraded, "Failed": me.GatewayStateDegraded, "Reconciling": me.GatewayStateProvisioning} {
		tg := r.repo.Teams[teamA]
		tg.GatewayPhase = phase
		r.repo.Teams[teamA] = tg
		if g, err := r.svc.PutTeamGateway(ctx, admin(), teamA, me.TeamGatewayPatch{}, nil); err != nil || g.GatewayState != want {
			t.Errorf("%s: %+v %v", phase, g, err)
		}
	}
	kind(t, second(r.svc.PutTeamGateway(ctx, admin(), "team_01JD2Q3R4S5T6V7W8X9Y0Z0009", me.TeamGatewayPatch{}, nil)), domain.KindNotFound, "")
}

func TestTeamEndpointsAndOffReasons(t *testing.T) {
	r := rig()
	ctx := t.Context()
	r.repo.Selections[teamA] = []me.Selection{{EndpointID: epOAI, Slug: "openai", SelectedBy: "team_admin"}}
	l, err := r.svc.ListTeamEndpoints(ctx, bound(authz.RoleTeamViewer), teamA, "", acting.Page{})
	if err != nil || len(l.Items) != 5 || l.Deployment != domain.GatewayShared {
		t.Fatalf("list %+v %v", l, err)
	}
	reasons := map[string]string{}
	for _, it := range l.Items {
		reasons[it.Slug] = it.Relation + ":" + it.OffReason
	}
	want := map[string]string{"openai": "selected:credential_missing", "azure-openai": "available:provider_disabled", "openrouter": "available:not_selected",
		"local-llm": "own:", "nous-portal": "available:not_selected"}
	for k, v := range want {
		if reasons[k] != v {
			t.Errorf("%s = %s, want %s", k, reasons[k], v)
		}
	}
	if l, _ := r.svc.ListTeamEndpoints(ctx, bound(authz.RoleTeamViewer), teamA, "own", acting.Page{}); len(l.Items) != 1 {
		t.Fatalf("own %+v", l.Items)
	}
	kind(t, second(r.svc.ListTeamEndpoints(ctx, bound(authz.RoleTeamViewer), teamA, "mine", acting.Page{})), domain.KindValidation, "")
	te := me.TeamEndpoint{Endpoint: me.Endpoint{ModelEndpoint: domain.ModelEndpoint{Enabled: false}, ProviderOn: true}}
	if me.OffReason(te) != me.OffEndpointDisabled {
		t.Fatal(me.OffReason(te))
	}
	te.Enabled, te.Relation, te.FederationState = true, me.RelationSelected, me.FedFailing
	if me.OffReason(te) != me.OffFederationNotReady {
		t.Fatal(me.OffReason(te))
	}
}

func TestSelections(t *testing.T) {
	r := rig()
	ctx := t.Context()
	ta := bound(authz.RoleTeamAdmin)
	res, err := r.svc.PutSelections(ctx, ta, teamA, []string{epOAI, epNou}, nil)
	if err != nil || len(res.Selections) != 2 || strings.Join(res.EndpointIDs(), ",") != epOAI+","+epNou {
		t.Fatalf("select %+v %v", res, err)
	}
	if got := events(r.ob); strings.Join(got, ",") != "model_endpoint.selected,model_endpoint.selected" {
		t.Fatalf("events %v", got)
	}
	// Extra terms or a service-selected provider need a service admin.
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, []string{epOAI, epNou, epOR}, nil)), domain.KindAccessDenied, domain.ReasonNeedsServiceAdmin)
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, []string{epAZ}, nil)), domain.KindAccessDenied, domain.ReasonNeedsServiceAdmin)
	r.repo.Admin = true
	if _, err := r.svc.PutSelections(ctx, admin(), teamA, []string{epOAI, epNou, epOR}, nil); err != nil {
		t.Fatal(err)
	}
	r.repo.Admin = false
	// Removing what a service admin selected needs a service admin.
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, []string{epOAI, epNou}, nil)), domain.KindAccessDenied, domain.ReasonNeedsServiceAdmin)
	if res, err = r.svc.PutSelections(ctx, admin(), teamA, []string{epOR}, nil); err != nil || len(res.Selections) != 1 {
		t.Fatalf("deselect %+v %v", res, err)
	}
	if got := events(r.ob); got[len(got)-1] != "model_endpoint.deselected" {
		t.Fatalf("events %v", got)
	}
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, []string{epOwn}, nil)), domain.KindValidation, "")
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, []string{"mep_01JD2Q3R4S5T6V7W8X9Y0Z0099"}, nil)), domain.KindValidation, "")
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, []string{"x", epOAI, epOAI}, nil)), domain.KindValidation, "")
	many := make([]string, me.MaxSelections+1)
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, many, nil)), domain.KindValidation, "")
	stale := domain.Version(1)
	kind(t, second(r.svc.PutSelections(ctx, ta, teamA, nil, &stale)), domain.KindPreconditionFailed, "")
}

func ownInput() me.NewEndpoint {
	e := domain.ModelEndpoint{Slug: "vllm-qwen", DisplayName: "Qwen", Protocols: []domain.Protocol{domain.ProtocolChat}, Location: domain.LocationExternal,
		BaseURL: "https://api.openai.com/v1", AuthKind: domain.AuthBearer, PricingMode: domain.PricingPerToken, Owner: "ml", Purpose: "evals"}
	return me.NewEndpoint{Endpoint: e, Models: []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "qwen", MaxOutputTokens: 100, Enabled: true}}}}
}

func TestTeamEndpointLifecycle(t *testing.T) {
	r := rig()
	ctx := t.Context()
	ta := bound(authz.RoleTeamAdmin)
	kind(t, second(r.svc.CreateTeamEndpoint(ctx, ta, teamA, ownInput(), nil)), domain.KindAccessDenied, domain.ReasonTeamEndpointsNotAllowed)
	tg := r.repo.Teams[teamA]
	tg.MayAdd = true
	r.repo.Teams[teamA] = tg
	res, err := r.svc.CreateTeamEndpoint(ctx, ta, teamA, ownInput(), nil)
	if err != nil || res.Value.TeamID != teamA || res.Value.Provider != "openai" || !res.Value.Enabled || res.Value.CredentialMode != domain.CredentialPerTeam {
		t.Fatalf("create %+v %v", res.Value, err)
	}
	priced := ownInput()
	priced.Endpoint.Slug = "priced"
	priced.Models[0].Price = &me.Price{Input: domain.MustMoney("1"), Output: domain.MustMoney("2")}
	kind(t, second(r.svc.CreateTeamEndpoint(ctx, ta, teamA, priced, nil)), domain.KindAccessDenied, domain.ReasonNeedsServiceAdmin)
	if res2, err := r.svc.CreateTeamEndpoint(ctx, admin(), teamA, priced, nil); err != nil || res2.Value.Models[0].PriceState != me.PriceApproved {
		t.Fatalf("admin priced %+v %v", res2.Value, err)
	}
	bad := ownInput()
	bad.Endpoint.Owner = ""
	kind(t, second(r.svc.CreateTeamEndpoint(ctx, ta, teamA, bad, nil)), domain.KindValidation, "")
	az := ownInput()
	az.Endpoint.Slug, az.Endpoint.BaseURL = "my-azure", "https://acme.openai.azure.com/openai/v1"
	kind(t, second(r.svc.CreateTeamEndpoint(ctx, ta, teamA, az, nil)), domain.KindValidation, domain.ReasonProviderDisabled)

	id := res.Value.ID
	corp, base := domain.LocationCorporate, "https://llm.corp.example/v1"
	out, err := r.svc.UpdateTeamEndpoint(ctx, ta, teamA, id, me.Patch{Location: &corp, BaseURL: &base}, res.Value.Version())
	if err != nil || out.Provider != "internal" {
		t.Fatalf("update %+v %v", out, err)
	}
	kind(t, second(r.svc.UpdateTeamEndpoint(ctx, ta, teamA, epOAI, me.Patch{}, 1)), domain.KindConflict, "not_team_endpoint")
	ms := []me.ModelInput{{EndpointModel: domain.EndpointModel{ModelName: "qwen-2", MaxOutputTokens: 10, Enabled: true}}}
	v := out.Version()
	if l, err := r.svc.PutTeamModels(ctx, ta, teamA, id, ms, &v); err != nil || l.Models[0].ModelName != "qwen-2" {
		t.Fatalf("models %+v %v", l, err)
	}
	ms[0].Price = &me.Price{}
	kind(t, second(r.svc.PutTeamModels(ctx, ta, teamA, id, ms, &v)), domain.KindAccessDenied, domain.ReasonNeedsServiceAdmin)
	cur := r.repo.Endpoints[id]
	kind(t, r.svc.DeleteTeamEndpoint(ctx, ta, teamA, id, domain.Version(cur.ModelEndpoint.Version)), domain.KindConflict, domain.ReasonEndpointEnabled)
	off := false
	if _, err := r.svc.UpdateTeamEndpoint(ctx, ta, teamA, id, me.Patch{Enabled: &off}, domain.Version(cur.ModelEndpoint.Version)); err != nil {
		t.Fatal(err)
	}
	cur = r.repo.Endpoints[id]
	if err := r.svc.DeleteTeamEndpoint(ctx, ta, teamA, id, domain.Version(cur.ModelEndpoint.Version)); err != nil {
		t.Fatal(err)
	}
	kind(t, r.svc.DeleteTeamEndpoint(ctx, ta, teamA, id, 1), domain.KindNotFound, "")
}

func TestDeleteBuiltinRefused(t *testing.T) {
	r := rig()
	e := r.repo.Endpoints[epAZ]
	e.Builtin = true
	r.repo.Endpoints[epAZ] = e
	kind(t, r.svc.DeleteEndpoint(t.Context(), admin(), epAZ, 1), domain.KindConflict, "invalid_state")
	kind(t, r.svc.DeleteEndpoint(t.Context(), admin(), epAZ, 9), domain.KindPreconditionFailed, "")
}

func TestInferenceReport(t *testing.T) {
	r := rig()
	ctx := t.Context()
	p50 := int64(120)
	r.repo.Report = me.Report{Rows: []me.ReportRow{{Route: "openai", Calls: 4, Errors: 1, Estimated: 2, LatencyP50: &p50}, {Route: "bedrock", Calls: 1}}}
	rep, err := r.svc.InferenceReport(ctx, admin(), "", me.RawReport{Range: "7d", GroupBy: "route,hour", Route: "openai"}, acting.Page{Limit: 1})
	if err != nil || len(rep.Rows) != 1 || rep.Next == "" || strings.Join(rep.GroupBy, ",") != "route,hour" || len(rep.Notes) == 0 {
		t.Fatalf("report %+v %v", rep, err)
	}
	if rep.Rows[0].ErrorRatePct() != 25 || rep.Rows[0].EstimatedPct() != 50 || (me.ReportRow{}).ErrorRatePct() != 0 {
		t.Fatalf("pct %v %v", rep.Rows[0].ErrorRatePct(), rep.Rows[0].EstimatedPct())
	}
	rep2, err := r.svc.InferenceReport(ctx, admin(), "", me.RawReport{Range: "7d", GroupBy: "route,hour", Route: "openai"}, acting.Page{Limit: 1, Cursor: rep.Next})
	if err != nil || len(rep2.Rows) != 1 || rep2.Rows[0].Route != "bedrock" || rep2.Next != "" {
		t.Fatalf("page 2 %+v %v", rep2, err)
	}
	kind(t, second(r.svc.InferenceReport(ctx, admin(), "", me.RawReport{Range: "24h"}, acting.Page{Cursor: rep.Next})), domain.KindValidation, "")
	kind(t, second(r.svc.InferenceReport(ctx, admin(), "", me.RawReport{}, acting.Page{Cursor: "garbage"})), domain.KindValidation, "")
	for _, raw := range []me.RawReport{{GroupBy: "planet"}, {GroupBy: "route,route"}, {GroupBy: "team,route,model,agent,hour"},
		{GroupBy: "hour", Range: "90d"}, {Route: "Bad Route"}, {Range: "1y"}} {
		if _, err := r.svc.InferenceReport(ctx, admin(), "", raw, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%+v: %v", raw, err)
		}
	}
	// Default grouping; the team report is pinned to the path's team.
	if rep, err := r.svc.InferenceReport(ctx, bound(authz.RoleTeamViewer), teamA, me.RawReport{}, acting.Page{}); err != nil || strings.Join(rep.GroupBy, ",") != "route,model" || r.repo.LastQuery.TeamID != teamA {
		t.Fatalf("team %+v %v", rep.GroupBy, err)
	}
	kind(t, second(r.svc.InferenceReport(ctx, bound(authz.RoleTeamViewer), teamA, me.RawReport{TeamID: teamB}, acting.Page{})), domain.KindValidation, "")
	r.repo.Errs["InferenceReport"] = errors.New("boom")
	if _, err := r.svc.InferenceReport(ctx, admin(), "", me.RawReport{}, acting.Page{}); err == nil {
		t.Fatal("error swallowed")
	}
}
