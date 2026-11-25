package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

func routedAgent() agent.Desired {
	d := agentDesired()
	d.Team.GatewayDeployment = agent.GatewayPerNamespace
	d.ModelID = "vllm/llama"
	d.Routes = agent.LLMRoutes{DefaultRoute: "vllm", DefaultModel: "llama", BedrockOn: true, Routes: []agent.RouteSpec{
		{Slug: "openai", Protocols: []string{"openai_chat"}, Pricing: "per_token", Models: []agent.RouteModel{{Name: "gpt-5", MaxOutputTokens: 8000}}},
		{Slug: "vllm", Protocols: []string{"openai_chat"}, Pricing: "free", Models: []agent.RouteModel{{Name: "llama", MaxOutputTokens: 2000}}},
	}}
	return d
}

// MEG-RC-3: on a team gateway the pod gets AFS_GATEWAY_URL on the team's gateway, the AFS_LLM_* and OPENAI_*
// variables after it, AFS_DEFAULT_MODEL as <route>/<model> and /etc/afs/llm-routes.json.
func TestAgentRouteDelivery(t *testing.T) {
	d := routedAgent()
	env := environment(d)
	env.Gateway = env.Gateway.For(d.Team)
	pod := render.PodTemplate(env)
	e := envOf(pod)
	if e["AFS_GATEWAY_URL"].Value != "https://afs-gateway.afs-t-alpha.svc:8443" || e["AFS_DEFAULT_MODEL"].Value != "vllm/llama" ||
		e["AFS_LLM_ROUTES"].Value != "vllm,openai" || e["AFS_LLM_VLLM_BASE_URL"].Value != "$(AFS_GATEWAY_URL)/r/vllm/v1" ||
		e["AFS_LLM_BEDROCK_BASE_URL"].Value == "" || e["OPENAI_API_KEY"].Value != "afs-gateway" || e["AFS_LLM_DEFAULT_MODEL"].Value != "llama" {
		t.Errorf("env = %+v", e)
	}
	var gwAt, routesAt int
	for i, v := range pod.Spec.Containers[0].Env {
		switch v.Name {
		case "AFS_GATEWAY_URL":
			gwAt = i
		case "AFS_LLM_ROUTES":
			routesAt = i
		}
	}
	if routesAt < gwAt {
		t.Error("route variables must follow AFS_GATEWAY_URL for $(AFS_GATEWAY_URL) to expand")
	}
	if _, ok := mountOf(pod)[render.RoutesPath]; !ok {
		t.Error("llm-routes.json not mounted")
	}
	cm := render.AgentAWSConfigMap(d)
	data, _ := cm.Object["data"].(map[string]any)
	var file struct {
		Version      int    `json:"version"`
		DefaultRoute string `json:"defaultRoute"`
		Routes       []struct {
			Route string `json:"route"`
		} `json:"routes"`
	}
	raw, _ := data["llm-routes.json"].(string)
	if err := json.Unmarshal([]byte(raw), &file); err != nil || file.Version != 1 || file.DefaultRoute != "vllm" || len(file.Routes) != 2 ||
		!strings.Contains(raw, `"baseUrl":"https://afs-gateway.afs-t-alpha.svc:8443/r/vllm/v1"`) || !strings.Contains(raw, `"maxOutputTokens":2000`) {
		t.Errorf("llm-routes.json = %s (%v)", raw, err)
	}
	if agent.PodTemplateHash(d) == agent.PodTemplateHash(agentDesired()) {
		t.Error("routes do not change the pod-template hash")
	}
}

// A team on the shared gateway gets no route variables, no file and an unchanged hash (1.1.0 behaviour).
func TestSharedTeamGetsNoRoutes(t *testing.T) {
	d := agentDesired()
	pod := render.PodTemplate(environment(d))
	e := envOf(pod)
	for _, n := range []string{"AFS_LLM_ROUTES", "OPENAI_BASE_URL", "AFS_LLM_BEDROCK_BASE_URL"} {
		if _, ok := e[n]; ok {
			t.Errorf("%s on a shared team", n)
		}
	}
	if _, ok := mountOf(pod)[render.RoutesPath]; ok {
		t.Error("llm-routes.json on a shared team")
	}
	if data, _ := render.AgentAWSConfigMap(d).Object["data"].(map[string]any); data["llm-routes.json"] != nil {
		t.Error("llm-routes.json in the ConfigMap of a shared team")
	}
	if agent.RoutesHash(d.Team, d.Routes) != "" {
		t.Error("shared team has a routes hash line")
	}
}

func TestPoolRouteDelivery(t *testing.T) {
	d := poolDesired()
	d.Team.GatewayDeployment = agent.GatewayPerNamespace
	d.Routes = agent.LLMRoutes{Routes: []agent.RouteSpec{{Slug: "vllm", Models: []agent.RouteModel{{Name: "llama"}}}}}
	env := poolEnv(d)
	env.Gateway = env.Gateway.For(d.Team)
	e := map[string]string{}
	for _, v := range render.PoolPodTemplate(env).Spec.Containers[0].Env {
		e[v.Name] = v.Value
	}
	if e["AFS_LLM_ROUTES"] != "vllm" || e["AFS_GATEWAY_URL"] != "https://afs-gateway."+d.Team.Namespace+".svc:8443" {
		t.Errorf("pool env = %v", e)
	}
	if data, _ := render.PoolAWSConfigMap(d).Object["data"].(map[string]any); data["llm-routes.json"] == nil {
		t.Error("pool llm-routes.json missing")
	}
}
