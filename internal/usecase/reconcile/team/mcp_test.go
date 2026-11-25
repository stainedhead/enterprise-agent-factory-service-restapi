package team_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

const mcpARN = "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-red/_gateway/mcp/jira-AbCdEf" //nolint:gosec // fixture ARN

func mcpFixture() team.TeamGateway {
	g := team.TeamGateway{Deployment: team.DeploymentPerNamespace, Mcp: team.McpGateway{Enabled: true, Servers: []team.McpServer{
		{ID: "mcp_1", Slug: "jira", Location: "external", URL: "https://jira.example.com/mcp", AuthKind: "oauth_user", CredentialARN: mcpARN, CredentialState: "set"},
		{ID: "mcp_2", Slug: "fed", Location: "corporate", URL: "http://tools.corp.example/mcp", AuthKind: "workload_federation", Audience: "api://b", //nolint:gosec // test fixture
			TokenEndpoint: "https://sts.corp.example/token"},
		{ID: "mcp_3", Slug: "fed2", Location: "corporate", URL: "::bad", AuthKind: "workload_federation", Audience: "api://a"},
		{ID: "mcp_4", Slug: "files", Location: "in_namespace", AuthKind: "static", Hosted: &team.HostedServer{Image: "x@sha256:00"}},
		{ID: "mcp_5", Slug: "off", Location: "external", URL: "https://off.example.com/mcp", AuthKind: "static", CredentialARN: mcpARN, CredentialState: "missing", Suspended: true},
	}}}
	return g
}

func TestMcpGateway(t *testing.T) {
	g := mcpFixture()
	eg := g.Egress("us-east-1")
	want := []team.EgressRule{{Port: 8080, InNamespace: true}, {Host: "jira.example.com", Port: 443}, {Host: "off.example.com", Port: 443},
		{Host: "sts.corp.example", Port: 443}, {Host: "tools.corp.example", Port: 80}}
	if !slices.Equal(eg, want) {
		t.Errorf("egress = %+v", eg)
	}
	creds := g.Credentials()
	if len(creds) != 2 || creds[0].Key != "mcp.jira" || creds[1].Key != "mcp.off" {
		t.Errorf("credentials = %+v", creds)
	}
	if a := g.Mcp.Audiences(); !slices.Equal(a, []string{"api://a", "api://b"}) {
		t.Errorf("audiences = %v", a)
	}
	if h := g.Mcp.HostedServers(); len(h) != 1 || h[0].Slug != "files" {
		t.Errorf("hosted = %+v", h)
	}
	conds := g.McpConditions()
	reason := func(slug string) string {
		for _, s := range conds {
			if s.Slug == slug {
				return s.Conditions[0].Reason
			}
		}
		return ""
	}
	if reason("jira") != team.ReasonMcpReady || reason("off") != team.ReasonMcpSuspended || reason("files") != team.ReasonMcpCredentialMissing {
		t.Errorf("conditions = %+v", conds)
	}
	g.Deployment = team.DeploymentShared
	if c := g.McpConditions(); c[0].Conditions[0].Reason != team.ReasonGatewayShared {
		t.Errorf("shared = %+v", c[0])
	}
	g.Mcp.Enabled = false
	if g.Mcp.Audiences() != nil || g.Mcp.HostedServers() != nil || len(g.Credentials()) != 0 || len(g.Egress("")) != 0 ||
		g.McpConditions()[0].Conditions[0].Reason != team.ReasonMcpOff {
		t.Error("MCP while off")
	}
	k := [][]byte{[]byte(strings.Repeat("a", 32)), []byte(strings.Repeat("b", 32))}
	if s := team.SessionKeys(k, "team_1"); strings.Count(s, ",") != 1 || s == team.SessionKeys(k, "team_2") || team.SessionKeys(nil, "x") != "" {
		t.Errorf("keys = %s", s)
	}
}

type mcpRec struct {
	routeRec
	n   int
	err error
}

func (r *mcpRec) ReportMcp(_ context.Context, _ string, s []team.McpStatus) error {
	r.n = len(s)
	return r.err
}

func TestControllerReportsMcp(t *testing.T) {
	d := desired(idA, "red")
	d.Gateway = mcpFixture()
	rec := &mcpRec{}
	src, cl := teamtest.NewSource(d), &teamtest.Cluster{}
	c, err := team.NewController(team.Config{Source: src, Cluster: cl, IAM: teamtest.NewIAM(), Profiles: teamtest.NewProfiles(),
		AWS:         team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: gw, SecretsPrefix: "afs"},
		TeamGateway: team.TeamGatewaySpec{Image: "registry.example.com/afs-gateway@sha256:00"}, Routes: rec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(t.Context(), ref(idA)); err != nil || rec.n != 5 {
		t.Fatalf("reported %d %v", rec.n, err)
	}
	rec.err = errors.New("db down")
	if _, err := c.Reconcile(t.Context(), ref(idA)); err == nil {
		t.Error("report failure hidden")
	}
}
