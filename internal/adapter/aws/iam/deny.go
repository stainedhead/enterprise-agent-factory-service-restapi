package iam

import (
	"context"
	"fmt"
	"slices"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsiam "github.com/aws/aws-sdk-go-v2/service/iam"

	afsaws "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// BudgetDenyPolicyName is the inline policy the budget backstop puts on a team's Bedrock role while the
// team's budget is spent (RC-6). It is an explicit deny, so it wins over the role's own allow. EnsureRole
// leaves it alone (it is not part of the team controller's desired policies); DeleteRole removes it.
const BudgetDenyPolicyName = "afs-budget-exhausted"

// budgetDenyDocument denies every model invocation action of the Bedrock runtime (Converse uses
// InvokeModel's permission, ConverseStream InvokeModelWithResponseStream's; the two Converse actions are
// listed as well for runtimes that check them by name).
const budgetDenyDocument = `{"Version":"2012-10-17","Statement":[{"Sid":"BudgetExhausted","Effect":"Deny","Action":["bedrock:InvokeModel","bedrock:InvokeModelWithResponseStream","bedrock:Converse","bedrock:ConverseStream"],"Resource":["*"]}]}`

// ownedRole checks that the role is one the reconciler created and is under path. A missing role reports
// found=false and no error; a foreign role or a different path is a terminal error.
func (c *Client) ownedRole(ctx context.Context, path, name string) (found bool, err error) {
	out, err := c.api.GetRole(ctx, &awsiam.GetRoleInput{RoleName: &name})
	if afsaws.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, afsaws.WrapError(err)
	}
	if !ownedBy(out.Role.Tags) {
		return true, engine.Terminal(fmt.Errorf("%w: %s", ErrNotOwned, name))
	}
	if got := awssdk.ToString(out.Role.Path); got != path {
		return true, engine.Terminal(fmt.Errorf("%w: %s is under %s, expected %s", ErrPathChanged, name, got, path))
	}
	return true, nil
}

// DenyBedrock puts the budget deny on the team's Bedrock role: idempotent, writing only when the policy
// is missing or differs. A role that does not exist yet is an error to retry (the team controller creates
// it); a role the reconciler did not create is never touched.
func (c *Client) DenyBedrock(ctx context.Context, path, roleName string) error {
	found, err := c.ownedRole(ctx, path, roleName)
	if err != nil {
		return err
	}
	if !found {
		return &engine.CodedError{Code: "NoSuchEntity", Err: fmt.Errorf("iam: the Bedrock role %s does not exist yet", roleName)}
	}
	have, err := c.listPolicies(ctx, roleName)
	if err != nil {
		return err
	}
	if slices.Contains(have, BudgetDenyPolicyName) {
		cur, err := c.api.GetRolePolicy(ctx, &awsiam.GetRolePolicyInput{RoleName: &roleName, PolicyName: awssdk.String(BudgetDenyPolicyName)})
		if err != nil {
			return afsaws.WrapError(err)
		}
		if sameJSON(decodeDoc(cur.PolicyDocument), budgetDenyDocument) {
			return nil
		}
	}
	doc := budgetDenyDocument
	if _, err := c.api.PutRolePolicy(ctx, &awsiam.PutRolePolicyInput{RoleName: &roleName, PolicyName: awssdk.String(BudgetDenyPolicyName), PolicyDocument: &doc}); err != nil {
		return afsaws.WrapError(err)
	}
	return nil
}

// LiftBedrockDeny removes the budget deny from the team's Bedrock role. A role that is gone, or has no
// such policy, is success; a role the reconciler did not create is never touched.
func (c *Client) LiftBedrockDeny(ctx context.Context, path, roleName string) error {
	found, err := c.ownedRole(ctx, path, roleName)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	have, err := c.listPolicies(ctx, roleName)
	if err != nil {
		return err
	}
	if !slices.Contains(have, BudgetDenyPolicyName) {
		return nil
	}
	if _, err := c.api.DeleteRolePolicy(ctx, &awsiam.DeleteRolePolicyInput{RoleName: &roleName, PolicyName: awssdk.String(BudgetDenyPolicyName)}); err != nil && !afsaws.IsNotFound(err) {
		return afsaws.WrapError(err)
	}
	return nil
}
