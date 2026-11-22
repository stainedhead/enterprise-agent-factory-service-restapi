package extsys_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/extsys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const team, agent = "team_1", "agt_1"

type repo struct {
	systems  map[string]domain.ExternalSystem
	teamOff  map[string]bool
	agentOff map[string]bool
	may      bool
}

// view computes the state as the use case's callers see it, like the SQL does.
func (r *repo) view(id string, agentLevel bool) domain.ExternalSystemAccess {
	x := r.systems[id]
	a := domain.ExternalSystemAccess{ExternalSystem: x}
	switch {
	case !x.TeamDeclared() && !x.Enabled:
		a.DisabledAt = domain.ExtSystemDisabledAtService
	case r.teamOff[id]:
		a.DisabledAt = domain.ExtSystemDisabledAtTeam
	case agentLevel && r.agentOff[id]:
		a.DisabledAt = domain.ExtSystemDisabledAtAgent
	}
	return a
}

func (r *repo) TeamMayDeclare(context.Context, port.Tx, string) (bool, error) { return r.may, nil }

func (r *repo) ListTeam(_ context.Context, _ port.Tx, _, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	var out []domain.ExternalSystemAccess
	for id := range r.systems {
		if id > after {
			out = append(out, r.view(id, false))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) GetTeam(_ context.Context, _ port.Tx, _, id string, _ bool) (domain.ExternalSystemAccess, error) {
	if _, ok := r.systems[id]; !ok {
		return domain.ExternalSystemAccess{}, domain.NotFound("external system")
	}
	return r.view(id, false), nil
}

func (r *repo) CatalogSlugExists(_ context.Context, _ port.Tx, slug string) (bool, error) {
	for _, x := range r.systems {
		if !x.TeamDeclared() && x.Slug == slug {
			return true, nil
		}
	}
	return false, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, s domain.ExternalSystem) error {
	s.Version = 1
	r.systems[s.ID] = s
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, s domain.ExternalSystem, expect domain.Version) error {
	s.Version = expect + 1
	r.systems[s.ID] = s
	return nil
}

func (r *repo) Delete(_ context.Context, _ port.Tx, _, id string, _ domain.Version) error {
	delete(r.systems, id)
	return nil
}

func (r *repo) SetTeamOff(_ context.Context, _ port.Tx, _, id, _ string, off bool) error {
	r.teamOff[id] = off
	return nil
}

func (r *repo) AgentExists(_ context.Context, _ port.Tx, _, agentID string) (bool, error) {
	return agentID == agent, nil
}

func (r *repo) ListAgent(_ context.Context, _ port.Tx, _, _, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	var out []domain.ExternalSystemAccess
	for id := range r.systems {
		if id > after {
			out = append(out, r.view(id, true))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) GetAgent(_ context.Context, _ port.Tx, _, _, id string) (domain.ExternalSystemAccess, error) {
	if _, ok := r.systems[id]; !ok {
		return domain.ExternalSystemAccess{}, domain.NotFound("external system")
	}
	return r.view(id, true), nil
}

func (r *repo) SetAgentOff(_ context.Context, _ port.Tx, _, _, id, _ string, off bool) error {
	r.agentOff[id] = off
	return nil
}

var (
	admin    = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}}
	viewer   = acting.Caller{Principal: authz.Principal{ID: "usr_v", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: team}}}}
	otherAdm = acting.Caller{Principal: authz.Principal{ID: "usr_o", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_2"}}}}
	security = acting.Caller{Principal: authz.Principal{ID: "usr_x", Type: authz.PrincipalUser, SecurityOperator: true}}
)

func rig() (*extsys.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{systems: map[string]domain.ExternalSystem{
		"ext_cat": {ID: "ext_cat", Slug: "github", Name: "GitHub", Category: domain.ExtDevTools, Enabled: true, Builtin: true, Version: 3},
		"ext_off": {ID: "ext_off", Slug: "legacy", Name: "Legacy", Category: domain.ExtOther, Enabled: false, Version: 1},
	}, teamOff: map[string]bool{}, agentOff: map[string]bool{}, may: true}
	return &extsys.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

func declare(slug string) domain.DeclareSystemParams {
	return domain.DeclareSystemParams{Slug: slug, Name: "Wiki", Category: domain.ExtOther, Endpoints: []string{"wiki.example.com"}, Owner: "platform", Purpose: "docs"}
}

func ver(v domain.Version) *domain.Version { return &v }

func TestDeclareUpdateDelete(t *testing.T) {
	s, r, audit := rig()
	ctx := t.Context()
	res, err := s.Declare(ctx, admin, team, declare("wiki"), nil)
	if err != nil || res.System.ID == "" || res.System.TeamID != team || !res.System.On() {
		t.Fatalf("declare = %+v %v", res, err)
	}
	id := res.System.ID
	if _, err := s.Declare(ctx, admin, team, declare("github"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("catalog slug = %v", err)
	}
	bad := declare("bad")
	bad.Owner = ""
	if _, err := s.Declare(ctx, admin, team, bad, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("no owner = %v", err)
	}
	purpose := "wiki for the team"
	got, err := s.Update(ctx, admin, team, id, domain.SystemUpdate{Purpose: &purpose}, 1)
	if err != nil || got.Purpose != purpose || got.Version != 2 {
		t.Fatalf("update = %+v %v", got, err)
	}
	off := false
	if _, err := s.Update(ctx, admin, team, id, domain.SystemUpdate{Enabled: &off}, 2); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("the team cannot flip the service-level switch: %v", err)
	}
	if _, err := s.Update(ctx, admin, team, id, domain.SystemUpdate{Purpose: &purpose}, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale update = %v", err)
	}
	if _, err := s.Update(ctx, admin, team, "ext_cat", domain.SystemUpdate{Purpose: &purpose}, 3); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("update of a catalog system = %v", err)
	}
	if _, err := s.Update(ctx, admin, team, "ext_none", domain.SystemUpdate{Purpose: &purpose}, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("update absent = %v", err)
	}
	if err := s.Delete(ctx, admin, team, "ext_cat", 3); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("delete of a catalog system = %v", err)
	}
	if err := s.Delete(ctx, admin, team, id, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete = %v", err)
	}
	if err := s.Delete(ctx, admin, team, id, 2); err != nil {
		t.Errorf("delete = %v", err)
	}
	if _, ok := r.systems[id]; ok {
		t.Error("system still stored")
	}
	if len(audit.Entries) != 3 { // declare, update, delete
		t.Errorf("audit = %d, want 3", len(audit.Entries))
	}
}

func TestDeclareNeedsTheFlag(t *testing.T) {
	s, r, audit := rig()
	r.may = false
	n := len(r.systems)
	for who, c := range map[string]acting.Caller{"team admin": admin, "viewer": viewer} {
		if _, err := s.Declare(t.Context(), c, team, declare("wiki"), nil); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s with the flag off = %v, want AccessDenied", who, err)
		}
	}
	if len(r.systems) != n {
		t.Error("a denied declare stored a system")
	}
	if len(audit.Entries) != 2 {
		t.Errorf("denied audit = %d, want 2", len(audit.Entries))
	}
}

func TestTeamState(t *testing.T) {
	s, _, _ := rig()
	ctx := t.Context()
	got, err := s.SetTeamState(ctx, admin, team, "ext_cat", false, nil)
	if err != nil || got.On() || got.DisabledAt != domain.ExtSystemDisabledAtTeam {
		t.Fatalf("off = %+v %v", got, err)
	}
	if got, err = s.SetTeamState(ctx, admin, team, "ext_cat", false, nil); err != nil || got.On() {
		t.Errorf("off twice = %+v %v", got, err)
	}
	if got, err = s.SetTeamState(ctx, admin, team, "ext_cat", true, ver(3)); err != nil || !got.On() {
		t.Fatalf("on = %+v %v", got, err)
	}
	if _, err := s.SetTeamState(ctx, admin, team, "ext_cat", true, ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	if _, err := s.SetTeamState(ctx, admin, team, "ext_off", true, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("switching on what the service has off = %v", err)
	}
	if _, err := s.SetTeamState(ctx, admin, team, "ext_off", false, nil); err != nil {
		t.Errorf("switching off what the service has off = %v", err)
	}
	if _, err := s.SetTeamState(ctx, admin, team, "ext_none", false, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("absent = %v", err)
	}
}

func TestAgentState(t *testing.T) {
	s, r, _ := rig()
	ctx := t.Context()
	got, err := s.SetAgentState(ctx, admin, team, agent, "ext_cat", false, nil)
	if err != nil || got.DisabledAt != domain.ExtSystemDisabledAtAgent {
		t.Fatalf("off = %+v %v", got, err)
	}
	if got, err = s.SetAgentState(ctx, admin, team, agent, "ext_cat", true, ver(3)); err != nil || !got.On() {
		t.Fatalf("on = %+v %v", got, err)
	}
	if _, err := s.SetAgentState(ctx, admin, team, agent, "ext_cat", true, ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	r.teamOff["ext_cat"] = true
	if _, err := s.SetAgentState(ctx, admin, team, agent, "ext_cat", true, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("on below a team that has it off = %v", err)
	}
	if _, err := s.SetAgentState(ctx, admin, team, agent, "ext_off", true, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("on below a service that has it off = %v", err)
	}
	if _, err := s.SetAgentState(ctx, admin, team, "agt_none", "ext_cat", false, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown agent = %v", err)
	}
	if _, err := s.SetAgentState(ctx, admin, team, agent, "ext_none", false, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown system = %v", err)
	}
}

func TestLists(t *testing.T) {
	s, _, _ := rig()
	ctx := t.Context()
	page, next, err := s.ListTeam(ctx, viewer, team, acting.Page{Limit: 1})
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("team page 1 = %v %q %v", page, next, err)
	}
	if rest, next, err := s.ListTeam(ctx, viewer, team, acting.Page{Limit: 1, Cursor: next}); err != nil || len(rest) != 1 || next != "" || rest[0].DisabledAt != domain.ExtSystemDisabledAtService {
		t.Fatalf("team page 2 = %v %q %v", rest, next, err)
	}
	if rows, _, err := s.ListAgent(ctx, viewer, team, agent, acting.Page{}); err != nil || len(rows) != 2 {
		t.Fatalf("agent list = %v %v", rows, err)
	}
	if _, _, err := s.ListAgent(ctx, viewer, team, "agt_none", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown agent = %v", err)
	}
}

func TestAuthorization(t *testing.T) {
	s, r, audit := rig()
	ctx := t.Context()
	purpose := "p"
	calls := map[string]struct {
		write bool
		call  func(acting.Caller) error
	}{
		"ListTeam":     {false, func(c acting.Caller) error { _, _, err := s.ListTeam(ctx, c, team, acting.Page{}); return err }},
		"ListAgent":    {false, func(c acting.Caller) error { _, _, err := s.ListAgent(ctx, c, team, agent, acting.Page{}); return err }},
		"SetTeamState": {true, func(c acting.Caller) error { _, err := s.SetTeamState(ctx, c, team, "ext_cat", false, nil); return err }},
		"SetAgentState": {true, func(c acting.Caller) error {
			_, err := s.SetAgentState(ctx, c, team, agent, "ext_cat", false, nil)
			return err
		}},
		"Declare": {true, func(c acting.Caller) error { _, err := s.Declare(ctx, c, team, declare("zzz"), nil); return err }},
		"Update": {true, func(c acting.Caller) error {
			_, err := s.Update(ctx, c, team, "ext_cat", domain.SystemUpdate{Purpose: &purpose}, 3)
			return err
		}},
		"Delete": {true, func(c acting.Caller) error { return s.Delete(ctx, c, team, "ext_cat", 3) }},
	}
	for name, tc := range calls {
		for who, c := range map[string]struct {
			c    acting.Caller
			want domain.ErrorKind
		}{"other team": {otherAdm, domain.KindNotFound}, "security": {security, domain.KindAccessDenied}} {
			if err := tc.call(c.c); domain.KindOf(err) != c.want {
				t.Errorf("%s as %s = %v, want %s", name, who, err, c.want)
			}
		}
		err := tc.call(viewer)
		switch {
		case tc.write && domain.KindOf(err) != domain.KindAccessDenied:
			t.Errorf("%s as viewer = %v, want AccessDenied", name, err)
		case !tc.write && err != nil:
			t.Errorf("%s as viewer = %v", name, err)
		}
	}
	if len(r.teamOff) != 0 || len(r.agentOff) != 0 || len(r.systems) != 2 {
		t.Error("a denied call changed data")
	}
	if len(audit.Entries) == 0 {
		t.Error("denied calls wrote no audit records")
	}
}

func TestRepositoryFailures(t *testing.T) {
	ctx := t.Context()
	purpose := "p"
	ops := map[string]func(*extsys.Service) error{
		"ListTeam": func(s *extsys.Service) error { _, _, err := s.ListTeam(ctx, admin, team, acting.Page{}); return err },
		"SetTeamState": func(s *extsys.Service) error {
			_, err := s.SetTeamState(ctx, admin, team, "ext_cat", false, nil)
			return err
		},
		"Declare": func(s *extsys.Service) error { _, err := s.Declare(ctx, admin, team, declare("wiki"), nil); return err },
		"Update": func(s *extsys.Service) error {
			_, err := s.Update(ctx, admin, team, "ext_own", domain.SystemUpdate{Purpose: &purpose}, 1)
			return err
		},
		"Delete": func(s *extsys.Service) error { return s.Delete(ctx, admin, team, "ext_own", 1) },
		"ListAgent": func(s *extsys.Service) error {
			_, _, err := s.ListAgent(ctx, admin, team, agent, acting.Page{})
			return err
		},
		"SetAgentState": func(s *extsys.Service) error {
			_, err := s.SetAgentState(ctx, admin, team, agent, "ext_cat", false, nil)
			return err
		},
	}
	for name, op := range ops {
		for n := 1; n < 12; n++ {
			r := &repo{systems: map[string]domain.ExternalSystem{
				"ext_cat": {ID: "ext_cat", Slug: "github", Enabled: true, Version: 1},
				"ext_own": {ID: "ext_own", TeamID: team, Slug: "own", Owner: "o", Purpose: "p", Enabled: true, Version: 1},
			}, teamOff: map[string]bool{}, agentOff: map[string]bool{}, may: true}
			f, audit := &flaky{repo: r, failAt: n}, &fakes.Audit{}
			s := &extsys.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: f}
			err := op(s)
			if f.calls < n {
				break
			}
			if err == nil || len(audit.Entries) != 0 {
				t.Errorf("%s failing at repository call %d: err %v, audit %d", name, n, err, len(audit.Entries))
			}
		}
	}
}
