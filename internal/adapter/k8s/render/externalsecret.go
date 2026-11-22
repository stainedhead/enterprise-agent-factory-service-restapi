package render

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Refresh interval of the ExternalSecrets: a secret switched back on, or a new version that ESO has not
// seen, reaches the namespace within this long (RC-US-2.3: within 60 s).
const externalSecretRefresh = "30s"

// FileKey is the key under which a file binding's value is stored in the files Secret: stable for a
// target path, and a valid Secret key whatever characters the path has.
func FileKey(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "f-" + hex.EncodeToString(sum[:])[:16]
}

// ExternalSecrets renders at most two ExternalSecrets: one that produces the env Secret and one for the
// files Secret. A binding whose secret names an external system that is off for the agent is left out
// (EXT-7), so the Secret loses the key and the pod's reference is removed in the same pass; switching the
// system on again brings it back on the next reconcile. The value is never rendered: only the Secrets
// Manager reference and the version.
func ExternalSecrets(d agent.Desired) []*unstructured.Unstructured {
	var env, files []agent.Binding
	for _, b := range d.Bindings {
		if !b.Delivered() {
			continue
		}
		if b.Delivery == agent.DeliveryEnv {
			env = append(env, b)
		} else {
			files = append(files, b)
		}
	}
	var out []*unstructured.Unstructured
	if len(env) > 0 {
		out = append(out, externalSecret(d, agent.EnvSecretName(d.Name), env, func(b agent.Binding) string { return b.Target }))
	}
	if len(files) > 0 {
		out = append(out, externalSecret(d, agent.FilesSecretName(d.Name), files, func(b agent.Binding) string { return FileKey(b.Target) }))
	}
	return out
}

func externalSecret(d agent.Desired, name string, bindings []agent.Binding, key func(agent.Binding) string) *unstructured.Unstructured {
	sort.Slice(bindings, func(i, j int) bool { return key(bindings[i]) < key(bindings[j]) })
	data := make([]any, 0, len(bindings))
	for _, b := range bindings {
		ref := map[string]any{"key": b.SecretARN}
		if b.VersionID != "" {
			ref["version"] = b.VersionID
		}
		data = append(data, map[string]any{"secretKey": key(b), "remoteRef": ref})
	}
	u := New("external-secrets.io/v1", "ExternalSecret", d.Team.Namespace, name)
	u.Object["spec"] = map[string]any{
		"refreshInterval": externalSecretRefresh,
		"secretStoreRef":  map[string]any{"name": SecretStoreName, "kind": "SecretStore"},
		"target":          map[string]any{"name": name, "creationPolicy": "Owner"},
		"data":            data,
	}
	return Managed(u, OwnerOfAgent(d))
}
