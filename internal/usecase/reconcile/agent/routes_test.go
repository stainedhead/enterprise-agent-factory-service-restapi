package agent_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

func TestRoutes(t *testing.T) {
	shared := agent.Team{ID: "team_x", Namespace: "afs-t-x", GatewayDeployment: "shared"}
	own := shared
	own.GatewayDeployment = agent.GatewayPerNamespace
	gw := agent.Gateway{Namespace: "afs-system", Name: "afs-gateway", Port: 8443}
	if gw.For(shared) != gw || gw.For(own).URL() != "https://afs-gateway.afs-t-x.svc:8443" {
		t.Errorf("For: %+v %+v", gw.For(shared), gw.For(own))
	}
	r := agent.LLMRoutes{DefaultRoute: "b", DefaultModel: "m2", Routes: []agent.RouteSpec{
		{Slug: "a", Models: []agent.RouteModel{{Name: "z"}, {Name: "y"}}},
		{Slug: "b", Models: []agent.RouteModel{{Name: "m2"}}},
		{Slug: "c"},
	}}
	if !slices.Equal(r.Slugs(), []string{"b", "a", "c"}) {
		t.Errorf("slugs = %v", r.Slugs())
	}
	if got := r.Effective([]string{"bedrock-x"}); !slices.Equal(got, []string{"bedrock-x", "a/z", "a/y", "b/m2"}) {
		t.Errorf("effective = %v", got)
	}
	if agent.RouteEnv(shared, r) != nil || len(agent.RouteEnv(own, r)) == 0 || agent.Delivered(shared) || !agent.Delivered(own) {
		t.Error("route env must be delivered on a team gateway only")
	}
	j := agent.RoutesJSON("https://gw", r)
	if !strings.HasPrefix(j, `{"version":1,"defaultRoute":"b","defaultModel":"m2","routes":[{"route":"b","baseUrl":"https://gw/r/b/v1"`) ||
		!strings.Contains(j, `"models":[{"name":"y","maxOutputTokens":0},{"name":"z","maxOutputTokens":0}]`) || !strings.Contains(j, `"route":"c","baseUrl":"https://gw/r/c/v1","protocols":[],"models":[]`) {
		t.Errorf("json = %s", j)
	}
	if agent.RoutesHash(shared, agent.LLMRoutes{}) != "" || agent.RoutesHash(shared, r) == "" ||
		agent.RoutesHash(own, r) == agent.RoutesHash(own, agent.LLMRoutes{BedrockOn: true}) {
		t.Error("routes hash")
	}
}
