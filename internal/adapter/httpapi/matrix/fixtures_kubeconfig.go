package matrix

import "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"

// Case of M5-E1 (GetTeamKubeconfig). The permission is conditional: viewers and team-admins hold it only
// while the team's viewerKubectl is on, so the case runs with the flag on and every role the oracle allows
// meets a document. The flag-off refusal of the same roles is proved by TestKubeconfigFlagOffIsRefused in
// cmd/afs-api (the harness runs one flag setting per case).

func init() {
	Register(Case{Op: "GetTeamKubeconfig", Path: "/v1/teams/{teamId}/kubeconfig", Flags: authz.TeamFlags{ViewerKubectl: true}})
}
