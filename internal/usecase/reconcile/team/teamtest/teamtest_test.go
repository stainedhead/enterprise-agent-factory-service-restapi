package teamtest

import (
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

func TestSourceCluster(t *testing.T) {
	s := NewSource(team.Desired{ID: "team_A", Models: []team.Model{{ID: "m"}}})
	if _, err := s.LoadTeam(t.Context(), "nope"); !isNotFound(err) {
		t.Errorf("missing: %v", err)
	}
	if err := s.SetInferenceProfileARN(t.Context(), "team_A", "m", "arn:x"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadTeam(t.Context(), "team_A")
	if got.Models[0].ProfileARN != "arn:x" {
		t.Errorf("ARN not reflected: %+v", got)
	}
	s.SetARNErr = ErrBoom
	if s.SetInferenceProfileARN(t.Context(), "team_A", "m", "y") == nil {
		t.Error("SetARNErr ignored")
	}
	c := &Cluster{Changed: []string{"a"}, Err: ErrBoom}
	res, err := c.ApplyTeam(t.Context(), team.Environment{})
	if !errors.Is(err, ErrBoom) || len(res.Changed) != 1 || len(c.Envs) != 1 {
		t.Errorf("ApplyTeam = %+v, %v", res, err)
	}
	if _, err := c.ApplyPlatform(t.Context(), team.PlatformSpec{}); !errors.Is(err, ErrBoom) || len(c.Platforms) != 1 {
		t.Errorf("ApplyPlatform: %v", err)
	}
}

func isNotFound(err error) bool { return domain.KindOf(err) == domain.KindNotFound }

func TestIAMAndProfiles(t *testing.T) {
	i := NewIAM()
	arn, err := i.EnsureRole(t.Context(), team.RoleSpec{Name: "afs-X", Path: "/afs/ns/"})
	if err != nil || arn != "arn:aws:iam::111122223333:role/afs/ns/afs-X" {
		t.Errorf("EnsureRole = %q, %v", arn, err)
	}
	if _, ok := i.Role("afs-X"); !ok {
		t.Error("role not stored")
	}
	i.Err, i.ErrRole = ErrBoom, "afs-Y"
	if _, err := i.EnsureRole(t.Context(), team.RoleSpec{Name: "afs-Y"}); !errors.Is(err, ErrBoom) {
		t.Errorf("ErrRole ignored: %v", err)
	}
	if _, err := i.EnsureRole(t.Context(), team.RoleSpec{Name: "afs-X"}); err != nil {
		t.Errorf("other role failed: %v", err)
	}
	_ = i.DeleteRole(t.Context(), "/afs/ns/", "afs-X")
	if _, ok := i.Role("afs-X"); ok {
		t.Error("role not deleted")
	}

	p := NewProfiles()
	pr, err := p.EnsureProfile(t.Context(), team.ProfileSpec{Name: "afs-1", Tags: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := p.EnsureProfile(t.Context(), team.ProfileSpec{Name: "afs-1"})
	if again.ARN != pr.ARN {
		t.Error("EnsureProfile is not idempotent")
	}
	if l, _ := p.ListProfiles(t.Context(), "k", "v"); len(l) != 1 {
		t.Errorf("list = %v", l)
	}
	if l, _ := p.ListProfiles(t.Context(), "k", "other"); len(l) != 0 {
		t.Errorf("list = %v", l)
	}
	p.ListErr, p.EnsureEr, p.DeleteEr = ErrBoom, ErrBoom, ErrBoom
	if _, err := p.ListProfiles(t.Context(), "k", "v"); err == nil {
		t.Error("ListErr ignored")
	}
	if _, err := p.EnsureProfile(t.Context(), team.ProfileSpec{Name: "afs-2"}); err == nil {
		t.Error("EnsureEr ignored")
	}
	if err := p.DeleteProfile(t.Context(), pr.ARN); err == nil {
		t.Error("DeleteEr ignored")
	}
	p.DeleteEr = nil
	if err := p.DeleteProfile(t.Context(), pr.ARN); err != nil || len(p.ByName) != 0 {
		t.Errorf("delete: %v, %v", err, p.ByName)
	}
}
