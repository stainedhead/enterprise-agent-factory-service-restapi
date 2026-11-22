// Package iam is the IAM adapter of the reconciler: it creates and updates the team's roles to match a
// RoleSpec and deletes them, touching only roles it created (tag afs-managed-by).
package iam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"reflect"
	"slices"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsiam "github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"

	afsaws "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// ErrNotOwned is returned for an existing role the reconciler did not create. It is terminal: the
// reconciler never adopts or deletes a role it does not own.
var ErrNotOwned = errors.New("iam: role exists but is not managed by the reconciler")

// ErrPathChanged is returned when an existing role has a different path; a role's path cannot change.
var ErrPathChanged = errors.New("iam: role exists under a different path")

// api is the part of the IAM client the adapter uses (the SDK client satisfies it; tests may fake it).
type api interface {
	GetRole(ctx context.Context, in *awsiam.GetRoleInput, opts ...func(*awsiam.Options)) (*awsiam.GetRoleOutput, error)
	CreateRole(ctx context.Context, in *awsiam.CreateRoleInput, opts ...func(*awsiam.Options)) (*awsiam.CreateRoleOutput, error)
	UpdateAssumeRolePolicy(ctx context.Context, in *awsiam.UpdateAssumeRolePolicyInput, opts ...func(*awsiam.Options)) (*awsiam.UpdateAssumeRolePolicyOutput, error)
	PutRolePermissionsBoundary(ctx context.Context, in *awsiam.PutRolePermissionsBoundaryInput, opts ...func(*awsiam.Options)) (*awsiam.PutRolePermissionsBoundaryOutput, error)
	DeleteRolePermissionsBoundary(ctx context.Context, in *awsiam.DeleteRolePermissionsBoundaryInput, opts ...func(*awsiam.Options)) (*awsiam.DeleteRolePermissionsBoundaryOutput, error)
	TagRole(ctx context.Context, in *awsiam.TagRoleInput, opts ...func(*awsiam.Options)) (*awsiam.TagRoleOutput, error)
	UntagRole(ctx context.Context, in *awsiam.UntagRoleInput, opts ...func(*awsiam.Options)) (*awsiam.UntagRoleOutput, error)
	ListRolePolicies(ctx context.Context, in *awsiam.ListRolePoliciesInput, opts ...func(*awsiam.Options)) (*awsiam.ListRolePoliciesOutput, error)
	GetRolePolicy(ctx context.Context, in *awsiam.GetRolePolicyInput, opts ...func(*awsiam.Options)) (*awsiam.GetRolePolicyOutput, error)
	PutRolePolicy(ctx context.Context, in *awsiam.PutRolePolicyInput, opts ...func(*awsiam.Options)) (*awsiam.PutRolePolicyOutput, error)
	DeleteRolePolicy(ctx context.Context, in *awsiam.DeleteRolePolicyInput, opts ...func(*awsiam.Options)) (*awsiam.DeleteRolePolicyOutput, error)
	DeleteRole(ctx context.Context, in *awsiam.DeleteRoleInput, opts ...func(*awsiam.Options)) (*awsiam.DeleteRoleOutput, error)
	ListRoles(ctx context.Context, in *awsiam.ListRolesInput, opts ...func(*awsiam.Options)) (*awsiam.ListRolesOutput, error)
}

// Client implements team.IAM.
type Client struct{ api api }

// New returns a Client on an SDK configuration.
func New(cfg awssdk.Config) *Client { return &Client{api: awsiam.NewFromConfig(cfg)} }

var _ team.IAM = (*Client)(nil)

// EnsureRole implements team.IAM.
func (c *Client) EnsureRole(ctx context.Context, spec team.RoleSpec) (string, error) {
	if spec.Name == "" || spec.Path == "" || spec.TrustPolicy == "" {
		return "", engine.Terminal(errors.New("iam: role spec needs a name, a path and a trust policy"))
	}
	tags := maps.Clone(spec.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[team.TagManagedBy] = team.ManagedByValue

	out, err := c.api.GetRole(ctx, &awsiam.GetRoleInput{RoleName: &spec.Name})
	if afsaws.IsNotFound(err) {
		return c.create(ctx, spec, tags)
	}
	if err != nil {
		return "", afsaws.WrapError(err)
	}
	role := out.Role
	if !ownedBy(role.Tags) {
		return "", engine.Terminal(fmt.Errorf("%w: %s", ErrNotOwned, spec.Name))
	}
	if awssdk.ToString(role.Path) != spec.Path {
		return "", engine.Terminal(fmt.Errorf("%w: %s is under %s, want %s", ErrPathChanged, spec.Name, awssdk.ToString(role.Path), spec.Path))
	}
	if err := c.sync(ctx, spec, tags, role); err != nil {
		return "", err
	}
	return awssdk.ToString(role.Arn), nil
}

func (c *Client) create(ctx context.Context, spec team.RoleSpec, tags map[string]string) (string, error) {
	in := &awsiam.CreateRoleInput{
		RoleName: &spec.Name, Path: &spec.Path, AssumeRolePolicyDocument: &spec.TrustPolicy,
		Tags: toTags(tags),
	}
	if spec.Description != "" {
		in.Description = &spec.Description
	}
	if spec.PermissionsBoundaryARN != "" {
		in.PermissionsBoundary = &spec.PermissionsBoundaryARN
	}
	out, err := c.api.CreateRole(ctx, in)
	if err != nil {
		return "", afsaws.WrapError(err)
	}
	if err := c.syncPolicies(ctx, spec, true); err != nil {
		return "", err
	}
	return awssdk.ToString(out.Role.Arn), nil
}

// sync brings an existing role to spec, writing only what differs.
func (c *Client) sync(ctx context.Context, spec team.RoleSpec, tags map[string]string, role *types.Role) error {
	if !sameJSON(decodeDoc(role.AssumeRolePolicyDocument), spec.TrustPolicy) {
		if _, err := c.api.UpdateAssumeRolePolicy(ctx, &awsiam.UpdateAssumeRolePolicyInput{RoleName: &spec.Name, PolicyDocument: &spec.TrustPolicy}); err != nil {
			return afsaws.WrapError(err)
		}
	}
	have := ""
	if role.PermissionsBoundary != nil {
		have = awssdk.ToString(role.PermissionsBoundary.PermissionsBoundaryArn)
	}
	switch {
	case spec.PermissionsBoundaryARN != "" && have != spec.PermissionsBoundaryARN:
		if _, err := c.api.PutRolePermissionsBoundary(ctx, &awsiam.PutRolePermissionsBoundaryInput{RoleName: &spec.Name, PermissionsBoundary: &spec.PermissionsBoundaryARN}); err != nil {
			return afsaws.WrapError(err)
		}
	case spec.PermissionsBoundaryARN == "" && have != "":
		if _, err := c.api.DeleteRolePermissionsBoundary(ctx, &awsiam.DeleteRolePermissionsBoundaryInput{RoleName: &spec.Name}); err != nil {
			return afsaws.WrapError(err)
		}
	}
	if err := c.syncTags(ctx, spec.Name, tags, role.Tags); err != nil {
		return err
	}
	return c.syncPolicies(ctx, spec, true)
}

// syncTags adds or fixes the desired tags and removes the ones AFS applied earlier (the afs- system keys and
// the keys recorded in afs-tag-keys) that are no longer desired. Tags put on the role by anyone else are left
// alone (tags.Stale).
func (c *Client) syncTags(ctx context.Context, name string, want map[string]string, have []types.Tag) error {
	haveMap := map[string]string{}
	for _, t := range have {
		haveMap[awssdk.ToString(t.Key)] = awssdk.ToString(t.Value)
	}
	diff := map[string]string{}
	for k, v := range want {
		if haveMap[k] != v {
			diff[k] = v
		}
	}
	if len(diff) > 0 {
		if _, err := c.api.TagRole(ctx, &awsiam.TagRoleInput{RoleName: &name, Tags: toTags(diff)}); err != nil {
			return afsaws.WrapError(err)
		}
	}
	stale := tags.Stale(haveMap, want)
	if len(stale) > 0 {
		slices.Sort(stale)
		if _, err := c.api.UntagRole(ctx, &awsiam.UntagRoleInput{RoleName: &name, TagKeys: stale}); err != nil {
			return afsaws.WrapError(err)
		}
	}
	return nil
}

// syncPolicies makes the role's inline policies exactly spec.Policies. The budget backstop's deny
// (BudgetDenyPolicyName, deny.go) is owned by the budget reconciler: it is left alone when keepBudgetDeny
// is set (EnsureRole), and removed with everything else when the role is deleted.
func (c *Client) syncPolicies(ctx context.Context, spec team.RoleSpec, keepBudgetDeny bool) error {
	have, err := c.listPolicies(ctx, spec.Name)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(spec.Policies))
	for n := range spec.Policies {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		doc := spec.Policies[n]
		if slices.Contains(have, n) {
			cur, err := c.api.GetRolePolicy(ctx, &awsiam.GetRolePolicyInput{RoleName: &spec.Name, PolicyName: &n})
			if err != nil {
				return afsaws.WrapError(err)
			}
			if sameJSON(decodeDoc(cur.PolicyDocument), doc) {
				continue
			}
		}
		if _, err := c.api.PutRolePolicy(ctx, &awsiam.PutRolePolicyInput{RoleName: &spec.Name, PolicyName: &n, PolicyDocument: &doc}); err != nil {
			return afsaws.WrapError(err)
		}
	}
	for _, n := range have {
		if _, ok := spec.Policies[n]; !ok && (!keepBudgetDeny || n != BudgetDenyPolicyName) {
			if _, err := c.api.DeleteRolePolicy(ctx, &awsiam.DeleteRolePolicyInput{RoleName: &spec.Name, PolicyName: &n}); err != nil {
				return afsaws.WrapError(err)
			}
		}
	}
	return nil
}

func (c *Client) listPolicies(ctx context.Context, role string) ([]string, error) {
	var names []string
	var marker *string
	for {
		out, err := c.api.ListRolePolicies(ctx, &awsiam.ListRolePoliciesInput{RoleName: &role, Marker: marker})
		if err != nil {
			return nil, afsaws.WrapError(err)
		}
		names = append(names, out.PolicyNames...)
		if !out.IsTruncated || out.Marker == nil {
			return names, nil
		}
		marker = out.Marker
	}
}

// DeleteRole implements team.IAM. It deletes only a role the reconciler created.
func (c *Client) DeleteRole(ctx context.Context, path, name string) error {
	out, err := c.api.GetRole(ctx, &awsiam.GetRoleInput{RoleName: &name})
	if afsaws.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return afsaws.WrapError(err)
	}
	if !ownedBy(out.Role.Tags) {
		return engine.Terminal(fmt.Errorf("%w: %s", ErrNotOwned, name))
	}
	if got := awssdk.ToString(out.Role.Path); got != path {
		return engine.Terminal(fmt.Errorf("%w: %s is under %s, expected %s", ErrPathChanged, name, got, path))
	}
	if err := c.syncPolicies(ctx, team.RoleSpec{Name: name}, false); err != nil { // no policies wanted: removes them all
		return err
	}
	if _, err := c.api.DeleteRole(ctx, &awsiam.DeleteRoleInput{RoleName: &name}); err != nil && !afsaws.IsNotFound(err) {
		return afsaws.WrapError(err)
	}
	return nil
}

// rolesRoot is the path prefix of every role the reconciler creates (/afs/<namespace>/).
const rolesRoot = "/afs/"

// RevokeTeam deletes every role under /afs/ that carries the managed-by tag and the team's
// afs-team-id tag: the team role, dedicated agent roles and the Bedrock role (lifecycle.IAMRevoker).
// Roles of other teams and roles the reconciler did not create are left alone; a role that is
// already gone is success. Every role is attempted and the errors are joined.
func (c *Client) RevokeTeam(ctx context.Context, teamID string) error {
	var errs []error
	var marker *string
	for {
		page, err := c.api.ListRoles(ctx, &awsiam.ListRolesInput{PathPrefix: awssdk.String(rolesRoot), Marker: marker})
		if err != nil {
			return afsaws.WrapError(err)
		}
		for _, r := range page.Roles {
			name, path := awssdk.ToString(r.RoleName), awssdk.ToString(r.Path)
			got, err := c.api.GetRole(ctx, &awsiam.GetRoleInput{RoleName: &name})
			if afsaws.IsNotFound(err) {
				continue
			}
			if err != nil {
				errs = append(errs, afsaws.WrapError(err))
				continue
			}
			if !ownedBy(got.Role.Tags) || !taggedWith(got.Role.Tags, team.TagTeamID, teamID) {
				continue
			}
			if err := c.DeleteRole(ctx, path, name); err != nil {
				errs = append(errs, err)
			}
		}
		if !page.IsTruncated || page.Marker == nil {
			return errors.Join(errs...)
		}
		marker = page.Marker
	}
}

func taggedWith(tags []types.Tag, key, value string) bool {
	for _, t := range tags {
		if awssdk.ToString(t.Key) == key && awssdk.ToString(t.Value) == value {
			return true
		}
	}
	return false
}

func ownedBy(tags []types.Tag) bool {
	for _, t := range tags {
		if awssdk.ToString(t.Key) == team.TagManagedBy && awssdk.ToString(t.Value) == team.ManagedByValue {
			return true
		}
	}
	return false
}

func toTags(m map[string]string) []types.Tag {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]types.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, types.Tag{Key: awssdk.String(k), Value: awssdk.String(m[k])})
	}
	return out
}

// decodeDoc turns the URL-encoded document IAM returns into JSON text.
func decodeDoc(s *string) string {
	if s == nil {
		return ""
	}
	if d, err := url.QueryUnescape(*s); err == nil {
		return d
	}
	return *s
}

// sameJSON compares two JSON documents ignoring formatting and key order.
func sameJSON(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
