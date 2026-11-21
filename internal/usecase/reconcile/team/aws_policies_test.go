package team

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

const (
	teamA = "team_01JD0000000000000000000001"
	teamB = "team_01JD0000000000000000000002"
	gwARN = "arn:aws:iam::111122223333:role/afs-gateway"
)

type parsed struct {
	Version   string
	Statement []struct {
		Sid       string
		Effect    string
		Principal map[string]any
		Action    []string
		Resource  []string
		Condition map[string]map[string]any
	}
}

func parse(t *testing.T, doc string) parsed {
	t.Helper()
	var p parsed
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("policy is not JSON: %v\n%s", err, doc)
	}
	if p.Version != "2012-10-17" || len(p.Statement) == 0 {
		t.Fatalf("policy = %+v", p)
	}
	return p
}

// grantsBedrockWildcard reports whether any Allow statement of doc allows bedrock:* (or *).
func grantsBedrockWildcard(p parsed) bool {
	for _, s := range p.Statement {
		if s.Effect != "Allow" {
			continue
		}
		for _, a := range s.Action {
			if a == "*" || a == "bedrock:*" || strings.HasSuffix(a, ":*") && strings.HasPrefix(a, "bedrock") {
				return true
			}
		}
	}
	return false
}

func TestNames(t *testing.T) {
	if n, err := TeamRoleName(teamA); err != nil || n != "afs-01JD0000000000000000000001" {
		t.Errorf("TeamRoleName = %q, %v", n, err)
	}
	if n, err := BedrockRoleName(teamA); err != nil || n != "afs-01JD0000000000000000000001-bedrock" {
		t.Errorf("BedrockRoleName = %q, %v", n, err)
	}
	if _, err := TeamRoleName("nounderscore"); err == nil {
		t.Error("an id without a prefix accepted")
	}
	if _, err := BedrockRoleName("x"); err == nil {
		t.Error("bad id accepted")
	}
	if _, err := ProfileName("x", "m"); err == nil {
		t.Error("bad id accepted")
	}
	if RolePath("afs-t-payments") != "/afs/afs-t-payments/" {
		t.Error("RolePath")
	}
	if RoleARN("aws", "111122223333", "/afs/ns/", "afs-X") != "arn:aws:iam::111122223333:role/afs/ns/afs-X" {
		t.Error("RoleARN")
	}
	// Two agents named "reviewer" in two teams: the names are by id, so the roles are distinct.
	a1, _ := RoleNameForID(domain.ID("agt_01JD0000000000000000000011"))
	a2, _ := RoleNameForID(domain.ID("agt_01JD0000000000000000000012"))
	if a1 == a2 || strings.Contains(a1, "reviewer") || len(a1) > 64 {
		t.Errorf("agent role names %q, %q", a1, a2)
	}
}

func TestProfileName(t *testing.T) {
	n, err := ProfileName(teamA, "anthropic.claude-sonnet-4-5-v1:0")
	if err != nil || n != "afs-01JD0000000000000000000001-anthropic-claude-sonnet-4-5-v1-0" {
		t.Errorf("ProfileName = %q, %v", n, err)
	}
	long, _ := ProfileName(teamA, strings.Repeat("x", 100))
	if len(long) > 64 {
		t.Errorf("name of %d characters", len(long))
	}
	trimmed, _ := ProfileName(teamA, strings.Repeat("x", 33)+".")
	if strings.HasSuffix(trimmed, "-") {
		t.Errorf("trailing separator: %q", trimmed)
	}
	if !profileNameOK(n) {
		t.Errorf("%q does not match Bedrock's name pattern", n)
	}
	n2, _ := ProfileName(teamB, "anthropic.claude-sonnet-4-5-v1:0")
	if n == n2 {
		t.Error("two teams share a profile name")
	}
}

func profileNameOK(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || (i > 0 && (c == '-' || c == '_'))
		if !ok {
			return false
		}
	}
	return true
}

func TestSourceARN(t *testing.T) {
	tests := []struct{ model, want string }{
		{"anthropic.claude-sonnet-4-5-v1:0", "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-sonnet-4-5-v1:0"},
		{"us.anthropic.claude-sonnet-4-5-v1:0", "arn:aws:bedrock:us-east-1:111122223333:inference-profile/us.anthropic.claude-sonnet-4-5-v1:0"},
		{"global.anthropic.claude-x", "arn:aws:bedrock:us-east-1:111122223333:inference-profile/global.anthropic.claude-x"},
		{"arn:aws:bedrock:us-east-1:111122223333:inference-profile/custom", "arn:aws:bedrock:us-east-1:111122223333:inference-profile/custom"},
	}
	for _, tt := range tests {
		if got := SourceARN("aws", "us-east-1", "111122223333", tt.model); got != tt.want {
			t.Errorf("SourceARN(%q) = %q", tt.model, got)
		}
	}
}

func TestTeamRoleTrust(t *testing.T) {
	doc, err := TeamRoleTrust("111122223333", "arn:aws:eks:us-east-1:111122223333:cluster/afs", "arn:aws:iam::111122223333:role/eso")
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, doc)
	if len(p.Statement) != 2 {
		t.Fatalf("statements = %d", len(p.Statement))
	}
	pod := p.Statement[0]
	if pod.Principal["Service"] != "pods.eks.amazonaws.com" || !contains(pod.Action, "sts:TagSession") || !contains(pod.Action, "sts:AssumeRole") {
		t.Errorf("pod identity statement = %+v", pod)
	}
	if pod.Condition["StringEquals"]["aws:SourceAccount"] != "111122223333" || pod.Condition["ArnEquals"]["aws:SourceArn"] == nil {
		t.Errorf("pod identity conditions = %+v", pod.Condition)
	}
	if p.Statement[1].Principal["AWS"] != "arn:aws:iam::111122223333:role/eso" {
		t.Errorf("eso statement = %+v", p.Statement[1])
	}
	// Optional parts left out.
	min, _ := TeamRoleTrust("", "", "")
	if pm := parse(t, min); len(pm.Statement) != 1 || pm.Statement[0].Condition != nil {
		t.Errorf("minimal trust = %+v", pm)
	}
}

func TestTeamRolePolicy(t *testing.T) {
	doc, err := TeamRolePolicy("afs", "afs-t-payments", teamA)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, doc)
	if grantsBedrockWildcard(p) {
		t.Error("team role allows bedrock:*")
	}
	var deny, read bool
	for _, s := range p.Statement {
		switch {
		case s.Effect == "Deny" && contains(s.Action, "bedrock:*"):
			deny = true
		case s.Effect == "Allow":
			read = true
			if len(s.Resource) != 1 || s.Resource[0] != "arn:*:secretsmanager:*:*:secret:afs/afs-t-payments/*" {
				t.Errorf("secret resource = %v", s.Resource)
			}
			if s.Condition["StringEquals"]["aws:PrincipalTag/afs-team-id"] != teamA {
				t.Errorf("secret access is not conditioned on the role tag: %+v", s.Condition)
			}
			for _, a := range s.Action {
				if !strings.HasPrefix(a, "secretsmanager:Get") && a != "secretsmanager:DescribeSecret" {
					t.Errorf("unexpected action %s", a)
				}
			}
		}
	}
	if !deny || !read {
		t.Errorf("deny=%v read=%v", deny, read)
	}
	for _, args := range [][3]string{{"", "ns", teamA}, {"afs", "", teamA}, {"afs", "ns", ""}} {
		if _, err := TeamRolePolicy(args[0], args[1], args[2]); err == nil {
			t.Errorf("TeamRolePolicy%v accepted", args)
		}
	}
}

func TestConditionHelpers(t *testing.T) {
	ns := ConditionNamespace("afs-t-x")
	if ns["StringEquals"].(map[string]any)["aws:PrincipalTag/kubernetes-namespace"] != "afs-t-x" {
		t.Errorf("namespace condition = %v", ns)
	}
	tm := ConditionTeamTag(teamA)
	if tm["StringEquals"].(map[string]any)["aws:PrincipalTag/afs-team-id"] != teamA {
		t.Errorf("team tag condition = %v", tm)
	}
}

func TestBedrockRoleTrustIsGatewayOnly(t *testing.T) {
	doc, err := BedrockRoleTrust(gwARN)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, doc)
	if len(p.Statement) != 1 || p.Statement[0].Principal["AWS"] != gwARN || len(p.Statement[0].Principal) != 1 || p.Statement[0].Effect != "Allow" {
		t.Errorf("trust = %+v", p.Statement)
	}
	if _, err := BedrockRoleTrust(""); err == nil {
		t.Error("empty gateway role accepted")
	}
}

func TestBedrockRolePolicy(t *testing.T) {
	prof1 := "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc"
	prof2 := "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/def"
	model := "arn:aws:bedrock:*::foundation-model/anthropic.claude-sonnet-4-5-v1:0"
	doc, err := BedrockRolePolicy([]ProfileGrant{{prof2, model}, {prof1, model}})
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, doc)
	if grantsBedrockWildcard(p) {
		t.Error("bedrock role allows bedrock:*")
	}
	if len(p.Statement) != 2 {
		t.Fatalf("statements = %d", len(p.Statement))
	}
	prof := p.Statement[0]
	if len(prof.Resource) != 2 || prof.Resource[0] != prof1 || prof.Resource[1] != prof2 {
		t.Errorf("profile resources = %v (must be exactly the team's profiles, sorted)", prof.Resource)
	}
	for _, s := range p.Statement {
		for _, a := range s.Action {
			if a != "bedrock:InvokeModel" && a != "bedrock:InvokeModelWithResponseStream" {
				t.Errorf("action %s is not an invoke action", a)
			}
		}
		for _, r := range s.Resource {
			if r == "*" {
				t.Errorf("wildcard resource in %+v", s)
			}
		}
	}
	behind := p.Statement[1]
	if behind.Resource[0] != model || behind.Condition["StringLike"]["bedrock:InferenceProfileArn"] == nil {
		t.Errorf("foundation model statement = %+v", behind)
	}

	// No model ARNs given: only the profile statement.
	one, _ := BedrockRolePolicy([]ProfileGrant{{ProfileARN: prof1}})
	if len(parse(t, one).Statement) != 1 {
		t.Error("statement for models that were not given")
	}
	// No profiles: every invocation is denied.
	none, _ := BedrockRolePolicy(nil)
	pn := parse(t, none)
	if len(pn.Statement) != 1 || pn.Statement[0].Effect != "Deny" || pn.Statement[0].Resource[0] != "*" {
		t.Errorf("empty policy = %+v", pn.Statement)
	}
	if _, err := BedrockRolePolicy([]ProfileGrant{{}}); err == nil {
		t.Error("a grant without a profile accepted")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
