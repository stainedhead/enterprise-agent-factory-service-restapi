package restore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents/agentstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

const (
	d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func img(d string) string { return "registry.example.com/agents/coder@" + d }

// countingGuard counts the queries the guard checks make.
type countingGuard struct {
	*agentstest.Repo
	templates, models, classes, secrets int
}

func (c *countingGuard) Template(ctx context.Context, tx port.Tx, n string) (agents.TemplateInfo, bool, error) {
	c.templates++
	return c.Repo.Template(ctx, tx, n)
}

func (c *countingGuard) ModelEnabled(ctx context.Context, tx port.Tx, t, m string) (bool, error) {
	c.models++
	return c.Repo.ModelEnabled(ctx, tx, t, m)
}

func (c *countingGuard) StorageClassEnabled(ctx context.Context, tx port.Tx, n string) (bool, error) {
	c.classes++
	return c.Repo.StorageClassEnabled(ctx, tx, n)
}

func (c *countingGuard) Secrets(ctx context.Context, tx port.Tx, t string, ids []string) (map[string]agents.SecretInfo, error) {
	c.secrets++
	return c.Repo.Secrets(ctx, tx, t, ids)
}

// guardRig is a rig whose single agent is now on the current spec and would be restored to target.
func guardRig(cur, target domain.Row) (*rig, *countingGuard) {
	r := newRig()
	g := &countingGuard{Repo: agentstest.NewRepo()}
	g.Models[team] = []string{"model.ok"}
	g.Templates["coder"] = agents.TemplateInfo{ID: "tpl_1", Name: "coder", Enabled: true, AllowedDigests: []string{d2}}
	g.Templates["retired"] = agents.TemplateInfo{ID: "tpl_2", Name: "retired", Enabled: false, AllowedDigests: []string{d1, d2}}
	r.svc.Guards = g
	r.repo.Targets = domain.RestoreState{"agents": {"agt_1": target}}
	r.repo.Now = domain.RestoreState{"agents": {"agt_1": cur}}
	return r, g
}

func agentRow(tpl, image, model, persona string) domain.Row {
	return domain.Row{"id": "agt_1", "state": "enabled", "template": tpl, "image": image, "model_id": model, "persona": persona}
}

// TestGuardedAgentStepsAreSkipped is FR-R01 AC1 and AC2: a restore never writes a template, digest or
// model the agents use case would refuse; the whole agent step is reported in skipped, naming the guard.
func TestGuardedAgentStepsAreSkipped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target domain.Row
		reason string
	}{
		{"digest removed from the template", agentRow("coder", img(d1), "model.ok", "old"), "template guard (digest)"},
		{"template disabled", agentRow("retired", img(d2), "model.ok", "old"), "template guard"},
		{"template gone", agentRow("vanished", img(d2), "model.ok", "old"), "template guard"},
		{"model not enabled for the team", agentRow("coder", img(d2), "model.off", "old"), "model guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := guardRig(agentRow("coder", img(d2), "model.ok", "new"), tc.target)
			res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.repo.Applied) != 0 || len(res.Changes) != 0 {
				t.Fatalf("a guarded step was applied: %+v", r.repo.Applied)
			}
			if len(res.Skipped) != 1 || res.Skipped[0].ResourceID != "agt_1" || res.Skipped[0].Action != domain.RestoreSkip ||
				!strings.Contains(res.Skipped[0].Reason, tc.reason) {
				t.Fatalf("skipped = %+v, want a reason naming %q", res.Skipped, tc.reason)
			}
			if got := r.audit.Entries[0].Record.After.(map[string]any)["skipped"]; got != 1 {
				t.Errorf("audit counts %v skipped", got)
			}
		})
	}
}

// TestGuardedStorageClassIsSkipped covers the storage class check. RestoreSpecs does not restore
// storage_class today (a volume class is not rewound), so the field is added for this test: the guard
// must hold if the spec ever covers it (FR-R01 AC2).
func TestGuardedStorageClassIsSkipped(t *testing.T) {
	for i := range domain.RestoreSpecs {
		if domain.RestoreSpecs[i].Kind == "agents" {
			orig := domain.RestoreSpecs[i].Fields
			domain.RestoreSpecs[i].Fields = append(append([]string{}, orig...), "storage_class")
			defer func() { domain.RestoreSpecs[i].Fields = orig }()
		}
	}
	cur, target := agentRow("coder", img(d2), "model.ok", "x"), agentRow("coder", img(d2), "model.ok", "x")
	cur["storage_class"], target["storage_class"] = "gp3", "io9"
	r, _ := guardRig(cur, target)
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.repo.Applied) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "storage class guard") {
		t.Fatalf("applied %+v skipped %+v", r.repo.Applied, res.Skipped)
	}
}

// TestUnguardedFieldsStillRestore is FR-R01 AC4: a revision that passes the guards, or changes only
// fields with no guard, applies.
func TestUnguardedFieldsStillRestore(t *testing.T) {
	r, g := guardRig(agentRow("coder", img(d2), "model.ok", "new"), agentRow("coder", img(d2), "model.ok", "old"))
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil)
	if err != nil || len(res.Changes) != 1 || len(res.Skipped) != 0 || len(r.repo.Applied) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if g.templates+g.models+g.classes != 0 {
		t.Errorf("a persona-only change queried the guards: %+v", g)
	}
	// A guarded field that passes is applied too.
	r, _ = guardRig(agentRow("coder", img(d2), "model.ok", "x"), agentRow("coder", img(d2), "model.ok2", "x"))
	r.svc.Guards.(*countingGuard).Models[team] = []string{"model.ok", "model.ok2"}
	if res, err = r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil); err != nil || len(res.Changes) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

// TestDryRunReportsTheSameSkipsAndWritesNothing is FR-R01 AC3.
func TestDryRunReportsTheSameSkipsAndWritesNothing(t *testing.T) {
	r, _ := guardRig(agentRow("coder", img(d2), "model.ok", "new"), agentRow("coder", img(d1), "model.ok", "old"))
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, true, nil, nil)
	if err != nil || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "template guard") {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.repo.Applied) != 0 || len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0 {
		t.Fatal("a dry run wrote something")
	}
}

// TestBindingRestoreIsGuarded: a binding restored to a secret that belongs to another agent or a pool is
// skipped, as CreateAgent would refuse it.
func TestBindingRestoreIsGuarded(t *testing.T) {
	r, g := guardRig(agentRow("coder", img(d2), "model.ok", "x"), agentRow("coder", img(d2), "model.ok", "x"))
	g.SecretsBy["sec_ok"] = agents.SecretInfo{}
	g.SecretsBy["sec_pool"] = agents.SecretInfo{PoolID: "pool_1"}
	g.SecretsBy["sec_other"] = agents.SecretInfo{AgentID: "agt_9"}
	bid := func(target string) string { return team + "/agt_1/" + target }
	row := func(secret, target string) domain.Row {
		return domain.Row{"team_id": team, "agent_id": "agt_1", "target": target, "secret_id": secret}
	}
	r.repo.Targets["agent_secret_bindings"] = map[string]domain.Row{
		bid("A"): row("sec_ok", "A"), bid("B"): row("sec_pool", "B"), bid("C"): row("sec_other", "C"), bid("D"): row("sec_gone", "D")}
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || res.Changes[0].ResourceID != bid("A") || len(res.Skipped) != 3 {
		t.Fatalf("changes %+v skipped %+v", res.Changes, res.Skipped)
	}
	for _, s := range res.Skipped {
		if !strings.Contains(s.Reason, "binding guard") {
			t.Errorf("reason %q", s.Reason)
		}
	}
	if g.secrets != 1 {
		t.Errorf("%d secret queries for 4 bindings, want 1", g.secrets)
	}
}

// TestGuardQueriesDoNotScaleWithAgents is the FR-R01 performance NFR: 200 agents on the same template,
// model and class cost one lookup each, not one per agent or per field.
func TestGuardQueriesDoNotScaleWithAgents(t *testing.T) {
	r, g := guardRig(nil, nil)
	cur, target := domain.RestoreState{"agents": {}}, domain.RestoreState{"agents": {}}
	for i := 0; i < 200; i++ {
		id := "agt_" + strings.Repeat("0", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		cur["agents"][id] = domain.Row{"id": id, "state": "enabled", "template": "coder", "image": img(d2), "model_id": "model.ok", "persona": "new"}
		target["agents"][id] = domain.Row{"id": id, "state": "enabled", "template": "coder", "image": img(d2), "model_id": "model.ok", "persona": "old"}
		target["agents"][id]["model_id"] = "model.ok"
		cur["agents"][id]["model_id"] = "model.ok2"
	}
	g.Models[team] = []string{"model.ok", "model.ok2"}
	r.repo.Targets, r.repo.Now = target, cur
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil)
	if err != nil || len(res.Changes) != 200 {
		t.Fatalf("%d changes, %v", len(res.Changes), err)
	}
	if g.templates > 1 || g.models > 1 || g.classes > 1 {
		t.Fatalf("lookups per run: templates %d models %d classes %d", g.templates, g.models, g.classes)
	}
}

func TestRestoreRefusesToRunWithoutAGuardSource(t *testing.T) {
	r := newRig()
	r.svc.Guards = nil
	if _, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil); err == nil {
		t.Fatal("a restore without a guard source must fail closed")
	}
	var _ restore.Repository = r.repo
}
