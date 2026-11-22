// Package agent reconciles an agent's workload (RC-2, RC-16, RC-17 agent part): the Agentic Teammates
// custom resource and everything the pod needs (ServiceAccount, Pod Identity association, volume, ConfigMap,
// ExternalSecrets, AWS config, tags, gateway environment), the agent's IAM identity, the volume and the
// pause, resume and restart lifecycle. It computes the desired state from the agent row and calls ports;
// the adapters render and apply.
package agent

import (
	"context"
	"time"
)

// Agent states (agents.state).
const (
	StateEnabled  = "enabled"
	StatePaused   = "paused"
	StateDeleting = "deleting"
	StateDeleted  = "deleted"
)

// IAM identities (agents.iam_identity).
const (
	IdentityTeamShared = "team_shared"
	IdentityDedicated  = "dedicated"
)

// Delivery of a secret binding.
const (
	DeliveryEnv  = "env"
	DeliveryFile = "file"
)

// Isolation tiers of a team.
const (
	TierShared    = "shared"
	TierDedicated = "dedicated"
)

// Resources are the container's requests and limits (agents.resources).
type Resources struct {
	CPU    string
	Memory string
}

// Volume is the agent's persistent volume and its backup settings (RC-16).
type Volume struct {
	SizeGiB      int
	StorageClass string
	// OnDelete is snapshot_then_delete, retain or delete.
	OnDelete string
	// SnapshotSchedule is off, daily or weekly.
	SnapshotSchedule string
	SnapshotRetain   int
}

// Binding is one secret delivered to the pod. The value is never here: only the reference.
type Binding struct {
	SecretID   string
	SecretName string
	// SecretARN is the Secrets Manager ARN the ExternalSecret reads.
	SecretARN string
	// VersionID is the secret's current version; a rotation changes it, and with it the pod-template hash.
	VersionID string
	// Delivery is env or file; Target is the variable name or the absolute path.
	Delivery string
	Target   string
	// ExternalSystemID is the external system the secret is a credential for, empty when it names none.
	ExternalSystemID string
	// SystemOff is true when that system is switched off for this agent at service, team or agent level:
	// the secret is then not delivered (EXT-7).
	SystemOff bool
}

// Delivered reports whether the binding reaches the pod.
func (b Binding) Delivered() bool { return b.ExternalSystemID == "" || !b.SystemOff }

// ConfigValue is one resolved, non-secret configuration value.
type ConfigValue struct {
	Name  string
	Value string
}

// RoleLink is a cross-account role the agent may assume; it becomes a profile in /etc/afs/aws/config.
type RoleLink struct {
	ProfileName       string
	RoleARN           string
	ExternalID        string
	MaxSessionSeconds int
	// Status is pending_trust, verified, failed or disabled; a disabled link gets no profile.
	Status string
}

// Team is what the agent needs to know of its team.
type Team struct {
	ID        string
	Slug      string
	Namespace string
	// State is active, suspended, deleting or deleted.
	State string
	// IsolationTier is shared or dedicated.
	IsolationTier string
	// WorkOrderAssigned is true when the team has an assigned work order, which is when the afs-api token is
	// projected (D29).
	WorkOrderAssigned bool
}

// Suspended reports whether the team's workloads must be at zero replicas.
func (t Team) Suspended() bool { return t.State == "suspended" }

// Desired is the agent row with everything the pod is built from, as the reconciler reads it.
type Desired struct {
	ID          string
	Name        string
	Description string
	Template    string
	Image       string
	Persona     string
	ModelID     string
	// State is enabled, paused, deleting or deleted.
	State      string
	Generation int64
	Resources  Resources
	// IAMIdentity is team_shared or dedicated.
	IAMIdentity string
	Volume      Volume
	// RestartNonce changes on every RestartAgent; it is a pod-template annotation, so the pod rolls once.
	RestartNonce string
	Team         Team
	Bindings     []Binding
	Config       []ConfigValue
	RoleLinks    []RoleLink
	// Tags are the agent's effective tags (afs-team-id and afs-agent-id always present).
	Tags map[string]string
	// TemplateRetired is true when the agent's template is disabled or its image digest is no longer
	// allowed (D54); the agent keeps running.
	TemplateRetired bool
	// TemplateReason says why, for the condition message.
	TemplateReason string
}

// Replicas is the replica count the pod must have: 1, or 0 when the agent is paused or the team is
// suspended.
func (d Desired) Replicas() int32 {
	if d.State == StatePaused || d.Team.Suspended() {
		return 0
	}
	return 1
}

// Source reads the agents the reconciler owns.
type Source interface {
	// LoadAgent returns the agent, or a domain NotFound error when it does not exist.
	LoadAgent(ctx context.Context, id string) (Desired, error)
	// ListTeamAgents returns the ids of the team's agents that are not deleted, for team-level hooks.
	ListTeamAgents(ctx context.Context, teamID string) ([]Listed, error)
}

// Listed is one agent of a team in a listing.
type Listed struct {
	ID    string
	Name  string
	State string
	// Namespace is the team's namespace.
	Namespace string
}

// Gateway locates afs-gateway (gateway contract §2).
type Gateway struct {
	Namespace string
	Name      string
	Port      int
}

// URL is AFS_GATEWAY_URL.
func (g Gateway) URL() string {
	return "https://" + g.Name + "." + g.Namespace + ".svc:" + itoa(g.Port)
}

// Environment is everything the cluster adapter renders for one agent.
type Environment struct {
	Agent Desired
	// RoleARN is the role the pod assumes through Pod Identity: the team role, or the agent's own.
	RoleARN string
	Region  string
	Gateway Gateway
	// Replicas is Agent.Replicas().
	Replicas int32
	// Hash is the pod-template hash of the referenced versions (PodTemplateHash).
	Hash string
	// NodePool describes the team's dedicated nodes; zero for the shared tier.
	NodePool NodePool
}

// NodePool is the team's dedicated Karpenter NodePool (TEAM-6).
type NodePool struct {
	// Dedicated is true for teams of the dedicated isolation tier.
	Dedicated bool
	// Tags are the team's tags for the nodes' AWS resources (the agent's own id is left out: the nodes
	// are the team's).
	Tags map[string]string
}

// ApplyResult lists the objects an apply created or changed, by engine.ObjectName.
type ApplyResult struct {
	Changed []string
}

// VolumeState is what the cluster says of the agent's volume.
type VolumeState struct {
	// Exists is false when the PVC is not there (a new agent whose pod did not start yet, or a rebuilt
	// cluster).
	Exists bool
	// Bound is true when the claim is bound to a volume.
	Bound bool
	// RequestedGiB is the size the claim asks for; CapacityGiB the size of the bound volume (0 unknown).
	RequestedGiB int
	CapacityGiB  int
	// Resizing is true while the volume is being expanded.
	Resizing bool
}

// Cluster applies an agent's Kubernetes objects. Implementations are idempotent.
type Cluster interface {
	// ApplyAgent applies the Agentic Teammates resource, the ServiceAccount, the ConfigMaps, the
	// ExternalSecrets and, for a dedicated team, the NodePool, and removes the agent's objects that are no
	// longer rendered (an ExternalSecret without bindings).
	ApplyAgent(ctx context.Context, env Environment) (ApplyResult, error)
	// DeleteAgent removes every object of the agent in the namespace: the resource first, then the rest.
	// Volumes are not touched: the on-delete rule decides (lifecycle).
	DeleteAgent(ctx context.Context, d Desired) error
}

// Volumes manages the agent's claim (RC-16).
type Volumes interface {
	// Volume reports the claim.
	Volume(ctx context.Context, d Desired) (VolumeState, error)
	// Expand patches the existing claim to d.Volume.SizeGiB when it is smaller (online expansion). The
	// claim is never shrunk.
	Expand(ctx context.Context, d Desired) (changed bool, err error)
	// Restore re-creates the missing claim from the EBS snapshot ebsSnapshotID.
	Restore(ctx context.Context, d Desired, ebsSnapshotID string) error
}

// Snapshots finds the snapshot a missing volume is restored from.
type Snapshots interface {
	// LatestReady returns the EBS snapshot id of the agent's newest completed snapshot.
	LatestReady(ctx context.Context, agentID string) (ebsSnapshotID string, ok bool, err error)
}

// PodIdentities manages the EKS Pod Identity association of an agent's ServiceAccount.
type PodIdentities interface {
	// Ensure makes the ServiceAccount of the namespace map to roleARN.
	Ensure(ctx context.Context, namespace, serviceAccount, roleARN string, tags map[string]string) error
	// Delete removes the association; a missing one is success.
	Delete(ctx context.Context, namespace, serviceAccount string) error
}

// Clock is the time source of scheduled work.
type Clock func() time.Time
