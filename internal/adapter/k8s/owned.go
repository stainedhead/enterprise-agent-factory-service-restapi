package k8s

import "k8s.io/apimachinery/pkg/runtime/schema"

// OwnedKinds are the kinds the reconciler creates and the drift watch observes. A kind whose CRD is
// not installed makes NewWatcher fail, which is reported at startup rather than silently skipped.
func OwnedKinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		{Version: "v1", Kind: "Namespace"},
		{Version: "v1", Kind: "ResourceQuota"},
		{Version: "v1", Kind: "LimitRange"},
		{Version: "v1", Kind: "ConfigMap"},
		{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
		{Group: "external-secrets.io", Version: "v1", Kind: "SecretStore"},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy"},
		{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBinding"},
	}
}
