package httpapi

import (
	"math"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	me "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
)

// Wire shapes of API contract v1.5.0 §4.13 (ModelEndpoint, TeamModelEndpoint and the rest). Optional
// strings that are empty are null; secret values are never part of a response.

type endpointHealthBody struct {
	State  string  `json:"state"`
	Reason *string `json:"reason"`
	Since  *string `json:"since"`
}

type endpointPriceBody struct {
	InputUsdPerMtok       domain.Money  `json:"inputUsdPerMtok"`
	OutputUsdPerMtok      domain.Money  `json:"outputUsdPerMtok"`
	CachedInputUsdPerMtok *domain.Money `json:"cachedInputUsdPerMtok,omitempty"`
	RequestFeeUsd         domain.Money  `json:"requestFeeUsd"`
}

type endpointModelBody struct {
	ModelName       string             `json:"modelName"`
	MaxOutputTokens int                `json:"maxOutputTokens"`
	ContextTokens   *int               `json:"contextTokens"`
	Enabled         bool               `json:"enabled"`
	Price           *endpointPriceBody `json:"price"`
	PriceState      string             `json:"priceState"`
	Callable        bool               `json:"callable"`
}

type modelEndpointBody struct {
	ID               string              `json:"id"`
	TeamID           *string             `json:"teamId"`
	Slug             string              `json:"slug"`
	DisplayName      string              `json:"displayName"`
	Description      string              `json:"description"`
	Owner            *string             `json:"owner"`
	Purpose          *string             `json:"purpose"`
	Provider         string              `json:"provider"`
	ProviderState    string              `json:"providerState"`
	Protocols        []string            `json:"protocols"`
	Location         string              `json:"location"`
	BaseURL          string              `json:"baseUrl"`
	CABundle         *string             `json:"caBundle"`
	AuthKind         string              `json:"authKind"`
	AuthHeader       *string             `json:"authHeader"`
	MetricsPort      *int                `json:"metricsPort"`
	CredentialMode   string              `json:"credentialMode"`
	PricingMode      string              `json:"pricingMode"`
	Metered          bool                `json:"metered"`
	Capped           bool                `json:"capped"`
	RequiresTerms    bool                `json:"requiresTerms"`
	UseProviderCost  bool                `json:"useProviderCost"`
	Enabled          bool                `json:"enabled"`
	TimeoutSeconds   int                 `json:"timeoutSeconds"`
	MaxBodyMib       int                 `json:"maxBodyMib"`
	ExternalSystemID *string             `json:"externalSystemId"`
	Builtin          bool                `json:"builtin"`
	Models           []endpointModelBody `json:"models"`
	TeamsSelected    int                 `json:"teamsSelected"`
	Health           endpointHealthBody  `json:"health"`
	CreatedAt        string              `json:"createdAt"`
	CreatedBy        string              `json:"createdBy"`
	UpdatedAt        string              `json:"updatedAt"`
	Version          int64               `json:"version"`
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func toHealthBody(h me.Health) endpointHealthBody {
	state := string(h.State)
	if state == "" {
		state = string(domain.RouteHealthUnknown)
	}
	return endpointHealthBody{State: state, Reason: nullStr(h.Reason), Since: w2TimePtr(h.Since)}
}

func toPriceBody(p *me.Price) *endpointPriceBody {
	if p == nil {
		return nil
	}
	return &endpointPriceBody{InputUsdPerMtok: p.Input, OutputUsdPerMtok: p.Output, CachedInputUsdPerMtok: p.CachedInput, RequestFeeUsd: p.Fee}
}

func toEndpointModels(ms []me.Model, mode domain.PricingMode) []endpointModelBody {
	out := make([]endpointModelBody, 0, len(ms))
	for _, m := range ms {
		out = append(out, endpointModelBody{ModelName: m.ModelName, MaxOutputTokens: m.MaxOutputTokens, ContextTokens: nullInt(m.ContextTokens),
			Enabled: m.Enabled, Price: toPriceBody(m.Price), PriceState: m.PriceState, Callable: m.Callable(mode)})
	}
	return out
}

func protocolNames(ps []domain.Protocol) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return out
}

func toModelEndpointBody(e me.Endpoint) modelEndpointBody {
	return modelEndpointBody{ID: e.ID, TeamID: nullStr(e.TeamID), Slug: e.Slug, DisplayName: e.DisplayName, Description: e.Description,
		Owner: nullStr(e.Owner), Purpose: nullStr(e.Purpose), Provider: e.Provider, ProviderState: onOff(e.ProviderOn),
		Protocols: protocolNames(e.Protocols), Location: string(e.Location), BaseURL: e.BaseURL, CABundle: nullStr(e.CABundle),
		AuthKind: string(e.AuthKind), AuthHeader: nullStr(e.AuthHeader), MetricsPort: nullInt(e.MetricsPort),
		CredentialMode: string(e.CredentialMode), PricingMode: string(e.PricingMode), Metered: e.Metered(), Capped: e.Capped(),
		RequiresTerms: e.RequiresTerms, UseProviderCost: e.UseProviderCost, Enabled: e.Enabled, TimeoutSeconds: e.TimeoutSeconds,
		MaxBodyMib: e.MaxBodyMiB, ExternalSystemID: nullStr(e.ExternalSystem), Builtin: e.Builtin,
		Models: toEndpointModels(e.Models, e.PricingMode), TeamsSelected: e.TeamsSelected, Health: toHealthBody(e.Health),
		CreatedAt: w2Time(e.CreatedAt), CreatedBy: e.CreatedBy, UpdatedAt: w2Time(e.UpdatedAt), Version: int64(e.Version())}
}

type endpointModelListBody struct {
	EndpointID string              `json:"endpointId"`
	Items      []endpointModelBody `json:"items"`
	Version    int64               `json:"version"`
}

func toEndpointModelList(l me.ModelList, mode domain.PricingMode) endpointModelListBody {
	return endpointModelListBody{EndpointID: l.EndpointID, Items: toEndpointModels(l.Models, mode), Version: int64(l.Version)}
}

type endpointModelPriceBody struct {
	EndpointID            string        `json:"endpointId"`
	ModelName             string        `json:"modelName"`
	EffectiveFrom         string        `json:"effectiveFrom"`
	InputUsdPerMtok       domain.Money  `json:"inputUsdPerMtok"`
	OutputUsdPerMtok      domain.Money  `json:"outputUsdPerMtok"`
	CachedInputUsdPerMtok *domain.Money `json:"cachedInputUsdPerMtok,omitempty"`
	RequestFeeUsd         domain.Money  `json:"requestFeeUsd"`
	Source                string        `json:"source"`
	ApprovedBy            *string       `json:"approvedBy"`
	CreatedAt             string        `json:"createdAt"`
}

func toEndpointPriceBody(p me.PriceRow) endpointModelPriceBody {
	return endpointModelPriceBody{EndpointID: p.EndpointID, ModelName: p.ModelName, EffectiveFrom: w2Time(p.EffectiveFrom),
		InputUsdPerMtok: p.Price.Input, OutputUsdPerMtok: p.Price.Output, CachedInputUsdPerMtok: p.Price.CachedInput,
		RequestFeeUsd: p.Price.Fee, Source: p.Source, ApprovedBy: nullStr(p.ApprovedBy), CreatedAt: w2Time(p.CreatedAt)}
}

type endpointProposalBody struct {
	ID                    string        `json:"id"`
	EndpointID            string        `json:"endpointId"`
	Slug                  string        `json:"slug"`
	ModelName             string        `json:"modelName"`
	EffectiveFrom         string        `json:"effectiveFrom"`
	InputUsdPerMtok       domain.Money  `json:"inputUsdPerMtok"`
	OutputUsdPerMtok      domain.Money  `json:"outputUsdPerMtok"`
	CachedInputUsdPerMtok *domain.Money `json:"cachedInputUsdPerMtok,omitempty"`
	RequestFeeUsd         domain.Money  `json:"requestFeeUsd"`
	Source                string        `json:"source"`
	Status                string        `json:"status"`
	FetchedAt             string        `json:"fetchedAt"`
	DecidedAt             *string       `json:"decidedAt"`
	DecidedBy             *string       `json:"decidedBy"`
	Note                  *string       `json:"note"`
	Version               int64         `json:"version"`
}

func toEndpointProposalBody(p me.Proposal) endpointProposalBody {
	return endpointProposalBody{ID: p.ID, EndpointID: p.EndpointID, Slug: p.Slug, ModelName: p.ModelName, EffectiveFrom: w2Time(p.EffectiveFrom),
		InputUsdPerMtok: p.Price.Input, OutputUsdPerMtok: p.Price.Output, CachedInputUsdPerMtok: p.Price.CachedInput,
		RequestFeeUsd: p.Price.Fee, Source: p.Source, Status: p.State, FetchedAt: w2Time(p.FetchedAt), DecidedAt: w2TimePtr(p.DecidedAt),
		DecidedBy: p.DecidedBy, Note: nullStr(p.Note), Version: int64(p.Version())}
}

type endpointTeamBody struct {
	TeamID              string             `json:"teamId"`
	TeamSlug            string             `json:"teamSlug"`
	TeamDisplayName     string             `json:"teamDisplayName"`
	Selected            bool               `json:"selected"`
	SelectedBy          *string            `json:"selectedBy"`
	CredentialState     string             `json:"credentialState"`
	CredentialRotatedAt *string            `json:"credentialRotatedAt"`
	FederationState     string             `json:"federationState"`
	GatewayDeployment   string             `json:"gatewayDeployment"`
	GatewayVersion      *string            `json:"gatewayVersion"`
	SpendMtdUsd         domain.Money       `json:"spendMtdUsd"`
	Health              endpointHealthBody `json:"health"`
}

func toEndpointTeamBody(t me.EndpointTeam) endpointTeamBody {
	return endpointTeamBody{TeamID: t.TeamID, TeamSlug: t.TeamSlug, TeamDisplayName: t.TeamDisplayName, Selected: t.Selected,
		SelectedBy: nullStr(t.SelectedBy), CredentialState: t.CredentialState, CredentialRotatedAt: w2TimePtr(t.CredentialRotatedAt),
		FederationState: t.FederationState, GatewayDeployment: string(t.GatewayDeployment), GatewayVersion: nullStr(t.GatewayVersion),
		SpendMtdUsd: t.SpendMTD, Health: toHealthBody(t.Health)}
}

type endpointCredentialBody struct {
	EndpointID string  `json:"endpointId"`
	Scope      string  `json:"scope"`
	State      string  `json:"state"`
	RotatedAt  *string `json:"rotatedAt"`
	Version    int64   `json:"version"`
}

func toEndpointCredentialBody(c me.Credential) endpointCredentialBody {
	return endpointCredentialBody{EndpointID: c.EndpointID, Scope: c.Scope, State: c.State, RotatedAt: w2TimePtr(c.RotatedAt), Version: int64(c.Version)}
}

type endpointFederationBody struct {
	EndpointID       string  `json:"endpointId"`
	TeamID           string  `json:"teamId"`
	Kind             string  `json:"kind"`
	TenantID         *string `json:"tenantId"`
	ClientID         *string `json:"clientId"`
	ProjectNumber    *string `json:"projectNumber"`
	PoolID           *string `json:"poolId"`
	ProviderID       *string `json:"providerId"`
	ProjectID        *string `json:"projectId"`
	Location         *string `json:"location"`
	Status           string  `json:"status"`
	LastError        *string `json:"lastError"`
	CheckedAt        *string `json:"checkedAt"`
	ExpectedIssuer   *string `json:"expectedIssuer"`
	ExpectedSubject  *string `json:"expectedSubject"`
	ExpectedAudience *string `json:"expectedAudience"`
	ExpectedRoleArn  *string `json:"expectedRoleArn"`
	Version          int64   `json:"version"`
}

func toEndpointFederationBody(f me.Federation) endpointFederationBody {
	return endpointFederationBody{EndpointID: f.EndpointID, TeamID: f.TeamID, Kind: f.Kind, TenantID: nullStr(f.TenantID), ClientID: nullStr(f.ClientID),
		ProjectNumber: nullStr(f.ProjectNumber), PoolID: nullStr(f.PoolID), ProviderID: nullStr(f.ProviderID), ProjectID: nullStr(f.ProjectID),
		Location: nullStr(f.Location), Status: f.Status, LastError: nullStr(f.LastError), CheckedAt: w2TimePtr(f.CheckedAt),
		ExpectedIssuer: nullStr(f.ExpectedIssuer), ExpectedSubject: nullStr(f.ExpectedSubject), ExpectedAudience: nullStr(f.ExpectedAudience),
		ExpectedRoleArn: nullStr(f.ExpectedRoleARN), Version: int64(f.Version)}
}

type providerStatementBody struct {
	ID         string       `json:"id"`
	EndpointID string       `json:"endpointId"`
	Period     string       `json:"period"`
	Source     string       `json:"source"`
	RowCount   int          `json:"rowCount"`
	TotalUsd   domain.Money `json:"totalUsd"`
	ImportedBy string       `json:"importedBy"`
	ImportedAt string       `json:"importedAt"`
}

func toProviderStatementBody(s me.Statement) providerStatementBody {
	return providerStatementBody{ID: s.ID, EndpointID: s.EndpointID, Period: s.Period.Format("2006-01"), Source: s.Source, RowCount: len(s.Rows),
		TotalUsd: s.Total(), ImportedBy: s.ImportedBy, ImportedAt: w2Time(s.ImportedAt)}
}

type providerImpactBody struct {
	Routes                 int     `json:"routes"`
	Teams                  int     `json:"teams"`
	Agents                 int     `json:"agents"`
	AgentsMovingToFallback int     `json:"agentsMovingToFallback"`
	AgentsDegraded         int     `json:"agentsDegraded"`
	DirectSystems          int     `json:"directSystems"`
	TeamsToSelect          int     `json:"teamsToSelect"`
	FallbackModel          *string `json:"fallbackModel"`
}

type inferenceProviderBody struct {
	Provider       string              `json:"provider"`
	DisplayName    string              `json:"displayName"`
	Hosts          []string            `json:"hosts"`
	Enabled        bool                `json:"enabled"`
	TeamSelection  string              `json:"teamSelection"`
	DisabledReason *string             `json:"disabledReason"`
	FallbackModel  *string             `json:"fallbackModel"`
	Routes         int                 `json:"routes"`
	Teams          int                 `json:"teams"`
	DirectSystems  int                 `json:"directSystems"`
	ChangedBy      *string             `json:"changedBy"`
	ChangedAt      *string             `json:"changedAt"`
	Impact         *providerImpactBody `json:"impact,omitempty"`
	Version        int64               `json:"version"`
}

func toInferenceProviderBody(p me.Provider) inferenceProviderBody {
	hosts := p.Hosts
	if hosts == nil {
		hosts = []string{}
	}
	out := inferenceProviderBody{Provider: p.Provider, DisplayName: p.DisplayName, Hosts: hosts, Enabled: p.Enabled,
		TeamSelection: string(p.TeamSelection), DisabledReason: nullStr(p.DisabledReason), FallbackModel: nullStr(p.FallbackModel),
		Routes: p.Routes, Teams: p.Teams, DirectSystems: p.DirectSystems, ChangedBy: nullStr(p.ChangedBy), Version: int64(p.Version)}
	if !p.ChangedAt.IsZero() {
		out.ChangedAt = w2TimePtr(&p.ChangedAt)
	}
	if im := p.Impact; im != nil {
		out.Impact = &providerImpactBody{Routes: im.Routes, Teams: im.Teams, Agents: im.Agents, AgentsMovingToFallback: im.AgentsMovingToFallback,
			AgentsDegraded: im.AgentsDegraded, DirectSystems: im.DirectSystems, TeamsToSelect: im.TeamsToSelect, FallbackModel: nullStr(im.FallbackModel)}
	}
	return out
}

type teamGatewayBody struct {
	TeamID               string  `json:"teamId"`
	Deployment           string  `json:"deployment"`
	MayAddModelEndpoints bool    `json:"mayAddModelEndpoints"`
	MayAddMcpServers     bool    `json:"mayAddMcpServers"`
	McpPlacement         string  `json:"mcpPlacement"`
	GatewayState         string  `json:"gatewayState"`
	GatewayURL           *string `json:"gatewayUrl"`
	Version              int64   `json:"version"`
}

func toTeamGatewayBody(g me.TeamGateway) teamGatewayBody {
	return teamGatewayBody{TeamID: g.TeamID, Deployment: string(g.Deployment), MayAddModelEndpoints: g.MayAdd, GatewayState: g.GatewayState,
		MayAddMcpServers: g.MayAddMcp, McpPlacement: g.McpPlacement,
		GatewayURL: nullStr(g.GatewayURL), Version: int64(g.Version)}
}

type teamModelEndpointBody struct {
	ID              string              `json:"id"`
	TeamID          *string             `json:"teamId"`
	Slug            string              `json:"slug"`
	DisplayName     string              `json:"displayName"`
	Description     *string             `json:"description"`
	Owner           *string             `json:"owner"`
	Purpose         *string             `json:"purpose"`
	Provider        string              `json:"provider"`
	ProviderState   string              `json:"providerState"`
	Protocols       []string            `json:"protocols"`
	Location        string              `json:"location"`
	BaseURL         *string             `json:"baseUrl"`
	BaseURLEnv      string              `json:"baseUrlEnv"`
	AuthKind        string              `json:"authKind"`
	CredentialMode  string              `json:"credentialMode"`
	PricingMode     string              `json:"pricingMode"`
	Metered         bool                `json:"metered"`
	Capped          bool                `json:"capped"`
	RequiresTerms   bool                `json:"requiresTerms"`
	Enabled         bool                `json:"enabled"`
	Relation        string              `json:"relation"`
	SelectedBy      *string             `json:"selectedBy"`
	SelectableBy    string              `json:"selectableBy"`
	CredentialState string              `json:"credentialState"`
	FederationState string              `json:"federationState"`
	OffReason       *string             `json:"offReason"`
	Models          []endpointModelBody `json:"models"`
	Health          endpointHealthBody  `json:"health"`
	Version         int64               `json:"version"`
}

// toTeamModelEndpointBody is a team's view: the base URL of a service endpoint is the gateway's business,
// so it is shown only for the team's own endpoints.
func toTeamModelEndpointBody(t me.TeamEndpoint) teamModelEndpointBody {
	var base *string
	if t.IsTeam() {
		base = nullStr(t.BaseURL)
	}
	return teamModelEndpointBody{ID: t.ID, TeamID: nullStr(t.TeamID), Slug: t.Slug, DisplayName: t.DisplayName, Description: nullStr(t.Description),
		Owner: nullStr(t.Owner), Purpose: nullStr(t.Purpose), Provider: t.Provider, ProviderState: onOff(t.ProviderOn),
		Protocols: protocolNames(t.Protocols), Location: string(t.Location), BaseURL: base, BaseURLEnv: domain.RouteEnvName(t.Slug),
		AuthKind: string(t.AuthKind), CredentialMode: string(t.CredentialMode), PricingMode: string(t.PricingMode), Metered: t.Metered(),
		Capped: t.Capped(), RequiresTerms: t.RequiresTerms, Enabled: t.Enabled, Relation: t.Relation, SelectedBy: nullStr(t.SelectedBy),
		SelectableBy: string(t.SelectableBy), CredentialState: t.CredentialState, FederationState: t.FederationState,
		OffReason: nullStr(t.OffReason), Models: toEndpointModels(t.Models, t.PricingMode), Health: toHealthBody(t.Health), Version: int64(t.Version())}
}

type teamModelEndpointListBody struct {
	Items                []teamModelEndpointBody `json:"items"`
	NextToken            string                  `json:"nextToken,omitempty"`
	GatewayDeployment    string                  `json:"gatewayDeployment"`
	MayAddModelEndpoints bool                    `json:"mayAddModelEndpoints"`
}

type selectionBody struct {
	EndpointID string `json:"endpointId"`
	Slug       string `json:"slug"`
	SelectedBy string `json:"selectedBy"`
}

type selectionsBody struct {
	TeamID      string          `json:"teamId"`
	EndpointIDs []string        `json:"endpointIds"`
	Selections  []selectionBody `json:"selections"`
}

func toSelectionsBody(r me.SelectionsResult) selectionsBody {
	out := selectionsBody{TeamID: r.TeamID, EndpointIDs: r.EndpointIDs(), Selections: make([]selectionBody, 0, len(r.Selections))}
	if out.EndpointIDs == nil {
		out.EndpointIDs = []string{}
	}
	for _, s := range r.Selections {
		out.Selections = append(out.Selections, selectionBody{EndpointID: s.EndpointID, Slug: s.Slug, SelectedBy: s.SelectedBy})
	}
	return out
}

type inferenceRowBody struct {
	TeamID                string        `json:"teamId,omitempty"`
	Route                 string        `json:"route,omitempty"`
	Model                 string        `json:"model,omitempty"`
	AgentID               string        `json:"agentId,omitempty"`
	PoolID                string        `json:"poolId,omitempty"`
	Day                   string        `json:"day,omitempty"`
	Hour                  *string       `json:"hour,omitempty"`
	Calls                 int64         `json:"calls"`
	Errors                int64         `json:"errors"`
	ErrorRatePct          float64       `json:"errorRatePct"`
	Throttled             int64         `json:"throttled"`
	Refused               int64         `json:"refused"`
	LatencyP50Ms          *int64        `json:"latencyP50Ms"`
	LatencyP95Ms          *int64        `json:"latencyP95Ms"`
	LatencyP99Ms          *int64        `json:"latencyP99Ms"`
	TTFTP50Ms             *int64        `json:"ttftP50Ms"`
	TTFTP95Ms             *int64        `json:"ttftP95Ms"`
	TTFTP99Ms             *int64        `json:"ttftP99Ms"`
	GatewayP99Ms          *int64        `json:"gatewayP99Ms"`
	InputTokens           int64         `json:"inputTokens"`
	OutputTokens          int64         `json:"outputTokens"`
	CachedTokens          int64         `json:"cachedTokens"`
	ReasoningTokens       int64         `json:"reasoningTokens"`
	OutputTokensPerSecond *float64      `json:"outputTokensPerSecond"`
	EstimatedPct          float64       `json:"estimatedPct"`
	CostUsd               domain.Money  `json:"costUsd"`
	EffectiveUsdPerMtok   *domain.Money `json:"effectiveUsdPerMtok"`
	PricingMode           string        `json:"pricingMode,omitempty"`
}

func toInferenceRowBody(r me.ReportRow) inferenceRowBody {
	out := inferenceRowBody{TeamID: r.TeamID, Route: r.Route, Model: r.Model, AgentID: r.AgentID, PoolID: r.PoolID, Day: r.Day,
		Hour: w2TimePtr(r.Hour), Calls: r.Calls, Errors: r.Errors, ErrorRatePct: r.ErrorRatePct(), Throttled: r.Throttled, Refused: r.Refused,
		LatencyP50Ms: r.LatencyP50, LatencyP95Ms: r.LatencyP95, LatencyP99Ms: r.LatencyP99, TTFTP50Ms: r.TTFTP50, TTFTP95Ms: r.TTFTP95,
		TTFTP99Ms: r.TTFTP99, GatewayP99Ms: r.GatewayP99, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CachedTokens: r.CachedTokens,
		ReasoningTokens: r.ReasoningTokens, OutputTokensPerSecond: r.OutputTokensPerSecond, EstimatedPct: r.EstimatedPct(), CostUsd: r.CostUSD,
		PricingMode: r.PricingMode}
	if out.Route == domain.RouteBedrock && out.PricingMode == string(domain.PricingPerToken) {
		out.PricingMode = domain.RouteBedrock
	}
	if tok := r.InputTokens + r.OutputTokens; tok > 0 && r.CostUSD.Micros() > 0 {
		m := domain.MoneyFromMicros(int64(math.Round(float64(r.CostUSD.Micros()) * 1e6 / float64(tok))))
		out.EffectiveUsdPerMtok = &m
	}
	return out
}

type inferenceReportBody struct {
	Range     reportRangeBody    `json:"range"`
	GroupBy   []string           `json:"groupBy"`
	Rows      []inferenceRowBody `json:"rows"`
	Totals    inferenceRowBody   `json:"totals"`
	Notes     []string           `json:"notes"`
	NextToken string             `json:"nextToken,omitempty"`
}
