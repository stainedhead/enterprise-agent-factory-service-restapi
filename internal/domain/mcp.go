package domain

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

// Governed MCP servers at the API (API contract v1.6.0 §4.14, database contract §7.5, MCP gateway PRD §7 to §9
// and Appendix C): request validation with the contract's reason codes, review transitions, federated identities
// and the events of each mutation. Policy evaluation, risk tiers, approval levels, digests and search live in
// package domain/mcp, which this file reuses. The database holds the authoritative CHECKs and guards (-db 000024).

// Error reasons of API contract v1.6.0 (§6 additions). slug_invalid, slug_reserved and slug_taken are shared
// with model endpoints (ReasonSlugInvalid, ReasonSlugReserved, ReasonSlugTaken).
const (
	ReasonURLNotHTTPS                  = "url_not_https"
	ReasonHostDenied                   = "host_denied"
	ReasonImageNotPinned               = "image_not_pinned"
	ReasonImageUnsigned                = "image_unsigned"
	ReasonRegistryNotAllowed           = "registry_not_allowed"
	ReasonAuthKindUnsupported          = "auth_kind_unsupported"
	ReasonServiceAccountRequired       = "service_account_required"
	ReasonAudienceNotAllowed           = "audience_not_allowed"
	ReasonAudienceReserved             = "audience_reserved"
	ReasonCaptureWindowTooLong         = "capture_window_too_long"
	ReasonMcpServerNotEnabledForTeam   = "mcp_server_not_enabled_for_team"
	ReasonInvalidPolicyRule            = "invalid_policy_rule"
	ReasonTeamServersNotAllowed        = "team_servers_not_allowed"
	ReasonApprovalNotPermittedByPolicy = "approval_not_permitted_by_policy"
	ReasonNotTeamServer                = "not_team_server"
	ReasonServerInUse                  = "server_in_use"
	ReasonServerNotPublished           = "server_not_published"
	ReasonServerNotApproved            = "server_not_approved"
	ReasonTeamGatewayRequired          = "team_gateway_required"
	ReasonInvalidReviewTransition      = "invalid_review_transition"
	ReasonFederatedIdentityLimit       = "federated_identity_limit"
	ReasonGovernedByMcpServer          = "governed_by_mcp_server"
	ReasonCaptureActive                = "capture_active"
)

// Admission refusal reasons (PRD §7.2), in the order afs.mcp_admission decides them.
const (
	ReasonMcpServerSuspended          = "server_suspended"
	ReasonMcpServerNotEnabledForAgent = "server_not_enabled_for_agent"
	ReasonPrimitiveBlockedService     = "primitive_blocked_service"
	ReasonPrimitiveBlockedTeam        = "primitive_blocked_team"
	ReasonPrimitiveBlockedAgent       = "primitive_blocked_agent"
	ReasonPrimitivePendingReview      = "primitive_pending_review"
)

// Limits of database contract §7.5.
const (
	MaxMcpPrimitivesPerServer  = 2000
	MaxMcpPolicyRules          = 500
	MaxFederatedIdentities     = 10
	MaxMcpReasonLength         = 500
	MaxMcpEnvNames             = 64
	MaxMcpHostedArgs           = 64
	MaxMcpEgressHosts          = 50
	DefaultFederatedExpiration = 3600
)

// McpTransport is how the gateway reaches a server.
type McpTransport string

// Transports.
const (
	McpTransportStreamableHTTP McpTransport = "streamable_http"
	McpTransportHosted         McpTransport = "hosted"
)

// McpAuthKind is how the gateway authenticates upstream (PRD §9).
type McpAuthKind string

// Auth kinds.
const (
	McpAuthNone               McpAuthKind = "none"
	McpAuthStatic             McpAuthKind = "static"
	McpAuthOAuthClient        McpAuthKind = "oauth_client"
	McpAuthWorkloadFederation McpAuthKind = "workload_federation"
	McpAuthOAuthUser          McpAuthKind = "oauth_user"
	McpAuthAWSSigV4           McpAuthKind = "aws_sigv4"
)

// NeedsCredential reports whether the kind stores a credential (an ARN, never a value here).
func (k McpAuthKind) NeedsCredential() bool {
	return k == McpAuthStatic || k == McpAuthOAuthClient || k == McpAuthOAuthUser
}

// McpReviewStatus is a server's review status.
type McpReviewStatus string

// Review statuses.
const (
	McpDraft         McpReviewStatus = "draft"
	McpPendingReview McpReviewStatus = "pending_review"
	McpApproved      McpReviewStatus = "approved"
	McpRejected      McpReviewStatus = "rejected"
	McpSuspended     McpReviewStatus = "suspended"
)

// DataClassifications in ascending order.
var DataClassifications = []string{"public", "internal", "confidential", "restricted"}

// McpTeamMode is a team switch's mode.
type McpTeamMode string

// Team switch modes.
const (
	McpTeamModeFull     McpTeamMode = "full"
	McpTeamModeReadOnly McpTeamMode = "read_only"
)

// McpAgentMode is how an agent connects (PRD §6.1).
type McpAgentMode string

// Agent modes.
const (
	McpAgentModeSearch McpAgentMode = "search"
	McpAgentModeFull   McpAgentMode = "full"
)

var (
	mcpImageRE     = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
	mcpEnvNameRE   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	mcpFedNameRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	mcpHeaderRE    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
	mcpURLHostPort = regexp.MustCompile(`^[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
)

// ValidMcpSlug checks a server slug: the route name /mcp/<slug> and the prefix of its tool names.
func ValidMcpSlug(slug string) *FieldError {
	if err := mcp.ValidateSlug(slug); err != nil {
		return &FieldError{Field: "slug", Reason: err.Error()}
	}
	return nil
}

// McpFederation is the workload federation of a server (PRD §9): who issues the token and where to exchange it.
type McpFederation struct {
	Issuer          string `json:"issuer"`
	Audience        string `json:"audience"`
	SubjectTemplate string `json:"subjectTemplate,omitempty"`
	TokenEndpoint   string `json:"tokenEndpoint"`
}

// McpHosted is the spec of a hosted server (PRD §10.2).
type McpHosted struct {
	Image        string   `json:"imageDigest"`
	Args         []string `json:"args"`
	EnvNames     []string `json:"envNames"`
	InstanceMode string   `json:"instanceMode"`
	EgressHosts  []string `json:"egressHosts"`
	// Resources are the pod's cpu and memory requests, for example {"cpu": "250m", "memory": "256Mi"}.
	Resources map[string]string `json:"resources,omitempty"`
	NeedsAWS  bool              `json:"needsAws"`
}

// McpServer is an MCP server of the catalog: enterprise (TeamID empty) or team-added.
type McpServer struct {
	ID                 string
	TeamID             string
	Slug               string
	DisplayName        string
	Owner              string
	Purpose            string
	DocURL             string
	DataClassification string
	Transport          McpTransport
	Location           EndpointLocation
	URL                string
	AuthKind           McpAuthKind
	AuthHeader         string
	Federation         *McpFederation
	Hosted             *McpHosted
	ReviewStatus       McpReviewStatus
	Published          bool
	DefaultForNewTeams bool
}

// IsTeam reports whether a team added the server.
func (s McpServer) IsTeam() bool { return s.TeamID != "" }

// Host is the URL's host, lower case, or "" for a hosted server.
func (s McpServer) Host() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Validate checks the server's own rules (database contract §7.5.1). Slug uniqueness, the host deny list and the
// registry list are checked against stored state and settings by the caller.
func (s McpServer) Validate() error {
	var fe []FieldError
	if f := ValidMcpSlug(s.Slug); f != nil {
		fe = append(fe, *f)
	}
	if strings.TrimSpace(s.DisplayName) == "" || len(s.DisplayName) > 100 {
		fe = append(fe, FieldError{Field: "displayName", Reason: "required, at most 100 characters"})
	}
	if len(s.Owner) > 200 || (s.IsTeam() && strings.TrimSpace(s.Owner) == "") {
		fe = append(fe, FieldError{Field: "owner", Reason: "required on a team server, at most 200 characters"})
	}
	if len(s.Purpose) > 1000 || (s.IsTeam() && strings.TrimSpace(s.Purpose) == "") {
		fe = append(fe, FieldError{Field: "purpose", Reason: "required on a team server, at most 1000 characters"})
	}
	if s.DocURL != "" && (!strings.HasPrefix(s.DocURL, "https://") || len(s.DocURL) > 500) {
		fe = append(fe, FieldError{Field: "docUrl", Reason: ReasonURLNotHTTPS})
	}
	if !isEnum(s.DataClassification, DataClassifications...) {
		fe = append(fe, FieldError{Field: "dataClassification", Reason: "must be public, internal, confidential or restricted"})
	}
	fe = append(fe, s.validateTransport()...)
	fe = append(fe, s.validateAuth()...)
	if s.IsTeam() && (s.Published || s.DefaultForNewTeams) {
		fe = append(fe, FieldError{Field: "published", Reason: "only enterprise servers are published"})
	}
	if s.DefaultForNewTeams && !s.Published {
		fe = append(fe, FieldError{Field: "defaultForNewTeams", Reason: ReasonServerNotPublished})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

func (s McpServer) validateTransport() []FieldError {
	switch s.Transport {
	case McpTransportHosted:
		var fe []FieldError
		if s.Location != LocationInNamespace {
			fe = append(fe, FieldError{Field: "location", Reason: "a hosted server is in_namespace"})
		}
		if s.URL != "" {
			fe = append(fe, FieldError{Field: "url", Reason: "a hosted server has no url"})
		}
		if s.Hosted == nil {
			return append(fe, FieldError{Field: "hosted", Reason: "required for a hosted server"})
		}
		return append(fe, s.Hosted.validate()...)
	case McpTransportStreamableHTTP:
		var fe []FieldError
		if s.Location != LocationExternal && s.Location != LocationCorporate {
			fe = append(fe, FieldError{Field: "location", Reason: "a remote server is external or corporate"})
		}
		if s.Hosted != nil {
			fe = append(fe, FieldError{Field: "hosted", Reason: "only for a hosted server"})
		}
		if !ValidMcpURL(s.URL) {
			fe = append(fe, FieldError{Field: "url", Reason: ReasonURLNotHTTPS})
		}
		return fe
	}
	return []FieldError{{Field: "transport", Reason: "must be streamable_http or hosted"}}
}

// ValidMcpURL reports whether u is an https URL with a host and no credentials, at most 500 characters.
func ValidMcpURL(u string) bool {
	if len(u) > 500 || !strings.HasPrefix(u, "https://") {
		return false
	}
	p, err := url.Parse(u)
	return err == nil && p.User == nil && p.Host != "" && mcpURLHostPort.MatchString(p.Host) && !strings.ContainsAny(u, " \t\n")
}

func (s McpServer) validateAuth() []FieldError {
	var fe []FieldError
	switch s.AuthKind {
	case McpAuthNone, McpAuthStatic, McpAuthOAuthClient, McpAuthOAuthUser:
	case McpAuthAWSSigV4:
		if s.Location == LocationInNamespace {
			fe = append(fe, FieldError{Field: "authKind", Reason: ReasonAuthKindUnsupported})
		}
	case McpAuthWorkloadFederation:
		f := s.Federation
		if f == nil || f.Issuer == "" || f.Audience == "" || f.TokenEndpoint == "" {
			fe = append(fe, FieldError{Field: "federation", Reason: "issuer, audience and tokenEndpoint are required"})
		} else if !ValidMcpURL(f.TokenEndpoint) {
			fe = append(fe, FieldError{Field: "federation.tokenEndpoint", Reason: ReasonURLNotHTTPS})
		}
	default:
		fe = append(fe, FieldError{Field: "authKind", Reason: ReasonAuthKindUnsupported})
	}
	if s.AuthKind != McpAuthWorkloadFederation && s.Federation != nil {
		fe = append(fe, FieldError{Field: "federation", Reason: "only for workload_federation"})
	}
	if s.AuthHeader != "" && !mcpHeaderRE.MatchString(s.AuthHeader) {
		fe = append(fe, FieldError{Field: "authHeader", Reason: "must be an HTTP header name"})
	}
	return fe
}

func (h McpHosted) validate() []FieldError {
	var fe []FieldError
	if !mcpImageRE.MatchString(h.Image) {
		fe = append(fe, FieldError{Field: "hosted.imageDigest", Reason: ReasonImageNotPinned})
	}
	if len(h.Args) > MaxMcpHostedArgs {
		fe = append(fe, FieldError{Field: "hosted.args", Reason: "at most 64"})
	}
	if !ValidMcpEnvNames(h.EnvNames) {
		fe = append(fe, FieldError{Field: "hosted.envNames", Reason: "at most 64 upper-snake names"})
	}
	if !isEnum(h.InstanceMode, "per_team", "per_agent") {
		fe = append(fe, FieldError{Field: "hosted.instanceMode", Reason: "must be per_team or per_agent"})
	}
	if len(h.EgressHosts) > MaxMcpEgressHosts {
		fe = append(fe, FieldError{Field: "hosted.egressHosts", Reason: "at most 50"})
	}
	for _, g := range h.EgressHosts {
		if !ValidHostGlob(g) {
			fe = append(fe, FieldError{Field: "hosted.egressHosts", Reason: "must be host or *.domain globs"})
			break
		}
	}
	return fe
}

// ValidMcpEnvNames checks hosted environment variable names (values are secret references, never here).
func ValidMcpEnvNames(names []string) bool {
	if len(names) > MaxMcpEnvNames {
		return false
	}
	for _, n := range names {
		if !mcpEnvNameRE.MatchString(n) {
			return false
		}
	}
	return true
}

// ImageRegistry returns the registry host of a pinned image reference ("" for a Docker Hub short name).
func ImageRegistry(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	first, _, ok := strings.Cut(ref, "/")
	if !ok || !strings.ContainsAny(first, ".:") && first != "localhost" {
		return ""
	}
	return strings.ToLower(first)
}

// RegistryAllowed reports whether the image's registry is in mcp.allowed_image_registries.
func RegistryAllowed(image string, allowed []string) bool {
	reg := ImageRegistry(image)
	for _, a := range allowed {
		if reg != "" && strings.EqualFold(a, reg) {
			return true
		}
	}
	return false
}

// ReviewTransitionAllowed mirrors the database guard (mcp_review_transition): draft → pending_review;
// pending_review → approved, rejected or draft; rejected → draft; approved → pending_review (a change of URL,
// image, location or auth); any status → suspended; suspended → the status it was suspended from.
func ReviewTransitionAllowed(from, to, suspendedFrom McpReviewStatus) bool {
	switch {
	case from == to:
		return true
	case to == McpSuspended:
		return true
	case from == McpSuspended:
		return to == suspendedFrom
	}
	switch from {
	case McpDraft:
		return to == McpPendingReview
	case McpPendingReview:
		return to == McpApproved || to == McpRejected || to == McpDraft
	case McpRejected:
		return to == McpDraft
	case McpApproved:
		return to == McpPendingReview
	}
	return false
}

// ReviewChangingUpdate reports whether an update of an approved server returns it to pending_review
// (contract §4.14 UpdateMcpServer: a change of URL, image, location or auth).
func ReviewChangingUpdate(before, after McpServer) bool {
	img := func(h *McpHosted) string {
		if h == nil {
			return ""
		}
		return h.Image
	}
	fed := func(f *McpFederation) McpFederation {
		if f == nil {
			return McpFederation{}
		}
		return *f
	}
	return before.URL != after.URL || before.Location != after.Location || before.AuthKind != after.AuthKind ||
		before.Transport != after.Transport || img(before.Hosted) != img(after.Hosted) || fed(before.Federation) != fed(after.Federation)
}

// TeamAdminMayApprove is the mcpApprovalPolicy condition (CD-45) for a team-admin, decided in the use case from
// mcp.team_server_approval and the server's tier. An unknown policy or tier fails closed to security.
func TeamAdminMayApprove(policy, tier string) bool {
	return mcp.RequiredApprover(mcp.ApprovalPolicy(policy), mcp.Tier(tier)) == mcp.ApproverTeamAdmin
}

// McpPolicyRule is one allow or block rule: exactly one of Name (exact or glob) and Class.
type McpPolicyRule struct {
	Name               string    `json:"name,omitempty"`
	Class              mcp.Class `json:"class,omitempty"`
	Effect             string    `json:"effect"`
	RateLimitPerMinute *int      `json:"rateLimitPerMinute,omitempty"`
}

// ValidateMcpPolicyRules checks a policy body (PutMcpPolicy, PutTeamMcpPolicy, agent blocks).
func ValidateMcpPolicyRules(rules []McpPolicyRule, field string) error {
	if len(rules) > MaxMcpPolicyRules {
		return Validation(FieldError{Field: field, Reason: ReasonInvalidPolicyRule + ": at most 500 rules"})
	}
	seen := map[string]bool{}
	for _, r := range rules {
		if bad := r.problem(); bad != "" {
			return Validation(FieldError{Field: field, Reason: ReasonInvalidPolicyRule + ": " + bad})
		}
		key := r.Name + "\x00" + string(r.Class)
		if seen[key] {
			return Validation(FieldError{Field: field, Reason: ReasonInvalidPolicyRule + ": duplicate selector " + r.Name + string(r.Class)})
		}
		seen[key] = true
	}
	return nil
}

func (r McpPolicyRule) problem() string {
	switch {
	case (r.Name == "") == (r.Class == ""):
		return "exactly one of name and class"
	case len(r.Name) > 500:
		return "name is at most 500 characters"
	case r.Class != "" && !isEnum(string(r.Class), string(mcp.ClassReadOnly), string(mcp.ClassDestructive), string(mcp.ClassOpenWorld)):
		return "class must be read_only, destructive or open_world"
	case r.Effect != "allow" && r.Effect != "block":
		return "effect must be allow or block"
	case r.RateLimitPerMinute != nil && *r.RateLimitPerMinute <= 0:
		return "rateLimitPerMinute must be positive"
	}
	return ""
}

// Rule converts the request rule to the policy package's rule.
func (r McpPolicyRule) Rule() mcp.Rule {
	rl := 0
	if r.RateLimitPerMinute != nil {
		rl = *r.RateLimitPerMinute
	}
	return mcp.Rule{Name: r.Name, Class: r.Class, Effect: mcp.Effect(r.Effect), RateLimit: rl}
}

// AgentMcp is the mcp body of CreateAgent, UpdateAgent, CreatePool and UpdatePool.
type AgentMcp struct {
	Access    string          `json:"access"`
	ServerIDs []string        `json:"serverIds"`
	Mode      McpAgentMode    `json:"mode"`
	Blocks    []McpAgentBlock `json:"blocks"`
}

// McpAgentBlock is an agent-level block rule.
type McpAgentBlock struct {
	ServerID string    `json:"serverId"`
	Name     string    `json:"name,omitempty"`
	Class    mcp.Class `json:"class,omitempty"`
	Effect   string    `json:"effect"`
}

// Validate checks the body against the team's effective server set (ids the team has on).
func (a AgentMcp) Validate(teamEnabled []string) error {
	var fe []FieldError
	if !isEnum(a.Access, "all", "selected") {
		fe = append(fe, FieldError{Field: "mcp.access", Reason: "must be all or selected"})
	}
	if a.Access == "all" && len(a.ServerIDs) > 0 {
		fe = append(fe, FieldError{Field: "mcp.serverIds", Reason: "only with access selected"})
	}
	if a.Mode != "" && a.Mode != McpAgentModeSearch && a.Mode != McpAgentModeFull {
		fe = append(fe, FieldError{Field: "mcp.mode", Reason: "must be search or full"})
	}
	for _, id := range a.ServerIDs {
		if !isEnum(id, teamEnabled...) {
			fe = append(fe, FieldError{Field: "mcp.serverIds", Reason: ReasonMcpServerNotEnabledForTeam})
			break
		}
	}
	if len(a.Blocks) > MaxMcpPolicyRules {
		fe = append(fe, FieldError{Field: "mcp.blocks", Reason: ReasonInvalidPolicyRule + ": at most 500 rules"})
	}
	for _, b := range a.Blocks {
		r := McpPolicyRule{Name: b.Name, Class: b.Class, Effect: b.Effect}
		bad := r.problem()
		switch {
		case b.Effect != "block":
			bad = "agent rules only block"
		case b.ServerID == "":
			bad = "serverId is required"
		}
		if bad != "" {
			fe = append(fe, FieldError{Field: "mcp.blocks", Reason: ReasonInvalidPolicyRule + ": " + bad})
			break
		}
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// McpEnv returns the AFS_MCP_* variable names an agent gets (PRD §6.1).
func McpEnv(mode McpAgentMode) []string {
	if mode == McpAgentModeFull {
		return []string{"AFS_MCP_URL", "AFS_MCP_CONFIG", "AFS_MCP_SERVERS"}
	}
	return []string{"AFS_MCP_URL", "AFS_MCP_CONFIG"}
}

// FederatedIdentity is an agent's or pool's projected-token identity (PRD Appendix C).
type FederatedIdentity struct {
	Name              string `json:"name"`
	Audience          string `json:"audience"`
	ExpirationSeconds int    `json:"expirationSeconds"`
	FilePath          string `json:"filePath"`
	EnvVar            string `json:"envVar"`
}

// ReservedAudiences may never be requested: AWS access goes through Pod Identity, and afs-gateway is the
// gateway's own audience.
var ReservedAudiences = []string{"sts.amazonaws.com", "afs-gateway"}

// WithDefaults fills the file path and variable name.
func (f FederatedIdentity) WithDefaults() FederatedIdentity {
	if f.ExpirationSeconds == 0 {
		f.ExpirationSeconds = DefaultFederatedExpiration
	}
	if f.FilePath == "" {
		f.FilePath = "/var/run/secrets/afs/federation/" + f.Name + "/token"
	}
	if f.EnvVar == "" {
		f.EnvVar = "AFS_FEDERATED_TOKEN_FILE_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
	}
	return f
}

var fedFileRE = regexp.MustCompile(`^/var/run/secrets/afs/federation/[a-z0-9-]+/[A-Za-z0-9._-]+$`)

// Validate checks the identity against federation.allowed_audiences (exact or glob) and the subject's count.
func (f FederatedIdentity) Validate(allowedAudiences []string, existing int) error {
	f = f.WithDefaults()
	if !mcpFedNameRE.MatchString(f.Name) {
		return Validation(FieldError{Field: "name", Reason: "must match ^[a-z][a-z0-9-]{0,30}$"})
	}
	if f.Audience == "" || len(f.Audience) > 300 {
		return Validation(FieldError{Field: "audience", Reason: "required, at most 300 characters"})
	}
	for _, r := range ReservedAudiences {
		if strings.EqualFold(f.Audience, r) {
			return Validation(FieldError{Field: "audience", Reason: ReasonAudienceReserved})
		}
	}
	ok := false
	for _, g := range allowedAudiences {
		if g == f.Audience || (strings.Contains(g, "*") && mcp.Glob(g, f.Audience)) {
			ok = true
			break
		}
	}
	if !ok {
		return Validation(FieldError{Field: "audience", Reason: ReasonAudienceNotAllowed})
	}
	if f.ExpirationSeconds < 600 || f.ExpirationSeconds > 86400 {
		return Validation(FieldError{Field: "expirationSeconds", Reason: "must be 600 to 86400"})
	}
	if !fedFileRE.MatchString(f.FilePath) {
		return Validation(FieldError{Field: "filePath", Reason: "must be under /var/run/secrets/afs/federation/"})
	}
	if !mcpEnvNameRE.MatchString(f.EnvVar) {
		return Validation(FieldError{Field: "envVar", Reason: "must be upper-snake"})
	}
	if existing >= MaxFederatedIdentities {
		return Conflict(ReasonFederatedIdentityLimit, "at most 10 federated identities per agent or pool", nil)
	}
	return nil
}

// ValidateCaptureWindow checks StartMcpQueryCapture (days ≤ mcp.query_capture_max_days, a reason).
func ValidateCaptureWindow(days, maxDays int, reason string) error {
	if days < 1 || days > maxDays {
		return Validation(FieldError{Field: "days", Reason: ReasonCaptureWindowTooLong})
	}
	if strings.TrimSpace(reason) == "" || len(reason) > MaxMcpReasonLength {
		return Validation(FieldError{Field: "reason", Reason: "required, at most 500 characters"})
	}
	return nil
}

// ValidateMcpReason checks a suspend or review note: required, at most 500 characters.
func ValidateMcpReason(field, note string) error {
	if strings.TrimSpace(note) == "" || len(note) > MaxMcpReasonLength {
		return Validation(FieldError{Field: field, Reason: "required, at most 500 characters"})
	}
	return nil
}

// Events of API contract v1.6.0 §4.14, by the mutation that emits them.

// McpServerLifecycleEvent returns mcp_server.created, .updated, .deleted, .published or .retired.
func McpServerLifecycleEvent(op string) EventType {
	return map[string]EventType{
		"create": EventMcpServerCreated, "update": EventMcpServerUpdated, "delete": EventMcpServerDeleted,
		"publish": EventMcpServerPublished, "retire": EventMcpServerRetired,
	}[op]
}

// McpReviewEvent returns the event of a review step: submitted, approved, rejected or changes_requested.
func McpReviewEvent(to McpReviewStatus, changesRequested bool) EventType {
	switch {
	case changesRequested:
		return EventMcpServerChangesRequested
	case to == McpPendingReview:
		return EventMcpServerSubmitted
	case to == McpApproved:
		return EventMcpServerApproved
	}
	return EventMcpServerRejected
}

// McpSuspendEvent returns mcp_server.suspended or .resumed.
func McpSuspendEvent(suspended bool) EventType {
	if suspended {
		return EventMcpServerSuspended
	}
	return EventMcpServerResumed
}

// McpPrimitiveEvent returns primitive_changed (held), primitive_approved or primitive_rejected.
func McpPrimitiveEvent(status string) EventType {
	switch status {
	case "approved":
		return EventMcpServerPrimitiveApproved
	case "blocked":
		return EventMcpServerPrimitiveRejected
	}
	return EventMcpServerPrimitiveChanged
}

// McpCredentialEvent returns credential_set or credential_removed (never a value in the event).
func McpCredentialEvent(set bool) EventType {
	if set {
		return EventMcpServerCredentialSet
	}
	return EventMcpServerCredentialRemoved
}

// McpPolicyEvent is emitted by PutMcpPolicy and PutTeamMcpPolicy.
const McpPolicyEvent = EventMcpServerPolicyChanged

// McpAuthEvent returns auth_expired or auth_restored (gateway, oauth_user refresh).
func McpAuthEvent(expired bool) EventType {
	if expired {
		return EventMcpServerAuthExpired
	}
	return EventMcpServerAuthRestored
}

// McpHealthEvent returns degraded or recovered (reconciler, condition McpServerReady).
func McpHealthEvent(ready bool) EventType {
	if ready {
		return EventMcpServerRecovered
	}
	return EventMcpServerDegraded
}

// TeamMcpSwitchEvent returns team_mcp_server.enabled or .disabled.
func TeamMcpSwitchEvent(enabled bool) EventType {
	if enabled {
		return EventTeamMcpServerEnabled
	}
	return EventTeamMcpServerDisabled
}

// McpSettingsEvent is emitted when an mcp.* setting changes (CD-49).
const McpSettingsEvent = EventMcpSettingsChanged

// McpCaptureEvent returns mcp_query_capture.started or .stopped.
func McpCaptureEvent(started bool) EventType {
	if started {
		return EventMcpQueryCaptureStarted
	}
	return EventMcpQueryCaptureStopped
}

// FederatedIdentityEvent is emitted by the put and delete of an agent or pool federated identity.
const FederatedIdentityEvent = EventAgentFederatedIdentityChanged
