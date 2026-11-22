package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Gateway CA bundle delivery (gateway contract §3).
const (
	// GatewayCAConfigMap is the ConfigMap copied into every team namespace.
	GatewayCAConfigMap = "afs-gateway-ca"
	// GatewayCAKey is its data key; agent pods mount it at /var/run/secrets/afs/gateway/ca.crt.
	GatewayCAKey = "ca.crt"
)

// GatewayCA renders the copy of the gateway's CA bundle in a team namespace.
func GatewayCA(t team.Desired, bundle string) *unstructured.Unstructured {
	u := New("v1", "ConfigMap", t.Namespace, GatewayCAConfigMap)
	u.Object["data"] = map[string]any{GatewayCAKey: bundle}
	return Managed(u, OwnerOfTeam(t))
}
