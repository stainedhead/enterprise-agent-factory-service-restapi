package tests

import (
	"slices"
	"testing"
)

func rulesOf(d obj) []obj {
	var out []obj
	rs, _ := d["rules"].([]any)
	for _, r := range rs {
		if m, ok := r.(obj); ok {
			out = append(out, m)
		}
	}
	return out
}

func strsOf(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// M5-E2: the reconciler can manage roles and rolebindings (to apply the viewer Role) and read what that Role
// grants, and nothing that would let it hand out more than it holds: no escalate, no bind, no secrets, no
// pod exec, attach, port-forward or proxy, no write on workloads.
func TestViewerKubectlRBAC(t *testing.T) {
	docs := render(t, "")
	role := find(t, docs, "ClusterRole", "afs-reconciler-viewer-kubectl")
	var managesRBAC bool
	for _, r := range rulesOf(role) {
		groups, resources, verbs := strsOf(r["apiGroups"]), strsOf(r["resources"]), strsOf(r["verbs"])
		for _, bad := range []string{"escalate", "bind", "impersonate", "*"} {
			if slices.Contains(verbs, bad) {
				t.Errorf("verb %q in %v", bad, r)
			}
		}
		for _, res := range resources {
			if slices.Contains([]string{"secrets", "pods/exec", "pods/attach", "pods/portforward", "pods/proxy", "clusterroles", "clusterrolebindings", "*"}, res) {
				t.Errorf("resource %q in %v", res, r)
			}
		}
		if slices.Contains(groups, "rbac.authorization.k8s.io") {
			managesRBAC = true
			if !slices.Equal(resources, []string{"roles", "rolebindings"}) {
				t.Errorf("rbac resources = %v: only namespaced roles and rolebindings", resources)
			}
			continue
		}
		for _, v := range verbs { // everything else is read-only
			if !slices.Contains([]string{"get", "list", "watch"}, v) {
				t.Errorf("write verb %q outside RBAC: %v", v, r)
			}
		}
	}
	if !managesRBAC {
		t.Error("the reconciler cannot manage roles and rolebindings")
	}
	b := find(t, docs, "ClusterRoleBinding", "afs-reconciler-viewer-kubectl")
	subs, _ := b["subjects"].([]any)
	if s, _ := subs[0].(obj); str(s, "name") != "afs-reconciler" || str(s, "namespace") != "afs-system" {
		t.Errorf("subject = %v", s)
	}
}

func TestViewerKubectlRBACCanBeSwitchedOff(t *testing.T) {
	docs := render(t, "reconciler:\n  viewerKubectl:\n    enabled: false\n")
	for _, d := range docs {
		if str(path(d, "metadata"), "name") == "afs-reconciler-viewer-kubectl" {
			t.Errorf("%s rendered with viewerKubectl.enabled=false", key(d))
		}
	}
}
