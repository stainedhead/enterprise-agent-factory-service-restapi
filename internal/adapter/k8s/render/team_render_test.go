package render_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

var desired = team.Desired{ID: "team_01JD0000000000000000000001", Slug: "x", Namespace: "afs-t-x", Quotas: team.Quotas{CPU: "4", Memory: "8Gi", Pods: 10}}

func TestEveryRenderedObjectIsManaged(t *testing.T) {
	env := team.Environment{Team: desired, TeamRoleARN: "arn:aws:iam::111122223333:role/afs-x", Region: "us-east-1", GatewayNamespace: "afs-system", GatewayName: "afs-gateway", GatewayPort: 8443}
	objs := []*unstructured.Unstructured{render.Namespace(desired), render.ResourceQuota(desired), render.LimitRange(desired),
		render.CiliumTeamPolicy(env), render.SecretStore(env), render.GatewayCA(desired, "pem"),
		render.CiliumGatewayIngress(team.PlatformSpec{GatewayNamespace: "afs-system", GatewayName: "afs-gateway", GatewayPort: 8443})}
	objs = append(objs, render.DefaultDeny(desired)...)
	objs = append(objs, render.AdmissionObjects(admission.Spec{ReconcilerUsers: []string{"r"}})...)
	for _, o := range objs {
		if !render.IsManaged(o) {
			t.Errorf("%s/%s is not managed", o.GetKind(), o.GetName())
		}
		if o.GetKind() != "Namespace" && !strings.HasPrefix(o.GetKind(), "Validating") && o.GetName() == "" {
			t.Errorf("%s has no name", o.GetKind())
		}
	}
}

func TestNamespaceEnforcesRestricted(t *testing.T) {
	l := render.Namespace(desired).GetLabels()
	for _, k := range []string{"enforce", "audit", "warn"} {
		if l["pod-security.kubernetes.io/"+k] != "restricted" {
			t.Errorf("%s is not restricted: %v", k, l)
		}
	}
	if l[render.LabelTeamNamespace] != "true" || l[render.LabelTeamID] != desired.ID {
		t.Errorf("labels = %v", l)
	}
}

func TestQuotaOmitsUnsetFields(t *testing.T) {
	q := render.ResourceQuota(team.Desired{Namespace: "afs-t-y", Quotas: team.Quotas{Pods: 5}})
	hard, _, _ := unstructured.NestedMap(q.Object, "spec", "hard")
	if len(hard) != 1 || hard["pods"] == nil {
		t.Errorf("hard = %v, want only pods", hard)
	}
}

func TestDefaultDenyCoversBothDirections(t *testing.T) {
	got := map[string]bool{}
	for _, o := range render.DefaultDeny(desired) {
		types, _, _ := unstructured.NestedSlice(o.Object, "spec", "policyTypes")
		sel, _, _ := unstructured.NestedMap(o.Object, "spec", "podSelector")
		if len(sel) != 0 || len(types) != 1 {
			t.Errorf("%s must select every pod and one direction: %v %v", o.GetName(), sel, types)
		}
		got[types[0].(string)] = true
	}
	if !got["Ingress"] || !got["Egress"] {
		t.Errorf("directions = %v", got)
	}
}

func TestAdmissionObjects(t *testing.T) {
	objs := render.AdmissionObjects(admission.Spec{ReconcilerUsers: []string{`a"b`, "c"}})
	if len(objs) != 2*len(render.AdmissionPolicyNames) {
		t.Fatalf("objects = %d", len(objs))
	}
	for i, name := range render.AdmissionPolicyNames {
		p, b := objs[2*i], objs[2*i+1]
		if p.GetKind() != "ValidatingAdmissionPolicy" || p.GetName() != name || b.GetKind() != "ValidatingAdmissionPolicyBinding" || b.GetName() != name+"-binding" {
			t.Errorf("pair %d = %s/%s %s/%s", i, p.GetKind(), p.GetName(), b.GetKind(), b.GetName())
		}
		if pn, _, _ := unstructured.NestedString(b.Object, "spec", "policyName"); pn != name {
			t.Errorf("binding of %s points at %s", name, pn)
		}
		acts, _, _ := unstructured.NestedSlice(b.Object, "spec", "validationActions")
		if len(acts) != 1 || acts[0] != "Deny" {
			t.Errorf("%s validationActions = %v, want Deny only", name, acts)
		}
		sel, _, _ := unstructured.NestedStringMap(b.Object, "spec", "matchResources", "namespaceSelector", "matchLabels")
		if sel[render.LabelTeamNamespace] != "true" {
			t.Errorf("%s binds beyond team namespaces: %v", name, sel)
		}
	}
	// The reconciler's usernames are quoted into the expression, never concatenated raw.
	vals, _, _ := unstructured.NestedSlice(objs[0].Object, "spec", "validations")
	expr := vals[0].(map[string]any)["expression"].(string)
	if !strings.Contains(expr, `["a\"b", "c"]`) {
		t.Errorf("expression = %s", expr)
	}
}
