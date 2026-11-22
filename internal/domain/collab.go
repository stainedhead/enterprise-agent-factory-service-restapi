package domain

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Paperclip team collaboration (API contract §4.4b, change PRD paperclip-collaboration-PRD.md).

// AgentRole is the role of an agent: a worker, or the team's one collaboration agent (Paperclip).
type AgentRole string

// AgentRole values.
const (
	AgentRoleWorker        AgentRole = "worker"
	AgentRoleCollaboration AgentRole = "collaboration"
)

// Valid reports whether v is a known AgentRole.
func (v AgentRole) Valid() bool { return v == AgentRoleWorker || v == AgentRoleCollaboration }

// RegistrationMode says who registers connected agents in Paperclip.
type RegistrationMode string

// RegistrationMode values.
const (
	RegistrationManual  RegistrationMode = "manual"
	RegistrationManaged RegistrationMode = "managed"
)

// Valid reports whether v is a known RegistrationMode.
func (v RegistrationMode) Valid() bool { return v == RegistrationManual || v == RegistrationManaged }

// LinkState is the observed state of a worker agent's link to Paperclip.
type LinkState string

// LinkState values.
const (
	LinkPending  LinkState = "pending"
	LinkLinked   LinkState = "linked"
	LinkFailed   LinkState = "failed"
	LinkDisabled LinkState = "disabled"
)

// Valid reports whether v is a known LinkState.
func (v LinkState) Valid() bool {
	switch v {
	case LinkPending, LinkLinked, LinkFailed, LinkDisabled:
		return true
	}
	return false
}

// SecretManagedBy says who creates and writes a secret row (secrets.managed_by).
type SecretManagedBy string

// SecretManagedBy values.
const (
	ManagedByUser      SecretManagedBy = "user"
	ManagedByAFS       SecretManagedBy = "afs"
	ManagedByPaperclip SecretManagedBy = "paperclip"
)

// Valid reports whether v is a known SecretManagedBy.
func (v SecretManagedBy) Valid() bool {
	return v == ManagedByUser || v == ManagedByAFS || v == ManagedByPaperclip
}

// CapabilityPaperclip is the agent template capability that marks a Paperclip template.
const CapabilityPaperclip = "paperclip"

// Names of the collaboration agent's AFS-managed secrets (COL-7) and of the worker key secret.
const (
	SecretPaperclipBetterAuth    = "paperclip-better-auth-secret"
	SecretPaperclipToolSigning   = "paperclip-tool-action-signing-secret"
	SecretPaperclipMasterKey     = "paperclip-secrets-master-key"
	SecretPaperclipBoardAPIKey   = "paperclip-board-api-key"
	SecretPaperclipAgentAPIKey   = "paperclip-agent-api-key"
	EnvBetterAuthSecret          = "BETTER_AUTH_SECRET" //nolint:gosec // an environment variable name, not a value
	EnvToolActionSigningSecret   = "PAPERCLIP_TOOL_ACTION_SIGNING_SECRET"
	EnvPaperclipSecretsMasterKey = "PAPERCLIP_SECRETS_MASTER_KEY"
)

// BootSecret is one of the three boot secrets of the collaboration agent and its environment name.
type BootSecret struct {
	Name string
	Env  string
}

// BootSecrets returns the three boot secrets in creation order.
func BootSecrets() []BootSecret {
	return []BootSecret{
		{SecretPaperclipBetterAuth, EnvBetterAuthSecret},
		{SecretPaperclipToolSigning, EnvToolActionSigningSecret},
		{SecretPaperclipMasterKey, EnvPaperclipSecretsMasterKey},
	}
}

// RotatableCollabKeys are the boot secrets RotateCollaborationKeys may rotate by default.
func RotatableCollabKeys() []string {
	return []string{SecretPaperclipBetterAuth, SecretPaperclipToolSigning}
}

// IsReservedSecretName reports whether a team may not create a secret of this name: the name
// paperclip and every name starting paperclip- (the worker key secret is the one exception, and only
// on a worker agent: the database decides that part).
func IsReservedSecretName(name string) bool {
	return name == "paperclip" || strings.HasPrefix(name, "paperclip-")
}

// CollaborationLink is a worker agent's connection to the team's Paperclip.
type CollaborationLink struct {
	AgentID          string
	AgentName        string
	Connect          bool
	PaperclipAgentID string
	LinkState        LinkState
	LastSyncedAt     *time.Time
	LastError        string
	Version          Version
}

// TeamCollaboration is the team's collaboration resource: the stored choices of a team that has a
// collaboration agent. Effective and the hostname are computed (ComputeEffective, Hostname).
type TeamCollaboration struct {
	TeamID           string
	AgentID          string
	WebUIEnabled     bool
	RESTAPIEnabled   bool
	Mode             RegistrationMode
	Version          Version
	Generation       Generation
	Status           Status
	Unmanaged        []string
	CreatedAt        time.Time
	CreatedBy        string
	UpdatedAt        time.Time
	AgentState       AgentState
	TeamState        TeamState
	TeamSlug         string
	MayExpose        bool
	ProviderClientID string
}

// CollabSettings are the service settings the effective computation reads.
type CollabSettings struct {
	BaseDomain      string // empty when unset
	PublicEndpoints bool
}

// Effective is what the edge honours for the team (API contract §4.4b).
type Effective struct {
	WebUI   bool
	RESTAPI bool
	Reasons []string
}

// Reasons why a surface is not effective.
const (
	ReasonTeamNotAllowed       = "team_not_allowed"
	ReasonServiceEndpointsOff  = "service_endpoints_off"
	ReasonBaseDomainUnset      = "base_domain_unset"
	ReasonProviderWithoutColab = "provider_without_collab_client"
)

// ComputeEffective applies the same rule as afs.collab_routes(): a surface is effective only when its
// stored choice, the team's guardrail, the service switch, a set base domain and (web UI only) a
// collaboration client on the team's provider all hold. reasons lists every failed shared condition.
func (c TeamCollaboration) ComputeEffective(s CollabSettings) Effective {
	var reasons []string
	add := func(cond bool, r string) {
		if !cond {
			reasons = append(reasons, r)
		}
	}
	add(c.MayExpose, ReasonTeamNotAllowed)
	add(s.PublicEndpoints, ReasonServiceEndpointsOff)
	add(s.BaseDomain != "", ReasonBaseDomainUnset)
	add(c.ProviderClientID != "", ReasonProviderWithoutColab)
	shared := c.MayExpose && s.PublicEndpoints && s.BaseDomain != ""
	if reasons == nil {
		reasons = []string{}
	}
	return Effective{WebUI: c.WebUIEnabled && shared && c.ProviderClientID != "", RESTAPI: c.RESTAPIEnabled && shared, Reasons: reasons}
}

// Hostname returns <slug>.<base domain>, or "" while the base domain is unset.
func (c TeamCollaboration) Hostname(s CollabSettings) string {
	if s.BaseDomain == "" {
		return ""
	}
	return c.TeamSlug + "." + s.BaseDomain
}

// PutCollaborationParams is a PutTeamCollaboration body.
type PutCollaborationParams struct {
	WebUIEnabled   bool
	RESTAPIEnabled bool
	Mode           RegistrationMode // empty keeps the current value
}

// ValidatePut checks the new choices against the guardrails and settings (API contract §4.4b). Only
// switching a surface on is checked; switching off is always accepted. agentReady is whether the
// collaboration agent is Ready (needed for managed registration).
func (c TeamCollaboration) ValidatePut(p PutCollaborationParams, s CollabSettings, agentReady bool) error {
	var fe []FieldError
	surface := func(on, was bool, field string, needsClient bool) {
		if !on {
			return
		}
		switch {
		case s.BaseDomain == "":
			fe = append(fe, FieldError{Field: field, Reason: "collab_base_domain_unset"})
		case !s.PublicEndpoints:
			fe = append(fe, FieldError{Field: field, Reason: "collab_public_endpoints_off"})
		case !c.MayExpose:
			fe = append(fe, FieldError{Field: field, Reason: "collaboration_not_allowed_for_team"})
		case needsClient && c.ProviderClientID == "":
			fe = append(fe, FieldError{Field: field, Reason: "provider_without_collab_client"})
		}
		_ = was
	}
	surface(p.WebUIEnabled, c.WebUIEnabled, "webUiEnabled", true)
	surface(p.RESTAPIEnabled, c.RESTAPIEnabled, "restApiEnabled", false)
	if p.Mode != "" && !p.Mode.Valid() {
		fe = append(fe, FieldError{Field: "registrationMode", Reason: "must be manual or managed"})
	}
	if p.Mode == RegistrationManaged && !agentReady {
		fe = append(fe, FieldError{Field: "registrationMode", Reason: "collaboration_agent_not_ready"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

var paperclipAgentIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidatePaperclipAgentID checks an optional Paperclip agent id.
func ValidatePaperclipAgentID(v string) error {
	if v != "" && !paperclipAgentIDRE.MatchString(v) {
		return Validation(FieldError{Field: "paperclipAgentId", Reason: "must match ^[A-Za-z0-9._:-]{1,128}$"})
	}
	return nil
}

// CollabReason values for details.reason of errors.
const (
	ReasonCollaborationAgentMissing = "collaboration_agent_missing"
	ReasonCollaborationAgentExists  = "collaboration_agent_exists"
	ReasonManagedSecret             = "managed_secret"
	ReasonReservedName              = "reserved_name"
	ReasonHostnameHeld              = "hostname_held"
	ReasonMasterKeyNotRotatable     = "master_key_not_rotatable"
	ReasonManagedRegistration       = "managed_registration"
)

// CollaborationAgentMissing is the error for a team without a collaboration agent: 404 for reads and
// 409 for mutations, both with details.reason collaboration_agent_missing (API contract §4.4b).
func CollaborationAgentMissing(mutation bool) *Error {
	const msg = "the team has no collaboration agent"
	if mutation {
		return Conflict(ReasonCollaborationAgentMissing, msg, nil)
	}
	return &Error{Kind: KindNotFound, Message: msg, Reason: ReasonCollaborationAgentMissing}
}

// CollaborationEndpoint is one row of ListCollaborationEndpoints (service admins).
type CollaborationEndpoint struct {
	TeamID       string
	Slug         string
	TeamState    TeamState
	Hostname     string
	AgentID      string
	WebUIEnabled bool
	RESTEnabled  bool
	Effective    Effective
	MayExpose    bool
	Phase        Phase
	LastAccessAt *time.Time
}

// AgentCollaboration is a worker agent's connection to the team's Paperclip as an agent reads it.
type AgentCollaboration struct {
	Connect          bool
	PaperclipAgentID string
	LinkState        LinkState
	LastSyncedAt     *time.Time
	LastError        string
}

// CollaborationInput is the optional collaboration object of CreateAgent and UpdateAgent bodies. A nil
// Connect leaves the connection unchanged on update and means false on create.
type CollaborationInput struct {
	Connect          *bool
	PaperclipAgentID string
}

// Validate checks the input's shape.
func (c CollaborationInput) Validate() error {
	if err := ValidatePaperclipAgentID(c.PaperclipAgentID); err != nil {
		return err
	}
	if c.PaperclipAgentID != "" && c.Connect != nil && !*c.Connect {
		return Validation(FieldError{Field: "collaboration.paperclipAgentId", Reason: "needs connect to be true"})
	}
	return nil
}

// Wants reports whether the input asks for a connection and an id.
func (c CollaborationInput) Wants() bool { return c.Connect != nil && *c.Connect }

// ManagedSecret is the Conflict for changing a secret AFS or Paperclip manages (API contract 4.6).
func ManagedSecret(by SecretManagedBy) *Error {
	return Conflict(ReasonManagedSecret, "this secret is managed by "+string(by)+" and cannot be changed through the API", nil)
}

// ReservedSecretName is the ValidationError for a name a team may not create.
func ReservedSecretName(name string) *Error {
	return Validation(FieldError{Field: "name", Reason: ReasonReservedName + ": " + name + " is reserved"})
}

// CollaborationAgentExists is the Conflict for a second live collaboration agent.
func CollaborationAgentExists() *Error {
	return Conflict(ReasonCollaborationAgentExists, "the team already has a collaboration agent", nil)
}

// GenerateSecretValue returns n random bytes from r as lower-case hex (a generated key, never typed).
func GenerateSecretValue(r io.Reader, n int) (SecretValue, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return SecretValue{}, err
	}
	return SecretValue{v: hex.EncodeToString(b)}, nil
}

// ValidateCollabSetting checks what a setting's pattern cannot: every entry of collab.allowed_source_cidrs
// must parse as an IPv4 or IPv6 prefix (the pattern admits strings such as 999.1.1.1/99). Other keys pass.
func ValidateCollabSetting(key string, raw json.RawMessage) error {
	if key != SettingCollabAllowedSourceCidrs {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return Validation(FieldError{Field: "value", Reason: key + " must be an array of CIDRs"})
	}
	for _, c := range list {
		if _, err := netip.ParsePrefix(c); err != nil {
			return Validation(FieldError{Field: "value", Reason: c + " is not a valid CIDR"})
		}
	}
	return nil
}
