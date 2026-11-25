package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

func gatewayEnv(g team.TeamGateway) team.Environment {
	d := desired
	d.Gateway = g
	return team.Environment{Team: d, Region: "us-east-1", GatewayNamespace: "afs-system", GatewayName: "afs-gateway", GatewayPort: 8443,
		TeamGateway: team.TeamGatewaySpec{Image: "registry.example.com/afs-gateway@sha256:" + strings.Repeat("a", 64),
			DatabaseSecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/_service/gateway-db-AbCdEf",
			DatabaseHost:      "afs.cluster-x.us-east-1.rds.amazonaws.com", TLSIssuer: "afs-internal"}}
}

// MEG-RC-1..4: the team gateway's objects, its replicas, its egress and the federation token.
func TestTeamGatewayRender(t *testing.T) {
	g := team.TeamGateway{Deployment: team.DeploymentPerNamespace, BedrockOn: true, Endpoints: []team.GatewayEndpoint{
		{ID: "mep_1", Slug: "openai", Location: "external", BaseURL: "https://api.openai.com/v1", AuthKind: "bearer",
			CredentialARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/_service/gateway/openai-AbCdEf"},
		{ID: "mep_2", Slug: "vllm", Location: "in_namespace", BaseURL: "http://vllm:8000/v1", AuthKind: "none"},
		{ID: "mep_3", Slug: "azure", Location: "external", BaseURL: "https://mx.openai.azure.com/openai/v1", AuthKind: "azure_federated",
			Federation: &team.Federation{Kind: "azure", TenantID: "t", ClientID: "c", Status: "ready"}},
	}}
	objs, err := render.TeamGateway(gatewayEnv(g))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, o := range objs {
		if !render.IsManaged(o) || o.GetNamespace() != desired.Namespace {
			t.Errorf("%s/%s not managed in the team namespace", o.GetKind(), o.GetName())
		}
		kinds[o.GetKind()+"/"+o.GetName()] = true
		b, _ := json.Marshal(o.Object)
		if strings.Contains(string(b), "sk-") {
			t.Errorf("%s holds a value", o.GetKind())
		}
	}
	for _, k := range []string{"ServiceAccount/afs-gateway", "Deployment/afs-gateway", "Service/afs-gateway", "PodDisruptionBudget/afs-gateway",
		"CiliumNetworkPolicy/afs-gateway-policy", "ExternalSecret/afs-gateway-credentials", "ExternalSecret/afs-gateway-db", "Certificate/afs-gateway-tls"} {
		if !kinds[k] {
			t.Errorf("missing %s (have %v)", k, kinds)
		}
	}
	var dep appsv1.Deployment
	for _, o := range objs {
		if o.GetKind() == "Deployment" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &dep); err != nil {
				t.Fatal(err)
			}
		}
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d", *dep.Spec.Replicas)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["AFS_GATEWAY_TEAM_ID"] != desired.ID || env["AFS_GATEWAY_TEAM_NAMESPACE"] != desired.Namespace || env["AFS_GATEWAY_FEDERATION_TOKEN"] == "" {
		t.Errorf("env = %v", env)
	}
	var aud string
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Projected != nil {
			aud = v.Projected.Sources[0].ServiceAccountToken.Audience
		}
	}
	if aud != team.AzureTokenAudience {
		t.Errorf("federation audience = %q", aud)
	}
	pol, _ := json.Marshal(objs[4].Object["spec"])
	for _, want := range []string{"api.openai.com", "mx.openai.azure.com", "login.microsoftonline.com", "bedrock-runtime.us-east-1.amazonaws.com",
		"afs.cluster-x.us-east-1.rds.amazonaws.com", `"8000"`, `"8443"`} {
		if !strings.Contains(string(pol), want) {
			t.Errorf("policy lacks %s: %s", want, pol)
		}
	}

	// Google only: the provider's audience. No federation: no token.
	google := team.TeamGateway{Deployment: team.DeploymentPerNamespace, Endpoints: []team.GatewayEndpoint{{ID: "mep_4", Slug: "vertex", Location: "external",
		BaseURL: "https://aiplatform.googleapis.com/v1", AuthKind: "google_federated",
		Federation: &team.Federation{Kind: "google", ProjectNumber: "1", PoolID: "pool", ProviderID: "prov"}}}}
	objs, _ = render.TeamGateway(gatewayEnv(google))
	b, _ := json.Marshal(objs[1].Object)
	if !strings.Contains(string(b), "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/prov") {
		t.Errorf("google audience missing: %s", b)
	}
	plain := gatewayEnv(team.TeamGateway{Deployment: team.DeploymentPerNamespace})
	plain.TeamGateway.TLSIssuer, plain.TeamGateway.DatabaseSecretARN = "", ""
	objs, _ = render.TeamGateway(plain)
	b, _ = json.Marshal(objs[1].Object)
	if strings.Contains(string(b), "FEDERATION") || len(objs) != 6 {
		t.Errorf("plain gateway: %d objects, %s", len(objs), b)
	}
	if len(render.TeamGatewayGVKs) != 8 {
		t.Errorf("removal list = %d", len(render.TeamGatewayGVKs))
	}
}

// The team's agents reach their own gateway while the team is per_namespace, the shared one otherwise.
func TestCiliumTeamPolicyFollowsDeployment(t *testing.T) {
	shared, _ := json.Marshal(render.CiliumTeamPolicy(gatewayEnv(team.TeamGateway{Deployment: team.DeploymentShared})).Object["spec"])
	own, _ := json.Marshal(render.CiliumTeamPolicy(gatewayEnv(team.TeamGateway{Deployment: team.DeploymentPerNamespace})).Object["spec"])
	if !strings.Contains(string(shared), "afs-system") || strings.Contains(string(own), "afs-system") || !strings.Contains(string(own), desired.Namespace) {
		t.Errorf("shared %s\nown %s", shared, own)
	}
}
