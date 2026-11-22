package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// ConfigDir is where every configuration value is also a file, named after the variable
// (/etc/afs/config/<NAME>, CFG-7).
const ConfigDir = "/etc/afs/config"

// AgentConfigMap renders the agent's non-secret configuration values: delivered as environment variables
// (envFrom) and as files under /etc/afs/config. Values are never secret (the database refuses names that
// look like credentials); secrets go through ExternalSecrets.
func AgentConfigMap(d agent.Desired) *unstructured.Unstructured {
	u := New("v1", "ConfigMap", d.Team.Namespace, agent.ConfigMapName(d.Name))
	data := map[string]any{}
	for _, c := range d.Config {
		data[c.Name] = c.Value
	}
	u.Object["data"] = data
	return Managed(u, OwnerOfAgent(d))
}
