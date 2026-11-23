package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) *Extract {
	t.Helper()
	c, err := os.ReadFile("testdata/api-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.ReadFile("testdata/prd-6.13.md")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := extract(c, p)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// TestGolden: the fixture copy of the contract renders exactly the committed files.
func TestGolden(t *testing.T) {
	files, err := render(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		if name == "api/contract/SOURCE" {
			continue // the fixture PRD is an excerpt, so its hash differs from the full PRD's
		}
		got, err := os.ReadFile(filepath.Join("..", "..", name)) //nolint:gosec // names are the generator's own outputs
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from the fixture's rendering; run make contract", name)
		}
	}
}

func TestOperations(t *testing.T) {
	ex := fixture(t)
	if len(ex.Operations) != 194 {
		t.Fatalf("operations = %d, want 194", len(ex.Operations))
	}
	byID := map[string]Operation{}
	for _, op := range ex.Operations {
		byID[op.OperationID] = op
	}
	cases := []struct {
		id, method, path, perm, scope string
	}{
		{"GetMe", "GET", "/v1/me", "", "any"},
		{"ListSignInProviders", "GET", "/v1/identity-providers", "", "public"},
		{"CreateTeam", "POST", "/v1/teams", "teams:create", "service"},
		{"ResumeTeam", "POST", "/v1/teams/{teamId}:resume", "teams:suspend", "team"},
		{"PutTeamModels", "PUT", "/v1/teams/{teamId}/models", "teams:set-guardrails", "team"},
		{"RestartAgent", "POST", "/v1/teams/{teamId}/agents/{agentId}:restart", "agents:write", "team"},
		{"GetTeamReport", "GET", "/v1/teams/{teamId}/reports/{kind}", "reports:read", "team"},
		{"ListTeamDirectory", "GET", "/v1/team-directory", "teams:read-directory", "service"},
		{"ListModels", "GET", "/v1/models", "models:read", "any"},
		{"ListSwarmTemplates", "GET", "/v1/admin/swarm-templates", "swarm-templates:read", "service"},
		{"CreateImport", "POST", "/v1/imports", "admin:export", "service"},
		{"GetReadiness", "GET", "/readyz", "", "public"},
	}
	for _, c := range cases {
		op, ok := byID[c.id]
		if !ok {
			t.Errorf("%s missing", c.id)
			continue
		}
		if op.Method != c.method || op.Path != c.path || op.Permission != c.perm || op.Scope != c.scope {
			t.Errorf("%s = %s %s %q %s, want %s %s %q %s", c.id, op.Method, op.Path, op.Permission, op.Scope, c.method, c.path, c.perm, c.scope)
		}
	}
	if op := byID["UpdateTeam"]; op.FieldPermissions["guardrails"] != "teams:set-guardrails" || op.FieldPermissions["allowedAwsAccounts"] != "teams:set-aws-accounts" {
		t.Errorf("UpdateTeam field permissions = %v", op.FieldPermissions)
	}
	if op := byID["DeclareTeamExternalSystem"]; op.Condition != "mayAddExternalSystems" {
		t.Errorf("DeclareTeamExternalSystem condition = %q", op.Condition)
	}
	if op := byID["UpdateWorkOrderRepo"]; len(op.AnyOf) != 2 || op.Scope != "any" {
		t.Errorf("UpdateWorkOrderRepo = %+v", op)
	}
	if op := byID["ListWorkOrders"]; !op.CSV || op.Condition != "assignedOrOwnRepos" || strings.Join(op.Query, ",") != "status,severity,slaState,teamId" {
		t.Errorf("ListWorkOrders = %+v", op)
	}
	if op := byID["ResumeAgent"]; op.CLI != "agents resume-agent" || op.SDK != "agents.resume()" {
		t.Errorf("ResumeAgent cli/sdk = %q %q", op.CLI, op.SDK)
	}
	if op := byID["PutTeamModels"]; op.CLI != "teams put-team-models" || op.SDK != "teams.models.put()" {
		t.Errorf("PutTeamModels cli/sdk = %q %q", op.CLI, op.SDK)
	}
	csv := 0
	for _, op := range ex.Operations {
		if op.CSV {
			csv++
		}
	}
	if csv != 10 {
		t.Errorf("CSV operations = %d, want 10", csv)
	}
}

func TestPermissionsErrorsEventsSettings(t *testing.T) {
	ex := fixture(t)
	if len(ex.Permissions) != 63 {
		t.Errorf("permissions = %d", len(ex.Permissions))
	}
	if len(ex.Errors) != 13 || len(ex.Events) != 68 || len(ex.Settings) != 19 {
		t.Errorf("errors %d events %d settings %d", len(ex.Errors), len(ex.Events), len(ex.Settings))
	}
	gw := 0
	for _, e := range ex.Errors {
		if e.GatewayOnly {
			gw++
		}
	}
	if gw != 2 {
		t.Errorf("gateway-only codes = %d, want 2 (BudgetExceeded, UpstreamError)", gw)
	}
	b, _ := json.Marshal(ex.Settings)
	for _, want := range []string{`"key":"api.cors_origins","type":"array"`, `"key":"work_orders.notification_channel_id","type":"string","default":null,"nullable":true`, `"min":60,"max":604800`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("settings missing %s", want)
		}
	}
}

func TestSplitRowAndHelpers(t *testing.T) {
	if got := splitRow("| a | b \\| c | `d` |"); strings.Join(got, "#") != "a#b | c#`d`" {
		t.Errorf("splitRow = %q", got)
	}
	if goName("teams:set-aws-accounts") != "TeamsSetAWSAccounts" || goName("api-keys:read") != "APIKeysRead" || goName("team.tags_changed") != "TeamTagsChanged" {
		t.Error("goName")
	}
	if grantCondition("✓ when `viewerKubectl`") != "viewerKubectl" || grantCondition("✓") != "" || grantCondition("✓ all") != "" {
		t.Error("grantCondition")
	}
	if _, err := extract([]byte("Contract v9.9.9"), nil); err == nil {
		t.Error("wrong contract version must fail")
	}
}

// CD-8: a database-only setting (default "unset") is in PRD §6.13 but GetSettings does not expose it, so
// the extract skips it instead of failing.
func TestParseSettingsSkipsDatabaseOnlyKeys(t *testing.T) {
	lines := strings.Split("### 6.13 Service settings\n\n| Key | Default | Allowed | Used by | Since |\n|---|---|---|---|---|\n"+
		"| `reports.idle_cpu_millicores` | `50` | 1 to 4000 | RPT-5 | 000013 |\n"+
		"| `work_orders.cost_center` | unset | string of 1 to 100 characters | WO-1 | 000016 |\n"+
		"| `work_orders.owner_email` | unset | string `local@domain` | WO-1 | 000016 |\n", "\n")
	got, err := parseSettings(lines)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "reports.idle_cpu_millicores" {
		t.Fatalf("settings = %+v, want only reports.idle_cpu_millicores", got)
	}
}
