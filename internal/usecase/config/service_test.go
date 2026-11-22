package config_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/config"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const team, agent = "team_1", "agt_1"

type repo struct {
	ns     map[string]domain.NamespaceConfigValue
	agents map[string]domain.AgentConfigValue
	live   map[string]bool
}

func (r *repo) usedBy(name domain.ConfigName) []string {
	var out []string
	for _, a := range r.agents {
		if a.Name == name && a.Source == domain.ConfigSourceNamespace {
			out = append(out, a.AgentID)
		}
	}
	sort.Strings(out)
	return out
}

func (r *repo) ListNamespace(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.NamespaceConfigValue, error) {
	var out []domain.NamespaceConfigValue
	for _, v := range r.ns {
		if v.TeamID == teamID && string(v.Name) > after {
			v.UsedBy = r.usedBy(v.Name)
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) GetNamespace(_ context.Context, _ port.Tx, _ string, name domain.ConfigName, _ bool) (domain.NamespaceConfigValue, error) {
	v, ok := r.ns[string(name)]
	if !ok {
		return domain.NamespaceConfigValue{}, domain.NotFound("configuration value")
	}
	v.UsedBy = r.usedBy(name)
	return v, nil
}

func (r *repo) InsertNamespace(_ context.Context, _ port.Tx, v domain.NamespaceConfigValue) error {
	v.Version = 1
	r.ns[string(v.Name)] = v
	return nil
}

func (r *repo) UpdateNamespace(_ context.Context, _ port.Tx, v domain.NamespaceConfigValue, expect domain.Version) error {
	v.Version = expect + 1
	r.ns[string(v.Name)] = v
	return nil
}

func (r *repo) DeleteNamespace(_ context.Context, _ port.Tx, _ string, name domain.ConfigName, _ domain.Version) error {
	delete(r.ns, string(name))
	return nil
}

func (r *repo) AgentExists(_ context.Context, _ port.Tx, _, agentID string) (bool, error) {
	return r.live[agentID], nil
}

func (r *repo) resolve(v domain.AgentConfigValue) domain.AgentConfigValue {
	v.ResolvedValue = v.Value
	if v.Source == domain.ConfigSourceNamespace {
		n := r.ns[string(v.Name)].Value
		v.ResolvedValue = &n
	}
	return v
}

func (r *repo) ListAgent(_ context.Context, _ port.Tx, _, agentID, after string, limit int) ([]domain.AgentConfigValue, error) {
	var out []domain.AgentConfigValue
	for _, v := range r.agents {
		if v.AgentID == agentID && string(v.Name) > after {
			out = append(out, r.resolve(v))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) GetAgent(_ context.Context, _ port.Tx, _, agentID string, name domain.ConfigName, _ bool) (domain.AgentConfigValue, error) {
	v, ok := r.agents[agentID+"/"+string(name)]
	if !ok {
		return domain.AgentConfigValue{}, domain.NotFound("configuration value")
	}
	return r.resolve(v), nil
}

func (r *repo) InsertAgent(_ context.Context, _ port.Tx, v domain.AgentConfigValue) error {
	v.Version = 1
	r.agents[v.AgentID+"/"+string(v.Name)] = v
	return nil
}

func (r *repo) UpdateAgent(_ context.Context, _ port.Tx, v domain.AgentConfigValue, expect domain.Version) error {
	v.Version = expect + 1
	r.agents[v.AgentID+"/"+string(v.Name)] = v
	return nil
}

func (r *repo) DeleteAgent(_ context.Context, _ port.Tx, _, agentID string, name domain.ConfigName, _ domain.Version) error {
	delete(r.agents, agentID+"/"+string(name))
	return nil
}

var (
	admin    = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}}
	viewer   = acting.Caller{Principal: authz.Principal{ID: "usr_v", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: team}}}}
	otherAdm = acting.Caller{Principal: authz.Principal{ID: "usr_o", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_2"}}}}
	security = acting.Caller{Principal: authz.Principal{ID: "usr_x", Type: authz.PrincipalUser, SecurityOperator: true}}
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
)

func rig() (*config.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{ns: map[string]domain.NamespaceConfigValue{}, agents: map[string]domain.AgentConfigValue{}, live: map[string]bool{agent: true}}
	return &config.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

func ver(v domain.Version) *domain.Version { return &v }
func str(s string) *string                 { return &s }

func TestNamespaceValues(t *testing.T) {
	s, _, audit := rig()
	ctx := t.Context()
	got, err := s.PutNamespace(ctx, admin, team, "LOG_LEVEL", "info", nil)
	if err != nil || got.Value != "info" || got.Version != 1 || got.ID == "" {
		t.Fatalf("create = %+v %v", got, err)
	}
	if _, err := s.PutNamespace(ctx, admin, team, "OTHER", "x", ver(4)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("If-Match on a new value = %v", err)
	}
	if got, err = s.PutNamespace(ctx, admin, team, "LOG_LEVEL", "debug", ver(1)); err != nil || got.Value != "debug" || got.Version != 2 {
		t.Fatalf("update = %+v %v", got, err)
	}
	if _, err := s.PutNamespace(ctx, admin, team, "LOG_LEVEL", "warn", ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	for name, tc := range map[string]struct {
		name  domain.ConfigName
		value string
	}{
		"lower case":        {"log_level", "x"},
		"credential name":   {"DB_PASSWORD", "x"},
		"too long":          {"BIG", strings.Repeat("x", 4097)},
		"NUL":               {"NULS", "a\x00b"},
		"bad owned repos":   {domain.OwnedReposName, "not-a-repo"},
		"owned repos no /*": {domain.OwnedReposName, "github.com/acme"},
	} {
		if _, err := s.PutNamespace(ctx, admin, team, tc.name, tc.value, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s = %v", name, err)
		}
	}
	if _, err := s.PutNamespace(ctx, admin, team, domain.OwnedReposName, "github.com/acme/*, github.com/acme/x", nil); err != nil {
		t.Errorf("valid owned repos = %v", err)
	}
	rows, next, err := s.ListNamespace(ctx, viewer, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("list = %v %q %v", rows, next, err)
	}
	if err := s.DeleteNamespace(ctx, admin, team, "LOG_LEVEL", 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete = %v", err)
	}
	if err := s.DeleteNamespace(ctx, admin, team, "ABSENT", 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("absent delete = %v", err)
	}
	if err := s.DeleteNamespace(ctx, admin, team, "LOG_LEVEL", 2); err != nil {
		t.Errorf("delete = %v", err)
	}
	if len(audit.Entries) != 4 { // create, update, owned repos, delete
		t.Errorf("audit = %d, want 4", len(audit.Entries))
	}
}

func TestAgentValues(t *testing.T) {
	s, r, _ := rig()
	ctx := t.Context()
	if _, err := s.PutAgent(ctx, admin, team, "agt_none", "X", config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("v")}, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown agent = %v", err)
	}
	if _, err := s.PutAgent(ctx, admin, team, agent, "LOG_LEVEL", config.AgentValueInput{Source: domain.ConfigSourceNamespace}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("link without a namespace value = %v", err)
	}
	if _, err := s.PutNamespace(ctx, admin, team, "LOG_LEVEL", "info", nil); err != nil {
		t.Fatal(err)
	}
	linked, err := s.PutAgent(ctx, admin, team, agent, "LOG_LEVEL", config.AgentValueInput{Source: domain.ConfigSourceNamespace}, nil)
	if err != nil || linked.Source != domain.ConfigSourceNamespace || linked.ResolvedValue == nil || *linked.ResolvedValue != "info" {
		t.Fatalf("link = %+v %v", linked, err)
	}
	if err := s.DeleteNamespace(ctx, admin, team, "LOG_LEVEL", 1); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("delete of a linked value = %v", err)
	} else if d := err.(*domain.Error).Details; d["agents"] == nil {
		t.Errorf("conflict lists no agents: %v", d)
	}
	lit, err := s.PutAgent(ctx, admin, team, agent, "LOG_LEVEL", config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("trace")}, ver(1))
	if err != nil || lit.Version != 2 || *lit.ResolvedValue != "trace" {
		t.Fatalf("literal = %+v %v", lit, err)
	}
	if _, err := s.PutAgent(ctx, admin, team, agent, "LOG_LEVEL", config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("x")}, ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	if _, err := s.PutAgent(ctx, admin, team, agent, "NEW", config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("x")}, ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("If-Match on a new value = %v", err)
	}
	for name, in := range map[string]config.AgentValueInput{
		"literal without value": {Source: domain.ConfigSourceLiteral},
		"link with value":       {Source: domain.ConfigSourceNamespace, Value: str("x")},
		"unknown source":        {Source: "env"},
	} {
		if _, err := s.PutAgent(ctx, admin, team, agent, "LOG_LEVEL", in, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s = %v", name, err)
		}
	}
	if _, err := s.PutAgent(ctx, admin, team, agent, "bad", config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("x")}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bad name = %v", err)
	}
	rows, next, err := s.ListAgent(ctx, viewer, team, agent, acting.Page{})
	if err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("list = %v %q %v", rows, next, err)
	}
	if _, _, err := s.ListAgent(ctx, viewer, team, "agt_none", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("list unknown agent = %v", err)
	}
	if err := s.DeleteAgent(ctx, admin, team, agent, "LOG_LEVEL", 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete = %v", err)
	}
	if err := s.DeleteAgent(ctx, admin, team, "agt_none", "LOG_LEVEL", 2); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("delete on unknown agent = %v", err)
	}
	if err := s.DeleteAgent(ctx, admin, team, agent, "NOPE", 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("delete absent = %v", err)
	}
	if err := s.DeleteAgent(ctx, admin, team, agent, "LOG_LEVEL", 2); err != nil || len(r.agents) != 0 {
		t.Errorf("delete = %v, rows %d", err, len(r.agents))
	}
}

func TestAuthorization(t *testing.T) {
	s, r, audit := rig()
	ctx := t.Context()
	r.ns["X"] = domain.NamespaceConfigValue{ID: "ncv_1", TeamID: team, Name: "X", Version: 1}
	calls := map[string]struct {
		write bool
		call  func(acting.Caller) error
	}{
		"ListNamespace":   {false, func(c acting.Caller) error { _, _, err := s.ListNamespace(ctx, c, team, acting.Page{}); return err }},
		"ListAgent":       {false, func(c acting.Caller) error { _, _, err := s.ListAgent(ctx, c, team, agent, acting.Page{}); return err }},
		"PutNamespace":    {true, func(c acting.Caller) error { _, err := s.PutNamespace(ctx, c, team, "Y", "v", nil); return err }},
		"DeleteNamespace": {true, func(c acting.Caller) error { return s.DeleteNamespace(ctx, c, team, "X", 1) }},
		"PutAgent": {true, func(c acting.Caller) error {
			_, err := s.PutAgent(ctx, c, team, agent, "Y", config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("v")}, nil)
			return err
		}},
		"DeleteAgent": {true, func(c acting.Caller) error { return s.DeleteAgent(ctx, c, team, agent, "Y", 1) }},
	}
	for name, tc := range calls {
		if err := tc.call(admin); tc.write && err != nil && domain.KindOf(err) == domain.KindAccessDenied {
			t.Errorf("%s denied to a team admin", name)
		}
		for who, c := range map[string]struct {
			c    acting.Caller
			want domain.ErrorKind
		}{
			"other team": {otherAdm, domain.KindNotFound},
			"security":   {security, domain.KindAccessDenied},
		} {
			if err := tc.call(c.c); domain.KindOf(err) != c.want {
				t.Errorf("%s as %s = %v, want %s", name, who, err, c.want)
			}
		}
		err := tc.call(viewer)
		switch {
		case tc.write && domain.KindOf(err) != domain.KindAccessDenied:
			t.Errorf("%s as viewer = %v, want AccessDenied", name, err)
		case !tc.write && err != nil:
			t.Errorf("%s as viewer = %v, want allowed", name, err)
		}
	}
	if err := calls["ListNamespace"].call(svcAdmin); err != nil {
		t.Errorf("service admin = %v", err)
	}
	if len(audit.Entries) == 0 {
		t.Error("denied calls wrote no audit records")
	}
}

func TestRepositoryFailures(t *testing.T) {
	ctx := t.Context()
	lit := config.AgentValueInput{Source: domain.ConfigSourceLiteral, Value: str("v")}
	link := config.AgentValueInput{Source: domain.ConfigSourceNamespace}
	ops := map[string]func(*config.Service) error{
		"ListNamespace": func(s *config.Service) error {
			_, _, err := s.ListNamespace(ctx, admin, team, acting.Page{})
			return err
		},
		"PutNamespaceNew": func(s *config.Service) error { _, err := s.PutNamespace(ctx, admin, team, "NEW", "v", nil); return err },
		"PutNamespaceOld": func(s *config.Service) error {
			_, err := s.PutNamespace(ctx, admin, team, "OLD", "v", ver(1))
			return err
		},
		"DeleteNamespace": func(s *config.Service) error { return s.DeleteNamespace(ctx, admin, team, "OLD", 1) },
		"ListAgent": func(s *config.Service) error {
			_, _, err := s.ListAgent(ctx, admin, team, agent, acting.Page{})
			return err
		},
		"PutAgentNew": func(s *config.Service) error {
			_, err := s.PutAgent(ctx, admin, team, agent, "NEW", lit, nil)
			return err
		},
		"PutAgentLink": func(s *config.Service) error {
			_, err := s.PutAgent(ctx, admin, team, agent, "OLD", link, nil)
			return err
		},
		"PutAgentOld": func(s *config.Service) error {
			_, err := s.PutAgent(ctx, admin, team, agent, "AOLD", lit, ver(1))
			return err
		},
		"DeleteAgent": func(s *config.Service) error { return s.DeleteAgent(ctx, admin, team, agent, "AOLD", 1) },
	}
	for name, op := range ops {
		for n := 1; n < 12; n++ {
			r := &repo{ns: map[string]domain.NamespaceConfigValue{"OLD": {ID: "ncv_1", TeamID: team, Name: "OLD", Value: "x", Version: 1}},
				agents: map[string]domain.AgentConfigValue{agent + "/AOLD": {ID: "acv_1", TeamID: team, AgentID: agent, Name: "AOLD", Source: domain.ConfigSourceLiteral, Value: str("x"), Version: 1}},
				live:   map[string]bool{agent: true}}
			f, audit := &flaky{repo: r, failAt: n}, &fakes.Audit{}
			s := &config.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: f}
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
