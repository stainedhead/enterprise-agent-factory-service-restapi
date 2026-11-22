package gateway

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// identify maps the request's source IP to the calling pod (gateway contract §3). The SigV4
// Authorization header and x-api-key the SDKs send never reach this package: the adapter drops them.
//
// An IP with no current team pod is 401, which is also what a deleted team's former IP gets. A token
// the runtime chose to send must be valid and name the same pod as the source IP.
func (s *Service) identify(ctx context.Context, req Request) (Caller, error) {
	if !s.pods.Ready() {
		return Caller{}, unavailable("pod directory is not synchronised", nil)
	}
	pod, ok := s.pods.Lookup(req.SourceIP)
	if !ok {
		return Caller{}, unauthenticated("source address is not a team pod")
	}
	if pod.TeamID == "" || (pod.AgentID == "") == (pod.PoolID == "") {
		// A team pod always belongs to its team and to exactly one agent or pool.
		return Caller{}, unauthenticated("source address is not an agent or pool pod")
	}
	if req.Token != "" {
		if err := s.checkToken(ctx, req.Token, pod); err != nil {
			return Caller{}, err
		}
	}
	return Caller{TeamID: pod.TeamID, Namespace: pod.Namespace, AgentID: pod.AgentID, PoolID: pod.PoolID,
		PodName: pod.Name, PodUID: pod.UID}, nil
}

func (s *Service) checkToken(ctx context.Context, token string, pod PodInfo) error {
	if s.tokens == nil {
		// Defence in depth is not configured: a token that cannot be reviewed is not trusted.
		return unavailable("token review is not configured", nil)
	}
	id, err := s.tokens.Review(ctx, token)
	switch {
	case errors.Is(err, ErrTokenInvalid):
		return unauthenticated("token is invalid")
	case err != nil:
		return unavailable("token review", err) // fail closed (GW-US-6.5)
	}
	if id.Namespace != pod.Namespace || id.PodName != pod.Name || (id.PodUID != "" && pod.UID != "" && id.PodUID != pod.UID) {
		return unauthenticated("token names a different pod")
	}
	return nil
}

// checkTeam refuses a team that cannot call: suspended is 403 team_suspended, anything not active is
// 401 (its pods are going away).
func (s *Service) checkTeam(ctx context.Context, c Caller) (TeamRecord, error) {
	t, err := s.store.Team(ctx, c.TeamID)
	switch {
	case errors.Is(err, ErrNotFound):
		return TeamRecord{}, unauthenticated("team does not exist")
	case err != nil:
		return TeamRecord{}, unavailable("database", err)
	}
	switch t.State {
	case "active":
		return t, nil
	case "suspended":
		return t, denied(domain.ReasonTeamSuspended, "the team is suspended")
	}
	return t, unauthenticated("team is being deleted")
}

// checkModel resolves the team's profile for the model. Only this team's team_models row is read, so a
// pod of team A can never reach team B's profile (GW-US-2.2).
func (s *Service) checkModel(ctx context.Context, c Caller, modelID string) (ModelAccess, error) {
	m, err := s.store.TeamModel(ctx, c.TeamID, modelID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ModelAccess{}, denied("", "the model is not enabled for the team")
	case err != nil:
		return ModelAccess{}, unavailable("database", err)
	case !m.Enabled:
		return ModelAccess{}, denied("", "the model is not enabled for the team")
	case m.ProfileARN == "":
		return ModelAccess{}, unavailable("the team's inference profile is not ready", nil)
	}
	return m, nil
}
