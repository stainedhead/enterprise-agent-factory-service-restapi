package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// WorkloadAudience is the audience of the projected ServiceAccount token an agent or pool presents to
// afs-api (API contract §2). A token for any other audience, afs-gateway included, is not accepted.
const WorkloadAudience = "afs-api"

// ClusterToken is what a verified projected ServiceAccount token says about the pod that holds it.
type ClusterToken struct {
	// Namespace and ServiceAccount identify the workload: agent-<name> or pool-<name> in a team's
	// namespace.
	Namespace      string
	ServiceAccount string
	// Pod is the name of the pod the token is bound to ("" when the token carries none).
	Pod string
}

// ClusterVerifier validates a projected ServiceAccount token offline against the cluster's OIDC issuer
// keys (RA-2b). It never calls the Kubernetes API. Verify returns domain.Unauthenticated for a token
// that is malformed, expired, too long-lived, for another audience, signed with an unknown key or issued
// by another issuer, and domain.Unavailable when the issuer keys cannot be fetched and none are cached.
type ClusterVerifier interface {
	// Issuer is the cluster's service account issuer URL.
	Issuer() string
	Verify(ctx context.Context, token string) (ClusterToken, error)
}

// WorkloadAuth implements WorkloadAuthenticator: it verifies the token, maps the namespace and
// ServiceAccount to the agent or pool through the database (afs.resolve_workload), and builds the
// workload principal. The principal's id is the agent or pool id; it holds only work-orders:report on
// work orders assigned to its team and may call GetMe (API contract §2).
type WorkloadAuth struct {
	Verifier ClusterVerifier
	Lookup   port.CredentialLookup
}

var _ WorkloadAuthenticator = (*WorkloadAuth)(nil)

// Matches reports whether token claims the cluster's issuer. It reads the claim without verifying
// anything: it only chooses which validation runs, and a token that claims the issuer must then pass
// it or be refused.
func (w *WorkloadAuth) Matches(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var c struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Issuer == "" {
		return false
	}
	return strings.TrimRight(c.Issuer, "/") == strings.TrimRight(w.Verifier.Issuer(), "/")
}

// Authenticate implements WorkloadAuthenticator. A ServiceAccount that is neither an agent nor a pool
// of an active team gets domain.Unauthenticated, like every other credential failure.
func (w *WorkloadAuth) Authenticate(ctx context.Context, token string) (*authz.Principal, error) {
	ct, err := w.Verifier.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	rec, ok, err := w.Lookup.ResolveWorkload(ctx, ct.Namespace, ct.ServiceAccount)
	if err != nil {
		return nil, err
	}
	if !ok || rec.TeamID == "" || (rec.AgentID == "") == (rec.PoolID == "") {
		return nil, domain.Unauthenticated()
	}
	wl := &authz.Workload{TeamID: rec.TeamID, AgentID: rec.AgentID, PoolID: rec.PoolID}
	id := rec.AgentID
	if rec.PoolID != "" {
		// Audit rows of a pool name the worker pod; an agent is one pod, so its id says enough.
		id, wl.WorkerPod = rec.PoolID, ct.Pod
	}
	return &authz.Principal{ID: id, Type: authz.PrincipalWorkload, Workload: wl}, nil
}
