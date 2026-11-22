package budget_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget/budgettest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

const (
	teamID = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	roleID = "/afs/afs-t-payments/afs-01JD2Q3R4S5T6V7W8X9Y0Z0001-bedrock"
)

type rig struct {
	r     *budget.Reconciler
	store *budgettest.Store
	iam   *budgettest.IAM
	calls *budgettest.Calls
	now   time.Time
}

func newRig() *rig {
	c := &budgettest.Calls{}
	g := &rig{store: budgettest.NewStore(c), iam: budgettest.NewIAM(c), calls: c, now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	g.r = &budget.Reconciler{Store: g.store, IAM: g.iam, Clock: func() time.Time { return g.now }}
	return g
}

func (g *rig) calledWith(prefix string) []string {
	var out []string
	for _, c := range g.calls.List {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func monthly(id string) domain.Budget {
	return domain.Budget{ID: id, TeamID: teamID, Period: domain.BudgetPeriodMonthly, StartsAt: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)}
}

// RC-US-12.1: each live budget's counter is opened for its current period and announced once; a pass
// that finds nothing new calls nothing.
func TestOpenPeriodsOncePerPeriod(t *testing.T) {
	g := newRig()
	life := domain.Budget{ID: "bud_life", TeamID: teamID, Period: domain.BudgetPeriodLifetime, StartsAt: time.Date(2026, 10, 3, 8, 30, 0, 0, time.UTC)}
	g.store.Budgets = []domain.Budget{monthly("bud_m"), life}
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"OpenPeriod bud_m 2026-11-01T00:00 announce=true", "OpenPeriod bud_life 2026-10-03T08:30 announce=true"}
	if got := g.calledWith("OpenPeriod"); !slices.Equal(got, want) {
		t.Fatalf("first pass: %v", got)
	}
	g.calls.List = nil
	g.now = g.now.Add(time.Hour)
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.calledWith("OpenPeriod"); len(got) != 0 {
		t.Fatalf("a period already opened was opened again: %v", got)
	}
}

// The next month's counter is opened before the boundary, without announcing, so it exists by the
// first second of the period; the announcement follows once the period has started.
func TestNextPeriodIsPreOpenedAndAnnouncedAtTheBoundary(t *testing.T) {
	g := newRig()
	g.store.Budgets = []domain.Budget{monthly("bud_m")}
	g.now = time.Date(2026, 11, 30, 23, 50, 0, 0, time.UTC)
	_ = g.r.Reconcile(t.Context())
	g.calls.List = nil
	g.now = time.Date(2026, 11, 30, 23, 58, 30, 0, time.UTC) // inside PreOpen
	_ = g.r.Reconcile(t.Context())
	if got := g.calledWith("OpenPeriod"); !slices.Equal(got, []string{"OpenPeriod bud_m 2026-12-01T00:00 announce=false"}) {
		t.Fatalf("pre-open: %v", got)
	}
	g.calls.List = nil
	g.now = time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	_ = g.r.Reconcile(t.Context())
	if got := g.calledWith("OpenPeriod"); len(got) != 0 {
		t.Fatalf("pre-opened twice: %v", got)
	}
	g.now = time.Date(2026, 12, 1, 0, 0, 5, 0, time.UTC)
	_ = g.r.Reconcile(t.Context())
	if got := g.calledWith("OpenPeriod"); !slices.Equal(got, []string{"OpenPeriod bud_m 2026-12-01T00:00 announce=true"}) {
		t.Fatalf("at the boundary: %v", got)
	}
}

func TestOpenPeriodFailureIsRetriedAndOthersContinue(t *testing.T) {
	g := newRig()
	g.store.Budgets = []domain.Budget{monthly("bud_a"), monthly("bud_b")}
	g.store.ErrOn["OpenPeriod"] = errors.New("boom")
	if err := g.r.Reconcile(t.Context()); err == nil {
		t.Fatal("the failure was swallowed")
	}
	if len(g.calledWith("OpenPeriod")) != 2 {
		t.Fatalf("one failing budget stopped the other: %v", g.calls.List)
	}
	delete(g.store.ErrOn, "OpenPeriod")
	g.calls.List = nil
	if err := g.r.Reconcile(t.Context()); err != nil || len(g.calledWith("OpenPeriod")) != 2 {
		t.Fatalf("retry: %v %v", err, g.calls.List)
	}
	// A budget that is gone is forgotten: when it returns it is opened again.
	g.store.Budgets = nil
	_ = g.r.Reconcile(t.Context())
	g.store.Budgets = []domain.Budget{monthly("bud_a")}
	g.calls.List = nil
	_ = g.r.Reconcile(t.Context())
	if len(g.calledWith("OpenPeriod")) != 1 {
		t.Fatalf("forgotten budget not reopened: %v", g.calls.List)
	}
}

// RC-US-12.3: a spent budget gets the condition and then the deny; RC-US-12.2: when it is no longer
// spent the deny is lifted and then the condition cleared.
func TestBackstopAppliesAndLifts(t *testing.T) {
	g := newRig()
	g.store.Teams = []budget.TeamBudget{{TeamID: teamID, Namespace: "afs-t-payments", Exhausted: true}}
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"MarkExhausted " + teamID, "DenyBedrock " + roleID}
	if got := append(g.calledWith("MarkExhausted"), g.calledWith("DenyBedrock")...); !slices.Equal(got, want) {
		t.Fatalf("apply: %v", got)
	}
	if i, j := slices.Index(g.calls.List, want[0]), slices.Index(g.calls.List, want[1]); i > j {
		t.Fatalf("the deny was placed before the condition was recorded: %v", g.calls.List)
	}
	if !g.iam.Denied[roleID] {
		t.Fatal("role not denied")
	}

	// Still spent next pass: re-asserted (the role may have drifted); nothing else.
	g.calls.List = nil
	_ = g.r.Reconcile(t.Context())
	if len(g.calledWith("DenyBedrock")) != 1 || len(g.calledWith("LiftBedrockDeny")) != 0 {
		t.Fatalf("re-assert: %v", g.calls.List)
	}

	// Raised: no longer exhausted but still marked → lift, then clear.
	g.store.Teams = []budget.TeamBudget{{TeamID: teamID, Namespace: "afs-t-payments", Marked: true}}
	g.calls.List = nil
	_ = g.r.Reconcile(t.Context())
	if got := append(g.calledWith("LiftBedrockDeny"), g.calledWith("ClearExhausted")...); !slices.Equal(got, []string{"LiftBedrockDeny " + roleID, "ClearExhausted " + teamID}) {
		t.Fatalf("lift: %v", got)
	}
	if i, j := slices.Index(g.calls.List, "LiftBedrockDeny "+roleID), slices.Index(g.calls.List, "ClearExhausted "+teamID); i > j {
		t.Fatalf("the condition was cleared before the deny was lifted: %v", g.calls.List)
	}
	if g.iam.Denied[roleID] {
		t.Fatal("role still denied")
	}
}

func TestBackstopFailuresKeepTheRecord(t *testing.T) {
	g := newRig()
	g.store.Teams = []budget.TeamBudget{{TeamID: teamID, Namespace: "afs-t-payments", Marked: true}}
	g.iam.Err["LiftBedrockDeny"] = errors.New("throttled")
	if err := g.r.Reconcile(t.Context()); err == nil {
		t.Fatal("expected the lift failure")
	}
	if len(g.calledWith("ClearExhausted")) != 0 {
		t.Fatalf("the condition was cleared while the deny was still on: %v", g.calls.List)
	}

	g2 := newRig()
	g2.store.Teams = []budget.TeamBudget{{TeamID: teamID, Namespace: "afs-t-payments", Exhausted: true}, {TeamID: "team_01JD2Q3R4S5T6V7W8X9Y0Z0002", Namespace: "afs-t-other", Exhausted: true}}
	g2.iam.Err["DenyBedrock"] = errors.New("no such role")
	err := g2.r.Reconcile(t.Context())
	if err == nil || len(g2.calledWith("DenyBedrock")) != 2 {
		t.Fatalf("a team whose role is missing stopped the others: %v %v", err, g2.calls.List)
	}

	// Neither exhausted nor marked: nothing to do.
	g3 := newRig()
	g3.store.Teams = []budget.TeamBudget{{TeamID: teamID, Namespace: "afs-t-payments"}}
	_ = g3.r.Reconcile(t.Context())
	if len(g3.calledWith("DenyBedrock"))+len(g3.calledWith("LiftBedrockDeny"))+len(g3.calledWith("MarkExhausted")) != 0 {
		t.Fatalf("%v", g3.calls.List)
	}
}

// A pass runs its steps in the order each feeds the next, and one failing step does not stop the rest.
func TestReconcileOrderAndIsolation(t *testing.T) {
	g := newRig()
	g.store.ErrOn["ExpireOverrides"] = errors.New("x")
	g.store.ErrOn["CarryLimits"] = errors.New("y")
	err := g.r.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "expire ceiling overrides") || !strings.Contains(err.Error(), "carry limits") {
		t.Fatalf("%v", err)
	}
	var steps []string
	for _, c := range g.calls.List {
		steps = append(steps, strings.Fields(c)[0])
	}
	if want := []string{"ExpireOverrides", "CarryLimits", "LiveBudgets", "Exhaustion"}; !slices.Equal(steps, want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
}

// RC-US-12.4: the sweep releases and reports; a clean sweep reports nothing.
func TestSweep(t *testing.T) {
	g := newRig()
	var got []int64
	g.r.Released = func(n int64) { got = append(got, n) }
	if err := g.r.Sweep(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("clean sweep: %v %v", err, got)
	}
	g.store.Released = 3
	if err := g.r.Sweep(t.Context()); err != nil || !slices.Equal(got, []int64{3}) {
		t.Fatalf("sweep: %v %v", err, got)
	}
	g.store.ErrOn["ReleaseExpired"] = errors.New("db down")
	if err := g.r.Sweep(t.Context()); err == nil {
		t.Fatal("a failed sweep must be reported")
	}
}

type adder struct{ jobs []engine.Job }

func (a *adder) AddJob(j engine.Job) { a.jobs = append(a.jobs, j) }

func TestJobsAreScheduled(t *testing.T) {
	g := newRig()
	var a adder
	g.r.Register(&a)
	if len(a.jobs) != 2 {
		t.Fatalf("%+v", a.jobs)
	}
	by := map[string]engine.Job{}
	for _, j := range a.jobs {
		by[j.Name] = j
	}
	if by["budget-reconcile"].Every > 30*time.Second || !by["budget-reconcile"].Immediate {
		t.Errorf("a pass every %v leaves no room for the 60 s of RC-US-12.2/12.3", by["budget-reconcile"].Every)
	}
	if by["reservation-sweeper"].Every != time.Minute {
		t.Errorf("sweeper every %v", by["reservation-sweeper"].Every)
	}
}

// A failing step does not stop the later ones, and every error is reported.
func TestEveryStepFailureIsReportedAndTheRestStillRuns(t *testing.T) {
	boom := errors.New("down")
	g := newRig()
	g.store.Budgets = []domain.Budget{monthly("bud_m")}
	g.store.Expiries = []budget.OverrideExpiry{{TeamID: teamID}}
	g.store.Limits = []budget.LimitChange{{TeamID: teamID, BudgetID: "bud_m"}}
	for _, m := range []string{"ExpireOverrides", "CarryLimits", "OpenPeriod", "Exhaustion"} {
		g.store.ErrOn[m] = boom
	}
	err := g.r.Reconcile(t.Context())
	if err == nil {
		t.Fatal("no error")
	}
	for _, m := range []string{"ExpireOverrides", "CarryLimits", "OpenPeriod", "Exhaustion"} {
		if len(g.calledWith(m)) == 0 {
			t.Errorf("%s was skipped after an earlier failure", m)
		}
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %v does not wrap the cause", err)
	}
	// Listing the budgets fails.
	g = newRig()
	g.store.ErrOn["LiveBudgets"] = boom
	if err := g.r.Reconcile(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("list: %v", err)
	}
}

// A pre-open that fails is retried on the next pass; a budget that is gone is forgotten.
func TestPreOpenRetriesAndGoneBudgetsAreForgotten(t *testing.T) {
	boom := errors.New("down")
	g := newRig()
	g.store.Budgets = []domain.Budget{monthly("bud_m")}
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.now = time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	g.store.ErrOn["OpenPeriod"] = boom
	if err := g.r.Reconcile(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("pre-open: %v", err)
	}
	delete(g.store.ErrOn, "OpenPeriod")
	g.calls.List = nil
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.calledWith("OpenPeriod bud_m 2026-12-01T00:00 announce=false"); len(got) != 1 {
		t.Fatalf("the failed pre-open was not retried: %v", g.calls.List)
	}
	g.store.Budgets = nil
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.store.Budgets = []domain.Budget{monthly("bud_m")}
	g.calls.List = nil
	if err := g.r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(g.calledWith("OpenPeriod")) == 0 {
		t.Fatal("a budget that came back was not reopened")
	}
}

// The backstop reports a store or IAM failure of one team and carries on with the next.
func TestBackstopFailuresAreReportedPerTeam(t *testing.T) {
	boom := errors.New("down")
	other := "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	cases := map[string]func(g *rig){
		"mark":  func(g *rig) { g.store.ErrOn["MarkExhausted"] = boom },
		"deny":  func(g *rig) { g.iam.Err["DenyBedrock"] = boom },
		"lift":  func(g *rig) { g.iam.Err["LiftBedrockDeny"] = boom },
		"clear": func(g *rig) { g.store.ErrOn["ClearExhausted"] = boom },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			g := newRig()
			lifting := name == "lift" || name == "clear"
			g.store.Teams = []budget.TeamBudget{
				{TeamID: teamID, Namespace: "afs-t-payments", Exhausted: !lifting, Marked: lifting},
				{TeamID: other, Namespace: "afs-t-other", Exhausted: !lifting, Marked: lifting},
			}
			setup(g)
			if err := g.r.Reconcile(t.Context()); !errors.Is(err, boom) {
				t.Fatalf("err %v", err)
			}
			n := 0
			for _, c := range g.calls.List {
				if strings.Contains(c, other) || strings.Contains(c, "afs-t-other") {
					n++
				}
			}
			if n == 0 {
				t.Errorf("the second team was not tried: %v", g.calls.List)
			}
		})
	}
	// A team with an unusable id fails alone.
	g := newRig()
	g.store.Teams = []budget.TeamBudget{{TeamID: "bogus", Exhausted: true}}
	if err := g.r.Reconcile(t.Context()); err == nil {
		t.Fatal("an invalid team id must be reported")
	}
}

// The defaults: no clock and no logger configured.
func TestDefaultClockAndLogger(t *testing.T) {
	g := newRig()
	r := &budget.Reconciler{Store: g.store, IAM: g.iam}
	g.store.Expiries = []budget.OverrideExpiry{{TeamID: teamID}}
	g.store.Limits = []budget.LimitChange{{TeamID: teamID, BudgetID: "bud_m"}}
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
}
