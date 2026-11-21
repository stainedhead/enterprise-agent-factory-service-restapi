package team

import "context"

// RoleSpec is the desired state of one IAM role. The adapter creates or updates the role to match it
// and removes inline policies that are not listed.
type RoleSpec struct {
	// Name and Path name the role by id (D25): "afs-<ulid>" under "/afs/<namespace>/".
	Name string
	Path string
	// Description is shown in the console.
	Description string
	// TrustPolicy is the assume-role policy document (JSON).
	TrustPolicy string
	// PermissionsBoundaryARN limits the role; empty means none.
	PermissionsBoundaryARN string
	// Tags carry the human names and ids (afs-team-id, afs-team-slug, ...).
	Tags map[string]string
	// Policies maps inline policy names to documents (JSON).
	Policies map[string]string
}

// IAM manages the team's roles. Implementations are idempotent.
type IAM interface {
	// EnsureRole creates the role or updates it to match spec, and returns its ARN.
	EnsureRole(ctx context.Context, spec RoleSpec) (arn string, err error)
	// DeleteRole removes a role and its inline policies; a missing role is not an error.
	DeleteRole(ctx context.Context, path, name string) error
}

// ProfileSpec is the desired state of one Bedrock application inference profile.
type ProfileSpec struct {
	// Name is unique in the account and region: "afs-<team ulid>-<n>" style, see ProfileName.
	Name string
	// SourceARN is the foundation model or system inference profile the profile copies from.
	SourceARN string
	Tags      map[string]string
}

// Profile is an existing inference profile.
type Profile struct {
	Name string
	ARN  string
	// SourceARN is the model it copies from.
	SourceARN string
}

// InferenceProfiles manages Bedrock application inference profiles (the control plane, not the
// runtime).
type InferenceProfiles interface {
	// EnsureProfile returns the profile with spec.Name, creating it when it does not exist.
	EnsureProfile(ctx context.Context, spec ProfileSpec) (Profile, error)
	// ListProfiles returns the application profiles tagged key=value.
	ListProfiles(ctx context.Context, tagKey, tagValue string) ([]Profile, error)
	// DeleteProfile deletes by ARN; a missing profile is not an error.
	DeleteProfile(ctx context.Context, arn string) error
}
