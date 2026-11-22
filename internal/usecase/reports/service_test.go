package reports_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

const (
	team  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	agent = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

type fakeRepo struct {
	ephemeral bool
	teams     map[string]bool
	rows      int
	last      domain.ReportQuery
	offset    int
	limit     int
}

func (f *fakeRepo) TeamIsEphemeral(_ context.Context, _ port.Tx, id string) (bool, error) {
	if !f.teams[id] {
		return false, domain.NotFound("team")
	}
	return f.ephemeral, nil
}

func (f *fakeRepo) AgentExists(_ context.Context, _ port.Tx, _, id string) error {
	if id != agent {
		return domain.NotFound("agent")
	}
	return nil
}

func (f *fakeRepo) Run(_ context.Context, _ port.Tx, q domain.ReportQuery, offset, limit int) (reports.Result, error) {
	f.last, f.offset, f.limit = q, offset, limit
	var rows []domain.ReportRow
	for i := offset; i < f.rows && len(rows) < limit; i++ {
		rows = append(rows, domain.ReportRow{Group: []string{"g"}, Measures: []any{int64(i), nil, nil, "0.00"}})
	}
	return reports.Result{Rows: rows, Totals: []any{int64(f.rows)}}, nil
}

type rig struct {
	svc  *reports.Service
	repo *fakeRepo
	aud  *fakes.Audit
}

func newRig() *rig {
	r := &rig{repo: &fakeRepo{teams: map[string]bool{team: true, other: true}, rows: 5}, aud: &fakes.Audit{}}
	r.svc = &reports.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.aud, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	return r
}

func viewer(tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_v", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: tm}}}}
}

func svcAdmin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func secOp() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_o", Type: authz.PrincipalUser, SecurityOperator: true}}
}

func TestAuthorization(t *testing.T) {
	ctx := t.Context()
	var none domain.RawReportQuery
	tests := []struct {
		name string
		call func(r *rig) error
		want domain.ErrorKind
	}{
		{"viewer reads own team report", func(r *rig) error {
			_, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, none, acting.Page{})
			return err
		}, ""},
		{"viewer of A gets 404 for B", func(r *rig) error {
			_, err := r.svc.TeamReport(ctx, viewer(team), other, domain.ReportKindUsage, none, acting.Page{})
			return err
		}, domain.KindNotFound},
		{"viewer gets 403 for a service report", func(r *rig) error {
			_, err := r.svc.ServiceReport(ctx, viewer(team), domain.ReportKindUsage, none, acting.Page{})
			return err
		}, domain.KindAccessDenied},
		{"service admin reads a service report", func(r *rig) error {
			_, err := r.svc.ServiceReport(ctx, svcAdmin(), domain.ReportKindTraffic, none, acting.Page{})
			return err
		}, ""},
		{"security operator denied a team report", func(r *rig) error {
			_, err := r.svc.TeamReport(ctx, secOp(), team, domain.ReportKindUsage, none, acting.Page{})
			return err
		}, domain.KindAccessDenied},
		{"viewer reads usage", func(r *rig) error {
			_, err := r.svc.Usage(ctx, viewer(team), team, none, acting.Page{})
			return err
		}, ""},
		{"viewer of A gets 404 for B usage", func(r *rig) error {
			_, err := r.svc.Usage(ctx, viewer(team), other, none, acting.Page{})
			return err
		}, domain.KindNotFound},
		{"unknown agent is 404", func(r *rig) error {
			_, err := r.svc.AgentReport(ctx, viewer(team), team, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009", domain.ReportKindUsage, none, acting.Page{})
			return err
		}, domain.KindNotFound},
		{"collaboration is not an agent report", func(r *rig) error {
			_, err := r.svc.AgentReport(ctx, viewer(team), team, agent, domain.ReportKindCollaboration, none, acting.Page{})
			return err
		}, domain.KindNotFound},
		{"collaboration team report", func(r *rig) error {
			_, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindCollaboration, none, acting.Page{})
			return err
		}, ""},
		{"collaboration service report", func(r *rig) error {
			_, err := r.svc.ServiceReport(ctx, svcAdmin(), domain.ReportKindCollaboration, none, acting.Page{})
			return err
		}, ""},
		{"collaboration service report denied to a team viewer", func(r *rig) error {
			_, err := r.svc.ServiceReport(ctx, viewer(team), domain.ReportKindCollaboration, none, acting.Page{})
			return err
		}, domain.KindAccessDenied},
		{"agent report", func(r *rig) error {
			_, err := r.svc.AgentReport(ctx, viewer(team), team, agent, domain.ReportKindActivity, none, acting.Page{})
			return err
		}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			err := tc.call(r)
			if got := domain.KindOf(err); tc.want == "" && err != nil || tc.want != "" && got != tc.want {
				t.Fatalf("err = %v", err)
			}
			if tc.want == domain.KindAccessDenied && len(r.aud.Entries) != 1 {
				t.Errorf("denied audit entries = %d", len(r.aud.Entries))
			}
		})
	}
}

func TestScopingAndValidation(t *testing.T) {
	ctx := t.Context()
	r := newRig()
	rep, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, domain.RawReportQuery{}, acting.Page{})
	if err != nil || r.repo.last.Filters.TeamID != team || len(rep.Notes) == 0 || rep.MeasureCols[0] != "calls" {
		t.Fatalf("%+v %v %+v", rep, err, r.repo.last.Filters)
	}
	if _, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, domain.RawReportQuery{TeamID: other}, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("teamId filter for another team: %v", err)
	}
	if _, err := r.svc.AgentReport(ctx, viewer(team), team, agent, domain.ReportKindUsage,
		domain.RawReportQuery{AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009"}, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("agentId filter for another agent: %v", err)
	}
	if _, err := r.svc.Usage(ctx, viewer(team), team, domain.RawReportQuery{GroupBy: "team"}, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("usage groupBy team: %v", err)
	}
	if _, err := r.svc.Usage(ctx, viewer(team), team, domain.RawReportQuery{}, acting.Page{}); err != nil || r.repo.last.Columns()[0] != "agent" {
		t.Errorf("usage default groupBy: %v %v", err, r.repo.last.Columns())
	}
	// Pools only in ephemeral teams.
	if _, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, domain.RawReportQuery{GroupBy: "pool"}, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("pool in a persistent team: %v", err)
	}
	r.repo.ephemeral = true
	if _, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, domain.RawReportQuery{GroupBy: "pool"}, acting.Page{}); err != nil {
		t.Errorf("pool in an ephemeral team: %v", err)
	}
	// Service reports may group by pool and filter by team.
	if _, err := r.svc.ServiceReport(ctx, svcAdmin(), domain.ReportKindUsage, domain.RawReportQuery{TeamID: team, GroupBy: "team"}, acting.Page{}); err != nil {
		t.Error(err)
	}
}

func TestPaging(t *testing.T) {
	ctx := t.Context()
	r := newRig()
	raw := domain.RawReportQuery{}
	p1, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, raw, acting.Page{Limit: 2})
	if err != nil || len(p1.Rows) != 2 || p1.NextCursor == "" || r.repo.limit != 3 {
		t.Fatalf("%+v %v", p1, err)
	}
	p2, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, raw, acting.Page{Limit: 2, Cursor: p1.NextCursor})
	if err != nil || len(p2.Rows) != 2 || r.repo.offset != 2 || p2.NextCursor == "" {
		t.Fatalf("%+v %v", p2, err)
	}
	p3, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, raw, acting.Page{Limit: 2, Cursor: p2.NextCursor})
	if err != nil || len(p3.Rows) != 1 || p3.NextCursor != "" {
		t.Fatalf("%+v %v", p3, err)
	}
	// A cursor of another query, and garbage, are refused.
	if _, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, domain.RawReportQuery{ModelID: "m"}, acting.Page{Limit: 2, Cursor: p1.NextCursor}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("foreign cursor: %v", err)
	}
	for _, c := range []string{"x", "-1.ab", "1"} {
		if _, err := r.svc.TeamReport(ctx, viewer(team), team, domain.ReportKindUsage, raw, acting.Page{Cursor: c}); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("cursor %q: %v", c, err)
		}
	}
}
