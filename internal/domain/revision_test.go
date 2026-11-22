package domain

import (
	"encoding/json"
	"testing"
)

func rows(kv ...Row) map[string]Row {
	out := map[string]Row{}
	for _, r := range kv {
		out[r["id"].(string)] = r
	}
	return out
}

func TestPlanRestore(t *testing.T) {
	target := RestoreState{
		"teams":                   rows(Row{"id": "team_1", "display_name": "Old", "description": "d", "owner_email": "a@x", "cost_center": "cc"}),
		"agents":                  rows(Row{"id": "agt_1", "state": "enabled", "persona": "old"}, Row{"id": "agt_gone", "state": "enabled"}),
		"team_tags":               rows(Row{"id": "team_1/env", "value": "prod"}, Row{"id": "team_1/gone", "value": "x"}),
		"namespace_config_values": rows(Row{"id": "ncv_1", "config_value": "1", "description": ""}),
		"budgets":                 rows(Row{"id": "bud_1", "amount_usd": json.Number("400.00"), "period": "monthly"}),
	}
	current := RestoreState{
		"teams":                   rows(Row{"id": "team_1", "display_name": "New", "description": "d", "owner_email": "a@x", "cost_center": "cc"}),
		"agents":                  rows(Row{"id": "agt_1", "state": "paused", "persona": "old"}, Row{"id": "agt_new", "state": "enabled"}),
		"team_tags":               rows(Row{"id": "team_1/env", "value": "dev"}, Row{"id": "team_1/extra", "value": "y"}),
		"namespace_config_values": rows(Row{"id": "ncv_1", "config_value": "1", "description": ""}, Row{"id": "ncv_2", "config_value": "2"}),
		"budgets":                 rows(Row{"id": "bud_1", "amount_usd": json.Number("400.00"), "period": "monthly"}),
	}
	got := PlanRestore(target, current)
	type line struct {
		kind, id string
		act      RestoreAction
	}
	var have []line
	for _, s := range got {
		have = append(have, line{s.Change.ResourceKind, s.Change.ResourceID, s.Change.Action})
	}
	want := []line{
		{"teams", "team_1", RestoreUpdate},
		{"agents", "agt_1", RestoreUpdate},
		{"agents", "agt_gone", RestoreSkip},
		{"agents", "agt_new", RestoreSkip},
		{"team_tags", "team_1/env", RestoreUpdate},
		{"team_tags", "team_1/gone", RestoreCreate},
		// deletes come last, in reverse spec order
		{"team_tags", "team_1/extra", RestoreDelete},
		{"namespace_config_values", "ncv_2", RestoreDelete},
	}
	if len(have) != len(want) {
		t.Fatalf("%v", have)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Fatalf("step %d: %v, want %v\nall: %v", i, have[i], want[i], have)
		}
	}
	if f := got[0].Change.Fields; len(f) != 1 || f[0].Field != "display_name" || f[0].From != "New" || f[0].To != "Old" {
		t.Fatalf("fields %+v", f)
	}
	if got[2].Change.Reason == "" || got[3].Change.Reason == "" {
		t.Fatal("a skip needs a reason")
	}
	if len(PlanRestore(target, target)) != 0 {
		t.Fatal("identical states must plan nothing")
	}
}

func TestPlanRestoreNeverBringsBackADeletingAgent(t *testing.T) {
	s := PlanRestore(RestoreState{"agents": rows(Row{"id": "agt_1", "state": "deleted"})}, RestoreState{"agents": rows(Row{"id": "agt_1", "state": "enabled"})})
	if len(s) != 1 || s[0].Change.Action != RestoreSkip {
		t.Fatalf("%+v", s)
	}
}

func TestRestoreSpecsAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range RestoreKinds() {
		if seen[k] {
			t.Fatal("duplicate " + k)
		}
		seen[k] = true
	}
	for _, k := range []string{"teams", "agents", "namespace_config_values", "agent_config_values", "external_system_optouts", "team_tags", "budgets"} {
		if !seen[k] {
			t.Errorf("%s is not restored", k)
		}
	}
}
