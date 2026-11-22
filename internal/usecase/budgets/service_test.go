package budgets_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/budgets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/budgets/budgetstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	eph   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0003"
)

var now = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

type rig struct {
	svc    *budgets.Service
	repo   *budgetstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: budgetstest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &budgets.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	r.repo.Teams[team] = domain.BudgetTeam{Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("2000")}
	r.repo.Teams[other] = domain.BudgetTeam{Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("2000")}
	r.repo.Teams[eph] = domain.BudgetTeam{Kind: domain.TeamKindEphemeral, MaxBudget: domain.MustMoney("500")}
	return r
}

func member(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

func serviceAdmin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_root", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func monthly(amount string) domain.PutBudgetParams {
	return domain.PutBudgetParams{Period: domain.BudgetPeriodMonthly, Amount: domain.MustMoney(amount)}
}

func TestPutCreatesThenReplaces(t *testing.T) {
	r := newRig()
	admin := member(authz.RoleTeamAdmin, team)
	pos, err := r.svc.Put(t.Context(), admin, team, monthly("1500"), nil)
	if err != nil || pos.Version != 1 || pos.Amount.String() != "1500.00" || pos.OnExhaust != domain.OnExhaustBlock {
		t.Fatalf("create: %+v %v", pos, err)
	}
	if len(r.audit.Entries) != 1 || r.audit.Entries[0].Record.Action != "PutBudget" || r.audit.Entries[0].Record.Before != nil {
		t.Fatalf("audit of the create: %+v", r.audit.Entries)
	}
	if len(r.outbox.Events) != 0 {
		t.Fatalf("CD-7: a budget change has no §7 event, got %d", len(r.outbox.Events))
	}
	v := domain.Version(1)
	pos, err = r.svc.Put(t.Context(), admin, team, monthly("1800"), &v)
	if err != nil || pos.Version != 2 || pos.Amount.String() != "1800.00" || pos.ID == "" {
		t.Fatalf("replace: %+v %v", pos, err)
	}
	if len(r.audit.Entries) != 2 || r.audit.Entries[1].Record.Before == nil || r.audit.Entries[1].Record.TeamID != team {
		t.Fatalf("audit of the replace: %+v", r.audit.Entries)
	}
	if _, err := r.svc.Put(t.Context(), admin, team, monthly("1900"), &v); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale If-Match: %v", err)
	}
	if r.repo.Budgets[team].Amount.String() != "1800.00" {
		t.Fatal("a refused replace changed the budget")
	}
	if _, err := r.svc.Put(t.Context(), admin, team, monthly("1900"), nil); err != nil {
		t.Fatalf("PUT without If-Match upserts (CD-2): %v", err)
	}
}

func TestPutWithIfMatchNeedsABudget(t *testing.T) {
	r := newRig()
	v := domain.Version(1)
	if _, err := r.svc.Put(t.Context(), member(authz.RoleTeamAdmin, team), team, monthly("100"), &v); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("%v", err)
	}
}

func TestPutCeiling(t *testing.T) {
	r := newRig()
	admin, root := member(authz.RoleTeamAdmin, team), serviceAdmin()
	// RA-US-13.1: above the ceiling is a 400 for a team-admin and for a service admin.
	for _, c := range []acting.Caller{admin, root} {
		if _, err := r.svc.Put(t.Context(), c, team, monthly("2000.01"), nil); domain.KindOf(err) != domain.KindValidation {
			t.Fatalf("above the ceiling: %v", err)
		}
	}
	if len(r.repo.Budgets) != 0 {
		t.Fatal("a refused budget was stored")
	}
	// RA-US-13.2: an unexpired override lifts the ceiling for a team-admin; an expired one does not.
	over, future, past := domain.MustMoney("5000"), now.Add(48*time.Hour), now.Add(-time.Hour)
	r.repo.Teams[team] = domain.BudgetTeam{Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("2000"), CeilingOverride: &over, CeilingOverrideExpiresAt: &future}
	if _, err := r.svc.Put(t.Context(), admin, team, monthly("5000"), nil); err != nil {
		t.Fatalf("up to the override: %v", err)
	}
	r.repo.Teams[team] = domain.BudgetTeam{Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("2000"), CeilingOverride: &over, CeilingOverrideExpiresAt: &past}
	if _, err := r.svc.Put(t.Context(), admin, team, monthly("5000"), nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("expired override: %v", err)
	}
}

func TestPutNotify(t *testing.T) {
	notify := func(p domain.PutBudgetParams) domain.PutBudgetParams { p.OnExhaust = domain.OnExhaustNotify; return p }
	for _, tc := range []struct {
		name   string
		caller acting.Caller
		team   string
		kind   domain.ErrorKind
		denied bool
	}{
		{"team-admin on persistent", member(authz.RoleTeamAdmin, team), team, domain.KindAccessDenied, true},
		{"service admin on persistent", serviceAdmin(), team, "", false},
		{"service admin on ephemeral", serviceAdmin(), eph, domain.KindValidation, false},
	} {
		r := newRig()
		p := notify(monthly("100"))
		if tc.team == eph {
			p.Period = domain.BudgetPeriodLifetime
		}
		pos, err := r.svc.Put(t.Context(), tc.caller, tc.team, p, nil)
		if domain.KindOf(err) != tc.kind || (tc.kind == "" && pos.OnExhaust != domain.OnExhaustNotify) {
			t.Errorf("%s: %+v %v", tc.name, pos, err)
		}
		// The 403 is audited once by the pipeline (DeniedAuditor.Stage), never by the use case.
		for _, e := range r.audit.Entries {
			if e.Own || e.Record.Outcome == port.OutcomeDenied {
				t.Errorf("%s: the use case wrote a denied audit record", tc.name)
			}
		}
		var de *domain.Error
		if ok := errors.As(err, &de); tc.denied && (!ok || de.RequiredPermission != string(authz.TeamsSetGuardrails)) {
			t.Errorf("%s: the denial names %v", tc.name, err)
		}
		if tc.denied && len(r.repo.Budgets) != 0 {
			t.Errorf("%s: the budget was written", tc.name)
		}
	}
}

func TestEphemeralKeepsItsLifetimeBudget(t *testing.T) {
	r := newRig()
	root := serviceAdmin()
	life := monthly("100")
	life.Period = domain.BudgetPeriodLifetime
	if _, err := r.svc.Put(t.Context(), root, eph, life, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Put(t.Context(), root, eph, monthly("100"), nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("an ephemeral team's budget stays a lifetime budget: %v", err)
	}
}

func TestGetReportsSpend(t *testing.T) {
	r := newRig()
	admin := member(authz.RoleTeamAdmin, team)
	if _, err := r.svc.Get(t.Context(), admin, team); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("no budget yet: %v", err)
	}
	pos, err := r.svc.Put(t.Context(), admin, team, monthly("2000"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pos.Spent.String() != "0.00" || pos.Forecast.String() != "0.00" || pos.ProjectedCapAt != nil {
		t.Fatalf("a budget without a counter has spent nothing: %+v", pos)
	}
	r.repo.Counters[pos.ID] = domain.SpendCounter{Spent: domain.MustMoney("1100"), Reserved: domain.MustMoney("3.2")}
	got, err := r.svc.Get(t.Context(), member(authz.RoleTeamViewer, team), team)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spent.String() != "1100.00" || got.Reserved.String() != "3.20" || got.PercentUsed != 55 ||
		got.Forecast.String() != "2129.032258" || got.ProjectedCapAt == nil {
		t.Fatalf("%+v", got)
	}
}

func TestAuthorization(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Put(t.Context(), serviceAdmin(), team, monthly("100"), nil); err != nil {
		t.Fatal(err)
	}
	viewer, stranger := member(authz.RoleTeamViewer, team), member(authz.RoleTeamAdmin, other)
	secop := acting.Caller{Principal: authz.Principal{ID: "key_s", Type: authz.PrincipalAPIKey, SecurityOperator: true}}
	for _, tc := range []struct {
		name string
		call func() error
		want domain.ErrorKind
	}{
		{"viewer reads", func() error { _, err := r.svc.Get(t.Context(), viewer, team); return err }, ""},
		{"viewer writes", func() error { _, err := r.svc.Put(t.Context(), viewer, team, monthly("50"), nil); return err }, domain.KindAccessDenied},
		{"another team's admin reads", func() error { _, err := r.svc.Get(t.Context(), stranger, team); return err }, domain.KindNotFound},
		{"another team's admin writes", func() error { _, err := r.svc.Put(t.Context(), stranger, team, monthly("50"), nil); return err }, domain.KindNotFound},
		{"security operator reads", func() error { _, err := r.svc.Get(t.Context(), secop, team); return err }, domain.KindAccessDenied},
		{"security operator writes", func() error { _, err := r.svc.Put(t.Context(), secop, team, monthly("50"), nil); return err }, domain.KindAccessDenied},
		{"service admin on a team that is not there", func() error {
			_, err := r.svc.Put(t.Context(), serviceAdmin(), "team_01JD2Q3R4S5T6V7W8X9Y0Z0099", monthly("50"), nil)
			return err
		}, domain.KindNotFound},
	} {
		if got := domain.KindOf(tc.call()); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if r.repo.Budgets[team].Amount.String() != "100.00" {
		t.Fatal("a denied write changed the budget")
	}
}
