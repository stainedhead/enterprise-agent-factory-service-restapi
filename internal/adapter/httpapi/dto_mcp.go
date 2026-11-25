package httpapi

import (
	"encoding/json"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

// mcpRequest is the v1.6.0 mcp object of the agent and pool create and update bodies (contract §4.14).
type mcpRequest struct {
	Access    *string                 `json:"access"`
	ServerIDs *[]string               `json:"serverIds"`
	Mode      *string                 `json:"mode"`
	Blocks    *[]domain.McpAgentBlock `json:"blocks"`
}

// patch converts the request to the use case's patch; nil stays nil.
func (m *mcpRequest) patch() *domain.McpAccessPatch {
	if m == nil {
		return nil
	}
	p := &domain.McpAccessPatch{Access: m.Access, ServerIDs: m.ServerIDs, Blocks: m.Blocks}
	if m.Mode != nil {
		mode := domain.McpAgentMode(*m.Mode)
		p.Mode = &mode
	}
	return p
}

// agentMcpBody is Agent.mcp and Pool.mcp (contract AgentMcp).
type agentMcpBody struct {
	Access           string                 `json:"access"`
	ServerIDs        []string               `json:"serverIds"`
	Mode             string                 `json:"mode"`
	Blocks           []domain.McpAgentBlock `json:"blocks"`
	EffectiveServers []string               `json:"effectiveServers"`
	Env              []string               `json:"env"`
}

func toAgentMcpBody(m *domain.McpAccessState) *agentMcpBody {
	if m == nil {
		return nil
	}
	return &agentMcpBody{Access: m.Access, ServerIDs: m.ServerIDs, Mode: string(m.Mode), Blocks: m.Blocks, EffectiveServers: m.Effective, Env: m.Env}
}

// ---- 1.3.0 MCP bodies (contract §4.14). Field names are the contract's; no credential value is ever a field. ----

type mcpServerBody struct {
	ID                 string                `json:"id"`
	TeamID             *string               `json:"teamId"`
	Scope              string                `json:"scope"`
	Slug               string                `json:"slug"`
	DisplayName        string                `json:"displayName"`
	Owner              string                `json:"owner"`
	Purpose            string                `json:"purpose"`
	DocURL             *string               `json:"docUrl"`
	DataClassification string                `json:"dataClassification"`
	Transport          string                `json:"transport"`
	Location           string                `json:"location"`
	URL                *string               `json:"url"`
	Hosted             *domain.McpHosted     `json:"hosted"`
	AuthKind           string                `json:"authKind"`
	AuthHeader         *string               `json:"authHeader"`
	CredentialMode     string                `json:"credentialMode,omitempty"`
	Federation         *domain.McpFederation `json:"federation"`
	ProtocolVersion    *string               `json:"protocolVersion"`
	ReviewStatus       string                `json:"reviewStatus"`
	Published          bool                  `json:"published"`
	RiskTier           string                `json:"riskTier,omitempty"`
	RiskReasons        []string              `json:"riskReasons"`
	Approver           string                `json:"approver,omitempty"`
	Checks             []mcpCheckBody        `json:"checks"`
	DriftPolicy        string                `json:"driftPolicy"`
	DefaultForNewTeams bool                  `json:"defaultForNewTeams"`
	AllowSampling      bool                  `json:"allowSampling"`
	TrustAnnotations   bool                  `json:"trustAnnotations"`
	Limits             domain.McpLimits      `json:"limits"`
	CredentialState    string                `json:"credentialState"`
	SuspendedReason    *string               `json:"suspendedReason"`
	Adoption           *mcpAdoptionBody      `json:"adoption,omitempty"`
	Version            int64                 `json:"version"`
	CreatedAt          string                `json:"createdAt"`
	CreatedBy          string                `json:"createdBy"`
	UpdatedAt          string                `json:"updatedAt"`
}

type mcpAdoptionBody struct {
	Teams  int `json:"teams"`
	Agents int `json:"agents"`
}

type mcpCheckBody struct {
	Name      string  `json:"name"`
	Outcome   string  `json:"outcome"`
	Detail    *string `json:"detail"`
	CheckedAt string  `json:"checkedAt"`
}

func toMcpChecks(cs []domain.McpCheck) []mcpCheckBody {
	out := make([]mcpCheckBody, 0, len(cs))
	for _, c := range cs {
		out = append(out, mcpCheckBody{Name: c.Name, Outcome: c.Outcome, Detail: optStr(c.Detail), CheckedAt: w2Time(c.CheckedAt)})
	}
	return out
}

func toMcpServerBody(v mcpcatalog.ServerView) mcpServerBody {
	s := v.Server
	riskReasons := s.RiskReasons
	if riskReasons == nil {
		riskReasons = []string{}
	}
	b := mcpServerBody{ID: s.ID, TeamID: optStr(s.TeamID), Scope: s.Scope(), Slug: s.Slug, DisplayName: s.DisplayName, Owner: s.Owner,
		Purpose: s.Purpose, DocURL: optStr(s.DocURL), DataClassification: s.DataClassification, Transport: string(s.Transport),
		Location: string(s.Location), URL: optStr(s.URL), Hosted: s.Hosted, AuthKind: string(s.AuthKind), AuthHeader: optStr(s.AuthHeader),
		CredentialMode: s.CredentialMode, Federation: s.Federation, ProtocolVersion: optStr(s.ProtocolVersion),
		ReviewStatus: string(s.ReviewStatus), Published: s.Published, RiskTier: s.RiskTier, RiskReasons: riskReasons, Approver: v.Approver,
		Checks: toMcpChecks(s.Checks), DriftPolicy: s.DriftPolicy, DefaultForNewTeams: s.DefaultForNewTeams, AllowSampling: s.AllowSampling,
		TrustAnnotations: s.TrustAnnotations, Limits: s.Limits, CredentialState: v.CredentialState, SuspendedReason: optStr(s.SuspendedReason),
		Version: int64(s.Version), CreatedAt: w2Time(s.CreatedAt), CreatedBy: s.CreatedBy, UpdatedAt: w2Time(s.UpdatedAt)}
	if v.Adoption != nil {
		b.Adoption = &mcpAdoptionBody{Teams: v.Adoption.Teams, Agents: v.Adoption.Agents}
	}
	return b
}

type mcpDecisionBody struct {
	Allowed bool    `json:"allowed"`
	Level   string  `json:"level"`
	Reason  *string `json:"reason"`
}

type mcpPrimitiveBody struct {
	ID             string           `json:"id"`
	ServerID       string           `json:"serverId"`
	Kind           string           `json:"kind"`
	Name           string           `json:"name"`
	Title          *string          `json:"title"`
	Annotations    json.RawMessage  `json:"annotations"`
	Digest         string           `json:"digest"`
	ApprovedDigest *string          `json:"approvedDigest"`
	Status         string           `json:"status"`
	Change         string           `json:"change,omitempty"`
	FirstSeen      string           `json:"firstSeen"`
	LastSeen       string           `json:"lastSeen"`
	Decision       *mcpDecisionBody `json:"decision,omitempty"`
}

func toMcpPrimitiveBody(v mcpcatalog.PrimitiveView) mcpPrimitiveBody {
	p := v.Primitive
	ann := p.Annotations
	if len(ann) == 0 {
		ann = json.RawMessage(`{}`)
	}
	b := mcpPrimitiveBody{ID: p.ID, ServerID: p.ServerID, Kind: p.Kind, Name: p.Name, Title: optStr(p.Title), Annotations: ann, Digest: p.Digest,
		ApprovedDigest: optStr(p.ApprovedDigest), Status: p.Status, Change: p.Change, FirstSeen: w2Time(p.FirstSeen), LastSeen: w2Time(p.LastSeen)}
	if d := v.Decision; d != nil {
		level := d.Level
		if d.Allowed && level == "" {
			level = "service"
		}
		b.Decision = &mcpDecisionBody{Allowed: d.Allowed, Level: level, Reason: optStr(d.Reason)}
	}
	return b
}

type mcpFieldDiffBody struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

type mcpPrimitiveChangeBody struct {
	Primitive          mcpPrimitiveBody   `json:"primitive"`
	ApprovedDefinition json.RawMessage    `json:"approvedDefinition"`
	PendingDefinition  json.RawMessage    `json:"pendingDefinition"`
	Diff               []mcpFieldDiffBody `json:"diff"`
	Checks             []mcpCheckBody     `json:"checks"`
	CrossesRiskLine    bool               `json:"crossesRiskLine"`
}

func toMcpPrimitiveChangeBody(c mcpcatalog.PrimitiveChange) mcpPrimitiveChangeBody {
	diff := make([]mcpFieldDiffBody, 0, len(c.Diff))
	for _, d := range c.Diff {
		diff = append(diff, mcpFieldDiffBody{Field: d.Field, Before: d.Before, After: d.After})
	}
	pending := c.PendingDefinition
	if len(pending) == 0 {
		pending = json.RawMessage(`{}`)
	}
	return mcpPrimitiveChangeBody{Primitive: toMcpPrimitiveBody(c.Primitive), ApprovedDefinition: rawOrNull(c.ApprovedDefinition),
		PendingDefinition: pending, Diff: diff, Checks: toMcpChecks(c.Checks), CrossesRiskLine: c.CrossesRiskLine}
}

type mcpPolicyBody struct {
	ServerID         string                 `json:"serverId"`
	Level            string                 `json:"level"`
	TeamID           *string                `json:"teamId"`
	Rules            []domain.McpPolicyRule `json:"rules"`
	DriftPolicy      string                 `json:"driftPolicy,omitempty"`
	AllowSampling    bool                   `json:"allowSampling"`
	TrustAnnotations bool                   `json:"trustAnnotations"`
	Version          int64                  `json:"version"`
}

func toMcpPolicyBody(p mcpcatalog.PolicyView) mcpPolicyBody {
	rules := p.Rules
	if rules == nil {
		rules = []domain.McpPolicyRule{}
	}
	return mcpPolicyBody{ServerID: p.ServerID, Level: p.Level, TeamID: optStr(p.TeamID), Rules: rules, DriftPolicy: p.DriftPolicy,
		AllowSampling: p.AllowSampling, TrustAnnotations: p.TrustAnnotations, Version: int64(p.Version)}
}

type mcpCredentialBody struct {
	Scope     string   `json:"scope"`
	Kind      string   `json:"kind,omitempty"`
	State     string   `json:"state"`
	Subject   *string  `json:"subject"`
	Scopes    []string `json:"scopes"`
	ExpiresAt *string  `json:"expiresAt"`
	RotatedAt *string  `json:"rotatedAt"`
}

// toMcpCredentialBody shows metadata only: the ARN and the value never leave the API.
func toMcpCredentialBody(v mcpcatalog.CredentialView) mcpCredentialBody {
	kind := v.Kind
	switch kind {
	case "static", "oauth_client", "oauth_user":
	default:
		kind = ""
	}
	scopes := v.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	b := mcpCredentialBody{Scope: v.Scope, Kind: kind, State: v.State, Subject: optStr(v.Subject), Scopes: scopes}
	if v.Record != nil {
		b.ExpiresAt, b.RotatedAt = w2TimePtr(v.Record.ExpiresAt), w2TimePtr(v.Record.RotatedAt)
	}
	return b
}

type mcpReviewItemBody struct {
	Kind        string   `json:"kind"`
	ServerID    string   `json:"serverId"`
	TeamID      *string  `json:"teamId"`
	Slug        string   `json:"slug"`
	PrimitiveID *string  `json:"primitiveId"`
	Name        *string  `json:"name"`
	RiskTier    string   `json:"riskTier,omitempty"`
	RiskReasons []string `json:"riskReasons"`
	Approver    string   `json:"approver"`
	Since       string   `json:"since"`
}

func toMcpReviewItemBody(i domain.McpReviewQueueItem) mcpReviewItemBody {
	reasons := i.RiskReasons
	if reasons == nil {
		reasons = []string{}
	}
	return mcpReviewItemBody{Kind: i.Kind, ServerID: i.ServerID, TeamID: optStr(i.TeamID), Slug: i.Slug, PrimitiveID: optStr(i.PrimitiveID),
		Name: optStr(i.Name), RiskTier: i.RiskTier, RiskReasons: reasons, Approver: i.Approver, Since: w2Time(i.Since)}
}

type mcpEventBody struct {
	ID                 int64    `json:"id"`
	At                 string   `json:"at"`
	RequestID          string   `json:"requestId"`
	TeamID             string   `json:"teamId"`
	AgentID            *string  `json:"agentId"`
	PoolID             *string  `json:"poolId"`
	WorkerPod          *string  `json:"workerPod"`
	ServerID           *string  `json:"serverId"`
	ServerKind         string   `json:"serverKind,omitempty"`
	Method             string   `json:"method"`
	Name               *string  `json:"name"`
	PrimitiveID        *string  `json:"primitiveId"`
	Digest             *string  `json:"digest"`
	Outcome            string   `json:"outcome"`
	Reason             *string  `json:"reason"`
	JSONRPCCode        *int     `json:"jsonrpcCode"`
	HTTPStatus         *int     `json:"httpStatus"`
	GatewayMs          *int     `json:"gatewayMs"`
	UpstreamMs         *int     `json:"upstreamMs"`
	RequestBytes       *int     `json:"requestBytes"`
	ResponseBytes      *int     `json:"responseBytes"`
	AuthKind           *string  `json:"authKind"`
	ProtocolDownstream *string  `json:"protocolDownstream"`
	ProtocolUpstream   *string  `json:"protocolUpstream"`
	ArgsSHA256         *string  `json:"argsSha256"`
	SearchResults      []string `json:"searchResults,omitempty"`
	SearchHit          *bool    `json:"searchHit"`
	QueryText          *string  `json:"queryText"`
}

func toMcpEventBody(e domain.McpEventRecord) mcpEventBody {
	return mcpEventBody{ID: e.ID, At: e.At.UTC().Format(time.RFC3339Nano), RequestID: e.RequestID, TeamID: e.TeamID, AgentID: optStr(e.AgentID),
		PoolID: optStr(e.PoolID), WorkerPod: optStr(e.WorkerPod), ServerID: optStr(e.ServerID), ServerKind: e.ServerKind, Method: e.Method,
		Name: optStr(e.Name), PrimitiveID: optStr(e.PrimitiveID), Digest: optStr(e.Digest), Outcome: e.Outcome, Reason: optStr(e.Reason),
		JSONRPCCode: e.JSONRPCCode, HTTPStatus: e.HTTPStatus, GatewayMs: e.GatewayMs, UpstreamMs: e.UpstreamMs, RequestBytes: e.RequestBytes,
		ResponseBytes: e.ResponseBytes, AuthKind: optStr(e.AuthKind), ProtocolDownstream: optStr(e.ProtocolDownstream),
		ProtocolUpstream: optStr(e.ProtocolUpstream), ArgsSHA256: optStr(e.ArgsSHA256), SearchResults: e.SearchResults, SearchHit: e.SearchHit,
		QueryText: optStr(e.QueryText)}
}

type mcpUsageRowBody struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	Calls         int64    `json:"calls"`
	Errors        int64    `json:"errors"`
	Refusals      int64    `json:"refusals"`
	P50Ms         *int     `json:"p50Ms"`
	P95Ms         *int     `json:"p95Ms"`
	Teams         *int     `json:"teams"`
	Agents        *int     `json:"agents"`
	Searches      *int64   `json:"searches"`
	SearchHitRate *float64 `json:"searchHitRate"`
	LastUsedAt    *string  `json:"lastUsedAt"`
}

type mcpUsageReportBody struct {
	GroupBy    string            `json:"groupBy"`
	View       string            `json:"view"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	WindowDays *int              `json:"windowDays"`
	Rows       []mcpUsageRowBody `json:"rows"`
}

func toMcpUsageReportBody(rows []domain.McpUsageRow, q domain.McpUsageQuery) mcpUsageReportBody {
	out := mcpUsageReportBody{GroupBy: q.GroupBy, View: q.View, From: w2Time(q.From), To: w2Time(q.To), Rows: make([]mcpUsageRowBody, 0, len(rows))}
	if q.View == "unused" {
		w := q.WindowDays
		out.WindowDays = &w
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, mcpUsageRowBody{Key: r.Key, Label: r.Label, Calls: r.Calls, Errors: r.Errors, Refusals: r.Refusals,
			P50Ms: r.P50Ms, P95Ms: r.P95Ms, Teams: r.Teams, Agents: r.Agents, Searches: r.Searches, SearchHitRate: r.SearchHitRate,
			LastUsedAt: w2TimePtr(r.LastUsedAt)})
	}
	return out
}

type mcpCaptureBody struct {
	ID        string  `json:"id"`
	StartedBy string  `json:"startedBy"`
	StartedAt string  `json:"startedAt"`
	Until     string  `json:"until"`
	Reason    string  `json:"reason"`
	StoppedAt *string `json:"stoppedAt"`
	StoppedBy *string `json:"stoppedBy"`
}

func toMcpCaptureBody(c domain.McpQueryCapture) mcpCaptureBody {
	return mcpCaptureBody{ID: c.ID, StartedBy: c.StartedBy, StartedAt: w2Time(c.StartedAt), Until: w2Time(c.Until), Reason: c.Reason,
		StoppedAt: w2TimePtr(c.StoppedAt), StoppedBy: optStr(c.StoppedBy)}
}

type teamMcpServerBody struct {
	Server          mcpServerBody      `json:"server"`
	Enabled         bool               `json:"enabled"`
	Mode            string             `json:"mode"`
	State           string             `json:"state"`
	Managed         string             `json:"managed"`
	Credential      *mcpCredentialBody `json:"credential"`
	FederationState string             `json:"federationState,omitempty"`
	UpdatedAt       *string            `json:"updatedAt"`
}

func toTeamMcpServerBody(v mcpcatalog.TeamServerView, teamID string) teamMcpServerBody {
	r := v.Row
	b := teamMcpServerBody{Server: toMcpServerBody(mcpcatalog.ServerView{Server: r.Server, CredentialState: v.CredentialState, Approver: v.Approver}),
		Mode: string(v.Mode()), State: r.State(), Managed: v.Managed(), FederationState: r.FederationState}
	if r.Switch != nil {
		b.Enabled = r.Switch.Enabled
		b.UpdatedAt = w2TimePtr(&r.Switch.UpdatedAt)
	}
	if r.Server.AuthKind.NeedsCredential() {
		scope := r.Server.CredentialScope(teamID)
		if r.Credential != nil {
			scope = r.Credential.Scope
		}
		c := toMcpCredentialBody(mcpcatalog.CredentialView{Scope: scope, Kind: string(r.Server.AuthKind), State: v.CredentialState,
			Record: r.Credential, Scopes: credScopes(r.Credential), Subject: credSubject(r.Credential)})
		b.Credential = &c
	}
	return b
}

func credScopes(c *domain.McpCredentialRecord) []string {
	if c == nil {
		return []string{}
	}
	return c.Scopes
}

func credSubject(c *domain.McpCredentialRecord) string {
	if c == nil {
		return ""
	}
	return c.Subject
}

type effectiveMcpPrimitiveBody struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Allowed   bool    `json:"allowed"`
	Level     string  `json:"level"`
	Reason    *string `json:"reason"`
	RateLimit *int    `json:"rateLimitPerMinute"`
}

type effectiveMcpServerBody struct {
	ServerID   string                      `json:"serverId"`
	Slug       string                      `json:"slug"`
	Scope      string                      `json:"scope"`
	Mode       string                      `json:"mode"`
	URL        string                      `json:"url"`
	Primitives []effectiveMcpPrimitiveBody `json:"primitives"`
}

type effectiveMcpBody struct {
	SubjectKind string                   `json:"subjectKind"`
	SubjectID   string                   `json:"subjectId"`
	Access      string                   `json:"access"`
	Mode        string                   `json:"mode"`
	Servers     []effectiveMcpServerBody `json:"servers"`
	Env         []string                 `json:"env"`
}

func toEffectiveMcpBody(e mcpcatalog.Effective) effectiveMcpBody {
	out := effectiveMcpBody{SubjectKind: e.Subject.Kind, SubjectID: e.Subject.ID, Access: e.Subject.Access, Mode: string(e.Subject.Mode),
		Servers: make([]effectiveMcpServerBody, 0, len(e.Servers)), Env: e.Env}
	if out.Env == nil {
		out.Env = []string{}
	}
	for _, s := range e.Servers {
		sb := effectiveMcpServerBody{ServerID: s.ServerID, Slug: s.Slug, Scope: s.Scope, Mode: s.Mode, URL: s.URL,
			Primitives: make([]effectiveMcpPrimitiveBody, 0, len(s.Primitives))}
		for _, p := range s.Primitives {
			level := p.Level
			if level == "" {
				level = "service"
			}
			sb.Primitives = append(sb.Primitives, effectiveMcpPrimitiveBody{Name: p.Name, Kind: p.Kind, Allowed: p.Allowed, Level: level,
				Reason: optStr(p.Reason), RateLimit: p.RateLimit})
		}
		out.Servers = append(out.Servers, sb)
	}
	return out
}

type federatedIdentityBody struct {
	Name              string `json:"name"`
	Audience          string `json:"audience"`
	ExpirationSeconds int    `json:"expirationSeconds"`
	FilePath          string `json:"filePath"`
	EnvVar            string `json:"envVar"`
	Issuer            string `json:"issuer"`
	Subject           string `json:"subject"`
	Version           int64  `json:"version"`
	CreatedAt         string `json:"createdAt"`
	CreatedBy         string `json:"createdBy"`
}

func toFederatedIdentityBody(v mcpcatalog.FederatedView) federatedIdentityBody {
	f := v.Identity
	return federatedIdentityBody{Name: f.Name, Audience: f.Audience, ExpirationSeconds: f.ExpirationSeconds, FilePath: f.FilePath, EnvVar: f.EnvVar,
		Issuer: v.Issuer, Subject: v.Subject, Version: int64(f.Version), CreatedAt: w2Time(f.CreatedAt), CreatedBy: f.CreatedBy}
}
