package render

import (
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Admission objects (RC-19).
const (
	admissionAPIVersion = "admissionregistration.k8s.io/v1"
	// DedicatedNodeKey is the node label and taint key that reserves nodes for one team; its value is the
	// team id. A pod may tolerate or select it only for its own team.
	DedicatedNodeKey = "afs.stainedhead.io/dedicated-team"
	afsLabelPrefix   = "afs.stainedhead.io/"
)

// AdmissionPolicyNames are the policies, each with a binding of the same name plus "-binding".
var AdmissionPolicyNames = []string{
	"afs-protect-network-policies",
	"afs-deny-external-services",
	"afs-deny-host-access",
	"afs-protect-afs-labels",
	"afs-own-team-nodes-only",
	"afs-deny-external-routes",
	"afs-protect-paperclip-service",
}

// OwnerAdmission is the owner of the admission objects.
func OwnerAdmission() Owner { return Owner{Kind: string(engine.KindAdmission), ID: "afs-admission"} }

func celList(users []string) string {
	q := make([]string, len(users))
	for i, u := range users {
		b, _ := json.Marshal(u)
		q[i] = string(b)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

type policyDef struct {
	name       string
	rules      []map[string]any
	validation []map[string]any
	variables  []map[string]any
}

func rule(ops []any, groups, versions, resources []any) map[string]any {
	return map[string]any{"operations": ops, "apiGroups": groups, "apiVersions": versions, "resources": resources}
}

func validation(expr, msg string) map[string]any {
	return map[string]any{"expression": expr, "message": msg}
}

// podSpecExprs are the checks on a PodSpec reachable as the CEL expression spec.
func podSpecChecks(spec string) []map[string]any {
	return []map[string]any{
		validation("!has("+spec+".hostNetwork) || !"+spec+".hostNetwork", "hostNetwork is not allowed in team namespaces"),
		validation("!has("+spec+".hostPID) || !"+spec+".hostPID", "hostPID is not allowed in team namespaces"),
		validation("!has("+spec+".hostIPC) || !"+spec+".hostIPC", "hostIPC is not allowed in team namespaces"),
		validation("!has("+spec+".volumes) || "+spec+".volumes.all(v, !has(v.hostPath))", "hostPath volumes are not allowed in team namespaces"),
		validation("(has("+spec+".containers) ? "+spec+".containers : []).all(c, !has(c.securityContext) || !has(c.securityContext.privileged) || !c.securityContext.privileged)", "privileged containers are not allowed in team namespaces"),
		validation("(has("+spec+".initContainers) ? "+spec+".initContainers : []).all(c, !has(c.securityContext) || !has(c.securityContext.privileged) || !c.securityContext.privileged)", "privileged init containers are not allowed in team namespaces"),
	}
}

func nodeChecks(spec string) []map[string]any {
	own := "namespaceObject.metadata.labels['" + LabelTeamID + "']"
	return []map[string]any{
		validation("!has("+spec+".tolerations) || "+spec+".tolerations.all(t, !has(t.key) || t.key != '"+DedicatedNodeKey+"' || (has(t.value) && t.value == "+own+"))",
			"a pod may tolerate dedicated nodes only of its own team"),
		validation("!has("+spec+".nodeSelector) || !('"+DedicatedNodeKey+"' in "+spec+".nodeSelector) || "+spec+".nodeSelector['"+DedicatedNodeKey+"'] == "+own,
			"a pod may select dedicated nodes only of its own team"),
	}
}

func policyDefs(s admission.Spec) []policyDef {
	users := celList(s.ReconcilerUsers)
	all := []any{"CREATE", "UPDATE"}
	podTemplates := []any{"deployments", "statefulsets", "daemonsets", "replicasets"}
	return []policyDef{
		{
			name: AdmissionPolicyNames[0],
			rules: []map[string]any{
				rule([]any{"CREATE", "UPDATE", "DELETE"}, []any{"networking.k8s.io"}, []any{"*"}, []any{"networkpolicies"}),
				rule([]any{"CREATE", "UPDATE", "DELETE"}, []any{"cilium.io"}, []any{"*"}, []any{"ciliumnetworkpolicies", "ciliumclusterwidenetworkpolicies"}),
			},
			validation: []map[string]any{validation("request.userInfo.username in "+users, "network policies in team namespaces are managed by the AFS reconciler only")},
		},
		{
			name:  AdmissionPolicyNames[1],
			rules: []map[string]any{rule(all, []any{""}, []any{"*"}, []any{"services"})},
			validation: []map[string]any{
				validation("!has(object.spec.type) || !(object.spec.type in ['LoadBalancer', 'NodePort'])",
					"Services of type LoadBalancer and NodePort are not allowed in team namespaces"),
				validation("!has(object.spec.externalIPs) || size(object.spec.externalIPs) == 0", "Services with externalIPs are not allowed in team namespaces"),
				validation("!has(object.spec.loadBalancerSourceRanges) || size(object.spec.loadBalancerSourceRanges) == 0",
					"Services with loadBalancerSourceRanges are not allowed in team namespaces"),
			},
		},
		{
			name: AdmissionPolicyNames[2],
			rules: []map[string]any{
				rule(all, []any{""}, []any{"*"}, []any{"pods"}),
				rule(all, []any{"apps"}, []any{"*"}, podTemplates),
				rule(all, []any{"batch"}, []any{"*"}, []any{"jobs"}),
			},
			// Pods carry the spec at spec, workloads at spec.template.spec.
			variables:  []map[string]any{{"name": "podSpec", "expression": "object.kind == 'Pod' ? object.spec : object.spec.template.spec"}},
			validation: podSpecChecks("variables.podSpec"),
		},
		{
			name: AdmissionPolicyNames[3],
			rules: []map[string]any{
				rule([]any{"UPDATE"}, []any{"*"}, []any{"*"}, []any{"*"}),
			},
			variables: []map[string]any{
				{"name": "oldLabels", "expression": "oldObject.metadata.?labels.orValue({})"},
				{"name": "newLabels", "expression": "object.metadata.?labels.orValue({})"},
			},
			validation: []map[string]any{validation(
				"request.userInfo.username in "+users+" || ("+
					"variables.oldLabels.filter(k, k.startsWith('"+afsLabelPrefix+"') || k == '"+LabelManagedBy+"').all(k, k in variables.newLabels && variables.newLabels[k] == variables.oldLabels[k]) && "+
					"variables.newLabels.filter(k, k.startsWith('"+afsLabelPrefix+"') || k == '"+LabelManagedBy+"').all(k, k in variables.oldLabels))",
				"AFS labels are managed by the AFS reconciler only")},
		},
		{
			name: AdmissionPolicyNames[4],
			rules: []map[string]any{
				rule(all, []any{""}, []any{"*"}, []any{"pods"}),
				rule(all, []any{"apps"}, []any{"*"}, podTemplates),
				rule(all, []any{"batch"}, []any{"*"}, []any{"jobs"}),
			},
			variables:  []map[string]any{{"name": "podSpec", "expression": "object.kind == 'Pod' ? object.spec : object.spec.template.spec"}},
			validation: nodeChecks("variables.podSpec"),
		},
		{
			// Public ingress exists nowhere in a team namespace (COL-22, RC-23): the only way in is afs-edge.
			name: AdmissionPolicyNames[5],
			rules: []map[string]any{
				rule(all, []any{"networking.k8s.io"}, []any{"*"}, []any{"ingresses"}),
				rule(all, []any{"gateway.networking.k8s.io"}, []any{"*"}, []any{"*"}),
			},
			validation: []map[string]any{validation("false", "Ingress and Gateway API routes are not allowed in team namespaces")},
		},
		{
			// A Service named paperclip, or one whose selector is empty or reaches the collaboration pod's
			// labels, belongs to the reconciler (RD-9): otherwise a team could publish Paperclip's port itself.
			name:  AdmissionPolicyNames[6],
			rules: []map[string]any{rule(all, []any{""}, []any{"*"}, []any{"services"})},
			validation: []map[string]any{validation(
				"request.userInfo.username in "+users+" || (object.metadata.name != '"+PaperclipServiceName+"' && has(object.spec.selector) && size(object.spec.selector) > 0 && "+
					"!(['app.kubernetes.io/name', '"+LabelTeamID+"', '"+LabelAgentID+"'].exists(k, k in object.spec.selector)))",
				"a Service named paperclip, or one that selects the collaboration pod, is managed by the AFS reconciler only")},
		},
	}
}

// AdmissionObjects renders the ValidatingAdmissionPolicies and their bindings for team namespaces
// (namespaces labelled afs.stainedhead.io/team-namespace=true), in apply order: policy, then binding.
func AdmissionObjects(s admission.Spec) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, d := range policyDefs(s) {
		p := New(admissionAPIVersion, "ValidatingAdmissionPolicy", "", d.name)
		rules := make([]any, len(d.rules))
		for i, r := range d.rules {
			rules[i] = map[string]any{"operations": r["operations"], "apiGroups": r["apiGroups"], "apiVersions": r["apiVersions"], "resources": r["resources"], "scope": "Namespaced"}
		}
		vals := make([]any, len(d.validation))
		for i, v := range d.validation {
			vals[i] = map[string]any{"expression": v["expression"], "message": v["message"], "reason": "Forbidden"}
		}
		spec := map[string]any{
			"failurePolicy":    "Fail",
			"matchConstraints": map[string]any{"resourceRules": rules},
			"validations":      vals,
		}
		if len(d.variables) > 0 {
			vars := make([]any, len(d.variables))
			for i, v := range d.variables {
				vars[i] = map[string]any{"name": v["name"], "expression": v["expression"]}
			}
			spec["variables"] = vars
		}
		p.Object["spec"] = spec
		out = append(out, Managed(p, OwnerAdmission()))

		b := New(admissionAPIVersion, "ValidatingAdmissionPolicyBinding", "", d.name+"-binding")
		b.Object["spec"] = map[string]any{
			"policyName":        d.name,
			"validationActions": []any{"Deny"},
			"matchResources": map[string]any{"namespaceSelector": map[string]any{
				"matchLabels": map[string]any{LabelTeamNamespace: "true"},
			}},
		}
		out = append(out, Managed(b, OwnerAdmission()))
	}
	return out
}
