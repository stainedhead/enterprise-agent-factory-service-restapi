package team_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

func gatewayFixture(deployment string) team.TeamGateway {
	return team.TeamGateway{Deployment: deployment, BedrockOn: true, Endpoints: []team.GatewayEndpoint{
		{ID: "mep_1", Slug: "openai", Location: "external", BaseURL: "https://api.openai.com/v1", AuthKind: "bearer", CredentialARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/_service/gateway/openai-AbCdEf"}, //nolint:gosec // fixture ARN
		{ID: "mep_2", Slug: "vllm", Location: "in_namespace", BaseURL: "http://vllm:8000/v1", AuthKind: "none"},
		{ID: "mep_3", Slug: "azure", Location: "external", BaseURL: "https://mx.openai.azure.com/openai/v1", AuthKind: "azure_federated",
			Federation: &team.Federation{Kind: "azure", TenantID: "t", ClientID: "c", Status: "pending"}},
		{ID: "mep_4", Slug: "vertex", Location: "external", BaseURL: "https://us-central1-aiplatform.googleapis.com/v1", AuthKind: "google_federated",
			Federation: &team.Federation{Kind: "google", ProjectNumber: "123", PoolID: "afs-pool", ProviderID: "afs-eks", Status: "failing"}},
		{ID: "mep_5", Slug: "corp", Location: "corporate", BaseURL: "https://llm.corp.example:9443/v1", AuthKind: "header"},
		{ID: "mep_6", Slug: "bad", Location: "external", BaseURL: "::not a url", AuthKind: "none"},
	}}
}

func TestTeamGatewayEgressAndCredentials(t *testing.T) {
	g := gatewayFixture(team.DeploymentPerNamespace)
	if team.TeamGatewayURL("afs-t-red") != "https://afs-gateway.afs-t-red.svc:8443" {
		t.Errorf("url = %s", team.TeamGatewayURL("afs-t-red"))
	}
	eg := g.Egress("us-east-1")
	want := []team.EgressRule{
		{Port: 8000, InNamespace: true},
		{Host: "api.openai.com", Port: 443},
		{Host: "bedrock-runtime.us-east-1.amazonaws.com", Port: 443},
		{Host: "iamcredentials.googleapis.com", Port: 443},
		{Host: "llm.corp.example", Port: 9443},
		{Host: "login.microsoftonline.com", Port: 443},
		{Host: "mx.openai.azure.com", Port: 443},
		{Host: "sts.googleapis.com", Port: 443},
		{Host: "us-central1-aiplatform.googleapis.com", Port: 443},
	}
	if !slices.Equal(eg, want) {
		t.Errorf("egress = %+v", eg)
	}
	g.BedrockOn = false
	if slices.ContainsFunc(g.Egress("us-east-1"), func(r team.EgressRule) bool { return r.Host == "bedrock-runtime.us-east-1.amazonaws.com" }) {
		t.Error("Bedrock egress while Bedrock is off")
	}
	cr := g.Credentials()
	if len(cr) != 1 || cr[0].Key != "openai" {
		t.Errorf("credentials = %+v", cr)
	}
	if !g.Federated("azure") || !g.Federated("google") || (team.TeamGateway{}).Federated("azure") {
		t.Error("federated")
	}
	if a := g.GoogleAudiences(); len(a) != 1 || a[0] != "//iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/afs-pool/providers/afs-eks" {
		t.Errorf("audiences = %v", a)
	}
}

func TestRouteHealth(t *testing.T) {
	got := map[string]team.RouteStatus{}
	for _, s := range gatewayFixture(team.DeploymentPerNamespace).RouteHealth() {
		got[s.Slug] = s
	}
	cases := map[string]struct {
		h      domain.RouteHealth
		reason string
	}{
		"openai": {domain.RouteHealthReady, team.ReasonRouteReady},
		"vllm":   {domain.RouteHealthReady, team.ReasonRouteReady},
		"azure":  {domain.RouteHealthDegraded, team.ReasonFederationPending},
		"vertex": {domain.RouteHealthUnavailable, team.ReasonFederationNotReady},
		"corp":   {domain.RouteHealthUnavailable, team.ReasonCredentialMissing},
	}
	for slug, c := range cases {
		if got[slug].Health != c.h || got[slug].Reason != c.reason {
			t.Errorf("%s = %+v", slug, got[slug])
		}
	}
	for _, s := range gatewayFixture(team.DeploymentShared).RouteHealth() {
		if s.Health != domain.RouteHealthUnavailable || s.Reason != team.ReasonGatewayShared {
			t.Errorf("shared %s = %+v", s.Slug, s)
		}
	}
}

type routeRec struct {
	team  string
	ready bool
	n     int
	err   error
}

func (r *routeRec) ReportRoutes(_ context.Context, teamID string, ready bool, routes []team.RouteStatus) error {
	r.team, r.ready, r.n = teamID, ready, len(routes)
	return r.err
}

func TestControllerReportsRoutes(t *testing.T) {
	d := desired(idA, "red")
	d.Gateway = gatewayFixture(team.DeploymentPerNamespace)
	rec := &routeRec{}
	src, cl := teamtest.NewSource(d), &teamtest.Cluster{}
	c, err := team.NewController(team.Config{Source: src, Cluster: cl, IAM: teamtest.NewIAM(), Profiles: teamtest.NewProfiles(),
		AWS:         team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: gw, SecretsPrefix: "afs"},
		TeamGateway: team.TeamGatewaySpec{Image: "registry.example.com/afs-gateway@sha256:00"}, Routes: rec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(t.Context(), ref(idA)); err != nil {
		t.Fatal(err)
	}
	if rec.team != idA || !rec.ready || rec.n != 6 {
		t.Errorf("reported %+v", rec)
	}
	rec.err = errors.New("db down")
	if _, err := c.Reconcile(t.Context(), ref(idA)); err == nil {
		t.Error("report failure hidden")
	}
}
