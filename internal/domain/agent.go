package domain

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// AgentState is the lifecycle state of an agent (agents.state). It is separate from the reconcile
// status: Agent.paused is true in state paused.
type AgentState string

// Agent states.
const (
	AgentEnabled  AgentState = "enabled"
	AgentPaused   AgentState = "paused"
	AgentDeleting AgentState = "deleting"
	AgentDeleted  AgentState = "deleted"
)

// Volume delete behaviours and snapshot schedules (strings, as the volume spec carries them).
const (
	OnDeleteSnapshotThenDelete = string(VolumeOnDeleteSnapshotThenDelete)
	OnDeleteDelete             = string(VolumeOnDeleteDelete)
	OnDeleteRetain             = string(VolumeOnDeleteRetain)
	SnapshotOff                = "off"
	SnapshotDaily              = "daily"
	SnapshotWeekly             = "weekly"
	// DefaultStorageClass is the database default storage class.
	DefaultStorageClass = "gp3"
)

// AgentResources are the pod's CPU and memory requests as Kubernetes quantities.
type AgentResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// AgentVolume is the agent's persistent volume. SizeGiB only grows; StorageClass is set at create.
type AgentVolume struct {
	SizeGiB          int
	StorageClass     string
	OnDelete         string
	SnapshotSchedule string
	SnapshotRetain   int
}

// DefaultAgentVolume is the volume of an agent that names none (the database defaults).
func DefaultAgentVolume() AgentVolume {
	return AgentVolume{SizeGiB: 20, StorageClass: DefaultStorageClass, OnDelete: OnDeleteSnapshotThenDelete,
		SnapshotSchedule: SnapshotDaily, SnapshotRetain: 7}
}

// VolumeSpec is a volume as a request gives it: a zero field is not set.
type VolumeSpec struct {
	SizeGiB          int
	StorageClass     string
	OnDelete         string
	SnapshotSchedule string
	SnapshotRetain   int
}

// Overlay returns v with every set field of s applied.
func (v AgentVolume) Overlay(s VolumeSpec) AgentVolume {
	if s.SizeGiB != 0 {
		v.SizeGiB = s.SizeGiB
	}
	if s.StorageClass != "" {
		v.StorageClass = s.StorageClass
	}
	if s.OnDelete != "" {
		v.OnDelete = s.OnDelete
	}
	if s.SnapshotSchedule != "" {
		v.SnapshotSchedule = s.SnapshotSchedule
	}
	if s.SnapshotRetain != 0 {
		v.SnapshotRetain = s.SnapshotRetain
	}
	return v
}

// Validate checks the volume's ranges.
func (v AgentVolume) Validate() error {
	var fe []FieldError
	if v.SizeGiB < 1 || v.SizeGiB > 1000 {
		fe = append(fe, FieldError{Field: "volume.sizeGiB", Reason: "must be 1 to 1000"})
	}
	if !storageClassRE.MatchString(v.StorageClass) {
		fe = append(fe, FieldError{Field: "volume.storageClass", Reason: "must be a storage class name"})
	}
	switch v.OnDelete {
	case OnDeleteSnapshotThenDelete, OnDeleteDelete, OnDeleteRetain:
	default:
		fe = append(fe, FieldError{Field: "volume.onDelete", Reason: "must be snapshot_then_delete, delete or retain"})
	}
	switch v.SnapshotSchedule {
	case SnapshotOff, SnapshotDaily, SnapshotWeekly:
	default:
		fe = append(fe, FieldError{Field: "volume.snapshotSchedule", Reason: "must be off, daily or weekly"})
	}
	if v.SnapshotRetain < 1 || v.SnapshotRetain > 90 {
		fe = append(fe, FieldError{Field: "volume.snapshotRetain", Reason: "must be 1 to 90"})
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

// SecretBinding delivers a secret to an agent as an environment variable or a file.
type SecretBinding struct {
	SecretID string
	Delivery string // env or file
	Target   string // variable name or absolute path
}

var (
	storageClassRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envTargetRE    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	fileTargetRE   = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
	templateRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
)

// ValidateBindings checks the shape of secret bindings: ids, delivery, targets, no repeated target.
func ValidateBindings(bs []SecretBinding) error {
	var fe []FieldError
	seen := map[string]bool{}
	for i, b := range bs {
		f := "secretBindings[" + itoa(i) + "]."
		if _, err := ParseID(PrefixSecret, b.SecretID); err != nil {
			fe = append(fe, FieldError{Field: f + "secretId", Reason: "must be a sec_ identifier"})
		}
		switch {
		case b.Delivery == "env" && !envTargetRE.MatchString(b.Target):
			fe = append(fe, FieldError{Field: f + "target", Reason: "an env target must be UPPER_SNAKE (^[A-Z_][A-Z0-9_]{0,127}$)"})
		case b.Delivery == "file" && !fileTargetRE.MatchString(b.Target):
			fe = append(fe, FieldError{Field: f + "target", Reason: "a file target must be an absolute path"})
		case b.Delivery != "env" && b.Delivery != "file":
			fe = append(fe, FieldError{Field: f + "delivery", Reason: "must be env or file"})
		}
		if seen[b.Target] {
			fe = append(fe, FieldError{Field: f + "target", Reason: "is bound twice"})
		}
		seen[b.Target] = true
	}
	if len(fe) > 0 {
		return Validation(fe...)
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func (r AgentResources) validate() []FieldError {
	var fe []FieldError
	if r.CPU != "" && !cpuQuotaRE.MatchString(r.CPU) {
		fe = append(fe, FieldError{Field: "resources.cpu", Reason: "must be a Kubernetes CPU quantity such as 1 or 500m"})
	}
	if r.Memory != "" && !storageQuotaRE.MatchString(r.Memory) {
		fe = append(fe, FieldError{Field: "resources.memory", Reason: "must be a Kubernetes quantity such as 2Gi"})
	}
	return fe
}

// Agent is a desired-state agent. It is one pod: it has no replica count (RA-US-4.6).
type Agent struct {
	ID          string
	TeamID      string
	Name        Name
	Description string
	Template    string
	Image       ImageRef
	ModelID     string
	Resources   AgentResources
	IAMIdentity IAMIdentity
	IAMRoleARN  string
	Volume      AgentVolume
	Bindings    []SecretBinding
	State       AgentState
	Version     Version
	Generation  Generation
	Status      Status
	CreatedAt   time.Time
	CreatedBy   string
	UpdatedAt   time.Time
}

// Paused reports whether the agent is paused.
func (a Agent) Paused() bool { return a.State == AgentPaused }

// NewAgentParams is a CreateAgent body.
type NewAgentParams struct {
	Name        string
	Description string
	Template    string
	Image       string
	ModelID     string
	Resources   AgentResources
	IAMIdentity IAMIdentity
	Volume      VolumeSpec
	Bindings    []SecretBinding
}

// NewAgent validates p and returns the agent in state enabled, generation 1.
func NewAgent(id, teamID string, p NewAgentParams, now time.Time) (Agent, error) {
	var fe []FieldError
	collect := func(err error) { fe = append(fe, fieldErrorsOf(err)...) }
	name, err := ParseName("name", p.Name)
	collect(err)
	image, err := ParseImageRef("image", p.Image)
	collect(err)
	if !templateRE.MatchString(p.Template) {
		fe = append(fe, FieldError{Field: "template", Reason: "must be a template name (^[a-z][a-z0-9-]{1,62}$)"})
	}
	if strings.TrimSpace(p.ModelID) == "" {
		fe = append(fe, FieldError{Field: "modelId", Reason: "is required"})
	}
	if utf8.RuneCountInString(p.Description) > 1000 {
		fe = append(fe, FieldError{Field: "description", Reason: "must be at most 1000 characters"})
	}
	fe = append(fe, p.Resources.validate()...)
	iam := p.IAMIdentity
	if iam == "" {
		iam = IAMIdentityTeamShared
	}
	if !iam.Valid() {
		fe = append(fe, FieldError{Field: "iamIdentity", Reason: "must be team_shared or dedicated"})
	}
	vol := DefaultAgentVolume().Overlay(p.Volume)
	collect(vol.Validate())
	collect(ValidateBindings(p.Bindings))
	if len(fe) > 0 {
		return Agent{}, Validation(fe...)
	}
	return Agent{ID: id, TeamID: teamID, Name: name, Description: p.Description, Template: p.Template, Image: image,
		ModelID: p.ModelID, Resources: p.Resources, IAMIdentity: iam, Volume: vol, Bindings: p.Bindings,
		State: AgentEnabled, Version: 1, Generation: 1, Status: PendingStatus(now), CreatedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

// AgentPatch is an UpdateAgent body: a nil or zero field is left unchanged. Bindings, when not
// nil, replace the agent's bindings.
type AgentPatch struct {
	Description *string
	Template    *string
	Image       *string
	ModelID     *string
	Resources   *AgentResources
	IAMIdentity *IAMIdentity
	Volume      *VolumeSpec
	Bindings    *[]SecretBinding
}

// Apply returns a with the patch applied and the generation raised; the volume may grow but not
// shrink and its storage class cannot change (RA-US-4.2).
func (a Agent) Apply(p AgentPatch, now time.Time) (Agent, error) {
	var fe []FieldError
	collect := func(err error) { fe = append(fe, fieldErrorsOf(err)...) }
	if a.State == AgentDeleting || a.State == AgentDeleted {
		return a, Conflict("invalid_state", "an agent in state "+string(a.State)+" cannot be changed", nil)
	}
	if p.Description != nil {
		if utf8.RuneCountInString(*p.Description) > 1000 {
			fe = append(fe, FieldError{Field: "description", Reason: "must be at most 1000 characters"})
		}
		a.Description = *p.Description
	}
	if p.Template != nil {
		if !templateRE.MatchString(*p.Template) {
			fe = append(fe, FieldError{Field: "template", Reason: "must be a template name"})
		}
		a.Template = *p.Template
	}
	if p.Image != nil {
		img, err := ParseImageRef("image", *p.Image)
		collect(err)
		a.Image = img
	}
	if p.ModelID != nil {
		if strings.TrimSpace(*p.ModelID) == "" {
			fe = append(fe, FieldError{Field: "modelId", Reason: "must not be empty"})
		}
		a.ModelID = *p.ModelID
	}
	if p.Resources != nil {
		fe = append(fe, p.Resources.validate()...)
		a.Resources = *p.Resources
	}
	if p.IAMIdentity != nil {
		if !p.IAMIdentity.Valid() {
			fe = append(fe, FieldError{Field: "iamIdentity", Reason: "must be team_shared or dedicated"})
		}
		a.IAMIdentity = *p.IAMIdentity
	}
	if p.Volume != nil {
		switch {
		case p.Volume.SizeGiB != 0 && p.Volume.SizeGiB < a.Volume.SizeGiB:
			fe = append(fe, FieldError{Field: "volume.sizeGiB", Reason: "a volume can grow but not shrink (now " + itoa(a.Volume.SizeGiB) + " GiB)"})
		case p.Volume.StorageClass != "" && p.Volume.StorageClass != a.Volume.StorageClass:
			fe = append(fe, FieldError{Field: "volume.storageClass", Reason: "cannot change after the agent is created"})
		}
		a.Volume = a.Volume.Overlay(*p.Volume)
		collect(a.Volume.Validate())
	}
	if p.Bindings != nil {
		collect(ValidateBindings(*p.Bindings))
		a.Bindings = *p.Bindings
	}
	if len(fe) > 0 {
		return a, Validation(fe...)
	}
	a.Generation++
	a.UpdatedAt = now.UTC()
	return a, nil
}

func (a Agent) transition(to AgentState, now time.Time) Agent {
	a.State = to
	a.Generation++
	a.UpdatedAt = now.UTC()
	return a
}

// Pause returns the agent paused: the pod scales to zero and the volume stays. Pausing a paused
// agent is a no-op (changed is false).
func (a Agent) Pause(now time.Time) (out Agent, changed bool, err error) {
	switch a.State {
	case AgentPaused:
		return a, false, nil
	case AgentEnabled:
		return a.transition(AgentPaused, now), true, nil
	}
	return a, false, Conflict("invalid_state", "an agent in state "+string(a.State)+" cannot be paused", nil)
}

// Resume returns the agent enabled again on the same volume; resuming a running agent is a no-op.
func (a Agent) Resume(now time.Time) (out Agent, changed bool, err error) {
	switch a.State {
	case AgentEnabled:
		return a, false, nil
	case AgentPaused:
		return a.transition(AgentEnabled, now), true, nil
	}
	return a, false, Conflict("invalid_state", "an agent in state "+string(a.State)+" cannot be resumed", nil)
}

// Restart asks for a fresh pod: the generation rises, which the reconciler turns into a new
// restart nonce on the pod template. Only a running agent restarts.
func (a Agent) Restart(now time.Time) (Agent, error) {
	if a.State != AgentEnabled {
		return a, Conflict("invalid_state", "an agent in state "+string(a.State)+" cannot be restarted; resume it first", nil)
	}
	a.Generation++
	a.UpdatedAt = now.UTC()
	return a, nil
}

// MarkDeleting returns the agent in state deleting with status Deleting; the reconciler runs the
// delete steps and finalizes it. Deleting a deleting agent is a no-op.
func (a Agent) MarkDeleting(now time.Time) (out Agent, changed bool, err error) {
	switch a.State {
	case AgentDeleting:
		return a.WithDeletingStatus(), false, nil
	case AgentDeleted:
		return a, false, NotFound("agent")
	}
	n := a.transition(AgentDeleting, now)
	return n.WithDeletingStatus(), true, nil
}

// WithDeletingStatus shows phase Deleting while the agent is deleting, because the reconciler may
// not have written the status yet.
func (a Agent) WithDeletingStatus() Agent {
	if a.State == AgentDeleting && a.Status.Phase != PhaseDeleting {
		a.Status.Phase = PhaseDeleting
	}
	return a
}

// Snapshot is one entry of an agent volume's snapshot history.
type Snapshot struct {
	ID          string
	AgentID     string
	Reason      string
	State       string
	SizeGiB     int
	RequestedAt time.Time
	RequestedBy string
	CompletedAt *time.Time
}
