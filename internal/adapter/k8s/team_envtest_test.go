//go:build envtest

package k8s_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

const teamA = "team_01JD0000000000000000000011"

// M1-G1a..G1d: the team controller with the real Kubernetes adapter converges the namespace, quota,
// default-deny policies, Cilium policies, SecretStore and gateway CA copy, and reports conditions.
func TestTeamEnvironmentConverges(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	envtesting.EnsureNamespace(t, env.Config, "afs-system", nil)
	src := render.New("v1", "ConfigMap", "afs-system", "afs-gateway-ca")
	src.Object["data"] = map[string]any{render.GatewayCAKey: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"}
	if _, err := c.Apply(ctx, render.Managed(src, render.OwnerPlatform())); err != nil {
		t.Fatal(err)
	}

	desired := team.Desired{ID: teamA, Slug: "g1", Namespace: "afs-t-g1", DisplayName: "G1", Kind: "persistent", State: "active", Generation: 7,
		Quotas: team.Quotas{CPU: "8", Memory: "16Gi", Pods: 20}}
	ctl, err := team.NewController(team.Config{
		Source: teamtest.NewSource(desired), Cluster: k8s.NewTeamCluster(c, k8s.GatewayCASource{Namespace: "afs-system", Name: "afs-gateway-ca"}),
		IAM: teamtest.NewIAM(), Profiles: teamtest.NewProfiles(),
		AWS:     team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", SecretsPrefix: "afs"},
		Gateway: team.PlatformSpec{GatewayNamespace: "afs-system", GatewayName: "afs-gateway", GatewayPort: 8443},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out engine.Outcome
	envtesting.Eventually(t, 60e9, "team Ready", func() bool {
		var err error
		out, err = ctl.Reconcile(ctx, engine.NewRef(engine.KindTeam, teamA, teamA))
		return err == nil && out.Generation == 7
	})
	ready := map[string]bool{}
	for _, cd := range out.Conditions {
		ready[cd.Type] = cd.Status == "True"
	}
	for _, typ := range []string{"NamespaceReady", "IamReady", "Ready"} {
		if !ready[typ] {
			t.Errorf("condition %s not True: %+v", typ, out.Conditions)
		}
	}

	ns, _ := c.Get(ctx, gvkNamespace, "", "afs-t-g1")
	if ns == nil || ns.GetLabels()["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("namespace PSA labels wrong: %v", ns)
	}
	for _, o := range []struct {
		gvkAPI, kind, name string
	}{
		{"v1", "ResourceQuota", render.QuotaName}, {"v1", "LimitRange", render.LimitRangeName},
		{"networking.k8s.io/v1", "NetworkPolicy", render.DenyIngressName}, {"networking.k8s.io/v1", "NetworkPolicy", render.DenyEgressName},
		{"v1", "ConfigMap", render.GatewayCAConfigMap}, {"external-secrets.io/v1", "SecretStore", render.SecretStoreName},
	} {
		u := render.New(o.gvkAPI, o.kind, "afs-t-g1", o.name)
		got, err := c.Get(ctx, u.GroupVersionKind(), "afs-t-g1", o.name)
		if err != nil || got == nil {
			t.Errorf("%s/%s missing: %v", o.kind, o.name, err)
		}
	}
	cm, _ := c.Get(ctx, gvkConfigMap, "afs-t-g1", render.GatewayCAConfigMap)
	if v, _, _ := unstructured.NestedString(cm.Object, "data", render.GatewayCAKey); v == "" {
		t.Error("gateway CA not copied")
	}
	cnps, err := c.List(ctx, gvkCNP, "afs-t-g1", "")
	if err != nil || len(cnps) == 0 {
		t.Errorf("no CiliumNetworkPolicy in team namespace: %v", err)
	}

	// Platform ingress policy admits only team namespaces.
	pc, err := team.NewPlatformController(k8s.NewTeamCluster(c, k8s.GatewayCASource{}), team.PlatformSpec{GatewayNamespace: "afs-system", GatewayName: "afs-gateway", GatewayPort: 8443})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Reconcile(ctx, engine.NewRef(engine.KindPlatform, "afs-platform", "")); err != nil {
		t.Fatal(err)
	}
	pol, _ := c.Get(ctx, gvkCNP, "afs-system", render.GatewayIngressPolicyName)
	if pol == nil {
		t.Fatal("gateway ingress policy missing")
	}
	from, _, _ := unstructured.NestedSlice(pol.Object, "spec", "ingress")
	if len(from) != 1 {
		t.Errorf("ingress rules = %d, want exactly one (team namespaces only)", len(from))
	}
}
