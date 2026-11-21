// Package team reconciles a team's environment (RC-1): the namespace and everything in it on the
// Kubernetes side, and the IAM roles and Bedrock inference profiles on the AWS side. It computes the
// desired state from the team row and calls ports; the ports' adapters render and apply.
package team

import (
	"context"
	"time"
)

// Quotas is the team's resource quota (teams.quotas, API contract §5): empty fields set no limit.
type Quotas struct {
	CPU     string
	Memory  string
	Storage string
	Pods    int64
}

// Model is a model enabled for the team and, once created, its inference profile.
type Model struct {
	ID         string
	ProfileARN string
}

// Desired is the team row and its models, as the reconciler reads them.
type Desired struct {
	ID          string
	Slug        string
	Namespace   string
	DisplayName string
	// Kind is persistent or ephemeral.
	Kind string
	// State is active, suspended, deleting or deleted.
	State      string
	Generation int64
	Quotas     Quotas
	Models     []Model
	// DeletedAt is set once the team is deleted.
	DeletedAt *time.Time
}

// Environment is everything the cluster adapter renders for one team.
type Environment struct {
	Team Desired
	// TeamRoleARN is the IAM role the namespace's SecretStore uses.
	TeamRoleARN string
	Region      string
	// GatewayNamespace, GatewayName and GatewayPort locate afs-gateway (gateway contract §2).
	GatewayNamespace string
	GatewayName      string
	GatewayPort      int
}

// ApplyResult lists the objects an apply created or changed, by engine.ObjectName.
type ApplyResult struct {
	Changed []string
}

// Cluster applies a team's Kubernetes objects: the namespace with its Pod Security labels, the quota and
// limit range, the default-deny policies, the Cilium policies for DNS and afs-gateway, the External
// Secrets SecretStore and the gateway CA bundle. Implementations are idempotent.
type Cluster interface {
	ApplyTeam(ctx context.Context, env Environment) (ApplyResult, error)
}

// PlatformCluster applies the cluster-wide objects the team environments depend on (the Cilium policy
// on afs-gateway that admits only team namespaces, D57).
type PlatformCluster interface {
	ApplyPlatform(ctx context.Context, p PlatformSpec) (ApplyResult, error)
}

// PlatformSpec locates afs-gateway.
type PlatformSpec struct {
	GatewayNamespace string
	GatewayName      string
	GatewayPort      int
}

// Source reads and updates the team rows the reconciler owns.
type Source interface {
	// LoadTeam returns the team, or a domain NotFound error when it does not exist.
	LoadTeam(ctx context.Context, id string) (Desired, error)
	// SetInferenceProfileARN records the team model's inference profile (team_models).
	SetInferenceProfileARN(ctx context.Context, teamID, modelID, arn string) error
}
