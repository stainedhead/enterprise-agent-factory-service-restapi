package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

var poolNow = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

const poolImage = "registry.example.com/agents/coder@sha256:4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"

func okPool() domain.NewPoolParams {
	return domain.NewPoolParams{Name: "fixer", Template: "claude-code", Image: poolImage, ModelID: "m1", Workers: 3}
}

func fieldOf(err error) string {
	if err == nil {
		return ""
	}
	fs := domain.FieldErrors(err)
	if len(fs) == 0 {
		return ""
	}
	return fs[0].Field
}

func TestNewPool(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*domain.NewPoolParams)
		field  string
	}{
		{"defaults", func(*domain.NewPoolParams) {}, ""},
		{"zero workers", func(p *domain.NewPoolParams) { p.Workers = 0 }, ""},
		{"200 workers", func(p *domain.NewPoolParams) { p.Workers = 200 }, ""},
		{"201 workers", func(p *domain.NewPoolParams) { p.Workers = 201 }, "workers"},
		{"negative workers", func(p *domain.NewPoolParams) { p.Workers = -1 }, "workers"},
		{"scratch 501", func(p *domain.NewPoolParams) { p.ScratchSizeGiB = 501 }, "scratchSizeGiB"},
		{"scratch 500", func(p *domain.NewPoolParams) { p.ScratchSizeGiB = 500 }, ""},
		{"bad shared volume", func(p *domain.NewPoolParams) { p.SharedVolume = "ebs" }, "sharedVolume"},
		{"efs", func(p *domain.NewPoolParams) { p.SharedVolume = "efs" }, ""},
		{"bad name", func(p *domain.NewPoolParams) { p.Name = "Fixer" }, "name"},
		{"image without digest", func(p *domain.NewPoolParams) { p.Image = "registry.example.com/a:latest" }, "image"},
		{"bad template", func(p *domain.NewPoolParams) { p.Template = "X" }, "template"},
		{"no model", func(p *domain.NewPoolParams) { p.ModelID = " " }, "modelId"},
		{"bad stage", func(p *domain.NewPoolParams) { p.StageID = "agt_x" }, "stageId"},
		{"long description", func(p *domain.NewPoolParams) { p.Description = strings.Repeat("x", 1001) }, "description"},
		{"bad cpu", func(p *domain.NewPoolParams) { p.Resources.CPU = "lots" }, "resources.cpu"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := okPool()
			tc.mutate(&p)
			got, err := domain.NewPool("pool_01JD2Q3R4S5T6V7W8X9Y0Z0001", "team_1", p, poolNow)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				if got.ScratchSizeGiB == 0 || got.SharedVolume == "" || got.State != domain.PoolStateEnabled || got.Generation != 1 {
					t.Fatalf("%+v", got)
				}
				return
			}
			if domain.KindOf(err) != domain.KindValidation || fieldOf(err) != tc.field {
				t.Fatalf("want %s: %v", tc.field, err)
			}
		})
	}
}

func TestPoolApplyScaleAndLifecycle(t *testing.T) {
	p, _ := domain.NewPool("pool_1", "team_1", okPool(), poolNow)
	desc, w := "d", 9
	n, err := p.Apply(domain.PoolPatch{Description: &desc, Workers: &w, StageID: domain.OptionalString{Set: true}}, poolNow)
	if err != nil || n.Workers != 9 || n.Generation != 2 {
		t.Fatalf("%+v %v", n, err)
	}
	bad := 300
	if _, err := p.Apply(domain.PoolPatch{Workers: &bad}, poolNow); domain.KindOf(err) != domain.KindValidation {
		t.Fatal(err)
	}
	sid := "nope"
	if _, err := p.Apply(domain.PoolPatch{StageID: domain.OptionalString{Set: true, Value: &sid}}, poolNow); fieldOf(err) != "stageId" {
		t.Fatal(err)
	}
	for _, c := range []struct {
		n    int
		want domain.ErrorKind
	}{{5, ""}, {0, ""}, {200, ""}, {201, domain.KindValidation}, {-1, domain.KindValidation}} {
		if _, _, err := p.Scale(c.n, poolNow); domain.KindOf(err) != c.want {
			t.Errorf("scale %d: %v", c.n, err)
		}
	}
	if _, changed, _ := p.Scale(3, poolNow); changed {
		t.Error("same count is a no-op")
	}
	paused, changed, err := p.Pause(poolNow)
	if err != nil || !changed || paused.State != domain.PoolStatePaused {
		t.Fatal(err)
	}
	if _, changed, _ := paused.Pause(poolNow); changed {
		t.Error("pause twice")
	}
	if r, changed, _ := paused.Resume(poolNow); !changed || r.State != domain.PoolStateEnabled {
		t.Error("resume")
	}
	if _, changed, _ := p.Resume(poolNow); changed {
		t.Error("resume running")
	}
	del, changed, err := p.MarkDeleting(poolNow)
	if err != nil || !changed || del.Status.Phase != domain.PhaseDeleting {
		t.Fatal(err)
	}
	if _, changed, err := del.MarkDeleting(poolNow); changed || err != nil {
		t.Fatal("delete twice", err)
	}
	for _, f := range []func() error{
		func() error { _, _, e := del.Pause(poolNow); return e },
		func() error { _, _, e := del.Resume(poolNow); return e },
		func() error { _, _, e := del.Scale(1, poolNow); return e },
		func() error { _, e := del.Apply(domain.PoolPatch{}, poolNow); return e },
	} {
		if domain.KindOf(f()) != domain.KindConflict {
			t.Error("a deleting pool must refuse changes")
		}
	}
	gone := del
	gone.State = domain.PoolStateDeleted
	if _, _, err := gone.MarkDeleting(poolNow); domain.KindOf(err) != domain.KindNotFound {
		t.Fatal(err)
	}
	if !paused.Paused() || p.Paused() {
		t.Error("Paused")
	}
}

func TestStages(t *testing.T) {
	for _, c := range []struct {
		pos   int
		field string
	}{{1, ""}, {50, ""}, {0, "position"}, {51, "position"}} {
		_, err := domain.NewStage("stg_1", "team_1", domain.NewStageParams{Name: "triage", Position: c.pos}, poolNow)
		if fieldOf(err) != c.field {
			t.Errorf("position %d: %v", c.pos, err)
		}
	}
	if _, err := domain.NewStage("stg_1", "t", domain.NewStageParams{Name: "A", Position: 1, Description: strings.Repeat("x", 501)}, poolNow); err == nil || len(domain.FieldErrors(err)) != 2 {
		t.Fatal(err)
	}
	s, _ := domain.NewStage("stg_1", "t", domain.NewStageParams{Name: "triage", Position: 1}, poolNow)
	pos, d := 3, "x"
	n, err := s.Apply(domain.StagePatch{Position: &pos, Description: &d}, poolNow)
	if err != nil || n.Position != 3 || n.Description != "x" {
		t.Fatal(err)
	}
	pos = 99
	if _, err := s.Apply(domain.StagePatch{Position: &pos}, poolNow); domain.KindOf(err) != domain.KindValidation {
		t.Fatal(err)
	}
}

func TestValidateHandoffs(t *testing.T) {
	a, b := "pool_01JD2Q3R4S5T6V7W8X9Y0Z0001", "pool_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	for _, c := range []struct {
		name string
		to   []string
		ok   bool
	}{{"none", nil, true}, {"one", []string{b}, true}, {"self", []string{a}, false}, {"dup", []string{b, b}, false}, {"not a pool", []string{"agt_1"}, false}} {
		if err := domain.ValidateHandoffs(a, c.to); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := domain.ValidateHandoffs(a, make([]string, 51)); err == nil {
		t.Error("too many")
	}
}

func TestPoolRoleLink(t *testing.T) {
	ok := domain.NewPoolRoleLinkParams{RoleName: "afs-cve", AccountID: "111122223333", ProfileName: "cve"}
	for _, c := range []struct {
		name   string
		mutate func(*domain.NewPoolRoleLinkParams)
		field  string
	}{
		{"ok", func(*domain.NewPoolRoleLinkParams) {}, ""},
		{"no role", func(p *domain.NewPoolRoleLinkParams) { p.RoleName = "" }, "roleName"},
		{"bad role", func(p *domain.NewPoolRoleLinkParams) { p.RoleName = "a b" }, "roleName"},
		{"no account", func(p *domain.NewPoolRoleLinkParams) { p.AccountID = "" }, "accountId"},
		{"bad account", func(p *domain.NewPoolRoleLinkParams) { p.AccountID = "12" }, "accountId"},
		{"no profile", func(p *domain.NewPoolRoleLinkParams) { p.ProfileName = "" }, "profileName"},
		{"bad profile", func(p *domain.NewPoolRoleLinkParams) { p.ProfileName = "Bad" }, "profileName"},
		{"session low", func(p *domain.NewPoolRoleLinkParams) { p.MaxSessionSeconds = 10 }, "maxSessionSeconds"},
	} {
		p := ok
		c.mutate(&p)
		l, err := domain.NewPoolRoleLink("prl_1", "team_1", "pool_1", "afs-x", p, poolNow)
		if c.field != "" {
			if fieldOf(err) != c.field {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err != nil || l.MaxSessionSeconds != 3600 || l.RoleARN != "arn:aws:iam::111122223333:role/afs-cve" || l.Status != domain.RoleLinkStatusPendingTrust || l.Audit()["poolId"] != "pool_1" {
			t.Fatalf("%+v %v", l, err)
		}
	}
}
