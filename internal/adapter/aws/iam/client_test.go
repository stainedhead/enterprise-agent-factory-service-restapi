package iam

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

const (
	roleName = "afs-01JD0000000000000000000001"
	rolePath = "/afs/afs-t-payments/"
	roleARN  = "arn:aws:iam::111122223333:role/afs/afs-t-payments/afs-01JD0000000000000000000001"
	boundary = "arn:aws:iam::111122223333:policy/afs-boundary"
	trustDoc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"pods.eks.amazonaws.com"},"Action":["sts:AssumeRole","sts:TagSession"]}]}`
	polDoc   = `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Action":["bedrock:*"],"Resource":["*"]}]}`
)

func spec() team.RoleSpec {
	return team.RoleSpec{
		Name: roleName, Path: rolePath, Description: "payments", TrustPolicy: trustDoc, PermissionsBoundaryARN: boundary,
		Tags:     map[string]string{team.TagTeamID: "team_01JD0000000000000000000001", team.TagTeamSlug: "payments"},
		Policies: map[string]string{"team": polDoc},
	}
}

// roleXML renders a GetRole result.
func roleXML(path, trust, bound string, tags map[string]string) string {
	var tb strings.Builder
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&tb, "<member><Key>%s</Key><Value>%s</Value></member>", k, tags[k])
	}
	pb := ""
	if bound != "" {
		pb = fmt.Sprintf("<PermissionsBoundary><PermissionsBoundaryType>Policy</PermissionsBoundaryType><PermissionsBoundaryArn>%s</PermissionsBoundaryArn></PermissionsBoundary>", bound)
	}
	return fmt.Sprintf(`<Role><Path>%s</Path><RoleName>%s</RoleName><RoleId>AROAEXAMPLE</RoleId><Arn>%s</Arn><CreateDate>2026-11-21T00:00:00Z</CreateDate><AssumeRolePolicyDocument>%s</AssumeRolePolicyDocument>%s<Tags>%s</Tags></Role>`,
		path, roleName, roleARN, url.QueryEscape(trust), pb, tb.String())
}

func policyNames(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "<member>%s</member>", n)
	}
	return fmt.Sprintf("<PolicyNames>%s</PolicyNames><IsTruncated>false</IsTruncated>", b.String())
}

func ownedTags() map[string]string {
	return map[string]string{team.TagTeamID: "team_01JD0000000000000000000001", team.TagTeamSlug: "payments", team.TagManagedBy: team.ManagedByValue}
}

func newClient(t *testing.T) (*Client, *awstest.Server) {
	t.Helper()
	srv := awstest.New(t)
	return New(srv.Config()), srv
}

func writes(srv *awstest.Server) []string {
	var out []string
	for _, a := range srv.Actions() {
		switch a {
		case "GetRole", "ListRolePolicies", "GetRolePolicy":
		default:
			out = append(out, a)
		}
	}
	return out
}

// M1-G2b: a missing role is created with its trust policy, boundary, tags and inline policy.
func TestEnsureRoleCreates(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryError(404, "NoSuchEntity", "role not found"))
	srv.OnAction("CreateRole", awstest.QueryOK("CreateRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames()))
	srv.OnAction("PutRolePolicy", awstest.QueryEmptyOK("PutRolePolicy"))

	arn, err := c.EnsureRole(t.Context(), spec())
	if err != nil || arn != roleARN {
		t.Fatalf("EnsureRole = %q, %v", arn, err)
	}
	req, _ := srv.Find("CreateRole")
	f := req.Form
	if f.Get("RoleName") != roleName || f.Get("Path") != rolePath || f.Get("PermissionsBoundary") != boundary || f.Get("AssumeRolePolicyDocument") != trustDoc || f.Get("Description") != "payments" {
		t.Errorf("CreateRole form = %v", f)
	}
	got := map[string]string{}
	for i := 1; f.Get(fmt.Sprintf("Tags.member.%d.Key", i)) != ""; i++ {
		got[f.Get(fmt.Sprintf("Tags.member.%d.Key", i))] = f.Get(fmt.Sprintf("Tags.member.%d.Value", i))
	}
	want := ownedTags()
	if len(got) != len(want) {
		t.Fatalf("tags = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tag %s = %q, want %q", k, got[k], v)
		}
	}
	put, _ := srv.Find("PutRolePolicy")
	if put.Form.Get("PolicyName") != "team" || put.Form.Get("PolicyDocument") != polDoc || put.Form.Get("RoleName") != roleName {
		t.Errorf("PutRolePolicy form = %v", put.Form)
	}
}

// M1-G2b: a role already in the desired state is not written to (idempotent, no IAM churn).
func TestEnsureRoleNoOpWhenInSync(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("team")))
	srv.OnAction("GetRolePolicy", awstest.QueryOK("GetRolePolicy", fmt.Sprintf("<RoleName>%s</RoleName><PolicyName>team</PolicyName><PolicyDocument>%s</PolicyDocument>", roleName, url.QueryEscape(polDoc))))
	arn, err := c.EnsureRole(t.Context(), spec())
	if err != nil || arn != roleARN {
		t.Fatalf("EnsureRole = %q, %v", arn, err)
	}
	if w := writes(srv); len(w) != 0 {
		t.Errorf("a role in sync was written to: %v", w)
	}
}

// M1-G2b: drift in trust, boundary, tags and policies is corrected; foreign tags survive.
func TestEnsureRoleCorrectsDrift(t *testing.T) {
	c, srv := newClient(t)
	tags := ownedTags()
	tags["afs-agent-name"] = "stale" // an afs- tag no longer desired
	tags["cost-center"] = "keep-me"  // not ours
	tags[team.TagTeamSlug] = "old-slug"
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, `{"Version":"2012-10-17","Statement":[]}`, "arn:aws:iam::111122223333:policy/other", tags)))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("team", "leftover")))
	srv.OnAction("GetRolePolicy", awstest.QueryOK("GetRolePolicy", fmt.Sprintf("<RoleName>%s</RoleName><PolicyName>team</PolicyName><PolicyDocument>%s</PolicyDocument>", roleName, url.QueryEscape(`{"Version":"2012-10-17","Statement":[]}`))))
	for _, a := range []string{"UpdateAssumeRolePolicy", "PutRolePermissionsBoundary", "TagRole", "UntagRole", "PutRolePolicy", "DeleteRolePolicy"} {
		srv.OnAction(a, awstest.QueryEmptyOK(a))
	}
	if _, err := c.EnsureRole(t.Context(), spec()); err != nil {
		t.Fatal(err)
	}
	want := []string{"UpdateAssumeRolePolicy", "PutRolePermissionsBoundary", "TagRole", "UntagRole", "PutRolePolicy", "DeleteRolePolicy"}
	got := writes(srv)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	untag, _ := srv.Find("UntagRole")
	if untag.Form.Get("TagKeys.member.1") != "afs-agent-name" || untag.Form.Get("TagKeys.member.2") != "" {
		t.Errorf("UntagRole = %v (only the stale afs- tag, never cost-center)", untag.Form)
	}
	tag, _ := srv.Find("TagRole")
	if tag.Form.Get("Tags.member.1.Key") != team.TagTeamSlug || tag.Form.Get("Tags.member.1.Value") != "payments" || tag.Form.Get("Tags.member.2.Key") != "" {
		t.Errorf("TagRole = %v (only the differing tag)", tag.Form)
	}
	del, _ := srv.Find("DeleteRolePolicy")
	if del.Form.Get("PolicyName") != "leftover" {
		t.Errorf("DeleteRolePolicy = %v", del.Form)
	}
}

func TestEnsureRoleDropsABoundaryThatIsNoLongerWanted(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames()))
	srv.OnAction("DeleteRolePermissionsBoundary", awstest.QueryEmptyOK("DeleteRolePermissionsBoundary"))
	s := spec()
	s.PermissionsBoundaryARN, s.Policies = "", nil
	if _, err := c.EnsureRole(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if srv.Count("DeleteRolePermissionsBoundary") != 1 {
		t.Errorf("actions = %v", srv.Actions())
	}
}

// M1-G2b: a role the reconciler did not create is never touched; the failure is terminal.
func TestEnsureRoleRefusesForeignRoleAndPathChange(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, "", map[string]string{"owner": "someone-else"})))
	_, err := c.EnsureRole(t.Context(), spec())
	if !errorsIs(err, ErrNotOwned) || !errorsIs(err, engine.ErrTerminal) {
		t.Errorf("foreign role: %v", err)
	}
	if w := writes(srv); len(w) != 0 {
		t.Errorf("foreign role written to: %v", w)
	}

	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML("/other/", trustDoc, "", ownedTags())))
	_, err = c2.EnsureRole(t.Context(), spec())
	if !errorsIs(err, ErrPathChanged) || !errorsIs(err, engine.ErrTerminal) {
		t.Errorf("path change: %v", err)
	}

	if _, err := c.EnsureRole(t.Context(), team.RoleSpec{Name: "x"}); !errorsIs(err, engine.ErrTerminal) {
		t.Errorf("incomplete spec: %v", err)
	}
}

// M1-G2b: AWS error codes reach the engine as codes (they become the condition reason).
func TestEnsureRoleReportsAWSErrorCodes(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryError(404, "NoSuchEntity", "no"))
	srv.OnAction("CreateRole", awstest.QueryError(403, "AccessDenied", "not authorized to perform iam:CreateRole"))
	_, err := c.EnsureRole(t.Context(), spec())
	if engine.CodeOf(err) != "AccessDenied" {
		t.Errorf("code = %q, err = %v", engine.CodeOf(err), err)
	}

	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryError(403, "AccessDenied", "no"))
	if _, err := c2.EnsureRole(t.Context(), spec()); engine.CodeOf(err) != "AccessDenied" {
		t.Errorf("GetRole error code = %q", engine.CodeOf(err))
	}
}

// M1-F4 relevance: throttling is a retryable error, with its code.
func TestThrottlingIsCoded(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryError(400, "Throttling", "Rate exceeded"))
	_, err := c.EnsureRole(t.Context(), spec())
	if engine.CodeOf(err) != "Throttling" || errorsIs(err, engine.ErrTerminal) {
		t.Errorf("throttling: code %q, terminal %v", engine.CodeOf(err), errorsIs(err, engine.ErrTerminal))
	}
}

func TestDeleteRole(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, boundary, ownedTags())))
	srv.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames("team", "extra")))
	srv.OnAction("DeleteRolePolicy", awstest.QueryEmptyOK("DeleteRolePolicy"))
	srv.OnAction("DeleteRole", awstest.QueryEmptyOK("DeleteRole"))
	if err := c.DeleteRole(t.Context(), rolePath, roleName); err != nil {
		t.Fatal(err)
	}
	if srv.Count("DeleteRolePolicy") != 2 || srv.Count("DeleteRole") != 1 {
		t.Errorf("actions = %v", srv.Actions())
	}
	acts := srv.Actions()
	if acts[len(acts)-1] != "DeleteRole" {
		t.Errorf("role deleted before its policies: %v", acts)
	}
}

func TestDeleteRoleMissingIsFine(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryError(404, "NoSuchEntity", "gone"))
	if err := c.DeleteRole(t.Context(), rolePath, roleName); err != nil {
		t.Errorf("deleting a missing role: %v", err)
	}
	// raced away between the policy cleanup and the delete
	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, "", ownedTags())))
	srv2.OnAction("ListRolePolicies", awstest.QueryOK("ListRolePolicies", policyNames()))
	srv2.OnAction("DeleteRole", awstest.QueryError(404, "NoSuchEntity", "gone"))
	if err := c2.DeleteRole(t.Context(), rolePath, roleName); err != nil {
		t.Errorf("role gone at delete: %v", err)
	}
}

func TestDeleteRoleRefusesForeignRoleAndWrongPath(t *testing.T) {
	c, srv := newClient(t)
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, "", map[string]string{"owner": "x"})))
	if err := c.DeleteRole(t.Context(), rolePath, roleName); !errorsIs(err, ErrNotOwned) {
		t.Errorf("foreign: %v", err)
	}
	c2, srv2 := newClient(t)
	srv2.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML("/elsewhere/", trustDoc, "", ownedTags())))
	if err := c2.DeleteRole(t.Context(), rolePath, roleName); !errorsIs(err, ErrPathChanged) {
		t.Errorf("wrong path: %v", err)
	}
	if w := writes(srv); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

func TestPolicyPagination(t *testing.T) {
	c, srv := newClient(t)
	page := 0
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Action != "ListRolePolicies" {
			return nil
		}
		page++
		if r.Form.Get("Marker") == "" {
			resp := awstest.QueryOK("ListRolePolicies", "<PolicyNames><member>a</member></PolicyNames><IsTruncated>true</IsTruncated><Marker>m1</Marker>")
			return &resp
		}
		resp := awstest.QueryOK("ListRolePolicies", "<PolicyNames><member>b</member></PolicyNames><IsTruncated>false</IsTruncated>")
		return &resp
	})
	names, err := c.listPolicies(t.Context(), roleName)
	if err != nil || len(names) != 2 || page != 2 {
		t.Errorf("names = %v, pages = %d, err = %v", names, page, err)
	}
	srv.OnAction("GetRole", awstest.QueryOK("GetRole", roleXML(rolePath, trustDoc, "", ownedTags())))
}

func TestSameJSONAndDecode(t *testing.T) {
	if !sameJSON(`{"a":1,"b":[1,2]}`, `{ "b": [1,2], "a": 1 }`) || sameJSON(`{"a":1}`, `{"a":2}`) || sameJSON(`not json`, `{}`) {
		t.Error("sameJSON")
	}
	if decodeDoc(nil) != "" || decodeDoc(ptr("%7B%22a%22%3A1%7D")) != `{"a":1}` || decodeDoc(ptr("%zz")) != "%zz" {
		t.Error("decodeDoc")
	}
}

func ptr(s string) *string { return &s }
