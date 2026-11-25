// Package modelendpoints implements the model endpoint, route and provider operations of API contract
// v1.5.0 §4.13 (model endpoint gateway PRD Appendix D.3.4, specs/261125-model-endpoint-gateway): the
// service catalog of model endpoints with their models, prices and price proposals, per-team credentials
// (write-only, through Secrets Manager) and workload identity federation, provider statements, the
// provider switches (Bedrock included, with the Bedrock-off fallback), the team gateway settings, a team's
// own endpoints and selections, and the inference report.
//
// Every mutation writes its rows, one audit record and one outbox event in one transaction; updates take
// If-Match. The database (migration 000023) derives the provider from the base URL host, enforces the
// provider switch, the slug rules, who may select what and the Bedrock-off guard; this package checks the
// same rules first where it can name the contract's reason, and maps what only the database can see.
package modelendpoints

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Resource kinds of the audit records.
const (
	kindEndpoint   = "model_endpoints"
	kindModels     = "endpoint_models"
	kindPrice      = "endpoint_model_prices"
	kindProposal   = "endpoint_price_proposals"
	kindCredential = "endpoint_credentials" //nolint:gosec // a table name
	kindFederation = "endpoint_federations"
	kindStatement  = "provider_statements"
	kindProvider   = "inference_providers"
	kindSelection  = "team_model_endpoints"
	kindTeam       = "teams"
)

// Price states of a route model (EndpointModel.priceState).
const (
	PriceApproved    = "approved"
	PricePending     = "pending"
	PriceNone        = "none"
	PriceNotRequired = "not_required"
)

// Credential and federation states of a team on an endpoint.
const (
	StateSet         = "set"
	StateMissing     = "missing"
	StateNotRequired = "not_required"
	FedPending       = "pending"
	FedReady         = "ready"
	FedFailing       = "failing"
)

// Proposal states.
const (
	ProposalPending    = "pending"
	ProposalApproved   = "approved"
	ProposalRejected   = "rejected"
	ProposalSuperseded = "superseded"
)

// Relations of an endpoint to a team (TeamModelEndpoint.relation).
const (
	RelationSelected  = "selected"
	RelationOwn       = "own"
	RelationAvailable = "available"
)

// Off reasons of a team's route (TeamModelEndpoint.offReason).
const (
	OffProviderDisabled   = "provider_disabled"
	OffEndpointDisabled   = "endpoint_disabled"
	OffNotSelected        = "not_selected"
	OffCredentialMissing  = "credential_missing" //nolint:gosec // an off reason, not a credential
	OffFederationNotReady = "federation_not_ready"
)

// Gateway states of a team (TeamGatewaySettings.gatewayState).
const (
	GatewayStateShared       = "shared"
	GatewayStateProvisioning = "provisioning"
	GatewayStateReady        = "ready"
	GatewayStateDegraded     = "degraded"
)

// Statement sources.
const (
	SourceNousStatement = "nous_statement"
	SourceOpenCodeCSV   = "opencode_csv"
)

// Price is a route model price: USD per million tokens and a per-call fee. CachedInput nil bills cached
// input at the input price.
type Price struct {
	Input       domain.Money
	Output      domain.Money
	CachedInput *domain.Money
	Fee         domain.Money
}

// Model is a route model with its price in force and price state.
type Model struct {
	domain.EndpointModel
	Price      *Price
	PriceState string
}

// Callable reports whether the model can be called: enabled and, on a per_token route, priced.
func (m Model) Callable(mode domain.PricingMode) bool {
	return m.Enabled && (mode != domain.PricingPerToken || m.Price != nil)
}

// Health is a route condition written by the reconciler; Unknown before the first evaluation.
type Health struct {
	State  domain.RouteHealth
	Reason string
	Since  *time.Time
}

// Endpoint is a model endpoint as the API shows it.
type Endpoint struct {
	domain.ModelEndpoint
	Models        []Model
	TeamsSelected int
	ProviderOn    bool
	Health        Health
}

// Version is the endpoint's version.
func (e Endpoint) Version() domain.Version { return domain.Version(e.ModelEndpoint.Version) }

// Metered reports whether calls are recorded with a cost (every route; free routes cost 0).
func (e Endpoint) Metered() bool { return e.PricingMode != domain.PricingFree }

// Capped reports whether calls reserve and settle against the team budget.
func (e Endpoint) Capped() bool { return e.PricingMode.Capped() }

// PriceRow is one endpoint_model_prices row.
type PriceRow struct {
	EndpointID    string
	ModelName     string
	EffectiveFrom time.Time
	Price         Price
	Source        string
	ApprovedBy    string
	CreatedAt     time.Time
}

// Proposal is an endpoint_price_proposals row.
type Proposal struct {
	ID            string
	EndpointID    string
	Slug          string
	ModelName     string
	TeamID        string
	EffectiveFrom time.Time
	Price         Price
	Source        string
	State         string
	FetchedAt     time.Time
	Note          string
	DecidedAt     *time.Time
	DecidedBy     *string
}

// Version is 1 while the proposal is open and 2 once decided (the table has no version column).
func (p Proposal) Version() domain.Version {
	if p.State == ProposalPending {
		return 1
	}
	return 2
}

// EndpointTeam is a team's view of one service endpoint (ListEndpointTeams).
type EndpointTeam struct {
	TeamID              string
	TeamSlug            string
	TeamDisplayName     string
	Selected            bool
	SelectedBy          string
	CredentialState     string
	CredentialRotatedAt *time.Time
	FederationState     string
	GatewayDeployment   domain.GatewayDeployment
	GatewayVersion      string
	SpendMTD            domain.Money
	Health              Health
}

// Credential is an endpoint_credentials row: a Secrets Manager reference, never a value.
type Credential struct {
	EndpointID string
	Scope      string
	TeamID     string
	ARN        string
	State      string
	RotatedAt  *time.Time
	Version    domain.Version
}

// Federation is an endpoint_federations row (identifiers only).
type Federation struct {
	EndpointID    string
	TeamID        string
	Kind          string
	TenantID      string
	ClientID      string
	ProjectNumber string
	PoolID        string
	ProviderID    string
	ProjectID     string
	Location      string
	Status        string
	LastError     string
	CheckedAt     *time.Time
	Version       domain.Version
	// Expected* are what the cloud admins configure on their side, computed by the use case.
	ExpectedIssuer   string
	ExpectedSubject  string
	ExpectedAudience string
	ExpectedRoleARN  string
}

// StatementRow is one row of a provider statement.
type StatementRow struct {
	TeamID    string
	Model     string
	AmountUSD domain.Money
	Tokens    *int64
}

// Statement is a provider_statements row.
type Statement struct {
	ID         string
	EndpointID string
	Period     time.Time
	Source     string
	Rows       []StatementRow
	ImportedBy string
	ImportedAt time.Time
}

// Total is the sum of the statement's amounts.
func (s Statement) Total() domain.Money {
	var t domain.Money
	for _, r := range s.Rows {
		t = t.Add(r.AmountUSD)
	}
	return t
}

// Provider is a provider switch with its counts.
type Provider struct {
	domain.InferenceProvider
	Routes        int
	Teams         int
	DirectSystems int
	Impact        *Impact
}

// Impact is what switching a provider off does (screen 68a).
type Impact struct {
	Routes                 int
	Teams                  int
	Agents                 int
	AgentsMovingToFallback int
	AgentsDegraded         int
	DirectSystems          int
	TeamsToSelect          int
	FallbackModel          string
}

// TeamGateway is a team's gateway settings.
type TeamGateway struct {
	TeamID     string
	Namespace  string
	State      string // the team's lifecycle state
	Deployment domain.GatewayDeployment
	MayAdd     bool
	// MayAddMcp and McpPlacement are the v1.6.0 MCP settings (API contract §4.14); McpPlacement reads the
	// service default (gateway.mcp.placement_default) when the team has none.
	MayAddMcp    bool
	McpPlacement string
	// McpPlacementStored is the team's own placement, "" when it follows the service default.
	McpPlacementStored string
	Version            domain.Version
	GatewayPhase       string // resource_status phase of the team gateway, "" before the first render
	GatewayURL         string
	GatewayState       string
	SharedGateway      string
}

// TeamEndpoint is an endpoint as one team sees it (ListTeamModelEndpoints).
type TeamEndpoint struct {
	Endpoint
	Relation        string
	SelectedBy      string
	SelectableBy    domain.TeamSelection
	CredentialState string
	FederationState string
	OffReason       string
}

// Selection is one of a team's opted-in service endpoints.
type Selection struct {
	EndpointID string
	Slug       string
	SelectedBy string
}

// Filter narrows ListModelEndpoints.
type Filter struct {
	IncludeTeams bool
	Provider     string
	Location     string
	PricingMode  string
}

// Repository is the 000023 store, used inside the transaction it is given. Row-level security limits
// what the caller's session sees and writes.
type Repository interface {
	// ListEndpoints returns endpoints with id greater than after, in id order: service endpoints and, with
	// IncludeTeams, team endpoints.
	ListEndpoints(ctx context.Context, tx port.Tx, f Filter, after string, limit int) ([]Endpoint, error)
	// GetEndpoint returns one endpoint with its models; NotFound when the caller cannot see it.
	GetEndpoint(ctx context.Context, tx port.Tx, id string, forUpdate bool) (Endpoint, error)
	// InsertEndpoint stores a new endpoint (the database derives its provider).
	InsertEndpoint(ctx context.Context, tx port.Tx, e domain.ModelEndpoint) error
	// UpdateEndpoint stores the mutable columns; PreconditionFailed when the version is not expect.
	UpdateEndpoint(ctx context.Context, tx port.Tx, e domain.ModelEndpoint, expect domain.Version) error
	// DeleteEndpoint removes an endpoint; PreconditionFailed when the version is not expect.
	DeleteEndpoint(ctx context.Context, tx port.Tx, id string, expect domain.Version) error
	// PutModels replaces the endpoint's model list and bumps the endpoint's version.
	PutModels(ctx context.Context, tx port.Tx, endpointID string, ms []domain.EndpointModel) error
	// InsertPrice appends a price row and returns it as stored.
	InsertPrice(ctx context.Context, tx port.Tx, p PriceRow) (PriceRow, error)

	// ListProposals returns proposals (of state and endpoint when not empty), newest first, id below after.
	ListProposals(ctx context.Context, tx port.Tx, state, endpointID, after string, limit int) ([]Proposal, error)
	// GetProposal returns one proposal; NotFound when absent.
	GetProposal(ctx context.Context, tx port.Tx, id string, forUpdate bool) (Proposal, error)
	// DecideProposal moves a pending proposal to state, with note when not empty.
	DecideProposal(ctx context.Context, tx port.Tx, id, state, note string) (Proposal, error)

	// ListEndpointTeams returns every live team's view of a service endpoint, team id above after.
	ListEndpointTeams(ctx context.Context, tx port.Tx, e Endpoint, after string, limit int) ([]EndpointTeam, error)

	// CredentialPath returns the Secrets Manager name of the endpoint's credential for scope.
	CredentialPath(ctx context.Context, tx port.Tx, e Endpoint, scope string) (string, error)
	// GetCredential returns a credential row; NotFound when there is none.
	GetCredential(ctx context.Context, tx port.Tx, endpointID, scope string) (Credential, error)
	// PutCredential inserts or updates a credential row (status set, rotated now) and returns it.
	PutCredential(ctx context.Context, tx port.Tx, c Credential) (Credential, error)

	// GetFederation returns a federation row; NotFound when there is none.
	GetFederation(ctx context.Context, tx port.Tx, endpointID, teamID string) (Federation, error)
	// PutFederation inserts or replaces the identifiers (status back to pending) and returns the row.
	PutFederation(ctx context.Context, tx port.Tx, f Federation) (Federation, error)

	// InsertStatement stores a statement; a second one for the endpoint, period and source is a Conflict.
	InsertStatement(ctx context.Context, tx port.Tx, s Statement) (Statement, error)

	// ListProviders returns every provider switch with its counts, by provider.
	ListProviders(ctx context.Context, tx port.Tx) ([]Provider, error)
	// GetProvider returns one provider switch; NotFound when absent.
	GetProvider(ctx context.Context, tx port.Tx, provider string, forUpdate bool) (Provider, error)
	// UpdateProvider stores the switch; PreconditionFailed when the version is not expect.
	UpdateProvider(ctx context.Context, tx port.Tx, p domain.InferenceProvider, expect domain.Version) error
	// ProviderImpact counts what switching the provider off would do; fallback is the Bedrock fallback.
	ProviderImpact(ctx context.Context, tx port.Tx, provider, fallback string) (Impact, error)
	// SelectForAllTeams selects the service endpoint for every live team that has not; returns how many.
	SelectForAllTeams(ctx context.Context, tx port.Tx, endpointID string) (int, error)
	// SetServiceDefaultModel writes models.default_model_id.
	SetServiceDefaultModel(ctx context.Context, tx port.Tx, value string) error
	// ServiceEndpointBySlug returns the service endpoint with the slug; NotFound when absent.
	ServiceEndpointBySlug(ctx context.Context, tx port.Tx, slug string) (Endpoint, error)

	// TeamGateway returns the team's gateway settings; NotFound when the caller cannot see the team.
	TeamGateway(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (TeamGateway, error)
	// PutTeamGateway stores the settings; PreconditionFailed when the team's version is not expect.
	PutTeamGateway(ctx context.Context, tx port.Tx, teamID string, d domain.GatewayDeployment, mayAdd, mayAddMcp bool, placement string, expect domain.Version) error

	// ListTeamEndpoints returns the team's endpoints (selected, own and available) with id above after.
	ListTeamEndpoints(ctx context.Context, tx port.Tx, teamID, relation, after string, limit int) ([]TeamEndpoint, error)
	// TeamEndpoint returns one endpoint as the team sees it; NotFound otherwise.
	TeamEndpoint(ctx context.Context, tx port.Tx, teamID, endpointID string) (TeamEndpoint, error)
	// TeamSelections returns the team's opted-in service endpoints, by endpoint id.
	TeamSelections(ctx context.Context, tx port.Tx, teamID string) ([]Selection, error)
	// Select opts the team in to a service endpoint (selected_by is the caller's).
	Select(ctx context.Context, tx port.Tx, teamID, endpointID string) error
	// Deselect removes the team's opt-in.
	Deselect(ctx context.Context, tx port.Tx, teamID, endpointID string) error

	// InferenceReport computes the inference report.
	InferenceReport(ctx context.Context, tx port.Tx, q ReportQuery) (Report, error)
}
