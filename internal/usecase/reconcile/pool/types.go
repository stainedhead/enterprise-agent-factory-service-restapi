// Package pool reconciles a swarm pool's workload (RC-11, RC-17 pool part): one Deployment with `workers`
// replicas that share one identity (ServiceAccount, Pod Identity role), the pool's secrets, configuration,
// AWS profiles and egress policy, the inbox queue of a pool that receives handoffs (D22, D32) and the
// verification of the pool's shared cross-account role links. It computes the desired state from the pool
// row and calls ports; the adapters render and apply.
//
// A pool has no persistent volume: every worker has a scratch volume that goes with the pod, and a pool
// may share one EFS volume. Scaling changes the replica count and nothing else, so no pod rolls when the
// worker count changes (the pod-template hash leaves the count out).
package pool

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

// Pool states (agent_pools.state).
const (
	StateEnabled  = "enabled"
	StatePaused   = "paused"
	StateDeleting = "deleting"
	StateDeleted  = "deleted"
)

// Shared volume choices (agent_pools.shared_volume).
const (
	SharedNone = "none"
	SharedEFS  = "efs"
)

// Conditions the pool reports besides the contract's (the list is open, API contract §1.1).
const (
	// CondWorkersReady is True when every desired worker is ready.
	CondWorkersReady = "WorkersReady"
	// CondInboxReady reports the pool's inbox queue.
	CondInboxReady = "InboxReady"
	// CondRoleLinksVerified reports the pool's shared cross-account role links.
	CondRoleLinksVerified = "RoleLinksVerified"
)

// WorkerPollInterval is how soon a pool whose workers are not all ready is looked at again.
const WorkerPollInterval = 10 * time.Second

// Peer is another pool of the team that a handoff names.
type Peer struct {
	ID   string
	Name string
}

// RoleLink is one cross-account link of the pool's shared role: the same role name in every listed
// account, one link per account (RC-US-9.3).
type RoleLink struct {
	ID          string
	AccountID   string
	RoleARN     string
	ProfileName string
	ExternalID  string
	// MaxSessionSeconds is the link's longest session.
	MaxSessionSeconds int
	Status            domain.RoleLinkStatus
	LastVerified      *time.Time
	// UpdatedAt is when the row last changed; for a failed link it is when it failed.
	UpdatedAt time.Time
	// Allowed is afs.account_allowed(team, account).
	Allowed bool
}

// Desired is the pool row with everything its workers are built from, as the reconciler reads it.
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
	// Workers is the desired worker count, 0 to 200.
	Workers   int32
	Resources agent.Resources
	// ScratchGiB is the size of each worker's scratch volume, 1 to 500.
	ScratchGiB int
	// SharedVolume is none or efs.
	SharedVolume string
	Team         agent.Team
	Bindings     []agent.Binding
	Config       []agent.ConfigValue
	RoleLinks    []RoleLink
	// Tags are the pool's effective tags (afs-team-id and afs-pool-id always present).
	Tags map[string]string
	// Systems are the external systems of the team's catalog as this pool sees them; a system that is
	// off contributes no endpoint to the egress policy.
	Systems []egress.System
	// SendsTo are the pools this pool hands work to; ReceivesFrom the pools that hand work to it.
	SendsTo      []Peer
	ReceivesFrom []Peer
	// TemplateRetired is true when the pool's template is disabled or its image digest is no longer
	// allowed (D54); the pool keeps running.
	TemplateRetired bool
	TemplateReason  string
}

// Replicas is the replica count the Deployment must have: the worker count, or 0 when the pool is paused or
// the team is suspended. The count is the one in the row, so a resume (and a team resume) restores exactly
// what was there before.
func (d Desired) Replicas() int32 {
	if d.State == StatePaused || d.Team.Suspended() {
		return 0
	}
	return d.Workers
}

// Receives reports whether another pool hands work to this one, which is when it has an inbox queue.
func (d Desired) Receives() bool { return len(d.ReceivesFrom) > 0 }

// Source reads the pools the reconciler owns.
type Source interface {
	// LoadPool returns the pool, or a domain NotFound error when it does not exist.
	LoadPool(ctx context.Context, id string) (Desired, error)
	// ListTeamPools returns the team's pools that are not deleted, for the team's delete finalizer.
	ListTeamPools(ctx context.Context, teamID string) ([]Listed, error)
}

// Listed is one pool of a team in a listing.
type Listed struct {
	ID    string
	Name  string
	State string
	// Namespace is the team's namespace.
	Namespace string
}

// Environment is everything the cluster adapter renders for one pool.
type Environment struct {
	Pool Desired
	// RoleARN is the pool's own role, which its pods assume through Pod Identity.
	RoleARN string
	Region  string
	Gateway agent.Gateway
	// Replicas is Pool.Replicas().
	Replicas int32
	// Hash is the pod-template hash of the referenced versions (PodTemplateHash).
	Hash string
	// InboxURL is the URL of the pool's inbox queue, empty when no pool hands work to it.
	InboxURL string
	// HandoffURLs maps each receiving pool's name to its inbox queue URL, for the pools this one sends to.
	HandoffURLs map[string]string
	// NodePool describes the team's dedicated nodes; zero for the shared tier.
	NodePool agent.NodePool
	// EFSStorageClass is the storage class of the shared EFS volume (platform setting).
	EFSStorageClass string
	// Egress is the pool's egress allowance: the endpoints of the systems that are on for it.
	Egress egress.AgentPolicy
}

// ApplyResult lists the objects an apply created or changed, by engine.ObjectName.
type ApplyResult struct{ Changed []string }

// Workers is what the cluster says of the pool's Deployment.
type Workers struct {
	// Exists is false when the Deployment is not there yet.
	Exists bool
	// Desired is the replica count of the Deployment and Ready the number of ready workers.
	Desired, Ready int32
}

// Cluster applies a pool's Kubernetes objects. Implementations are idempotent.
type Cluster interface {
	// ApplyPool applies the ServiceAccount, the ConfigMaps, the ExternalSecrets, the egress policy, the
	// shared volume claim and, for a dedicated team, the NodePool, then the Deployment; it removes the
	// pool's objects that are no longer rendered (an ExternalSecret without bindings, the shared claim
	// of a pool that no longer shares one).
	ApplyPool(ctx context.Context, env Environment) (ApplyResult, error)
	// Workers reports the Deployment's replicas.
	Workers(ctx context.Context, d Desired) (Workers, error)
	// DeletePool removes every object of the pool in the namespace: the Deployment first, then the rest.
	DeletePool(ctx context.Context, d Desired) error
}

// Queue is an SQS inbox queue.
type Queue struct {
	Name string
	ARN  string
	URL  string
}

// QueueSpec is the desired state of one pool's inbox queue.
type QueueSpec struct {
	Name string
	Tags map[string]string
	// ReceiverRoleARN is the one role that may receive from the queue: the pool's own.
	ReceiverRoleARN string
	// SenderRoleARNs are the roles that may send to it: the roles of the pools with a handoff to it.
	SenderRoleARNs []string
}

// Queues manages the inbox queues. Implementations are idempotent.
type Queues interface {
	// Locate returns the name, ARN and URL a queue of that name has; it calls nothing.
	Locate(name string) Queue
	// Ensure creates the queue when it does not exist and sets its policy and tags, so that only the senders
	// can send and only the receiver can receive.
	Ensure(ctx context.Context, spec QueueSpec) (Queue, error)
	// Delete removes the queue; a missing queue is success.
	Delete(ctx context.Context, name string) error
}

// LinkChange is a status written back to a pool role link.
type LinkChange struct {
	TeamID, PoolID, LinkID string
	// From and To are the statuses before and after.
	From, To   domain.RoleLinkStatus
	VerifiedAt *time.Time
	Error      string
	AccountID  string
	RoleARN    string
	// Event is the contract event to publish with the change, or "".
	Event domain.EventType
}

// LinkRecorder writes a change: the status columns, an audit record and the event, in one transaction.
type LinkRecorder interface {
	RecordLink(ctx context.Context, c LinkChange) error
}

// Clock is the time source of scheduled work.
type Clock func() time.Time
