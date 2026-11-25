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

// Model is a model callable for the team (enabled for the service, in the team's set and priced) and, once
// created, its inference profile.
type Model struct {
	ID         string
	ProfileARN string
	// Inactive is true when a profile row exists that is not active yet (creating) or is retiring: the model
	// is callable again, so the row is rewritten active.
	Inactive bool
}

// RetiringModel is a model that is no longer callable for the team and still has an inference profile: the
// profile stays for the grace period (7 days) so a model that comes back keeps its profile, then it is deleted.
type RetiringModel struct {
	ID         string
	ProfileARN string
	// RetireAfter is when the profile may be deleted; zero while the profile is not yet marked retiring.
	RetireAfter time.Time
}

// ProfileGrace is how long the inference profile of a model that stopped being callable is kept (MOD-7).
const ProfileGrace = 7 * 24 * time.Hour

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
	// Retiring are the models that are not callable now and still have a profile row.
	Retiring []RetiringModel
	// Tags are the team's tags beyond the system ones: fixed and required service tags, defaults and the
	// team's own (afs.team_effective_tags without the system rows).
	Tags map[string]string
	// DeletedAt is set once the team is deleted.
	DeletedAt *time.Time
	// Gateway is the team's gateway deployment and route set (v1.2.0).
	Gateway TeamGateway
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
	// TeamGateway configures the team's own gateway; used only while Team.Gateway is per_namespace.
	TeamGateway TeamGatewaySpec
}

// TeamGatewaySpec is the service-level configuration of team gateways (Helm values, never per team).
type TeamGatewaySpec struct {
	// Image is the afs-gateway image (AFS_TEAM_GATEWAY_IMAGE).
	Image string
	// DatabaseSecretARN is the Secrets Manager reference of the afs_gateway connection URL
	// (AFS_TEAM_GATEWAY_DB_SECRET_ARN); DatabaseHost and DatabasePort are its egress destination.
	DatabaseSecretARN string
	DatabaseHost      string
	DatabasePort      int
	// TLSIssuer is the cert-manager ClusterIssuer of the gateway's serving certificate
	// (AFS_TEAM_GATEWAY_TLS_ISSUER); it must chain to the CA bundle agents trust. Empty: no Certificate.
	TLSIssuer string
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
	// RetireProfile marks the team's profile of the model retiring until after.
	RetireProfile(ctx context.Context, teamID, modelID string, after time.Time) error
	// DropProfile removes the team's profile row of the model, once its AWS profile is deleted.
	DropProfile(ctx context.Context, teamID, modelID string) error
}
