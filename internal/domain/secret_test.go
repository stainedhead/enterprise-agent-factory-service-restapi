package domain

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretValueNeverPrints(t *testing.T) {
	v, err := NewSecretValue("value", "hunter2-example")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%v %+v %#v %s %q %x", v, v, v, v, v, v), slog.AnyValue(v).Resolve().String()} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("value leaked: %s", s)
		}
	}
	if _, err := json.Marshal(v); err == nil {
		t.Error("json.Marshal must fail")
	}
	if _, err := json.Marshal(map[string]any{"x": struct{ V SecretValue }{v}}); err == nil {
		t.Error("nested marshal must fail")
	}
	if v.Expose() != "hunter2-example" {
		t.Error("Expose")
	}
}

func TestNewSecretValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"one byte", "x", true},
		{"max", strings.Repeat("a", MaxSecretValueBytes), true},
		{"empty", "", false},
		{"over", strings.Repeat("a", MaxSecretValueBytes+1), false},
		{"bad utf8", "\xff\xfe", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewSecretValue("value", tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "\xff") {
				t.Error("error echoes the value")
			}
		})
	}
}

func TestNewSecretParamsValidate(t *testing.T) {
	val, _ := NewSecretValue("value", "v")
	agent := "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	pool := "pool_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	ok := NewSecretParams{Scope: SecretScopeTeam, Name: "github-pat", Value: val}
	tests := []struct {
		name string
		mod  func(*NewSecretParams)
		ok   bool
	}{
		{"team", func(*NewSecretParams) {}, true},
		{"agent", func(p *NewSecretParams) { p.Scope, p.AgentID = SecretScopeAgent, agent }, true},
		{"pool", func(p *NewSecretParams) { p.Scope, p.PoolID = SecretScopePool, pool }, true},
		{"agent without id", func(p *NewSecretParams) { p.Scope = SecretScopeAgent }, false},
		{"team with agent", func(p *NewSecretParams) { p.AgentID = agent }, false},
		{"pool with agent", func(p *NewSecretParams) { p.Scope, p.PoolID, p.AgentID = SecretScopePool, pool, agent }, false},
		{"agent with pool", func(p *NewSecretParams) { p.Scope, p.AgentID, p.PoolID = SecretScopeAgent, agent, pool }, false},
		{"bad scope", func(p *NewSecretParams) { p.Scope = "x" }, false},
		{"bad name", func(p *NewSecretParams) { p.Name = "Bad_Name" }, false},
		{"no value", func(p *NewSecretParams) { p.Value = SecretValue{} }, false},
		{"bad ext", func(p *NewSecretParams) { p.ExternalSystemID = "x" }, false},
		{"ext", func(p *NewSecretParams) { p.ExternalSystemID = "ext_01JD2Q3R4S5T6V7W8X9Y0Z0001" }, true},
		{"rotation on", func(p *NewSecretParams) { p.Rotation = Rotation{true, 90} }, true},
		{"rotation on no days", func(p *NewSecretParams) { p.Rotation = Rotation{true, 0} }, false},
		{"rotation off days", func(p *NewSecretParams) { p.Rotation = Rotation{false, 5} }, false},
		{"rotation 366", func(p *NewSecretParams) { p.Rotation = Rotation{true, 366} }, false},
		{"long description", func(p *NewSecretParams) { p.Description = strings.Repeat("a", 1001) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := ok
			tt.mod(&p)
			if err := p.Validate(); (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestSecretPatch(t *testing.T) {
	d := "new"
	tests := []struct {
		name string
		p    SecretPatch
		ok   bool
	}{
		{"empty", SecretPatch{}, false},
		{"description", SecretPatch{Description: &d}, true},
		{"clear ext", SecretPatch{ExternalSystemSet: true, ExternalSystemNull: true}, true},
		{"bad ext", SecretPatch{ExternalSystemSet: true, ExternalSystemID: "nope"}, false},
		{"rotation", SecretPatch{Rotation: &Rotation{true, 30}}, true},
		{"bad rotation", SecretPatch{Rotation: &Rotation{true, 0}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.p.Validate(); (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
	s := SecretPatch{Description: &d, ExternalSystemSet: true, ExternalSystemNull: true, Rotation: &Rotation{true, 7}}.Apply(Secret{ExternalSystemID: "ext_x"})
	if s.Description != "new" || s.ExternalSystemID != "" || s.Rotation.Days != 7 {
		t.Errorf("apply: %+v", s)
	}
}

func TestScopeOfAndBound(t *testing.T) {
	if ScopeOf("a", "") != SecretScopeAgent || ScopeOf("", "p") != SecretScopePool || ScopeOf("", "") != SecretScopeTeam {
		t.Error("ScopeOf")
	}
	e := SecretBound([]string{"agt_1"})
	if e.Kind != KindConflict || e.Details["agentIds"] == nil {
		t.Error("SecretBound")
	}
}
