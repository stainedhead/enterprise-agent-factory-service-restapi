package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMcpNamingHelpers(t *testing.T) {
	if got := McpHostedName("github"); got != "afs-mcp-github" {
		t.Fatalf("name %q", got)
	}
	if got := McpHostedURL("afs-t-a", "github"); got != "http://afs-mcp-github.afs-t-a.svc:8080/mcp" {
		t.Fatalf("url %q", got)
	}
	if got := McpCredentialKey("github"); got != "mcp.github" {
		t.Fatalf("key %q", got)
	}
	for in, want := range map[string]string{
		"api://Tools":                  "api-tools",
		"https://x.example/y/":         "https-x-example-y",
		"://":                          "default",
		strings.Repeat("a", 70):        strings.Repeat("a", 63),
		strings.Repeat("a", 62) + "-b": strings.Repeat("a", 62),
	} {
		if got := McpFederationDir(in); got != want {
			t.Errorf("dir(%q) = %q, want %q", in, got, want)
		}
	}
	if got := McpServiceAccountSubject("ns", "sa"); got != "system:serviceaccount:ns:sa" {
		t.Fatalf("subject %q", got)
	}
}

func TestMcpAccessPatchMerge(t *testing.T) {
	cur := AgentMcp{Access: "selected", ServerIDs: []string{"mcp_1"}, Mode: McpAgentModeFull}
	if got := (McpAccessPatch{}).Merge(AgentMcp{}); got.Access != "all" {
		t.Fatalf("default access %q", got.Access)
	}
	all := "all"
	if got := (McpAccessPatch{Access: &all}).Merge(cur); got.Access != "all" || got.ServerIDs != nil {
		t.Fatalf("switch to all %+v", got)
	}
	sel, ids, mode := "selected", []string{"mcp_2"}, McpAgentModeSearch
	blocks := []McpAgentBlock{{}}
	got := (McpAccessPatch{Access: &sel, ServerIDs: &ids, Mode: &mode, Blocks: &blocks}).Merge(cur)
	if got.ServerIDs[0] != "mcp_2" || got.Mode != McpAgentModeSearch || len(got.Blocks) != 1 {
		t.Fatalf("merge %+v", got)
	}
	a := AgentMcp{Access: "all"}.Audit()
	if len(a["serverIds"].([]string)) != 0 || len(a["blocks"].([]McpAgentBlock)) != 0 {
		t.Fatalf("audit %+v", a)
	}
}

func TestMcpLimitsValidate(t *testing.T) {
	if err := (McpLimits{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, l := range []McpLimits{{RequestBytes: 10}, {ResponseBytes: 10}, {TimeoutSeconds: 601}, {ConcurrentPerAgent: 65}} {
		if err := l.Validate(); err == nil {
			t.Errorf("%+v accepted", l)
		}
	}
}

func TestMcpServerRecordScopes(t *testing.T) {
	ent := McpServerRecord{CredentialMode: McpCredentialShared}
	if ent.Scope() != "enterprise" || ent.CredentialScope("team_a") != McpSharedScope {
		t.Fatal("shared enterprise")
	}
	ent.CredentialMode = McpCredentialPerTeam
	if ent.CredentialScope("team_a") != "team_a" || ent.CredentialScope("") != McpSharedScope {
		t.Fatal("per-team enterprise")
	}
	team := McpServerRecord{McpServer: McpServer{TeamID: "team_b"}}
	if team.Scope() != "team" || team.CredentialScope("team_a") != "team_b" {
		t.Fatal("team server")
	}
}

func TestTeamMcpServerRowState(t *testing.T) {
	pending := TeamMcpServerRow{Server: McpServerRecord{McpServer: McpServer{TeamID: "t", ReviewStatus: McpPendingReview}}}
	if pending.State() != "pending" {
		t.Fatal("pending")
	}
	on := TeamMcpServerRow{Server: McpServerRecord{McpServer: McpServer{ReviewStatus: McpApproved}}, Switch: &TeamMcpSwitch{Enabled: true}}
	if on.State() != "on" {
		t.Fatal("on")
	}
	on.Switch.Enabled = false
	if on.State() != "off" {
		t.Fatal("off")
	}
	if McpCredentialStateOf(McpAuthNone, nil) != McpCredNotRequired || McpCredentialStateOf(McpAuthStatic, nil) != McpCredMissing ||
		McpCredentialStateOf(McpAuthStatic, &McpCredentialRecord{Status: McpCredExpired}) != McpCredExpired {
		t.Fatal("credential state")
	}
}

func TestMcpUsageQueryValidate(t *testing.T) {
	now := time.Now()
	ok := McpUsageQuery{GroupBy: "server", View: "usage", From: now.Add(-time.Hour), To: now}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []McpUsageQuery{
		{GroupBy: "x", View: "usage", From: ok.From, To: now},
		{TeamID: "t", GroupBy: "team", View: "usage", From: ok.From, To: now},
		{GroupBy: "server", View: "x", From: ok.From, To: now},
		{TeamID: "t", GroupBy: "server", View: "adoption", From: ok.From, To: now},
		{GroupBy: "server", View: "unused", WindowDays: 7, From: ok.From, To: now},
		{GroupBy: "server", View: "usage", From: now, To: now},
	}
	for _, q := range bad {
		if q.Validate() == nil {
			t.Errorf("%+v accepted", q)
		}
	}
}

func TestMcpRuleDiffAndRefused(t *testing.T) {
	five := 5
	before := []McpPolicyRule{{Name: "a", Effect: "allow"}, {Name: "b", Effect: "deny"}}
	after := []McpPolicyRule{{Name: "a", Effect: "allow", RateLimitPerMinute: &five}, {Name: "b", Effect: "deny"}}
	if add, rm := McpRuleDiff(before, after); add != 1 || rm != 1 {
		t.Fatalf("diff %d %d", add, rm)
	}
	e := Refused("p", "not_team_server", "nope")
	if e.Reason != "not_team_server" || e.Message != "nope" {
		t.Fatalf("refused %+v", e)
	}
	if Refused("p", "r", "").Message == "" {
		t.Fatal("default message")
	}
}

func TestExternalSystemGovernedConflict(t *testing.T) {
	if (ExternalSystem{}).GovernedConflict() != nil {
		t.Fatal("ungoverned")
	}
	err := ExternalSystem{GovernedBy: "mcp_1"}.GovernedConflict()
	var de *Error
	if !errors.As(err, &de) || de.Reason != ReasonGovernedByMcpServer {
		t.Fatalf("conflict %v", err)
	}
}
