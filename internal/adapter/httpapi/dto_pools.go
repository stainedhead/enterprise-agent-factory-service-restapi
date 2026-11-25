package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
)

// stageBody is the wire shape of Stage (API contract §4.4a).
type stageBody struct {
	ID          string `json:"id"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"createdAt"`
	CreatedBy   string `json:"createdBy"`
	UpdatedAt   string `json:"updatedAt"`
	Name        string `json:"name"`
	Position    int    `json:"position"`
	Description string `json:"description"`
}

func toStageBody(s domain.Stage) stageBody {
	return stageBody{ID: s.ID, Version: int64(s.Version), CreatedAt: w2Time(s.CreatedAt), CreatedBy: s.CreatedBy,
		UpdatedAt: w2Time(s.UpdatedAt), Name: string(s.Name), Position: s.Position, Description: s.Description}
}

type createStageRequest struct {
	Name        string `json:"name"`
	Position    int    `json:"position"`
	Description string `json:"description"`
}

type updateStageRequest struct {
	Position    *int    `json:"position"`
	Description *string `json:"description"`
}

// poolBody is the wire shape of Pool.
type poolBody struct {
	ID             string                `json:"id"`
	Version        int64                 `json:"version"`
	CreatedAt      string                `json:"createdAt"`
	CreatedBy      string                `json:"createdBy"`
	UpdatedAt      string                `json:"updatedAt"`
	Generation     int64                 `json:"generation"`
	Status         domain.Status         `json:"status"`
	Name           string                `json:"name"`
	Description    string                `json:"description"`
	StageID        *string               `json:"stageId"`
	Template       string                `json:"template"`
	Image          string                `json:"image"`
	Persona        string                `json:"persona"`
	ModelID        string                `json:"modelId"`
	Models         agentModelsBody       `json:"models"`
	Workers        int                   `json:"workers"`
	Resources      domain.AgentResources `json:"resources"`
	ScratchSizeGiB int                   `json:"scratchSizeGiB"`
	SharedVolume   string                `json:"sharedVolume"`
	State          string                `json:"state"`
	Handoffs       []string              `json:"handoffs"`
}

func toPoolBody(p domain.Pool) poolBody {
	b := poolBody{ID: p.ID, Version: int64(p.Version), CreatedAt: w2Time(p.CreatedAt), CreatedBy: p.CreatedBy,
		UpdatedAt: w2Time(p.UpdatedAt), Generation: int64(p.Generation), Status: p.Status, Name: string(p.Name),
		Description: p.Description, Template: p.Template, Image: string(p.Image), Persona: p.Persona, ModelID: p.Models.DefaultModelID, Models: toAgentModelsBody(p.Models),
		Workers: p.Workers, Resources: p.Resources, ScratchSizeGiB: p.ScratchSizeGiB, SharedVolume: p.SharedVolume,
		State: string(p.State), Handoffs: append([]string{}, p.Handoffs...)}
	if p.StageID != "" {
		s := p.StageID
		b.StageID = &s
	}
	if b.Status.Conditions == nil {
		b.Status.Conditions = []domain.Condition{}
	}
	return b
}

type createPoolRequest struct {
	Name           string                `json:"name"`
	Description    string                `json:"description"`
	StageID        *string               `json:"stageId"`
	Template       string                `json:"template"`
	Image          string                `json:"image"`
	Persona        string                `json:"persona"`
	ModelID        string                `json:"modelId"`
	Models         *modelsRequest        `json:"models"`
	Workers        *int                  `json:"workers"`
	Resources      domain.AgentResources `json:"resources"`
	ScratchSizeGiB int                   `json:"scratchSizeGiB"`
	SharedVolume   string                `json:"sharedVolume"`
	Mcp            *mcpRequest           `json:"mcp"`
}

type updatePoolRequest struct {
	Description    *string                `json:"description"`
	StageID        w2Nullable[string]     `json:"stageId"`
	Template       *string                `json:"template"`
	Image          *string                `json:"image"`
	Persona        *string                `json:"persona"`
	ModelID        *string                `json:"modelId"`
	Models         *modelsRequest         `json:"models"`
	Workers        *int                   `json:"workers"`
	Resources      *domain.AgentResources `json:"resources"`
	ScratchSizeGiB *int                   `json:"scratchSizeGiB"`
	SharedVolume   *string                `json:"sharedVolume"`
	Mcp            *mcpRequest            `json:"mcp"`
}

func (u updatePoolRequest) patch() domain.PoolPatch {
	p := domain.PoolPatch{Description: u.Description, Template: u.Template, Image: u.Image, Persona: u.Persona,
		ModelID: u.ModelID, Models: u.Models.input(), Workers: u.Workers, Resources: u.Resources, ScratchSizeGiB: u.ScratchSizeGiB, SharedVolume: u.SharedVolume}
	if u.StageID.Set {
		p.StageID.Set = true
		if !u.StageID.Null {
			v := u.StageID.Value
			p.StageID.Value = &v
		}
	}
	return p
}

type poolStatusBody struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	Generation int64         `json:"generation"`
	Status     domain.Status `json:"status"`
	Workers    struct {
		Desired   int `json:"desired"`
		Reporting int `json:"reporting"`
		Working   int `json:"working"`
		Idle      int `json:"idle"`
		Impaired  int `json:"impaired"`
	} `json:"workers"`
}

func toPoolStatusBody(s pools.Status) poolStatusBody {
	b := poolStatusBody{ID: s.Pool.ID, Kind: pools.StatusKind, Generation: int64(s.Pool.Generation), Status: s.Pool.Status}
	if b.Status.Conditions == nil {
		b.Status.Conditions = []domain.Condition{}
	}
	b.Workers.Desired, b.Workers.Reporting, b.Workers.Working = s.Workers.Desired, s.Workers.Reporting, s.Workers.Working
	b.Workers.Idle, b.Workers.Impaired = s.Workers.Idle, s.Workers.Impaired
	return b
}

type poolHandoffsBody struct {
	ToPoolIDs []string `json:"toPoolIds"`
	Version   int64    `json:"version"`
}

func toPoolHandoffsBody(h pools.Handoffs) poolHandoffsBody {
	return poolHandoffsBody{ToPoolIDs: append([]string{}, h.ToPoolIDs...), Version: int64(h.Version)}
}

type poolBindingsBody struct {
	Items   []secretBindingBody `json:"items"`
	Version int64               `json:"version"`
}

func toPoolBindingsBody(s pools.BindingSet) poolBindingsBody {
	b := poolBindingsBody{Items: make([]secretBindingBody, 0, len(s.Items)), Version: int64(s.Version)}
	for _, x := range s.Items {
		b.Items = append(b.Items, secretBindingBody{SecretID: x.SecretID, Delivery: x.Delivery, Target: x.Target})
	}
	return b
}

func toPoolConfigBody(v domain.PoolConfigValue) agentConfigBody {
	return agentConfigBody{Name: string(v.Name), Source: string(v.Source), Value: v.Value, ResolvedValue: v.ResolvedValue,
		Version: int64(v.Version), UpdatedAt: w2Time(v.UpdatedAt)}
}

type poolRoleLinkBody struct {
	ID                string  `json:"id"`
	Version           int64   `json:"version"`
	CreatedAt         string  `json:"createdAt"`
	CreatedBy         string  `json:"createdBy"`
	UpdatedAt         string  `json:"updatedAt"`
	RoleName          string  `json:"roleName"`
	AccountID         string  `json:"accountId"`
	ProfileName       string  `json:"profileName"`
	RoleArn           string  `json:"roleArn"`
	ExternalID        string  `json:"externalId"`
	MaxSessionSeconds int     `json:"maxSessionSeconds"`
	Status            string  `json:"status"`
	LastVerifiedAt    *string `json:"lastVerifiedAt"`
	LastError         *string `json:"lastError"`
}

func toPoolRoleLinkBody(l domain.PoolRoleLink) poolRoleLinkBody {
	var lastErr *string
	if l.LastError != "" {
		e := l.LastError
		lastErr = &e
	}
	return poolRoleLinkBody{ID: l.ID, Version: int64(l.Version), CreatedAt: w2Time(l.CreatedAt), CreatedBy: l.CreatedBy,
		UpdatedAt: w2Time(l.UpdatedAt), RoleName: l.RoleName, AccountID: l.AccountID, ProfileName: l.ProfileName,
		RoleArn: l.RoleARN, ExternalID: l.ExternalID, MaxSessionSeconds: l.MaxSessionSeconds, Status: string(l.Status),
		LastVerifiedAt: w2TimePtr(l.LastVerifiedAt), LastError: lastErr}
}
