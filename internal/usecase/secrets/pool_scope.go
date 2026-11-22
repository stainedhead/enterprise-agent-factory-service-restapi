package secrets

import "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"

// Tag keys of the secrets afs-api writes, by owner.
const (
	// TagTeamID names the team of every secret.
	TagTeamID = "afs-team-id"
	// TagAgentID names the agent of an agent-scoped secret.
	TagAgentID = "afs-agent-id"
	// TagPoolID names the pool of a pool-scoped secret (M5-B4); the path comes from the four-argument
	// afs.secret_path, which the repository asks the database for.
	TagPoolID = "afs-pool-id"
)

// scopeTags returns the Secrets Manager tags of a new secret: its team and the one agent or pool that
// owns it. The scope is validated before this runs, so at most one of the two ids is set.
func scopeTags(teamID string, in domain.NewSecretParams) map[string]string {
	tags := map[string]string{TagTeamID: teamID}
	switch {
	case in.AgentID != "":
		tags[TagAgentID] = in.AgentID
	case in.PoolID != "":
		tags[TagPoolID] = in.PoolID
	}
	return tags
}
