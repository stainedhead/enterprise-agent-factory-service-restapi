// Package mcpcatalog implements the governed MCP servers of API contract v1.6.0 §4.14 (MCP gateway PRD §7 to §11,
// Appendix A to D): the enterprise catalog and its review, the servers a team adds and switches on, service and team
// policy, upstream credential references, the effective MCP access of an agent or pool, the call ledger and its
// reports, the query capture window, and per-agent federated identities.
//
// Service paths act on any server and are for service admins and security operators; team members act through team
// paths, whose team id comes from the path (CD-45). A team-admin's approval right is the conditional grant
// mcpApprovalPolicy, decided here from mcp.team_server_approval and the server's risk tier. Every mutation writes its
// row, an audit record and its §7 event in one transaction, except the §4.12 exemptions (refresh, federation test,
// OAuth start, and stopping an already expired capture window), which are audited only. No credential value is
// stored, returned or logged: values go to Secrets Manager and the database keeps the ARN.
package mcpcatalog

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Audit resource kinds.
const (
	kindServer     = "mcp_servers"
	kindPrimitive  = "mcp_primitives"
	kindSwitch     = "team_mcp_servers"
	kindPolicy     = "mcp_policies"
	kindCredential = "mcp_credentials"
	kindCapture    = "mcp_query_captures"
	kindFederation = "mcp_federation_states"
	kindAgentFI    = "agent_federated_identities"
	kindPoolFI     = "pool_federated_identities"
)

// Settings are the mcp.* and federation.* settings the use cases decide with (PRD Appendix B).
type Settings struct {
	Enabled           bool
	Approval          string
	HostDenyList      []string
	CorporateDomains  []string
	AllowedRegistries []string
	SignaturePolicy   string
	DefaultAgentMode  domain.McpAgentMode
	CaptureMaxDays    int
	AllowedAudiences  []string
}

// ServerFilter filters ListMcpServers.
type ServerFilter struct {
	Scope        string // enterprise, team or all ("" is all)
	ReviewStatus string
	RiskTier     string
	TeamID       string
	Location     string
}

// PrimitiveFilter filters a server's inventory.
type PrimitiveFilter struct {
	Kind   string
	Status string
}

// TeamInfo is what the MCP rules need of a team.
type TeamInfo struct {
	ID                string
	Slug              string
	Namespace         string
	MayAddMcpServers  bool
	GatewayDeployment string
}

// Adoption counts the teams that have a server on and the agents and pools that reach it.
type Adoption struct {
	Teams  int
	Agents int
}

// Policy level selects the rules of one scope.
type PolicyScope struct {
	ServerID  string
	Level     string // service, team, agent or pool
	TeamID    string
	SubjectID string
}

// Repository is the MCP store (-db 000024) as the caller's session sees it.
type Repository interface {
	Settings(ctx context.Context, tx port.Tx) (Settings, error)

	ListServers(ctx context.Context, tx port.Tx, f ServerFilter, after string, limit int) ([]domain.McpServerRecord, error)
	// GetServer returns any server the session can see; NotFound otherwise. forUpdate locks the row.
	GetServer(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.McpServerRecord, error)
	// InsertServer stores a new server and its hosted spec; a taken slug is a Conflict slug_taken.
	InsertServer(ctx context.Context, tx port.Tx, s domain.McpServerRecord) error
	// UpdateServer stores every mutable column and the hosted spec; PreconditionFailed when the version is not expect.
	UpdateServer(ctx context.Context, tx port.Tx, s domain.McpServerRecord, expect domain.Version) error
	// DeleteServer removes a server; PreconditionFailed when the version is not expect.
	DeleteServer(ctx context.Context, tx port.Tx, id string, expect domain.Version) error
	// ServerInUse reports whether any team has the server switched on.
	ServerInUse(ctx context.Context, tx port.Tx, id string) (bool, error)
	Adoption(ctx context.Context, tx port.Tx, id string) (Adoption, error)
	// RequestRefresh asks the gateway to refresh the server's inventory (refresh_requested_at).
	RequestRefresh(ctx context.Context, tx port.Tx, id string) error

	ListPrimitives(ctx context.Context, tx port.Tx, serverID string, f PrimitiveFilter, after string, limit int) ([]domain.McpPrimitiveRecord, error)
	GetPrimitive(ctx context.Context, tx port.Tx, serverID, id string, forUpdate bool) (domain.McpPrimitiveRecord, error)
	// DecidePrimitive sets a primitive's status (approved adopts the held definition and pins its digest in the
	// database) and records the caller as the decider.
	DecidePrimitive(ctx context.Context, tx port.Tx, id, status string) error
	InsertReview(ctx context.Context, tx port.Tx, r domain.McpReview) error
	ReviewQueue(ctx context.Context, tx port.Tx) ([]domain.McpReviewQueueItem, error)

	ListRules(ctx context.Context, tx port.Tx, s PolicyScope) ([]domain.McpPolicyRule, error)
	// ReplaceRules replaces the scope's rules; ids names the new rows in order.
	ReplaceRules(ctx context.Context, tx port.Tx, s PolicyScope, rules []domain.McpPolicyRule, ids []string) error

	TeamInfo(ctx context.Context, tx port.Tx, teamID string) (TeamInfo, error)
	// ListTeamServers returns every server the team can see, in id order (at most a few hundred).
	ListTeamServers(ctx context.Context, tx port.Tx, teamID string) ([]domain.TeamMcpServerRow, error)
	// GetTeamServer returns a server the team can see (published or switched enterprise, or its own); NotFound otherwise.
	GetTeamServer(ctx context.Context, tx port.Tx, teamID, serverID string) (domain.TeamMcpServerRow, error)
	// PutSwitch upserts the team's switch; the database refuses team_gateway_required and team_mcp_servers_scope.
	PutSwitch(ctx context.Context, tx port.Tx, sw domain.TeamMcpSwitch) error
	// TeamEnabledServers returns the ids of the servers switched on for the team.
	TeamEnabledServers(ctx context.Context, tx port.Tx, teamID string) ([]string, error)

	GetCredential(ctx context.Context, tx port.Tx, serverID, scope string) (*domain.McpCredentialRecord, error)
	CredentialPath(ctx context.Context, tx port.Tx, teamID, slug string) (string, error)
	UpsertCredential(ctx context.Context, tx port.Tx, c domain.McpCredentialRecord) error
	// InsertOAuthState stores a connect state's hash for the team and server.
	InsertOAuthState(ctx context.Context, tx port.Tx, hash []byte, teamID, serverID string, expiresAt time.Time) error
	// ClaimOAuthState marks a live state used and returns it, or nil when it is unknown, used or expired.
	ClaimOAuthState(ctx context.Context, tx port.Tx, hash []byte) (*OAuthState, error)
	DeleteCredential(ctx context.Context, tx port.Tx, serverID, scope string) error

	RequestFederationTest(ctx context.Context, tx port.Tx, serverID, teamID string) error

	// Subject returns a live agent or pool of the team; NotFound otherwise.
	Subject(ctx context.Context, tx port.Tx, teamID, kind, id string) (domain.McpSubject, error)
	// SubjectAccount returns the namespace and service account of an agent or pool.
	SubjectAccount(ctx context.Context, tx port.Tx, teamID, kind, id string) (namespace, account string, err error)
	Admission(ctx context.Context, tx port.Tx, kind, id string) ([]domain.McpAdmissionRow, error)

	// ListEvents returns ledger rows newest first, after the keyset cursor; service reads go through
	// afs.mcp_events_for_review, team reads through row-level security.
	ListEvents(ctx context.Context, tx port.Tx, f domain.McpEventFilter, service bool, cursor string, limit int) ([]domain.McpEventRecord, error)
	UsageReport(ctx context.Context, tx port.Tx, q domain.McpUsageQuery) ([]domain.McpUsageRow, error)

	OpenCapture(ctx context.Context, tx port.Tx) (*domain.McpQueryCapture, error)
	// LatestCapture is the last window not yet marked stopped (open or expired), or nil.
	LatestCapture(ctx context.Context, tx port.Tx) (*domain.McpQueryCapture, error)
	InsertCapture(ctx context.Context, tx port.Tx, c domain.McpQueryCapture) error
	StopCapture(ctx context.Context, tx port.Tx, id string, at time.Time, by string) error

	ListFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID string) ([]domain.FederatedIdentityRecord, error)
	GetFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID, name string) (*domain.FederatedIdentityRecord, error)
	InsertFederated(ctx context.Context, tx port.Tx, kind string, f domain.FederatedIdentityRecord) error
	UpdateFederated(ctx context.Context, tx port.Tx, kind string, f domain.FederatedIdentityRecord, expect domain.Version) error
	DeleteFederated(ctx context.Context, tx port.Tx, kind, teamID, subjectID, name string, expect domain.Version) error
}

// SecretWriter is the Secrets Manager write path for credential values (the only AWS call this package makes).
type SecretWriter interface {
	Create(ctx context.Context, name string, value domain.SecretValue, token string, tags map[string]string) (arn string, err error)
	PutValue(ctx context.Context, arn string, value domain.SecretValue, token string) error
	// ForceDelete removes a secret this request created when its transaction fails.
	ForceDelete(ctx context.Context, arn string) error
	// ScheduleDeletion schedules a disconnected credential's secret for deletion.
	ScheduleDeletion(ctx context.Context, arn string, days int) error
}

// Service is the MCP use case set.
type Service struct {
	acting.Deps
	Repo    Repository
	Secrets SecretWriter
	// ClusterIssuer is the cluster's OIDC issuer, shown on federated identities for the target's trust setup.
	ClusterIssuer string
	// OAuthHTTP discovers authorization servers and redeems codes for the OAuth connect flow (oauth.go).
	OAuthHTTP OAuthClient
	// OAuth configures the OAuth connect flow.
	OAuth OAuthSettings
}

// checkService re-applies a service-path permission.
func (s *Service) checkService(ctx context.Context, c acting.Caller, action string, perm authz.Permission) error {
	if authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil
	}
	return s.Denied(ctx, c, action, "", perm)
}

// checkTeam re-applies a team-path permission: team scope, the role on the team, the permission.
func (s *Service) checkTeam(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission) error {
	switch authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

// refuse writes the denied audit record and returns AccessDenied with a contract reason.
func (s *Service) refuse(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission, reason, msg string) error {
	_ = s.Denied(ctx, c, action, teamID, perm)
	return domain.Refused(string(perm), reason, msg)
}

func (s *Service) read(ctx context.Context, c acting.Caller, fn func(ctx context.Context, tx port.Tx) error) error {
	return s.Tx.ReadTx(ctx, c.Session(), fn)
}

func (s *Service) write(ctx context.Context, c acting.Caller, fn func(ctx context.Context, tx port.Tx) error) error {
	return s.Tx.InTx(ctx, c.Session(), fn)
}

// newIDs returns n ids with prefix.
func (s *Service) newIDs(prefix domain.IDPrefix, n int) ([]string, error) {
	out := make([]string, n)
	for i := range out {
		id, err := s.IDs.NewID(prefix)
		if err != nil {
			return nil, err
		}
		out[i] = id.String()
	}
	return out, nil
}

func (s *Service) newID(prefix domain.IDPrefix) (string, error) {
	id, err := s.IDs.NewID(prefix)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// approverFor is who decides a server's review under the policy: security for enterprise servers.
func approverFor(st Settings, srv domain.McpServerRecord) string {
	if !srv.IsTeam() {
		return "security"
	}
	if domain.TeamAdminMayApprove(st.Approval, srv.RiskTier) {
		return "team_admin"
	}
	return "security"
}

func auditServer(x domain.McpServerRecord) map[string]any {
	m := map[string]any{"id": x.ID, "slug": x.Slug, "displayName": x.DisplayName, "owner": x.Owner, "purpose": x.Purpose,
		"docUrl": x.DocURL, "dataClassification": x.DataClassification, "transport": x.Transport, "location": x.Location,
		"url": x.URL, "authKind": x.AuthKind, "authHeader": x.AuthHeader, "credentialMode": x.CredentialMode,
		"reviewStatus": x.ReviewStatus, "published": x.Published, "defaultForNewTeams": x.DefaultForNewTeams,
		"riskTier": x.RiskTier, "driftPolicy": x.DriftPolicy, "allowSampling": x.AllowSampling,
		"trustAnnotations": x.TrustAnnotations, "limits": x.Limits}
	if x.TeamID != "" {
		m["teamId"] = x.TeamID
	}
	if x.Federation != nil {
		m["federation"] = *x.Federation
	}
	if x.Hosted != nil {
		m["hosted"] = *x.Hosted
	}
	if x.CABundle != "" {
		m["caBundle"] = "set"
	}
	if x.SuspendedReason != "" {
		m["suspendedReason"] = x.SuspendedReason
	}
	return m
}

func serverEventData(x domain.McpServerRecord) map[string]any {
	m := map[string]any{"serverId": x.ID, "slug": x.Slug, "scope": x.Scope()}
	if x.TeamID != "" {
		m["teamId"] = x.TeamID
	}
	return m
}

// emitServer publishes a server event in the server's team (empty for an enterprise server).
func (s *Service) emitServer(ctx context.Context, tx port.Tx, c acting.Caller, typ domain.EventType, x domain.McpServerRecord, extra map[string]any) error {
	data := serverEventData(x)
	for k, v := range extra {
		data[k] = v
	}
	return s.Emit(ctx, tx, c, typ, x.TeamID, kindServer, x.ID, data)
}

// lockServer reads a server for update and checks its version when expect is set.
func (s *Service) lockServer(ctx context.Context, tx port.Tx, id string, expect *domain.Version) (domain.McpServerRecord, error) {
	cur, err := s.Repo.GetServer(ctx, tx, id, true)
	if err != nil {
		return cur, err
	}
	if expect != nil {
		if err := domain.CheckVersion(cur.Version, *expect); err != nil {
			return cur, err
		}
	}
	return cur, nil
}

// store updates a server and reads it back.
func (s *Service) store(ctx context.Context, tx port.Tx, next domain.McpServerRecord) (domain.McpServerRecord, error) {
	if err := s.Repo.UpdateServer(ctx, tx, next, next.Version); err != nil {
		return next, err
	}
	return s.Repo.GetServer(ctx, tx, next.ID, false)
}
