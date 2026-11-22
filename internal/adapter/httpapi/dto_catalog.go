package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Wire shapes of the service catalogs (API contract §5: Model, ModelPrice, StorageClass,
// AgentTemplate, ExternalSystem). DTOs are mapped explicitly from the domain types.

// modelBody (the Model wire shape) is declared with the team model list in h2_teams.go.
func toModelBody(m domain.Model) modelBody {
	return modelBody{ID: m.ID, Provider: m.Provider, DisplayName: m.DisplayName, Enabled: m.Enabled, ContextWindow: m.ContextWindow,
		MaxOutputTokens: m.MaxOutputTokens, Version: int64(m.Version), UpdatedAt: w2Time(m.UpdatedAt)}
}

type modelPriceBody struct {
	ModelID              string `json:"modelId"`
	EffectiveFrom        string `json:"effectiveFrom"`
	InputUsdPerMtok      string `json:"inputUsdPerMtok"`
	OutputUsdPerMtok     string `json:"outputUsdPerMtok"`
	CacheReadUsdPerMtok  string `json:"cacheReadUsdPerMtok"`
	CacheWriteUsdPerMtok string `json:"cacheWriteUsdPerMtok"`
	Source               string `json:"source"`
	CreatedAt            string `json:"createdAt"`
	CreatedBy            string `json:"createdBy"`
}

func toModelPriceBody(p domain.ModelPrice) modelPriceBody {
	return modelPriceBody{ModelID: p.ModelID, EffectiveFrom: w2Time(p.EffectiveFrom), InputUsdPerMtok: p.Input.String(),
		OutputUsdPerMtok: p.Output.String(), CacheReadUsdPerMtok: p.CacheRead.String(), CacheWriteUsdPerMtok: p.CacheWrite.String(),
		Source: string(p.Source), CreatedAt: w2Time(p.CreatedAt), CreatedBy: p.CreatedBy}
}

type storageClassBody struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	VolumeType  string `json:"volumeType"`
	Enabled     bool   `json:"enabled"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

func toStorageClassBody(c domain.StorageClass) storageClassBody {
	return storageClassBody{Name: c.Name, Description: c.Description, VolumeType: string(c.VolumeType), Enabled: c.Enabled,
		Version: int64(c.Version), CreatedAt: w2Time(c.CreatedAt), UpdatedAt: w2Time(c.UpdatedAt)}
}

type agentTemplateBody struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Description      string           `json:"description"`
	Harness          string           `json:"harness"`
	DefaultResources domain.Resources `json:"defaultResources"`
	AllowedDigests   []string         `json:"allowedDigests"`
	CurrentDigest    string           `json:"currentDigest"`
	Enabled          bool             `json:"enabled"`
	Capabilities     []string         `json:"capabilities"`
	Version          int64            `json:"version"`
	CreatedAt        string           `json:"createdAt"`
	UpdatedAt        string           `json:"updatedAt"`
}

func toAgentTemplateBody(t domain.AgentTemplate) agentTemplateBody {
	return agentTemplateBody{ID: t.ID, Name: t.Name, Description: t.Description, Harness: t.Harness, DefaultResources: t.DefaultResources,
		AllowedDigests: t.AllowedDigests, CurrentDigest: t.CurrentDigest, Enabled: t.Enabled, Capabilities: nonNilStrings(t.Capabilities), Version: int64(t.Version),
		CreatedAt: w2Time(t.CreatedAt), UpdatedAt: w2Time(t.UpdatedAt)}
}

// extSystemBase holds the fields of an ExternalSystem every view carries.
type extSystemBase struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Description string   `json:"description"`
	DocsURL     string   `json:"docsUrl,omitempty"`
	Endpoints   []string `json:"endpoints"`
	Enabled     bool     `json:"enabled"`
	Builtin     bool     `json:"builtin"`
	Owner       string   `json:"owner,omitempty"`
	Purpose     string   `json:"purpose,omitempty"`
	Scope       string   `json:"scope"`
	Unmetered   bool     `json:"unmetered"`
	Version     int64    `json:"version"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

func toExtSystemBase(s domain.ExternalSystem, enabled bool) extSystemBase {
	scope := string(domain.ExtSystemScopeService)
	if s.TeamDeclared() {
		scope = string(domain.ExtSystemScopeTeam)
	}
	return extSystemBase{ID: s.ID, Slug: s.Slug, Name: s.Name, Category: string(s.Category), Description: s.Description, DocsURL: s.DocsURL,
		Endpoints: s.Endpoints, Enabled: enabled, Builtin: s.Builtin, Owner: s.Owner, Purpose: s.Purpose, Scope: scope, Unmetered: s.Unmetered(),
		Version: int64(s.Version), CreatedAt: w2Time(s.CreatedAt), UpdatedAt: w2Time(s.UpdatedAt)}
}

// catalogSystemBody is a service-level catalog entry as service admins see it: enabled is the
// entry's own switch.
type catalogSystemBody = extSystemBase

func toCatalogSystemBody(s domain.ExternalSystem) catalogSystemBody {
	return toExtSystemBase(s, s.Enabled)
}

// extSystemAccessBody is a system as a team or an agent sees it: enabled is the effective state at
// that level, disabledAt names the level that switched it off, teamState is the team's own state.
type extSystemAccessBody struct {
	extSystemBase
	TeamState  string  `json:"teamState"`
	DisabledAt *string `json:"disabledAt"`
}

func toExtSystemAccessBody(a domain.ExternalSystemAccess) extSystemAccessBody {
	b := extSystemAccessBody{extSystemBase: toExtSystemBase(a.ExternalSystem, a.On()), TeamState: string(domain.ExtSystemTeamStateOn)}
	if !a.TeamOn() {
		b.TeamState = string(domain.ExtSystemTeamStateOff)
	}
	if a.DisabledAt != "" {
		at := string(a.DisabledAt)
		b.DisabledAt = &at
	}
	return b
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
