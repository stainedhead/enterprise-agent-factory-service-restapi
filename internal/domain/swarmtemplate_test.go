package domain

import (
	"strings"
	"testing"
	"time"
)

func validSpec() map[string]any {
	return map[string]any{
		"stages": []any{map[string]any{"name": "scan", "position": float64(1)}},
		"pools": []any{
			map[string]any{"name": "fixer", "stage": "scan", "template": "coder", "image": "registry.example.com/a@sha256:" + strings.Repeat("a", 64), "modelId": "m1", "workers": float64(2)},
			map[string]any{"name": "reviewer", "template": "coder", "image": "registry.example.com/a@sha256:" + strings.Repeat("a", 64), "modelId": "m1"},
		},
		"handoffs":     []any{map[string]any{"from": "fixer", "to": "reviewer"}},
		"configValues": []any{map[string]any{"pool": "fixer", "name": "TARGET", "value": "x"}},
		"secrets":      []any{map[string]any{"name": "gh-app", "pool": "fixer"}},
	}
}

func TestValidateSwarmSpec(t *testing.T) {
	mut := func(f func(s map[string]any)) map[string]any { s := validSpec(); f(s); return s }
	pool0 := func(s map[string]any) map[string]any { return s["pools"].([]any)[0].(map[string]any) }
	tests := []struct {
		name string
		spec map[string]any
		want string // field substring of the first error, "" for valid
	}{
		{"valid", validSpec(), ""},
		{"nil", nil, "spec"},
		{"no pools", mut(func(s map[string]any) { s["pools"] = []any{} }), "spec.pools"},
		{"bad pool name", mut(func(s map[string]any) { pool0(s)["name"] = "Bad" }), "pools[0].name"},
		{"dup pool", mut(func(s map[string]any) { s["pools"].([]any)[1].(map[string]any)["name"] = "fixer" }), "pools[1].name"},
		{"unpinned image", mut(func(s map[string]any) { pool0(s)["image"] = "x:latest" }), "pools[0].image"},
		{"no model", mut(func(s map[string]any) { delete(pool0(s), "modelId") }), "pools[0].modelId"},
		{"unknown stage", mut(func(s map[string]any) { pool0(s)["stage"] = "nope" }), "pools[0].stage"},
		{"workers", mut(func(s map[string]any) { pool0(s)["workers"] = float64(201) }), "pools[0].workers"},
		{"fractional workers", mut(func(s map[string]any) { pool0(s)["workers"] = 1.5 }), "pools[0].workers"},
		{"shared volume", mut(func(s map[string]any) { pool0(s)["sharedVolume"] = "nfs" }), "sharedVolume"},
		{"handoff unknown", mut(func(s map[string]any) { s["handoffs"] = []any{map[string]any{"from": "fixer", "to": "x"}} }), "handoffs[0].to"},
		{"handoff self", mut(func(s map[string]any) { s["handoffs"] = []any{map[string]any{"from": "fixer", "to": "fixer"}} }), "handoffs[0]"},
		{"config pool", mut(func(s map[string]any) { s["configValues"] = []any{map[string]any{"pool": "x", "name": "A"}} }), "configValues[0].pool"},
		{"config credential name", mut(func(s map[string]any) {
			s["configValues"] = []any{map[string]any{"pool": "fixer", "name": "API_TOKEN"}}
		}), "configValues[0].name"},
		{"secret value", mut(func(s map[string]any) { s["secrets"] = []any{map[string]any{"name": "gh-app", "value": "hunter2"}} }), "secrets[0].value"},
		{"tags", mut(func(s map[string]any) { s["tags"] = map[string]any{"a": 1} }), "tags.a"},
		{"stage position", mut(func(s map[string]any) { s["stages"].([]any)[0].(map[string]any)["position"] = float64(99) }), "stages[0].position"},
		{"pools not list", mut(func(s map[string]any) { s["handoffs"] = "x" }), "handoffs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fe := ValidateSwarmSpec("spec", tc.spec)
			if tc.want == "" {
				if len(fe) != 0 {
					t.Fatalf("unexpected %v", fe)
				}
				return
			}
			if len(fe) == 0 || !strings.Contains(fe[0].Field, tc.want) {
				t.Fatalf("got %v, want field containing %q", fe, tc.want)
			}
		})
	}
}

func TestSwarmTemplateLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	p := NewSwarmTemplateParams{Name: "cve-fix", Kind: "cve_remediation", Spec: validSpec(), BudgetUSD: "500.00", TTLHours: 48, TeardownRule: "at_work_complete"}
	tpl, err := NewSwarmTemplate("swt_x", p, now)
	if err != nil || !tpl.Enabled || tpl.Version != 1 || tpl.Budget.String() != "500.00" {
		t.Fatalf("%+v %v", tpl, err)
	}
	for name, mut := range map[string]func(*NewSwarmTemplateParams){
		"name":   func(p *NewSwarmTemplateParams) { p.Name = "X" },
		"kind":   func(p *NewSwarmTemplateParams) { p.Kind = "other" },
		"budget": func(p *NewSwarmTemplateParams) { p.BudgetUSD = "0" },
		"money":  func(p *NewSwarmTemplateParams) { p.BudgetUSD = "abc" },
		"ttl":    func(p *NewSwarmTemplateParams) { p.TTLHours = 0 },
		"rule":   func(p *NewSwarmTemplateParams) { p.TeardownRule = "never" },
		"desc":   func(p *NewSwarmTemplateParams) { p.Description = strings.Repeat("x", 1001) },
	} {
		q := p
		mut(&q)
		if _, err := NewSwarmTemplate("swt_x", q, now); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	off := false
	p.Enabled = &off
	if d, _ := NewSwarmTemplate("swt_x", p, now); d.Enabled {
		t.Error("enabled false ignored")
	}

	if !(SwarmTemplatePatch{}).Empty() {
		t.Error("empty patch")
	}
	b, ttl, rule := "10.00", 10, "at_expiry"
	u, err := tpl.Apply(SwarmTemplatePatch{BudgetUSD: &b, TTLHours: &ttl, TeardownRule: &rule}, now.Add(time.Hour))
	if err != nil || u.Budget.String() != "10.00" || u.TTLHours != 10 || u.TeardownRule != "at_expiry" {
		t.Fatalf("%+v %v", u, err)
	}
	bad, zero := "x", 0
	if _, err := tpl.Apply(SwarmTemplatePatch{BudgetUSD: &bad, TTLHours: &zero, Spec: map[string]any{}}, now); err == nil {
		t.Error("bad patch accepted")
	}
	if tpl.WithEnabled(false, now).Enabled {
		t.Error("WithEnabled")
	}
	if tpl.Audit()["pools"] != 2 {
		t.Errorf("audit %v", tpl.Audit())
	}
}
