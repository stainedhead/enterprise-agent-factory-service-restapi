package render_test

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

func viewerTeam() team.Desired {
	return team.Desired{ID: "team_01JD0000000000000000000099", Slug: "payments", Namespace: "afs-t-payments"}
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// RC-US-11.1: the viewer Role can only get, list and watch, and never reaches secrets, exec, attach,
// port-forward or proxy, whichever way the rules are written.
func TestViewerRoleIsReadOnly(t *testing.T) {
	role := render.ViewerRole("afs-t-payments", viewerTeam())
	if role.GetKind() != "Role" || role.GetNamespace() != "afs-t-payments" || role.GetName() != "afs-viewer" {
		t.Fatalf("role = %s %s/%s", role.GetKind(), role.GetNamespace(), role.GetName())
	}
	if !render.IsManaged(role) || render.OwnerOf(role).TeamID != viewerTeam().ID || render.OwnerOf(role).Kind != "kubectl" {
		t.Errorf("labels = %v", role.GetLabels())
	}
	rules := role.Object["rules"].([]any)
	if len(rules) == 0 {
		t.Fatal("no rules")
	}
	for _, r := range rules {
		m := r.(map[string]any)
		verbs := strs(m["verbs"])
		for _, v := range verbs {
			if !slices.Contains([]string{"get", "list", "watch"}, v) {
				t.Errorf("verb %q in %v", v, m)
			}
		}
		for _, res := range strs(m["resources"]) {
			groups := strs(m["apiGroups"])
			switch {
			case res == "secrets", res == "pods/exec", res == "pods/attach", res == "pods/portforward", res == "pods/proxy",
				res == "configmaps", res == "serviceaccounts", res == "roles", res == "rolebindings":
				t.Errorf("resource %q is reachable: %v", res, m)
			case res == "*" && !(len(groups) == 1 && groups[0] == "agenticteammates.stainedhead.io"):
				t.Errorf("wildcard resource outside the agents' API group: %v", m)
			}
		}
		if slices.Contains(strs(m["apiGroups"]), "*") {
			t.Errorf("wildcard group: %v", m)
		}
	}
	// Workloads, logs and events are what a viewer is there for.
	have := map[string]bool{}
	for _, r := range rules {
		for _, res := range strs(r.(map[string]any)["resources"]) {
			have[res] = true
		}
	}
	for _, want := range []string{"pods", "pods/log", "events", "deployments"} {
		if !have[want] {
			t.Errorf("a viewer cannot read %s", want)
		}
	}
}

func TestViewerRoleBindingNamesTheGroupsOnly(t *testing.T) {
	b := render.ViewerRoleBinding("afs-t-payments", viewerTeam(), []string{"afs-kubectl-viewers-aaa", "afs-kubectl-viewers-bbb"})
	ref := b.Object["roleRef"].(map[string]any)
	if ref["kind"] != "Role" || ref["name"] != "afs-viewer" {
		t.Errorf("roleRef = %v", ref)
	}
	subs := b.Object["subjects"].([]any)
	if len(subs) != 2 {
		t.Fatalf("subjects = %v", subs)
	}
	for _, s := range subs {
		m := s.(map[string]any)
		if m["kind"] != "Group" {
			t.Errorf("subject %v is not a group", m)
		}
	}
	var _ *unstructured.Unstructured = b
}
