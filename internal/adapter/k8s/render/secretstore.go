package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// SecretStoreName is the External Secrets SecretStore of every team namespace.
const SecretStoreName = "afs-secrets"

// SecretStore renders the team's External Secrets SecretStore: Secrets Manager in the configured
// region, accessed through the team's IAM role, so a team's ExternalSecrets can read only what the role
// allows (the team's path, RC-1).
func SecretStore(env team.Environment) *unstructured.Unstructured {
	u := New("external-secrets.io/v1", "SecretStore", env.Team.Namespace, SecretStoreName)
	u.Object["spec"] = map[string]any{"provider": map[string]any{"aws": map[string]any{
		"service": "SecretsManager",
		"region":  env.Region,
		"role":    env.TeamRoleARN,
	}}}
	return Managed(u, OwnerOfTeam(env.Team))
}
