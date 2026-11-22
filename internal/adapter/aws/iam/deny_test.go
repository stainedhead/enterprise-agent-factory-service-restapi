package iam

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func policyOut(name, doc string) string {
	return fmt.Sprintf("<RoleName>%s</RoleName><PolicyName>%s</PolicyName><PolicyDocument>%s</PolicyDocument>", roleName, name, url.QueryEscape(doc))
}

// M3-F5: the backstop puts one explicit deny of model invocation on the team's Bedrock role.
func TestDenyBedrockPutsTheExplicitDeny(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("bedrock")))
	srv.OnAction("PutRolePolicy", awstest.QueryEmptyOK("PutRolePolicy"))
	if err := c.DenyBedrock(t.Context(), rolePath, roleName); err != nil {
		t.Fatal(err)
	}
	put, ok := srv.Find("PutRolePolicy")
	if !ok || put.Form.Get("PolicyName") != BudgetDenyPolicyName || put.Form.Get("RoleName") != roleName {
		t.Fatalf("PutRolePolicy = %v", put.Form)
	}
	var doc struct {
		Statement []struct {
			Effect   string
			Action   []string
			Resource []string
		}
	}
	if err := json.Unmarshal([]byte(put.Form.Get("PolicyDocument")), &doc); err != nil || len(doc.Statement) != 1 {
		t.Fatalf("document %q: %v", put.Form.Get("PolicyDocument"), err)
	}
	st := doc.Statement[0]
	if st.Effect != "Deny" || len(st.Resource) != 1 || st.Resource[0] != "*" {
		t.Fatalf("statement = %+v", st)
	}
	for _, want := range []string{"bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"} {
		found := false
		for _, a := range st.Action {
			found = found || a == want
		}
		if !found {
			t.Errorf("deny lacks %s: %v", want, st.Action)
		}
	}
	if srv.Count("DeleteRolePolicy") != 0 {
		t.Errorf("the role's own policy was touched: %v", srv.Actions())
	}
}

func TestDenyBedrockIsIdempotent(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("bedrock", BudgetDenyPolicyName)))
	srv.OnAction("GetRolePolicy", awstest.QueryOK("GetRolePolicy", policyOut(BudgetDenyPolicyName, budgetDenyDocument)))
	if err := c.DenyBedrock(t.Context(), rolePath, roleName); err != nil {
		t.Fatal(err)
	}
	if w := writes(srv); len(w) != 0 {
		t.Errorf("a role already denied was written to: %v", w)
	}
}

func TestDenyBedrockRefusals(t *testing.T) {
	// A foreign role is never touched; a missing one is an error to retry (the team controller creates it).
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, "", map[string]string{"owner": "someone-else"})))
	if err := c.DenyBedrock(t.Context(), rolePath, roleName); !errorsIs(err, ErrNotOwned) {
		t.Errorf("foreign role: %v", err)
	}
	if w := writes(srv); len(w) != 0 {
		t.Errorf("foreign role written to: %v", w)
	}
	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryError(404, "NoSuchEntity", "role not found"))
	var coded *engine.CodedError
	if err := c2.DenyBedrock(t.Context(), rolePath, roleName); !errors.As(err, &coded) || coded.Code != "NoSuchEntity" || len(writes(srv2)) != 0 {
		t.Errorf("missing role: %v", err)
	}
	c3, srv3 := newClient(t)
	srv3.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML("/other/", trustDoc, "", ownedTags())))
	if err := c3.DenyBedrock(t.Context(), rolePath, roleName); !errorsIs(err, ErrPathChanged) {
		t.Errorf("wrong path: %v", err)
	}
}

func TestLiftBedrockDeny(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("bedrock", BudgetDenyPolicyName)))
	srv.OnAction("DeleteRolePolicy", awstest.QueryEmptyOK("DeleteRolePolicy"))
	if err := c.LiftBedrockDeny(t.Context(), rolePath, roleName); err != nil {
		t.Fatal(err)
	}
	del, ok := srv.Find("DeleteRolePolicy")
	if !ok || del.Form.Get("PolicyName") != BudgetDenyPolicyName || srv.Count("DeleteRolePolicy") != 1 {
		t.Fatalf("lifted %v; actions %v", del.Form, srv.Actions())
	}

	// Already lifted: no write. A role that is gone has nothing to lift.
	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv2.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("bedrock")))
	if err := c2.LiftBedrockDeny(t.Context(), rolePath, roleName); err != nil || len(writes(srv2)) != 0 {
		t.Errorf("already lifted: %v %v", err, srv2.Actions())
	}
	c3, srv3 := newClient(t)
	srv3.OnAction("GetRole", awstest.QueryError(404, "NoSuchEntity", "role not found"))
	if err := c3.LiftBedrockDeny(t.Context(), rolePath, roleName); err != nil || len(writes(srv3)) != 0 {
		t.Errorf("missing role: %v %v", err, srv3.Actions())
	}
	c4, srv4 := newClient(t)
	srv4.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, "", map[string]string{"owner": "someone-else"})))
	if err := c4.LiftBedrockDeny(t.Context(), rolePath, roleName); !errorsIs(err, ErrNotOwned) || len(writes(srv4)) != 0 {
		t.Errorf("foreign role: %v %v", err, srv4.Actions())
	}
}

// The team controller's EnsureRole removes policies it does not list; the backstop's deny must survive
// it, and DeleteRole must still remove it (a role with an inline policy cannot be deleted).
func TestEnsureRoleKeepsTheBudgetDenyAndDeleteRemovesIt(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("team", BudgetDenyPolicyName)))
	srv.OnAction("GetRolePolicy", awstest.QueryOK("GetRolePolicy", policyOut("team", polDoc)))
	if _, err := c.EnsureRole(t.Context(), spec()); err != nil {
		t.Fatal(err)
	}
	if w := writes(srv); len(w) != 0 {
		t.Errorf("EnsureRole wrote %v; the budget deny must be left alone", w)
	}

	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv2.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("team", BudgetDenyPolicyName)))
	srv2.OnAction("DeleteRolePolicy", awstest.QueryEmptyOK("DeleteRolePolicy"))
	srv2.OnAction("DeleteRole", awstest.QueryEmptyOK("DeleteRole"))
	if err := c2.DeleteRole(t.Context(), rolePath, roleName); err != nil {
		t.Fatal(err)
	}
	if srv2.Count("DeleteRolePolicy") != 2 {
		t.Errorf("DeleteRole left a policy: %v", srv2.Actions())
	}
}
