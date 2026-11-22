package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// agentBody is the wire shape of Agent (API contract §4.4). It has no replica count.
type agentBody struct {
	ID          string                `json:"id"`
	Version     int64                 `json:"version"`
	Generation  int64                 `json:"generation"`
	CreatedAt   string                `json:"createdAt"`
	CreatedBy   string                `json:"createdBy"`
	UpdatedAt   string                `json:"updatedAt"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Template    string                `json:"template"`
	Image       string                `json:"image"`
	ModelID     string                `json:"modelId"`
	Resources   domain.AgentResources `json:"resources"`
	IAMIdentity string                `json:"iamIdentity"`
	IAMRoleArn  string                `json:"iamRoleArn,omitempty"`
	Paused      bool                  `json:"paused"`
	Volume      agentVolumeBody       `json:"volume"`
	Bindings    []secretBindingBody   `json:"secretBindings"`
	Status      domain.Status         `json:"status"`
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

func toAgentBody(a domain.Agent) agentBody {
	b := agentBody{ID: a.ID, Version: int64(a.Version), Generation: int64(a.Generation), CreatedAt: w2Time(a.CreatedAt),
		CreatedBy: a.CreatedBy, UpdatedAt: w2Time(a.UpdatedAt), Name: string(a.Name), Description: a.Description,
		Template: a.Template, Image: string(a.Image), ModelID: a.ModelID, Resources: a.Resources,
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
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Template    string                `json:"template"`
	Image       string                `json:"image"`
	ModelID     string                `json:"modelId"`
	Resources   domain.AgentResources `json:"resources"`
	IAMIdentity string                `json:"iamIdentity"`
	Volume      *volumeRequest        `json:"volume"`
	SecretBinds []bindingRequest      `json:"secretBindings"`
}

type updateAgentRequest struct {
	Description *string                `json:"description"`
	Template    *string                `json:"template"`
	Image       *string                `json:"image"`
	ModelID     *string                `json:"modelId"`
	Resources   *domain.AgentResources `json:"resources"`
	IAMIdentity *string                `json:"iamIdentity"`
	Volume      *volumeRequest         `json:"volume"`
	SecretBinds *[]bindingRequest      `json:"secretBindings"`
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
