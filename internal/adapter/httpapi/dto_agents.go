package httpapi

import (
	"encoding/json"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
)

// agentBody is the wire shape of Agent (API contract §4.4). It has no replica count.
type agentBody struct {
	ID            string                  `json:"id"`
	Version       int64                   `json:"version"`
	Generation    int64                   `json:"generation"`
	CreatedAt     string                  `json:"createdAt"`
	CreatedBy     string                  `json:"createdBy"`
	UpdatedAt     string                  `json:"updatedAt"`
	Name          string                  `json:"name"`
	Description   string                  `json:"description"`
	Template      *string                 `json:"template"`
	Image         *string                 `json:"image"`
	ModelID       string                  `json:"modelId"`
	Models        agentModelsBody         `json:"models"`
	Runtime       string                  `json:"runtime,omitempty"`
	Resources     domain.AgentResources   `json:"resources"`
	IAMIdentity   string                  `json:"iamIdentity"`
	IAMRoleArn    string                  `json:"iamRoleArn,omitempty"`
	Paused        bool                    `json:"paused"`
	Volume        agentVolumeBody         `json:"volume"`
	Bindings      []secretBindingBody     `json:"secretBindings"`
	Status        domain.Status           `json:"status"`
	Role          string                  `json:"role"`
	Collaboration *agentCollaborationBody `json:"collaboration,omitempty"`
}

// agentModelsBody is Agent.models and Pool.models (API contract §4.4 Models).
type agentModelsBody struct {
	Access            string   `json:"access"`
	ModelIDs          []string `json:"modelIds"`
	DefaultModelID    string   `json:"defaultModelId"`
	EffectiveModelIDs []string `json:"effectiveModelIds"`
}

func toAgentModelsBody(m domain.AgentModels) agentModelsBody {
	b := agentModelsBody{Access: string(m.Access), ModelIDs: m.ModelIDs, DefaultModelID: m.DefaultModelID, EffectiveModelIDs: m.Effective}
	if b.ModelIDs == nil {
		b.ModelIDs = []string{}
	}
	if b.EffectiveModelIDs == nil {
		b.EffectiveModelIDs = []string{}
	}
	return b
}

// modelsRequest is the models object of an agent or pool create or update body; a nil member is omitted.
type modelsRequest struct {
	Access         *string         `json:"access"`
	ModelIDs       *[]string       `json:"modelIds"`
	DefaultModelID *string         `json:"defaultModelId"`
	RouteAccess    *string         `json:"routeAccess"`
	Routes         *[]string       `json:"routes"`
	DefaultRoute   json.RawMessage `json:"defaultRoute"`
}

func (q *modelsRequest) input() *domain.ModelsInput {
	if q == nil {
		return nil
	}
	in := &domain.ModelsInput{ModelIDs: q.ModelIDs, DefaultModelID: q.DefaultModelID}
	if q.RouteAccess != nil || q.Routes != nil || q.DefaultRoute != nil {
		r := &domain.RoutesInput{Access: q.RouteAccess, Routes: q.Routes}
		var s *string
		if q.DefaultRoute != nil && json.Unmarshal(q.DefaultRoute, &s) == nil {
			r.DefaultRoute, r.DefaultRouteNull = s, s == nil
		}
		in.Routes = r
	}
	if q.Access != nil {
		a := domain.ModelAccess(*q.Access)
		in.Access = &a
	}
	return in
}

type agentCollaborationBody struct {
	Connect          bool    `json:"connect"`
	PaperclipAgentID *string `json:"paperclipAgentId"`
	LinkState        string  `json:"linkState"`
	LastSyncedAt     *string `json:"lastSyncedAt"`
	LastError        *string `json:"lastError"`
}

// collaborationRequest is the collaboration object of the agent create and update bodies.
type collaborationRequest struct {
	Connect          *bool  `json:"connect"`
	PaperclipAgentID string `json:"paperclipAgentId"`
}

func (q *collaborationRequest) input() *domain.CollaborationInput {
	if q == nil {
		return nil
	}
	return &domain.CollaborationInput{Connect: q.Connect, PaperclipAgentID: q.PaperclipAgentID}
}

type agentVolumeBody struct {
	SizeGiB          int    `json:"sizeGiB"`
	StorageClass     string `json:"storageClass"`
	OnDelete         string `json:"onDelete"`
	SnapshotSchedule string `json:"snapshotSchedule"`
	SnapshotRetain   int    `json:"snapshotRetain"`
}

type secretBindingBody struct {
	SecretID string `json:"secretId"`
	Delivery string `json:"delivery"`
	Target   string `json:"target"`
}

// toAgentBodyFor renders the agent for a caller: on the orchestration agent the template and image are
// null unless the caller is a service admin (CD-32).
func toAgentBodyFor(c acting.Caller, a domain.Agent) agentBody {
	b := toAgentBody(a)
	if a.IsOrchestration() && !c.Principal.ServiceAdmin {
		b.Template, b.Image = nil, nil
	}
	return b
}

func toAgentBody(a domain.Agent) agentBody {
	tpl, img := a.Template, string(a.Image)
	b := agentBody{ID: a.ID, Version: int64(a.Version), Generation: int64(a.Generation), CreatedAt: w2Time(a.CreatedAt),
		CreatedBy: a.CreatedBy, UpdatedAt: w2Time(a.UpdatedAt), Name: string(a.Name), Description: a.Description,
		Template: &tpl, Image: &img, ModelID: a.Models.DefaultModelID, Models: toAgentModelsBody(a.Models), Resources: a.Resources,
		IAMIdentity: string(a.IAMIdentity), IAMRoleArn: a.IAMRoleARN, Paused: a.Paused(),
		Volume: agentVolumeBody{SizeGiB: a.Volume.SizeGiB, StorageClass: a.Volume.StorageClass, OnDelete: a.Volume.OnDelete,
			SnapshotSchedule: a.Volume.SnapshotSchedule, SnapshotRetain: a.Volume.SnapshotRetain},
		Bindings: make([]secretBindingBody, 0, len(a.Bindings)), Status: a.Status}
	for _, x := range a.Bindings {
		b.Bindings = append(b.Bindings, secretBindingBody{SecretID: x.SecretID, Delivery: x.Delivery, Target: x.Target})
	}
	if b.Status.Conditions == nil {
		b.Status.Conditions = []domain.Condition{}
	}
	b.Role = string(domain.AgentRoleWorker)
	if a.Role != "" {
		b.Role = string(a.Role)
	}
	if a.IsOrchestration() {
		b.Runtime = "orchestration"
	}
	if c := a.Collaboration; c != nil && !a.IsOrchestration() {
		cb := &agentCollaborationBody{Connect: c.Connect, LinkState: string(c.LinkState), LastSyncedAt: w2TimePtr(c.LastSyncedAt)}
		if c.PaperclipAgentID != "" {
			cb.PaperclipAgentID = &c.PaperclipAgentID
		}
		if c.LastError != "" {
			cb.LastError = &c.LastError
		}
		b.Collaboration = cb
	}
	return b
}

// volumeRequest is AgentVolume as a request carries it; a nil field is not set.
type volumeRequest struct {
	SizeGiB          *int    `json:"sizeGiB"`
	StorageClass     *string `json:"storageClass"`
	OnDelete         *string `json:"onDelete"`
	SnapshotSchedule *string `json:"snapshotSchedule"`
	SnapshotRetain   *int    `json:"snapshotRetain"`
}

// spec returns the volume the request sets. A zero number is invalid, not "unset".
func (v *volumeRequest) spec() (domain.VolumeSpec, error) {
	var s domain.VolumeSpec
	if v == nil {
		return s, nil
	}
	if v.SizeGiB != nil {
		if *v.SizeGiB < 1 {
			return s, bad("volume.sizeGiB", "must be 1 to 1000")
		}
		s.SizeGiB = *v.SizeGiB
	}
	if v.SnapshotRetain != nil {
		if *v.SnapshotRetain < 1 {
			return s, bad("volume.snapshotRetain", "must be 1 to 90")
		}
		s.SnapshotRetain = *v.SnapshotRetain
	}
	if v.StorageClass != nil {
		s.StorageClass = *v.StorageClass
	}
	if v.OnDelete != nil {
		s.OnDelete = *v.OnDelete
	}
	if v.SnapshotSchedule != nil {
		s.SnapshotSchedule = *v.SnapshotSchedule
	}
	return s, nil
}

type bindingRequest struct {
	SecretID string `json:"secretId"`
	Delivery string `json:"delivery"`
	Target   string `json:"target"`
}

func bindingsOf(in []bindingRequest) []domain.SecretBinding {
	out := make([]domain.SecretBinding, 0, len(in))
	for _, b := range in {
		out = append(out, domain.SecretBinding{SecretID: b.SecretID, Delivery: b.Delivery, Target: b.Target})
	}
	return out
}

type createAgentRequest struct {
	Name          string                `json:"name"`
	Description   string                `json:"description"`
	Template      string                `json:"template"`
	Image         string                `json:"image"`
	ModelID       string                `json:"modelId"`
	Models        *modelsRequest        `json:"models"`
	Resources     domain.AgentResources `json:"resources"`
	IAMIdentity   string                `json:"iamIdentity"`
	Volume        *volumeRequest        `json:"volume"`
	SecretBinds   []bindingRequest      `json:"secretBindings"`
	Role          string                `json:"role"`
	Collaboration *collaborationRequest `json:"collaboration"`
	Mcp           *mcpRequest           `json:"mcp"`
}

type updateAgentRequest struct {
	Name          *string                `json:"name"`
	Description   *string                `json:"description"`
	Template      *string                `json:"template"`
	Image         *string                `json:"image"`
	ModelID       *string                `json:"modelId"`
	Models        *modelsRequest         `json:"models"`
	Resources     *domain.AgentResources `json:"resources"`
	IAMIdentity   *string                `json:"iamIdentity"`
	Volume        *volumeRequest         `json:"volume"`
	SecretBinds   *[]bindingRequest      `json:"secretBindings"`
	Role          *string                `json:"role"`
	Collaboration *collaborationRequest  `json:"collaboration"`
	Mcp           *mcpRequest            `json:"mcp"`
}

type snapshotBody struct {
	ID          string  `json:"id"`
	AgentID     string  `json:"agentId"`
	Reason      string  `json:"reason"`
	State       string  `json:"state"`
	SizeGiB     int     `json:"sizeGiB,omitempty"`
	RequestedAt string  `json:"requestedAt"`
	RequestedBy string  `json:"requestedBy"`
	CompletedAt *string `json:"completedAt"`
}

func toSnapshotBody(s domain.Snapshot) snapshotBody {
	return snapshotBody{ID: s.ID, AgentID: s.AgentID, Reason: s.Reason, State: s.State, SizeGiB: s.SizeGiB,
		RequestedAt: w2Time(s.RequestedAt), RequestedBy: s.RequestedBy, CompletedAt: w2TimePtr(s.CompletedAt)}
}
