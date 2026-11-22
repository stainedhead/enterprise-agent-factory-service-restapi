package domain

// Closed enumerations of API contract §5. api/contract_test.go checks each against the OpenAPI enums.

// TeamKind is an API contract §5 enumeration.
type TeamKind string

// TeamKind values.
const (
	TeamKindPersistent TeamKind = "persistent"
	TeamKindEphemeral  TeamKind = "ephemeral"
)

// Valid reports whether v is a known TeamKind.
func (v TeamKind) Valid() bool { return validEnum(EnumValues["TeamKind"], string(v)) }

// TeamState is an API contract §5 enumeration.
type TeamState string

// TeamState values.
const (
	TeamStateActive    TeamState = "active"
	TeamStateSuspended TeamState = "suspended"
	TeamStateDeleting  TeamState = "deleting"
	TeamStateDeleted   TeamState = "deleted"
)

// Valid reports whether v is a known TeamState.
func (v TeamState) Valid() bool { return validEnum(EnumValues["TeamState"], string(v)) }

// IsolationTier is an API contract §5 enumeration.
type IsolationTier string

// IsolationTier values.
const (
	IsolationTierShared    IsolationTier = "shared"
	IsolationTierDedicated IsolationTier = "dedicated"
)

// Valid reports whether v is a known IsolationTier.
func (v IsolationTier) Valid() bool { return validEnum(EnumValues["IsolationTier"], string(v)) }

// TeardownRule is an API contract §5 enumeration.
type TeardownRule string

// TeardownRule values.
const (
	TeardownRuleAtExpiry       TeardownRule = "at_expiry"
	TeardownRuleAtCapOrExpiry  TeardownRule = "at_cap_or_expiry"
	TeardownRuleAtWorkComplete TeardownRule = "at_work_complete"
)

// Valid reports whether v is a known TeardownRule.
func (v TeardownRule) Valid() bool { return validEnum(EnumValues["TeardownRule"], string(v)) }

// TeardownReason is an API contract §5 enumeration.
type TeardownReason string

// TeardownReason values.
const (
	TeardownReasonExpired      TeardownReason = "expired"
	TeardownReasonCapReached   TeardownReason = "cap_reached"
	TeardownReasonWorkComplete TeardownReason = "work_complete"
	TeardownReasonManual       TeardownReason = "manual"
)

// Valid reports whether v is a known TeardownReason.
func (v TeardownReason) Valid() bool { return validEnum(EnumValues["TeardownReason"], string(v)) }

// TeamRole is an API contract §5 enumeration.
type TeamRole string

// TeamRole values.
const (
	TeamRoleTeamAdmin        TeamRole = "team_admin"
	TeamRoleTeamCollaborator TeamRole = "team_collaborator"
	TeamRoleTeamViewer       TeamRole = "team_viewer"
)

// Valid reports whether v is a known TeamRole.
func (v TeamRole) Valid() bool { return validEnum(EnumValues["TeamRole"], string(v)) }

// SubjectType is an API contract §5 enumeration.
type SubjectType string

// SubjectType values.
const (
	SubjectTypeUser  SubjectType = "user"
	SubjectTypeGroup SubjectType = "group"
)

// Valid reports whether v is a known SubjectType.
func (v SubjectType) Valid() bool { return validEnum(EnumValues["SubjectType"], string(v)) }

// PrincipalType is an API contract §5 enumeration.
type PrincipalType string

// PrincipalType values.
const (
	PrincipalTypeUser     PrincipalType = "user"
	PrincipalTypeApiKey   PrincipalType = "api_key"
	PrincipalTypeWorkload PrincipalType = "workload"
)

// Valid reports whether v is a known PrincipalType.
func (v PrincipalType) Valid() bool { return validEnum(EnumValues["PrincipalType"], string(v)) }

// SecretDelivery is an API contract §5 enumeration.
type SecretDelivery string

// SecretDelivery values.
const (
	SecretDeliveryEnv  SecretDelivery = "env"
	SecretDeliveryFile SecretDelivery = "file"
)

// Valid reports whether v is a known SecretDelivery.
func (v SecretDelivery) Valid() bool { return validEnum(EnumValues["SecretDelivery"], string(v)) }

// IAMIdentity is an API contract §5 enumeration.
type IAMIdentity string

// IAMIdentity values.
const (
	IAMIdentityTeamShared IAMIdentity = "team_shared"
	IAMIdentityDedicated  IAMIdentity = "dedicated"
)

// Valid reports whether v is a known IAMIdentity.
func (v IAMIdentity) Valid() bool { return validEnum(EnumValues["IAMIdentity"], string(v)) }

// VolumeOnDelete is an API contract §5 enumeration.
type VolumeOnDelete string

// VolumeOnDelete values.
const (
	VolumeOnDeleteSnapshotThenDelete VolumeOnDelete = "snapshot_then_delete"
	VolumeOnDeleteDelete             VolumeOnDelete = "delete"
	VolumeOnDeleteRetain             VolumeOnDelete = "retain"
)

// Valid reports whether v is a known VolumeOnDelete.
func (v VolumeOnDelete) Valid() bool { return validEnum(EnumValues["VolumeOnDelete"], string(v)) }

// SnapshotSchedule is an API contract §5 enumeration.
type SnapshotSchedule string

// SnapshotSchedule values.
const (
	SnapshotScheduleOff    SnapshotSchedule = "off"
	SnapshotScheduleDaily  SnapshotSchedule = "daily"
	SnapshotScheduleWeekly SnapshotSchedule = "weekly"
)

// Valid reports whether v is a known SnapshotSchedule.
func (v SnapshotSchedule) Valid() bool { return validEnum(EnumValues["SnapshotSchedule"], string(v)) }

// RoleLinkStatus is an API contract §5 enumeration.
type RoleLinkStatus string

// RoleLinkStatus values.
const (
	RoleLinkStatusPendingTrust RoleLinkStatus = "pending_trust"
	RoleLinkStatusVerified     RoleLinkStatus = "verified"
	RoleLinkStatusFailed       RoleLinkStatus = "failed"
	RoleLinkStatusDisabled     RoleLinkStatus = "disabled"
)

// Valid reports whether v is a known RoleLinkStatus.
func (v RoleLinkStatus) Valid() bool { return validEnum(EnumValues["RoleLinkStatus"], string(v)) }

// PoolState is an API contract §5 enumeration.
type PoolState string

// PoolState values.
const (
	PoolStateEnabled  PoolState = "enabled"
	PoolStatePaused   PoolState = "paused"
	PoolStateDeleting PoolState = "deleting"
	PoolStateDeleted  PoolState = "deleted"
)

// Valid reports whether v is a known PoolState.
func (v PoolState) Valid() bool { return validEnum(EnumValues["PoolState"], string(v)) }

// SharedVolume is an API contract §5 enumeration.
type SharedVolume string

// SharedVolume values.
const (
	SharedVolumeNone SharedVolume = "none"
	SharedVolumeEfs  SharedVolume = "efs"
)

// Valid reports whether v is a known SharedVolume.
func (v SharedVolume) Valid() bool { return validEnum(EnumValues["SharedVolume"], string(v)) }

// WorkOrderKind is an API contract §5 enumeration.
type WorkOrderKind string

// WorkOrderKind values.
const (
	WorkOrderKindCveRemediation WorkOrderKind = "cve_remediation"
	WorkOrderKindGeneral        WorkOrderKind = "general"
)

// Valid reports whether v is a known WorkOrderKind.
func (v WorkOrderKind) Valid() bool { return validEnum(EnumValues["WorkOrderKind"], string(v)) }

// Severity is an API contract §5 enumeration.
type Severity string

// Severity values.
const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// Valid reports whether v is a known Severity.
func (v Severity) Valid() bool { return validEnum(EnumValues["Severity"], string(v)) }

// WorkOrderStatus is an API contract §5 enumeration.
type WorkOrderStatus string

// WorkOrderStatus values.
const (
	WorkOrderStatusOpen       WorkOrderStatus = "open"
	WorkOrderStatusAssigned   WorkOrderStatus = "assigned"
	WorkOrderStatusInProgress WorkOrderStatus = "in_progress"
	WorkOrderStatusPrOpened   WorkOrderStatus = "pr_opened"
	WorkOrderStatusDone       WorkOrderStatus = "done"
	WorkOrderStatusFailed     WorkOrderStatus = "failed"
	WorkOrderStatusCancelled  WorkOrderStatus = "cancelled"
)

// Valid reports whether v is a known WorkOrderStatus.
func (v WorkOrderStatus) Valid() bool { return validEnum(EnumValues["WorkOrderStatus"], string(v)) }

// SLAState is an API contract §5 enumeration.
type SLAState string

// SLAState values.
const (
	SLAStateOnTrack  SLAState = "on_track"
	SLAStateDueSoon  SLAState = "due_soon"
	SLAStateBreached SLAState = "breached"
	SLAStateMet      SLAState = "met"
	SLAStateMissed   SLAState = "missed"
)

// Valid reports whether v is a known SLAState.
func (v SLAState) Valid() bool { return validEnum(EnumValues["SLAState"], string(v)) }

// WorkOrderSource is an API contract §5 enumeration.
type WorkOrderSource string

// WorkOrderSource values.
const (
	WorkOrderSourceApi         WorkOrderSource = "api"
	WorkOrderSourceEventbridge WorkOrderSource = "eventbridge"
)

// Valid reports whether v is a known WorkOrderSource.
func (v WorkOrderSource) Valid() bool { return validEnum(EnumValues["WorkOrderSource"], string(v)) }

// RepoStatus is an API contract §5 enumeration.
type RepoStatus string

// RepoStatus values.
const (
	RepoStatusPending     RepoStatus = "pending"
	RepoStatusInProgress  RepoStatus = "in_progress"
	RepoStatusPrOpened    RepoStatus = "pr_opened"
	RepoStatusMerged      RepoStatus = "merged"
	RepoStatusNotAffected RepoStatus = "not_affected"
	RepoStatusFailed      RepoStatus = "failed"
	RepoStatusSkipped     RepoStatus = "skipped"
)

// Valid reports whether v is a known RepoStatus.
func (v RepoStatus) Valid() bool { return validEnum(EnumValues["RepoStatus"], string(v)) }

// OwnerSource is an API contract §5 enumeration.
type OwnerSource string

// OwnerSource values.
const (
	OwnerSourceExplicit   OwnerSource = "explicit"
	OwnerSourceTeamConfig OwnerSource = "team_config"
)

// Valid reports whether v is a known OwnerSource.
func (v OwnerSource) Valid() bool { return validEnum(EnumValues["OwnerSource"], string(v)) }

// ReportKind is an API contract §5 enumeration.
type ReportKind string

// ReportKind values.
const (
	ReportKindUsage    ReportKind = "usage"
	ReportKindCosts    ReportKind = "costs"
	ReportKindActivity ReportKind = "activity"
	ReportKindTraffic  ReportKind = "traffic"
)

// Valid reports whether v is a known ReportKind.
func (v ReportKind) Valid() bool { return validEnum(EnumValues["ReportKind"], string(v)) }

// Cadence is an API contract §5 enumeration.
type Cadence string

// Cadence values.
const (
	CadenceDaily   Cadence = "daily"
	CadenceWeekly  Cadence = "weekly"
	CadenceMonthly Cadence = "monthly"
)

// Valid reports whether v is a known Cadence.
func (v Cadence) Valid() bool { return validEnum(EnumValues["Cadence"], string(v)) }

// ReportFormat is an API contract §5 enumeration.
type ReportFormat string

// ReportFormat values.
const (
	ReportFormatCsv ReportFormat = "csv"
)

// Valid reports whether v is a known ReportFormat.
func (v ReportFormat) Valid() bool { return validEnum(EnumValues["ReportFormat"], string(v)) }

// TagSource is an API contract §5 enumeration.
type TagSource string

// TagSource values.
const (
	TagSourceSystem         TagSource = "system"
	TagSourceService        TagSource = "service"
	TagSourceTeam           TagSource = "team"
	TagSourceServiceDefault TagSource = "service_default"
	TagSourceMissing        TagSource = "missing"
)

// Valid reports whether v is a known TagSource.
func (v TagSource) Valid() bool { return validEnum(EnumValues["TagSource"], string(v)) }

// BudgetPeriod is an API contract §5 enumeration.
type BudgetPeriod string

// BudgetPeriod values.
const (
	BudgetPeriodMonthly  BudgetPeriod = "monthly"
	BudgetPeriodLifetime BudgetPeriod = "lifetime"
)

// Valid reports whether v is a known BudgetPeriod.
func (v BudgetPeriod) Valid() bool { return validEnum(EnumValues["BudgetPeriod"], string(v)) }

// OnExhaust is an API contract §5 enumeration.
type OnExhaust string

// OnExhaust values.
const (
	OnExhaustBlock  OnExhaust = "block"
	OnExhaustNotify OnExhaust = "notify"
)

// Valid reports whether v is a known OnExhaust.
func (v OnExhaust) Valid() bool { return validEnum(EnumValues["OnExhaust"], string(v)) }

// IdPKind is an API contract §5 enumeration.
type IdPKind string

// IdPKind values.
const (
	IdPKindOkta    IdPKind = "okta"
	IdPKindEntraId IdPKind = "entra_id"
)

// Valid reports whether v is a known IdPKind.
func (v IdPKind) Valid() bool { return validEnum(EnumValues["IdPKind"], string(v)) }

// ExportStatus is an API contract §5 enumeration.
type ExportStatus string

// ExportStatus values.
const (
	ExportStatusPending   ExportStatus = "pending"
	ExportStatusRunning   ExportStatus = "running"
	ExportStatusSucceeded ExportStatus = "succeeded"
	ExportStatusFailed    ExportStatus = "failed"
)

// Valid reports whether v is a known ExportStatus.
func (v ExportStatus) Valid() bool { return validEnum(EnumValues["ExportStatus"], string(v)) }

// PriceProposalStatus is an API contract §5 enumeration.
type PriceProposalStatus string

// PriceProposalStatus values.
const (
	PriceProposalStatusPending    PriceProposalStatus = "pending"
	PriceProposalStatusApproved   PriceProposalStatus = "approved"
	PriceProposalStatusRejected   PriceProposalStatus = "rejected"
	PriceProposalStatusSuperseded PriceProposalStatus = "superseded"
)

// Valid reports whether v is a known PriceProposalStatus.
func (v PriceProposalStatus) Valid() bool {
	return validEnum(EnumValues["PriceProposalStatus"], string(v))
}

// CostTagAWSStatus is an API contract §5 enumeration.
type CostTagAWSStatus string

// CostTagAWSStatus values.
const (
	CostTagAWSStatusActive   CostTagAWSStatus = "active"
	CostTagAWSStatusInactive CostTagAWSStatus = "inactive"
	CostTagAWSStatusUnknown  CostTagAWSStatus = "unknown"
)

// Valid reports whether v is a known CostTagAWSStatus.
func (v CostTagAWSStatus) Valid() bool { return validEnum(EnumValues["CostTagAWSStatus"], string(v)) }

// SecretScope is an API contract §5 enumeration.
type SecretScope string

// SecretScope values.
const (
	SecretScopeTeam  SecretScope = "team"
	SecretScopeAgent SecretScope = "agent"
	SecretScopePool  SecretScope = "pool"
)

// Valid reports whether v is a known SecretScope.
func (v SecretScope) Valid() bool { return validEnum(EnumValues["SecretScope"], string(v)) }

// ConfigSource is an API contract §5 enumeration.
type ConfigSource string

// ConfigSource values.
const (
	ConfigSourceLiteral   ConfigSource = "literal"
	ConfigSourceNamespace ConfigSource = "namespace"
)

// Valid reports whether v is a known ConfigSource.
func (v ConfigSource) Valid() bool { return validEnum(EnumValues["ConfigSource"], string(v)) }

// ExtSystemScope is an API contract §5 enumeration.
type ExtSystemScope string

// ExtSystemScope values.
const (
	ExtSystemScopeService ExtSystemScope = "service"
	ExtSystemScopeTeam    ExtSystemScope = "team"
)

// Valid reports whether v is a known ExtSystemScope.
func (v ExtSystemScope) Valid() bool { return validEnum(EnumValues["ExtSystemScope"], string(v)) }

// ExtSystemTeamState is an API contract §5 enumeration.
type ExtSystemTeamState string

// ExtSystemTeamState values.
const (
	ExtSystemTeamStateOn  ExtSystemTeamState = "on"
	ExtSystemTeamStateOff ExtSystemTeamState = "off"
)

// Valid reports whether v is a known ExtSystemTeamState.
func (v ExtSystemTeamState) Valid() bool {
	return validEnum(EnumValues["ExtSystemTeamState"], string(v))
}

// ExtSystemDisabledAt is an API contract §5 enumeration.
type ExtSystemDisabledAt string

// ExtSystemDisabledAt values.
const (
	ExtSystemDisabledAtService ExtSystemDisabledAt = "service"
	ExtSystemDisabledAtTeam    ExtSystemDisabledAt = "team"
	ExtSystemDisabledAtAgent   ExtSystemDisabledAt = "agent"
)

// Valid reports whether v is a known ExtSystemDisabledAt.
func (v ExtSystemDisabledAt) Valid() bool {
	return validEnum(EnumValues["ExtSystemDisabledAt"], string(v))
}

// ServiceTagMode is an API contract §5 enumeration.
type ServiceTagMode string

// ServiceTagMode values.
const (
	ServiceTagModeFixed    ServiceTagMode = "fixed"
	ServiceTagModeRequired ServiceTagMode = "required"
)

// Valid reports whether v is a known ServiceTagMode.
func (v ServiceTagMode) Valid() bool { return validEnum(EnumValues["ServiceTagMode"], string(v)) }

// ImportResult is an API contract §5 enumeration.
type ImportResult string

// ImportResult values.
const (
	ImportResultCreated ImportResult = "created"
	ImportResultUpdated ImportResult = "updated"
	ImportResultSkipped ImportResult = "skipped"
	ImportResultFailed  ImportResult = "failed"
)

// Valid reports whether v is a known ImportResult.
func (v ImportResult) Valid() bool { return validEnum(EnumValues["ImportResult"], string(v)) }

// SwarmTemplateKind is an API contract §5 enumeration.
type SwarmTemplateKind string

// SwarmTemplateKind values.
const (
	SwarmTemplateKindCveRemediation SwarmTemplateKind = "cve_remediation"
	SwarmTemplateKindGeneral        SwarmTemplateKind = "general"
)

// Valid reports whether v is a known SwarmTemplateKind.
func (v SwarmTemplateKind) Valid() bool { return validEnum(EnumValues["SwarmTemplateKind"], string(v)) }

// ActorType is an API contract §5 enumeration.
type ActorType string

// ActorType values.
const (
	ActorTypeUser   ActorType = "user"
	ActorTypeApiKey ActorType = "api_key"
	ActorTypeSystem ActorType = "system"
	ActorTypeAgent  ActorType = "agent"
	ActorTypePool   ActorType = "pool"
)

// Valid reports whether v is a known ActorType.
func (v ActorType) Valid() bool { return validEnum(EnumValues["ActorType"], string(v)) }

// AuditOutcome is an API contract §5 enumeration.
type AuditOutcome string

// AuditOutcome values.
const (
	AuditOutcomeAllowed AuditOutcome = "allowed"
	AuditOutcomeDenied  AuditOutcome = "denied"
	AuditOutcomeFailed  AuditOutcome = "failed"
)

// Valid reports whether v is a known AuditOutcome.
func (v AuditOutcome) Valid() bool { return validEnum(EnumValues["AuditOutcome"], string(v)) }

// EnumValues maps each enumeration name to its values, in contract order.
var EnumValues = map[string][]string{
	"TeamKind":            {"persistent", "ephemeral"},
	"TeamState":           {"active", "suspended", "deleting", "deleted"},
	"IsolationTier":       {"shared", "dedicated"},
	"TeardownRule":        {"at_expiry", "at_cap_or_expiry", "at_work_complete"},
	"TeardownReason":      {"expired", "cap_reached", "work_complete", "manual"},
	"TeamRole":            {"team_admin", "team_collaborator", "team_viewer"},
	"SubjectType":         {"user", "group"},
	"PrincipalType":       {"user", "api_key", "workload"},
	"SecretDelivery":      {"env", "file"},
	"IAMIdentity":         {"team_shared", "dedicated"},
	"VolumeOnDelete":      {"snapshot_then_delete", "delete", "retain"},
	"SnapshotSchedule":    {"off", "daily", "weekly"},
	"RoleLinkStatus":      {"pending_trust", "verified", "failed", "disabled"},
	"PoolState":           {"enabled", "paused", "deleting", "deleted"},
	"SharedVolume":        {"none", "efs"},
	"WorkOrderKind":       {"cve_remediation", "general"},
	"Severity":            {"critical", "high", "medium", "low"},
	"WorkOrderStatus":     {"open", "assigned", "in_progress", "pr_opened", "done", "failed", "cancelled"},
	"SLAState":            {"on_track", "due_soon", "breached", "met", "missed"},
	"WorkOrderSource":     {"api", "eventbridge"},
	"RepoStatus":          {"pending", "in_progress", "pr_opened", "merged", "not_affected", "failed", "skipped"},
	"OwnerSource":         {"explicit", "team_config"},
	"ReportKind":          {"usage", "costs", "activity", "traffic"},
	"Cadence":             {"daily", "weekly", "monthly"},
	"ReportFormat":        {"csv"},
	"TagSource":           {"system", "service", "team", "service_default", "missing"},
	"BudgetPeriod":        {"monthly", "lifetime"},
	"OnExhaust":           {"block", "notify"},
	"IdPKind":             {"okta", "entra_id"},
	"ExportStatus":        {"pending", "running", "succeeded", "failed"},
	"PriceProposalStatus": {"pending", "approved", "rejected", "superseded"},
	"CostTagAWSStatus":    {"active", "inactive", "unknown"},
	"SecretScope":         {"team", "agent", "pool"},
	"ConfigSource":        {"literal", "namespace"},
	"ExtSystemScope":      {"service", "team"},
	"ExtSystemTeamState":  {"on", "off"},
	"ExtSystemDisabledAt": {"service", "team", "agent"},
	"ServiceTagMode":      {"fixed", "required"},
	"ImportResult":        {"created", "updated", "skipped", "failed"},
	"SwarmTemplateKind":   {"cve_remediation", "general"},
	"ActorType":           {"user", "api_key", "system", "agent", "pool"},
	"AuditOutcome":        {"allowed", "denied", "failed"},
}

func validEnum(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
