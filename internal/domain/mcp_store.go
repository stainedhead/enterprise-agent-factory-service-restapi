package domain

import (
	"encoding/json"
	"strconv"
	"time"
)

// Stored MCP records (database contract §7.5, -db 000024) as the API reads and writes them. They carry no credential
// value: a credential is an ARN plus metadata, and the API only ever exposes its state.

// Identifier prefixes of the MCP tables (-db 000024, afs.ulid_ok checks).
const (
	PrefixMcpServer      IDPrefix = "mcp"
	PrefixMcpPrimitive   IDPrefix = "mpr"
	PrefixMcpPolicy      IDPrefix = "mpol"
	PrefixMcpCredential  IDPrefix = "mcr"
	PrefixMcpReview      IDPrefix = "mrv"
	PrefixMcpCapture     IDPrefix = "mqc"
	PrefixAgentFederated IDPrefix = "afi"
	PrefixPoolFederated  IDPrefix = "pfi"
)

// McpCheck is one automatic check of a server (PRD §8.1): reachability, tls, protocol_version, host_policy,
// inventory, description_injection, image_signature, image_digest, image_scan.
type McpCheck struct {
	Name      string    `json:"name"`
	Outcome   string    `json:"outcome"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Check outcomes.
const (
	McpCheckPass = "pass"
	McpCheckWarn = "warn"
	McpCheckFail = "fail"
)

// McpLimits are a server's own limits; zero members take mcp.limits.
type McpLimits struct {
	RequestBytes       int `json:"requestBytes,omitempty"`
	ResponseBytes      int `json:"responseBytes,omitempty"`
	TimeoutSeconds     int `json:"timeoutSeconds,omitempty"`
	ConcurrentPerAgent int `json:"concurrentPerAgent,omitempty"`
}

// Validate checks the ranges of the contract's McpLimits.
func (l McpLimits) Validate() error {
	bad := func(v, lo, hi int) bool { return v != 0 && (v < lo || v > hi) }
	switch {
	case bad(l.RequestBytes, 1024, 16777216):
		return Validation(FieldError{Field: "limits.requestBytes", Reason: "must be 1024 to 16777216"})
	case bad(l.ResponseBytes, 1024, 67108864):
		return Validation(FieldError{Field: "limits.responseBytes", Reason: "must be 1024 to 67108864"})
	case bad(l.TimeoutSeconds, 1, 600):
		return Validation(FieldError{Field: "limits.timeoutSeconds", Reason: "must be 1 to 600"})
	case bad(l.ConcurrentPerAgent, 1, 64):
		return Validation(FieldError{Field: "limits.concurrentPerAgent", Reason: "must be 1 to 64"})
	}
	return nil
}

// Credential modes of an enterprise server.
const (
	McpCredentialShared  = "shared"
	McpCredentialPerTeam = "per_team"
)

// Drift policies.
const (
	McpDriftHold           = "hold"
	McpDriftAutoCompatible = "auto_approve_compatible"
)

// Credential states as the API shows them.
const (
	McpCredSet         = "set"
	McpCredMissing     = "missing"
	McpCredExpired     = "expired"
	McpCredNotRequired = "not_required"
)

// McpSharedScope is the credential scope of an enterprise server's one shared credential.
const McpSharedScope = "_shared"

// McpServerRecord is a stored server with its review, risk and bookkeeping fields.
type McpServerRecord struct {
	McpServer
	CABundle         string
	CredentialMode   string
	ProtocolVersion  string
	SuspendedFrom    McpReviewStatus
	SuspendedReason  string
	RiskTier         string
	RiskReasons      []string
	Checks           []McpCheck
	DriftPolicy      string
	AllowSampling    bool
	TrustAnnotations bool
	Limits           McpLimits
	Version          Version
	Generation       int64
	CreatedAt        time.Time
	CreatedBy        string
	UpdatedAt        time.Time
}

// Scope is enterprise or team.
func (s McpServerRecord) Scope() string {
	if s.IsTeam() {
		return "team"
	}
	return "enterprise"
}

// CredentialScope is the scope of the credential the gateway uses for teamID: the shared one, or the team's.
func (s McpServerRecord) CredentialScope(teamID string) string {
	if !s.IsTeam() && (s.CredentialMode == McpCredentialShared || teamID == "") {
		return McpSharedScope
	}
	if s.IsTeam() {
		return s.TeamID
	}
	return teamID
}

// McpPrimitiveRecord is one tool, resource, resource template or prompt of a server's inventory.
type McpPrimitiveRecord struct {
	ID                string
	ServerID          string
	TeamID            string
	Kind              string
	Name              string
	Title             string
	Summary           string
	Annotations       json.RawMessage
	Definition        json.RawMessage
	PendingDefinition json.RawMessage
	Digest            string
	ApprovedDigest    string
	Change            string
	Status            string
	DecidedBy         string
	FirstSeen         time.Time
	LastSeen          time.Time
}

// Primitive statuses.
const (
	McpPrimitiveApproved = "approved"
	McpPrimitivePending  = "pending_review"
	McpPrimitiveBlocked  = "blocked"
)

// McpDecisionView is a primitive's admission decision for one subject or team.
type McpDecisionView struct {
	Allowed bool   `json:"allowed"`
	Level   string `json:"level"`
	Reason  string `json:"reason"`
}

// McpReview is one review record (append-only, afs.mcp_reviews).
type McpReview struct {
	ID          string
	ServerID    string
	TeamID      string
	PrimitiveID string
	At          time.Time
	Checks      []McpCheck
	RiskTier    string
	Approver    string
	Decision    string
	Note        string
	DecidedBy   string
}

// Review decisions.
const (
	McpDecisionApproved         = "approved"
	McpDecisionRejected         = "rejected"
	McpDecisionChangesRequested = "changes_requested"
	McpDecisionBlocked          = "blocked"
)

// McpReviewQueueItem is one entry of afs.mcp_review_queue().
type McpReviewQueueItem struct {
	Kind        string
	ServerID    string
	TeamID      string
	PrimitiveID string
	Slug        string
	Name        string
	RiskTier    string
	RiskReasons []string
	Approver    string
	Since       time.Time
}

// TeamMcpSwitch is a team's switch for one server (afs.team_mcp_servers).
type TeamMcpSwitch struct {
	TeamID    string
	ServerID  string
	Enabled   bool
	Mode      McpTeamMode
	EnabledBy string
	EnabledAt time.Time
	Version   Version
	UpdatedAt time.Time
}

// McpCredentialRecord is a credential reference (afs.mcp_credentials): an ARN and metadata, never a value.
type McpCredentialRecord struct {
	ID        string
	ServerID  string
	Scope     string
	TeamID    string
	Kind      string
	SecretARN string
	Subject   string
	Scopes    []string
	ExpiresAt *time.Time
	Status    string
	RotatedAt *time.Time
	Version   Version
}

// TeamMcpServerRow is a server as one team sees it, with the team's switch, credential and federation state.
type TeamMcpServerRow struct {
	Server          McpServerRecord
	Switch          *TeamMcpSwitch
	Credential      *McpCredentialRecord
	FederationState string
}

// State is on, off or pending (a team server not yet approved) for the team.
func (r TeamMcpServerRow) State() string {
	if r.Server.IsTeam() && r.Server.ReviewStatus != McpApproved && r.Server.ReviewStatus != McpSuspended {
		return "pending"
	}
	if r.Switch != nil && r.Switch.Enabled {
		return "on"
	}
	return "off"
}

// McpCredentialStateOf is the credentialState of a server for a credential row (nil: none stored).
func McpCredentialStateOf(kind McpAuthKind, c *McpCredentialRecord) string {
	if !kind.NeedsCredential() {
		return McpCredNotRequired
	}
	if c == nil {
		return McpCredMissing
	}
	return c.Status
}

// McpAdmissionRow is one row of afs.mcp_admission(kind, id).
type McpAdmissionRow struct {
	ServerID    string
	Slug        string
	ServerKind  string
	Mode        string
	PrimitiveID string
	Kind        string
	Name        string
	Digest      string
	Allowed     bool
	Level       string
	Reason      string
	RateLimit   *int
}

// McpSubject is an agent or a pool.
type McpSubject struct {
	Kind   string // agent or pool
	TeamID string
	ID     string
	Access string
	Mode   McpAgentMode
}

// McpEventRecord is one ledger row (afs.mcp_events). Never arguments, results or credentials.
type McpEventRecord struct {
	ID                 int64
	At                 time.Time
	RequestID          string
	TeamID             string
	AgentID            string
	PoolID             string
	WorkerPod          string
	ServerID           string
	ServerKind         string
	Method             string
	Name               string
	PrimitiveID        string
	Digest             string
	Outcome            string
	Reason             string
	JSONRPCCode        *int
	HTTPStatus         *int
	GatewayMs          *int
	UpstreamMs         *int
	RequestBytes       *int
	ResponseBytes      *int
	AuthKind           string
	ProtocolDownstream string
	ProtocolUpstream   string
	ArgsSHA256         string
	SearchResults      []string
	SearchHit          *bool
	QueryText          string
}

// McpEventFilter filters the ledger.
type McpEventFilter struct {
	From, To                time.Time
	TeamID, AgentID, PoolID string
	ServerID, Method, Name  string
	Outcome, Reason         string
}

// McpUsageQuery is a usage report request.
type McpUsageQuery struct {
	TeamID     string
	GroupBy    string
	View       string
	From, To   time.Time
	WindowDays int
}

// Usage report groupings and views.
var (
	McpUsageGroupings = []string{"server", "primitive", "team", "agent", "day"}
	McpUsageViews     = []string{"usage", "unused", "refusals", "discovery", "adoption"}
)

// Validate checks the grouping, the view and the window; team reports have no team grouping and no adoption view.
func (q McpUsageQuery) Validate() error {
	if !isEnum(q.GroupBy, McpUsageGroupings...) || (q.TeamID != "" && q.GroupBy == "team") {
		return Validation(FieldError{Field: "groupBy", Reason: "must be server, primitive, team, agent or day"})
	}
	if !isEnum(q.View, McpUsageViews...) || (q.TeamID != "" && q.View == "adoption") {
		return Validation(FieldError{Field: "view", Reason: "must be usage, unused, refusals, discovery or adoption"})
	}
	if q.View == "unused" && q.WindowDays != 30 && q.WindowDays != 90 {
		return Validation(FieldError{Field: "window", Reason: "must be 30 or 90"})
	}
	if !q.To.After(q.From) {
		return Validation(FieldError{Field: "to", Reason: "must be after from"})
	}
	return nil
}

// McpUsageRow is one report row.
type McpUsageRow struct {
	Key           string
	Label         string
	Calls         int64
	Errors        int64
	Refusals      int64
	P50Ms         *int
	P95Ms         *int
	Teams         *int
	Agents        *int
	Searches      *int64
	SearchHitRate *float64
	LastUsedAt    *time.Time
}

// McpQueryCapture is a query capture window (afs.mcp_query_captures).
type McpQueryCapture struct {
	ID        string
	StartedBy string
	StartedAt time.Time
	Until     time.Time
	Reason    string
	StoppedAt *time.Time
	StoppedBy string
}

// FederatedIdentityRecord is a stored agent or pool federated identity.
type FederatedIdentityRecord struct {
	FederatedIdentity
	ID        string
	TeamID    string
	SubjectID string
	Version   Version
	CreatedAt time.Time
	CreatedBy string
}

// McpServiceAccountSubject is the subject of a federated identity token: the agent's or pool's service account.
func McpServiceAccountSubject(namespace, serviceAccount string) string {
	return "system:serviceaccount:" + namespace + ":" + serviceAccount
}

// Refused returns an AccessDenied error with a contract reason (team_servers_not_allowed, not_team_server,
// approval_not_permitted_by_policy).
func Refused(perm, reason, message string) *Error {
	e := AccessDenied(perm)
	e.Reason = reason
	if message != "" {
		e.Message = message
	}
	return e
}

// McpRuleKey identifies a rule within one scope: its selector.
func McpRuleKey(r McpPolicyRule) string { return r.Name + "\x00" + string(r.Class) }

// McpRuleDiff counts the rules added and removed between two rule sets (a changed effect or rate counts as both).
func McpRuleDiff(before, after []McpPolicyRule) (added, removed int) {
	enc := func(r McpPolicyRule) string {
		rl := ""
		if r.RateLimitPerMinute != nil {
			rl = strconv.Itoa(*r.RateLimitPerMinute)
		}
		return McpRuleKey(r) + "\x00" + r.Effect + "\x00" + rl
	}
	b := map[string]bool{}
	for _, r := range before {
		b[enc(r)] = true
	}
	a := map[string]bool{}
	for _, r := range after {
		a[enc(r)] = true
		if !b[enc(r)] {
			added++
		}
	}
	for k := range b {
		if !a[k] {
			removed++
		}
	}
	return added, removed
}
