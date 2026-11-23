package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	okImage = "registry.example.com/agents/coder@sha256:4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	secA    = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

func okParams() NewAgentParams {
	return NewAgentParams{Name: "coder", Template: "claude-code", Image: okImage, ModelID: "anthropic.claude-sonnet"}
}

func TestNewAgent(t *testing.T) {
	now := time.Now()
	a, err := NewAgent("agt_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_x", okParams(), now)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != AgentEnabled || a.Generation != 1 || a.Version != 1 || a.IAMIdentity != IAMIdentityTeamShared ||
		a.Volume != DefaultAgentVolume() || a.Status.Phase != PhasePending || a.Paused() {
		t.Errorf("defaults: %+v", a)
	}
	tests := []struct {
		name  string
		mod   func(*NewAgentParams)
		field string
	}{
		{"image without digest", func(p *NewAgentParams) { p.Image = "registry.example.com/agents/coder:latest" }, "image"},
		{"image tag and digest text", func(p *NewAgentParams) { p.Image = "@sha256:" + strings.Repeat("a", 64) }, "image"},
		{"bad name", func(p *NewAgentParams) { p.Name = "Coder" }, "name"},
		{"no template", func(p *NewAgentParams) { p.Template = "" }, "template"},
		{"bad cpu", func(p *NewAgentParams) { p.Resources.CPU = "lots" }, "resources.cpu"},
		{"bad memory", func(p *NewAgentParams) { p.Resources.Memory = "2 gigs" }, "resources.memory"},
		{"bad identity", func(p *NewAgentParams) { p.IAMIdentity = "root" }, "iamIdentity"},
		{"volume too big", func(p *NewAgentParams) { p.Volume.SizeGiB = 1001 }, "volume.sizeGiB"},
		{"bad on delete", func(p *NewAgentParams) { p.Volume.OnDelete = "burn" }, "volume.onDelete"},
		{"bad schedule", func(p *NewAgentParams) { p.Volume.SnapshotSchedule = "hourly" }, "volume.snapshotSchedule"},
		{"retain too many", func(p *NewAgentParams) { p.Volume.SnapshotRetain = 91 }, "volume.snapshotRetain"},
		{"bad binding id", func(p *NewAgentParams) { p.Bindings = []SecretBinding{{"x", "env", "A"}} }, "secretBindings[0].secretId"},
		{"bad env target", func(p *NewAgentParams) { p.Bindings = []SecretBinding{{secA, "env", "lower"}} }, "secretBindings[0].target"},
		{"bad file target", func(p *NewAgentParams) { p.Bindings = []SecretBinding{{secA, "file", "relative"}} }, "secretBindings[0].target"},
		{"bad delivery", func(p *NewAgentParams) { p.Bindings = []SecretBinding{{secA, "pipe", "A"}} }, "secretBindings[0].delivery"},
		{"duplicate target", func(p *NewAgentParams) { p.Bindings = []SecretBinding{{secA, "env", "A"}, {secA, "env", "A"}} }, "secretBindings[1].target"},
		{"long description", func(p *NewAgentParams) { p.Description = strings.Repeat("d", 1001) }, "description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := okParams()
			tt.mod(&p)
			_, err := NewAgent("agt_1", "team_x", p, now)
			var e *Error
			if !errors.As(err, &e) || e.Kind != KindValidation {
				t.Fatalf("err = %v", err)
			}
			found := false
			for _, f := range e.Fields {
				found = found || f.Field == tt.field
			}
			if !found {
				t.Errorf("fields %v, want %s", e.Fields, tt.field)
			}
		})
	}
	good := okParams()
	good.Bindings = []SecretBinding{{secA, "env", "GITHUB_PAT"}, {secA, "file", "/etc/creds/pat"}}
	good.Volume = VolumeSpec{SizeGiB: 50, OnDelete: OnDeleteRetain}
	if g, err := NewAgent("agt_1", "team_x", good, now); err != nil || g.Volume.SizeGiB != 50 || g.Volume.StorageClass != "gp3" {
		t.Errorf("good: %+v %v", g, err)
	}
}

// RA-US-4.2: the volume grows but never shrinks and its storage class never changes.
func TestAgentApply(t *testing.T) {
	now := time.Now()
	base, _ := NewAgent("agt_1", "team_x", okParams(), now)
	s := func(v string) *string { return &v }
	tests := []struct {
		name  string
		p     AgentPatch
		field string
	}{
		{"shrink", AgentPatch{Volume: &VolumeSpec{SizeGiB: 10}}, "volume.sizeGiB"},
		{"class change", AgentPatch{Volume: &VolumeSpec{StorageClass: "io2-fast"}}, "volume.storageClass"},
		{"bad image", AgentPatch{Image: s("repo:latest")}, "image"},
		{"bad template", AgentPatch{Template: s("X")}, "template"},
		{"bad binding", AgentPatch{Bindings: &[]SecretBinding{{"x", "env", "A"}}}, "secretBindings[0].secretId"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := base.Apply(tt.p, now)
			var e *Error
			if !errors.As(err, &e) || e.Kind != KindValidation || e.Fields[0].Field != tt.field {
				t.Fatalf("err = %v", err)
			}
		})
	}
	grow := AgentPatch{Volume: &VolumeSpec{SizeGiB: 40, StorageClass: "gp3"}, Description: s("new"), Image: s(okImage),
		Resources: &AgentResources{CPU: "2"}, Bindings: &[]SecretBinding{{secA, "env", "PAT"}}}
	n, err := base.Apply(grow, now)
	if err != nil || n.Volume.SizeGiB != 40 || n.Generation != 2 || n.Description != "new" || len(n.Bindings) != 1 || n.Resources.CPU != "2" {
		t.Fatalf("grow: %+v %v", n, err)
	}
	same, err := n.Apply(AgentPatch{Volume: &VolumeSpec{SizeGiB: 40}}, now)
	if err != nil || same.Volume.SizeGiB != 40 {
		t.Errorf("same size: %v", err)
	}
	for _, st := range []AgentState{AgentDeleting, AgentDeleted} {
		d := base
		d.State = st
		if _, err := d.Apply(AgentPatch{Description: s("x")}, now); KindOf(err) != KindConflict {
			t.Errorf("%s: %v", st, err)
		}
	}
}

func TestAgentLifecycle(t *testing.T) {
	now := time.Now()
	a, _ := NewAgent("agt_1", "team_x", okParams(), now)
	p, changed, err := a.Pause(now)
	if err != nil || !changed || !p.Paused() || p.Generation != 2 {
		t.Fatalf("pause: %+v %v %v", p, changed, err)
	}
	if _, changed, err := p.Pause(now); err != nil || changed {
		t.Errorf("pause twice: %v %v", changed, err)
	}
	if _, err := p.Restart(now); KindOf(err) != KindConflict {
		t.Errorf("restart paused: %v", err)
	}
	r, changed, err := p.Resume(now)
	if err != nil || !changed || r.Paused() || r.Generation != 3 {
		t.Fatalf("resume: %+v %v", r, err)
	}
	if _, changed, _ := r.Resume(now); changed {
		t.Error("resume running must be a no-op")
	}
	rs, err := r.Restart(now)
	if err != nil || rs.Generation != 4 {
		t.Fatalf("restart: %+v %v", rs, err)
	}
	d, changed, err := rs.MarkDeleting(now)
	if err != nil || !changed || d.State != AgentDeleting || d.Status.Phase != PhaseDeleting {
		t.Fatalf("delete: %+v %v", d, err)
	}
	if _, changed, err := d.MarkDeleting(now); err != nil || changed {
		t.Errorf("delete twice: %v %v", changed, err)
	}
	if _, _, err := d.Pause(now); KindOf(err) != KindConflict {
		t.Errorf("pause deleting: %v", err)
	}
	if _, _, err := d.Resume(now); KindOf(err) != KindConflict {
		t.Errorf("resume deleting: %v", err)
	}
	if _, err := d.Restart(now); KindOf(err) != KindConflict {
		t.Errorf("restart deleting: %v", err)
	}
	gone := a
	gone.State = AgentDeleted
	if _, _, err := gone.MarkDeleting(now); KindOf(err) != KindNotFound {
		t.Errorf("delete deleted: %v", err)
	}
}
