package agent_test

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

func withBinding() agent.Desired {
	d := desired()
	d.Bindings = []agent.Binding{
		{SecretID: "sec_A", SecretName: "gh", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-alpha/gh-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "GH_TOKEN"},
	}
	d.Config = []agent.ConfigValue{{Name: "LOG_LEVEL", Value: "info"}, {Name: "REGION", Value: "eu"}}
	return d
}

// M2-F4: the hash follows the referenced versions and nothing else.
func TestPodTemplateHash(t *testing.T) {
	base := agent.PodTemplateHash(withBinding())
	if base != agent.PodTemplateHash(withBinding()) {
		t.Fatal("hash is not deterministic")
	}
	tests := []struct {
		name    string
		mutate  func(d *agent.Desired)
		changes bool
	}{
		{"secret rotated", func(d *agent.Desired) { d.Bindings[0].VersionID = "v2" }, true},
		{"config value changed", func(d *agent.Desired) { d.Config[0].Value = "debug" }, true},
		{"config value added", func(d *agent.Desired) { d.Config = append(d.Config, agent.ConfigValue{Name: "X", Value: "1"}) }, true},
		{"binding retargeted", func(d *agent.Desired) { d.Bindings[0].Target = "TOKEN" }, true},
		{"image", func(d *agent.Desired) { d.Image = "registry.example.com/hermes@sha256:def" }, true},
		{"persona", func(d *agent.Desired) { d.Persona = "terse" }, true},
		{"resources", func(d *agent.Desired) { d.Resources.CPU = "2" }, true},
		{"tag changed", func(d *agent.Desired) { d.Tags["cost-center"] = "1234" }, true},
		{"role link added", func(d *agent.Desired) {
			d.RoleLinks = []agent.RoleLink{{ProfileName: "prod", RoleARN: "arn:aws:iam::444455556666:role/x", ExternalID: "afs-0123456789abcdefghij"}}
		}, true},
		{"work order assigned", func(d *agent.Desired) { d.Team.WorkOrderAssigned = true }, true},
		{"config order only", func(d *agent.Desired) { d.Config[0], d.Config[1] = d.Config[1], d.Config[0] }, false},
		{"paused", func(d *agent.Desired) { d.State = agent.StatePaused }, false},
		{"team suspended", func(d *agent.Desired) { d.Team.State = "suspended" }, false},
		{"restart nonce", func(d *agent.Desired) { d.RestartNonce = "42" }, false},
		{"volume grown", func(d *agent.Desired) { d.Volume.SizeGiB = 50 }, false},
		{"description", func(d *agent.Desired) { d.Description = "x" }, false},
		{"secret for an off system", func(d *agent.Desired) {
			d.Bindings = append(d.Bindings, agent.Binding{SecretID: "sec_B", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "OKTA_TOKEN", ExternalSystemID: "ext_1", SystemOff: true})
		}, false},
		{"disabled role link", func(d *agent.Desired) {
			d.RoleLinks = []agent.RoleLink{{ProfileName: "prod", RoleARN: "arn:aws:iam::444455556666:role/x", Status: "disabled"}}
		}, false},
	}
	for _, tt := range tests {
		d := withBinding()
		tt.mutate(&d)
		if got := agent.PodTemplateHash(d) != base; got != tt.changes {
			t.Errorf("%s: hash changed = %v, want %v", tt.name, got, tt.changes)
		}
	}
}

// The agent that does not reference a rotated secret keeps its hash.
func TestRotationRollsOnlyReferencingAgents(t *testing.T) {
	a, b := withBinding(), desired()
	b.ID, b.Name = "agt_01JD0000000000000000000022", "other"
	hb := agent.PodTemplateHash(b)
	a.Bindings[0].VersionID = "v2" // the rotation
	if agent.PodTemplateHash(b) != hb {
		t.Error("an unrelated agent's hash moved")
	}
	if agent.PodTemplateHash(a) == agent.PodTemplateHash(withBinding()) {
		t.Error("the referencing agent's hash did not move")
	}
}

func TestBindingDelivered(t *testing.T) {
	tests := []struct {
		b    agent.Binding
		want bool
	}{
		{agent.Binding{}, true},
		{agent.Binding{ExternalSystemID: "ext_1"}, true},
		{agent.Binding{ExternalSystemID: "ext_1", SystemOff: true}, false},
		{agent.Binding{SystemOff: true}, true}, // names no system, so nothing switches it off
	}
	for _, tt := range tests {
		if tt.b.Delivered() != tt.want {
			t.Errorf("%+v Delivered = %v", tt.b, !tt.want)
		}
	}
}

func TestNames(t *testing.T) {
	if agent.PVCName("reviewer") != "data-agent-reviewer-0" || agent.ServiceAccountName("reviewer") != "agent-reviewer" {
		t.Error("workload names drifted from the resolve_workload convention")
	}
	if n, err := agent.RoleName("agt_01JD0000000000000000000021"); err != nil || n != "afs-01JD0000000000000000000021" {
		t.Errorf("RoleName = %q, %v", n, err)
	}
	if _, err := agent.RoleName("nope"); err == nil {
		t.Error("RoleName accepted an unprefixed id")
	}
	if got := (agent.Gateway{Namespace: "afs-system", Name: "afs-gateway", Port: 8443}).URL(); got != "https://afs-gateway.afs-system.svc:8443" {
		t.Errorf("gateway url = %s", got)
	}
	if agent.SnapshotClassName("agt_01JD0000000000000000000021") != "afs-01jd0000000000000000000021" {
		t.Error("snapshot class name")
	}
}
