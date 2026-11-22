package tests

import (
	"slices"
	"strings"
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

func deployEnv(t *testing.T, docs []obj, name string) map[string]string {
	t.Helper()
	d := find(t, docs, "Deployment", name)
	return envOf(path(d, "spec", "template", "spec")["containers"].([]any)[0].(obj))
}

// M5-C/E: the settings of pool workloads and viewer kubectl reach the binaries as environment variables.
func TestM5EnvironmentReachesTheBinaries(t *testing.T) {
	docs := render(t, "")
	rec, api := deployEnv(t, docs, "afs-reconciler"), deployEnv(t, docs, "afs-api")
	if rec["AFS_EFS_STORAGE_CLASS"] != "efs-sc" {
		t.Errorf("AFS_EFS_STORAGE_CLASS = %q, want the default efs-sc", rec["AFS_EFS_STORAGE_CLASS"])
	}
	if _, ok := rec["AFS_VIEWER_KUBECTL_ROLES"]; ok {
		t.Error("AFS_VIEWER_KUBECTL_ROLES rendered with no roles configured")
	}
	if _, ok := api["AFS_CLUSTER_ENDPOINT"]; ok {
		t.Error("AFS_CLUSTER_ENDPOINT rendered with no endpoint configured")
	}
	if api["AFS_CLUSTER_NAME"] != "afs-dev" {
		t.Errorf("api cluster name = %q", api["AFS_CLUSTER_NAME"])
	}

	docs = render(t, `global:
  cluster:
    endpoint: https://EXAMPLE0123456789.gr7.us-east-1.eks.amazonaws.com
    caData: Q0FEQVRB
reconciler:
  efsStorageClass: efs-prod
  viewerKubectl:
    roles:
      default: arn:aws:iam::111122223333:role/afs-viewers
`)
	rec, api = deployEnv(t, docs, "afs-reconciler"), deployEnv(t, docs, "afs-api")
	if rec["AFS_EFS_STORAGE_CLASS"] != "efs-prod" || rec["AFS_VIEWER_KUBECTL_ROLES"] != `{"default":"arn:aws:iam::111122223333:role/afs-viewers"}` {
		t.Errorf("reconciler env = %v", rec)
	}
	if api["AFS_CLUSTER_ENDPOINT"] != "https://EXAMPLE0123456789.gr7.us-east-1.eks.amazonaws.com" || api["AFS_CLUSTER_CA_DATA"] != "Q0FEQVRB" {
		t.Errorf("api env = %v", api)
	}
	for name, env := range map[string]map[string]string{"api": api, "reconciler": rec} {
		for k, v := range env {
			if strings.Contains(strings.ToLower(k), "token") || strings.Contains(v, "BEGIN ") {
				t.Errorf("%s env %s looks like a credential", name, k)
			}
		}
	}
}
