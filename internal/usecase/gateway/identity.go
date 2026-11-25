package gateway

import (
	"context"
	"errors"
	"net/netip"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Authenticate identifies the caller only. The HTTP adapter calls it before it reads a request body, so
// an unknown address costs a map lookup and not a 20 MB read; Do identifies again, which is cheap.
func (s *Service) Authenticate(ctx context.Context, ip netip.Addr, token string) (Caller, error) {
	return s.identify(ctx, Request{SourceIP: ip, Token: token})
}

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
	if s.cfg.TeamNamespace != "" && pod.Namespace != s.cfg.TeamNamespace {
		// A team gateway serves its own namespace only (gateway contract §12.1).
		return Caller{}, unauthenticated("source address is not a pod of this team gateway's namespace")
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

// Admission reasons the gateway reports (domain.ReasonModel*), and the message of each.
var admissionMessage = map[string]string{
	domain.ReasonProviderDisabled:          "the Bedrock provider is switched off",
	domain.ReasonModelNotEnabledForService: "the model is not enabled for the service",
	domain.ReasonModelNotEnabledForTeam:    "the model is not enabled for the team",
	domain.ReasonModelNotEnabledForAgent:   "the model is not enabled for this agent",
	domain.ReasonModelNotPriced:            "the model has no effective price",
}

// checkModel admits the model for the calling agent or pool (service, team, agent or pool, and priced: the
// first failing level names the reason), then resolves the team's inference profile for it. Admission failing
// closed is a 503; a model that is admitted but whose profile is not active yet is a 503 model_provisioning
// with Retry-After 30 (CD-40), never a direct call. Only this team's profile row is read, so a pod of team A can
// never reach team B's profile (GW-US-2.2).
func (s *Service) checkModel(ctx context.Context, c Caller, modelID string) (ModelAccess, error) {
	kind, id := "agent", c.AgentID
	if c.PoolID != "" {
		kind, id = "pool", c.PoolID
	}
	reasons, err := s.store.ModelAdmission(ctx, kind, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return ModelAccess{}, s.refuseModel(c, modelID, domain.ReasonModelNotEnabledForAgent)
	case err != nil:
		return ModelAccess{}, unavailable("database", err)
	}
	reason, known := reasons[modelID]
	switch {
	case !known:
		return ModelAccess{}, s.refuseModel(c, modelID, domain.ReasonModelNotEnabledForService)
	case reason != "":
		return ModelAccess{}, s.refuseModel(c, modelID, reason)
	}
	m, err := s.store.TeamModel(ctx, c.TeamID, modelID)
	switch {
	case errors.Is(err, ErrNotFound):
		return ModelAccess{}, provisioning()
	case err != nil:
		return ModelAccess{}, unavailable("database", err)
	case !m.Enabled:
		return ModelAccess{}, s.refuseModel(c, modelID, domain.ReasonModelNotEnabledForTeam)
	case m.ProfileARN == "":
		return ModelAccess{}, provisioning()
	}
	return m, nil
}

func (s *Service) refuseModel(c Caller, modelID, reason string) error {
	if s.ModelDenials != nil {
		s.ModelDenials.ModelDenied(c.TeamID, modelID, reason)
	}
	s.log.Info("model refused", "team", c.TeamID, "agent", c.AgentID, "pool", c.PoolID, "model", modelID, "reason", reason)
	msg := admissionMessage[reason]
	if msg == "" {
		msg = "the model is not enabled for the team"
	}
	return denied(reason, msg)
}

// provisioning is the 503 of an admitted model whose team inference profile is not active yet.
func provisioning() error {
	return &domain.Error{Kind: domain.KindUnavailable, Message: "the team's inference profile for the model is being created",
		Reason: "model_provisioning", RetryAfterSeconds: 30, Details: map[string]any{"reason": "model_provisioning"}}
}
