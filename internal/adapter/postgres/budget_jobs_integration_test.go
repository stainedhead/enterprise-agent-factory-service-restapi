package postgres_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget/budgettest"
)

type budgetIDs struct{}

func (budgetIDs) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), rand.Reader)
}

type budgetRig struct {
	t     *testing.T
	db    *pgtest.DB
	admin *pgx.Conn
	r     *budget.Reconciler
	iam   *budgettest.IAM
	calls *budgettest.Calls
	now   time.Time
	freed []int64
}

func newBudgetRig(t *testing.T) *budgetRig {
	t.Helper()
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	g := &budgetRig{t: t, db: db, admin: db.Admin(t), calls: &budgettest.Calls{}, now: time.Now().UTC()}
	g.iam = budgettest.NewIAM(g.calls)
	// The tests write outbox rows; other tests in this package read the outbox in id order, so take
	// them back out when done.
	var from int64
	if err := g.admin.QueryRow(t.Context(), `SELECT COALESCE(max(id), 0) FROM afs.outbox_events`).Scan(&from); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := g.admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE id > $1 AND event_type LIKE 'budget.%'`, from); err != nil {
			t.Error(err)
		}
	})
	jobs := postgres.NewBudgetJobs(runner, audit, postgres.NewOutboxWriter(), budgetIDs{})
	g.r = &budget.Reconciler{Store: jobs, IAM: g.iam, Clock: func() time.Time { return g.now }, Released: func(n int64) { g.freed = append(g.freed, n) }}
	return g
}

// leader returns a reconciler with no memory of what the previous one did (a new leader).
func (g *budgetRig) leader() *budget.Reconciler {
	runner, audit := reconcilerRunner(g.t, g.db)
	return &budget.Reconciler{Store: postgres.NewBudgetJobs(runner, audit, postgres.NewOutboxWriter(), budgetIDs{}), IAM: g.iam, Clock: func() time.Time { return g.now }}
}

func (g *budgetRig) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.admin.Exec(g.t.Context(), q, args...); err != nil {
		g.t.Fatalf("%s: %v", q, err)
	}
}

func (g *budgetRig) count(q string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.admin.QueryRow(g.t.Context(), q, args...).Scan(&n); err != nil {
		g.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (g *budgetRig) text(q string, args ...any) string {
	g.t.Helper()
	var s string
	if err := g.admin.QueryRow(g.t.Context(), q, args...).Scan(&s); err != nil {
		g.t.Fatalf("%s: %v", q, err)
	}
	return s
}

func (g *budgetRig) reconcile(r *budget.Reconciler) {
	g.t.Helper()
	if err := r.Reconcile(g.t.Context()); err != nil {
		g.t.Fatal(err)
	}
}

// team seeds a team (ceiling 1000) with a budget and returns the team and budget ids.
func (g *budgetRig) team(period string, amount int, extra string) (teamID, budgetID string) {
	tm := g.db.SeedTeam(g.t)
	budgetID = pgtest.NewID("bud")
	g.exec(`INSERT INTO afs.budgets (id, team_id, period, amount_usd`+extra+`) VALUES ($1, $2, $3, $4)`, budgetID, tm.ID, period, amount)
	return tm.ID, budgetID
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// RC-US-12.1 (M3-F1): each budget's counter for the period exists and budget.reset is published once,
// whether the reconciler or the gateway made the counter, and a new leader does not publish it again.
func TestBudgetJobsOpenPeriods(t *testing.T) {
	g := newBudgetRig(t)
	teamA, budA := g.team("monthly", 400, "")
	teamB, budB := g.team("monthly", 200, "") // the gateway's first call made this counter
	teamC, budC := g.team("lifetime", 100, "")
	g.exec(`SELECT afs.ensure_spend_counter($1, $2)`, budB, monthStart(g.now))

	g.reconcile(g.r)
	for _, tc := range []struct{ team, bud, limit string }{{teamA, budA, "400.000000"}, {teamB, budB, "200.000000"}} {
		if got := g.text(`SELECT limit_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`, tc.bud, monthStart(g.now)); got != tc.limit {
			t.Errorf("monthly counter of %s: limit %s", tc.team, got)
		}
		if n := g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.reset' AND aggregate_id = $1 AND team_id = $2`, tc.bud, tc.team); n != 1 {
			t.Errorf("budget.reset for %s published %d times", tc.bud, n)
		}
	}
	// A lifetime budget's period starts at starts_at.
	if n := g.count(`SELECT count(*) FROM afs.spend_counters c JOIN afs.budgets b ON b.id = c.budget_id WHERE b.id = $1 AND c.period_start = b.starts_at`, budC); n != 1 {
		t.Errorf("lifetime counter: %d", n)
	}
	if n := g.count(`SELECT count(*) FROM afs.audit_log WHERE action = 'budget.period_opened' AND team_id = ANY($1)`, []string{teamA, teamB, teamC}); n != 3 {
		t.Errorf("audit marks: %d", n)
	}
	payload := g.text(`SELECT payload::text FROM afs.outbox_events WHERE event_type = 'budget.reset' AND aggregate_id = $1`, budA)
	if payload == "" {
		t.Fatal("no payload")
	}

	// Same leader, new leader: nothing more is published; the limit stays.
	g.reconcile(g.r)
	g.reconcile(g.leader())
	if n := g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.reset' AND aggregate_id = ANY($1)`, []string{budA, budB, budC}); n != 3 {
		t.Errorf("budget.reset published %d times for 3 periods", n)
	}

	// The next month's counter is opened ahead of the boundary, without an event; the event follows at the boundary.
	next := monthStart(g.now).AddDate(0, 1, 0)
	g.now = next.Add(-time.Minute)
	g.reconcile(g.leader())
	if n := g.count(`SELECT count(*) FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`, budA, next); n != 1 {
		t.Errorf("next month's counter by the first second: %d", n)
	}
	if n := g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.reset' AND aggregate_id = $1`, budA); n != 1 {
		t.Errorf("announced before the period started: %d events", n)
	}
	g.now = next.Add(5 * time.Second)
	l := g.leader()
	g.reconcile(l)
	g.reconcile(l)
	if n := g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.reset' AND aggregate_id = $1`, budA); n != 2 {
		t.Errorf("after the boundary: %d events, want 2 (one per period)", n)
	}
}

// A deleted team's budget is left alone.
func TestBudgetJobsSkipDeletedTeams(t *testing.T) {
	g := newBudgetRig(t)
	teamID, budID := g.team("monthly", 100, "")
	g.exec(`UPDATE afs.teams SET state = 'deleting' WHERE id = $1`, teamID)
	g.reconcile(g.r)
	if n := g.count(`SELECT count(*) FROM afs.spend_counters WHERE budget_id = $1`, budID); n != 0 {
		t.Errorf("a counter was opened for a deleting team: %d", n)
	}
}

// RC-US-12.2 (M3-F2): a changed budget amount reaches limit_usd.
func TestBudgetJobsCarryLimits(t *testing.T) {
	g := newBudgetRig(t)
	_, budID := g.team("monthly", 400, "")
	g.reconcile(g.r)
	old := monthStart(g.now).AddDate(0, -1, 0)
	g.exec(`INSERT INTO afs.spend_counters (budget_id, period_start, limit_usd, spent_usd) VALUES ($1, $2, 400, 399)`, budID, old)

	g.exec(`UPDATE afs.budgets SET amount_usd = 900 WHERE id = $1`, budID)
	g.reconcile(g.r)
	cur := `SELECT limit_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`
	if got := g.text(cur, budID, monthStart(g.now)); got != "900.000000" {
		t.Errorf("current limit %s", got)
	}
	if got := g.text(cur, budID, old); got != "400.000000" {
		t.Errorf("a closed period's limit changed: %s", got)
	}
	g.exec(`UPDATE afs.budgets SET amount_usd = 150 WHERE id = $1`, budID)
	g.reconcile(g.r)
	if got := g.text(cur, budID, monthStart(g.now)); got != "150.000000" {
		t.Errorf("lowered limit %s", got)
	}
}

// RC-US-12.4 (M3-F3): a hold a killed gateway left behind is released once it expires, audited, and
// counted; a clean sweep audits nothing.
func TestBudgetJobsSweepReleasesExpiredReservations(t *testing.T) {
	g := newBudgetRig(t)
	teamID, budID := g.team("monthly", 100, "")
	g.reconcile(g.r)
	start := monthStart(g.now)

	gw := g.db.URLs["afs_gateway"]
	conn, err := pgx.Connect(t.Context(), gw)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var holdID string
	if err := conn.QueryRow(t.Context(), `SELECT afs.reserve_spend_held($1, $2, 12.5, 'gw-pod-killed')::text`, budID, start).Scan(&holdID); err != nil {
		t.Fatal(err)
	}
	reserved := `SELECT reserved_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`
	if got := g.text(reserved, budID, start); got != "12.500000" {
		t.Fatalf("reserved %s", got)
	}
	audits := func() int {
		return g.count(`SELECT count(*) FROM afs.audit_log WHERE action = 'ReleaseExpiredReservations' AND after ->> 'released' IS NOT NULL`)
	}
	before := audits()

	// Not expired yet: a sweep changes nothing.
	if err := g.r.Sweep(t.Context()); err != nil || len(g.freed) != 0 || audits() != before {
		t.Fatalf("early sweep: %v %v", err, g.freed)
	}
	// The TTL passes (the gateway is dead and never settles).
	g.exec(`UPDATE afs.spend_reservations SET expires_at = now() - interval '1 minute', created_at = now() - interval '16 minutes' WHERE id = $1::uuid`, holdID)
	if err := g.r.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(g.freed) != 1 || g.freed[0] != 1 || audits() != before+1 {
		t.Fatalf("freed %v audits %d", g.freed, audits()-before)
	}
	if got := g.text(reserved, budID, start); got != "0.000000" {
		t.Errorf("the hold still counts: reserved %s", got)
	}
	if n := g.count(`SELECT count(*) FROM afs.spend_reservations WHERE id = $1::uuid`, holdID); n != 0 {
		t.Errorf("the hold is still there")
	}
	if err := g.r.Sweep(t.Context()); err != nil || len(g.freed) != 1 || audits() != before+1 {
		t.Fatalf("clean sweep: %v %v", err, g.freed)
	}
	_ = teamID
}

// RC-US-12.5 (M3-F4): an expired override is cleared, a budget above the base ceiling is lowered to it, and
// budget.ceiling_override_expired is published once.
func TestBudgetJobsExpireOverrides(t *testing.T) {
	g := newBudgetRig(t)
	over, overBud := g.team("monthly", 100, "")
	g.exec(`UPDATE afs.budgets SET amount_usd = 100 WHERE id = $1`, overBud)
	g.exec(`UPDATE afs.teams SET ceiling_override_usd = 3000, ceiling_override_expires_at = now() + interval '1 hour' WHERE id = $1`, over)
	g.exec(`UPDATE afs.budgets SET amount_usd = 2500 WHERE id = $1`, overBud) // allowed under the override

	under, underBud := g.team("monthly", 300, "")
	g.exec(`UPDATE afs.teams SET ceiling_override_usd = 3000, ceiling_override_expires_at = now() + interval '1 hour' WHERE id = $1`, under)

	live, _ := g.team("monthly", 100, "") // an override still in force is left alone
	g.exec(`UPDATE afs.teams SET ceiling_override_usd = 3000, ceiling_override_expires_at = now() + interval '1 day' WHERE id = $1`, live)

	events := func(team string) int {
		return g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.ceiling_override_expired' AND team_id = $1`, team)
	}
	g.now = time.Now().UTC().Add(2 * time.Hour)
	g.reconcile(g.r)
	g.reconcile(g.r)
	g.reconcile(g.leader())

	for _, tc := range []struct {
		team, bud, amount string
		lowered           bool
	}{{over, overBud, "1000.00", true}, {under, underBud, "300.00", false}} {
		if got := g.text(`SELECT amount_usd::text FROM afs.budgets WHERE id = $1`, tc.bud); got != tc.amount {
			t.Errorf("budget of %s: %s, want %s", tc.team, got, tc.amount)
		}
		if n := g.count(`SELECT count(*) FROM afs.teams WHERE id = $1 AND ceiling_override_usd IS NULL AND ceiling_override_expires_at IS NULL`, tc.team); n != 1 {
			t.Errorf("override of %s not cleared", tc.team)
		}
		if n := events(tc.team); n != 1 {
			t.Errorf("budget.ceiling_override_expired for %s published %d times", tc.team, n)
		}
		var lowered bool
		var base string
		if err := g.admin.QueryRow(t.Context(), `SELECT (payload #>> '{data,budgetLowered}')::boolean, payload #>> '{data,baseCeilingUsd}' FROM afs.outbox_events WHERE event_type = 'budget.ceiling_override_expired' AND team_id = $1`, tc.team).Scan(&lowered, &base); err != nil {
			t.Fatal(err)
		}
		if lowered != tc.lowered || base != "1000.00" {
			t.Errorf("event of %s: lowered=%v base=%s", tc.team, lowered, base)
		}
		if n := g.count(`SELECT count(*) FROM afs.audit_log WHERE action = 'ExpireCeilingOverride' AND team_id = $1`, tc.team); n != 1 {
			t.Errorf("audit records of %s: %d", tc.team, n)
		}
	}
	if events(live) != 0 || g.count(`SELECT count(*) FROM afs.teams WHERE id = $1 AND ceiling_override_usd IS NOT NULL`, live) != 1 {
		t.Error("an override still in force was touched")
	}
	// The lowered budget's counter follows within the same pass (limit carried after the expiry).
	if got := g.text(`SELECT limit_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`, overBud, monthStart(g.now)); got != "1000.000000" {
		t.Errorf("counter limit after the expiry: %s", got)
	}
}

// RC-US-12.2, RC-US-12.3 (M3-F5): a spent block-mode budget gets BudgetExhausted=True on the team, its agents
// and its pools and the deny on its Bedrock role; raising the budget, a notify budget, or a new period
// has neither.
func TestBudgetJobsBackstop(t *testing.T) {
	g := newBudgetRig(t)
	teamID, budID := g.team("monthly", 100, "")
	other, otherBud := g.team("monthly", 100, "") // a different team stays untouched
	g.reconcile(g.r)
	g.exec(`UPDATE afs.spend_counters SET spent_usd = 100 WHERE budget_id = $1`, otherBud)
	g.exec(`UPDATE afs.budgets SET on_exhaust = 'notify' WHERE id = $1`, otherBud)
	for _, rs := range [][2]string{{"team", teamID}, {"agent", pgtest.NewID("agt")}, {"agent", pgtest.NewID("agt")}, {"pool", pgtest.NewID("pool")}} {
		g.exec(`INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase, conditions)
VALUES ($1, $2, $3, 'Ready', '[{"type":"IamReady","status":"True","lastTransitionTime":"2026-11-22T10:00:00Z"}]')`, rs[0], rs[1], teamID)
	}
	g.exec(`INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase) VALUES ('team', $1, $1, 'Ready')`, other)

	marked := func(team string) int {
		return g.count(`SELECT count(*) FROM afs.resource_status WHERE team_id = $1 AND conditions @> '[{"type":"BudgetExhausted","status":"True"}]'`, team)
	}
	namespace := g.text(`SELECT namespace FROM afs.teams WHERE id = $1`, teamID)
	role := "/afs/" + namespace + "/afs-" + teamID[len("team_"):] + "-bedrock"

	// Below the limit: nothing.
	g.exec(`UPDATE afs.spend_counters SET spent_usd = 99.99 WHERE budget_id = $1`, budID)
	g.reconcile(g.r)
	if marked(teamID) != 0 || g.iam.Denied[role] {
		t.Fatal("backstop applied below the limit")
	}

	// Spent: condition on all four rows (the other conditions kept) and the deny.
	g.exec(`UPDATE afs.spend_counters SET spent_usd = 100 WHERE budget_id = $1`, budID)
	g.reconcile(g.r)
	if marked(teamID) != 4 || !g.iam.Denied[role] {
		t.Fatalf("marked %d denied %v (%v)", marked(teamID), g.iam.Denied, g.calls.List)
	}
	if n := g.count(`SELECT count(*) FROM afs.resource_status WHERE team_id = $1 AND conditions @> '[{"type":"IamReady"}]'`, teamID); n != 4 {
		t.Errorf("another condition was lost: %d", n)
	}
	if marked(other) != 0 || g.iam.Denied["/afs/"+g.text(`SELECT namespace FROM afs.teams WHERE id = $1`, other)+"/afs-"+other[len("team_"):]+"-bedrock"] {
		t.Error("a notify-mode budget at its limit was blocked")
	}
	if n := g.count(`SELECT count(*) FROM afs.audit_log WHERE action = 'BudgetExhausted' AND team_id = $1`, teamID); n != 1 {
		t.Errorf("audit of entering the backstop: %d", n)
	}
	g.reconcile(g.r) // repeated: re-asserts the deny, no second transition audit
	if n := g.count(`SELECT count(*) FROM afs.audit_log WHERE action = 'BudgetExhausted' AND team_id = $1`, teamID); n != 1 {
		t.Errorf("audit of entering the backstop after a repeat: %d", n)
	}

	// A new pool's status row appears while blocked: it gets the condition on the next pass.
	g.exec(`INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase) VALUES ('agent', $1, $2, 'Ready')`, pgtest.NewID("agt"), teamID)
	g.reconcile(g.r)
	if marked(teamID) != 5 {
		t.Errorf("a resource created while blocked was not marked: %d", marked(teamID))
	}

	// Raised within the ceiling: carried into the limit, deny lifted, condition removed in one pass.
	g.exec(`UPDATE afs.budgets SET amount_usd = 500 WHERE id = $1`, budID)
	g.reconcile(g.r)
	if marked(teamID) != 0 || g.iam.Denied[role] {
		t.Fatalf("after the raise: marked %d denied %v", marked(teamID), g.iam.Denied)
	}
	if n := g.count(`SELECT count(*) FROM afs.audit_log WHERE action = 'BudgetRestored' AND team_id = $1`, teamID); n != 1 {
		t.Errorf("audit of leaving the backstop: %d", n)
	}

	// Spent again, then the month rolls over: the new counter starts at zero and lifts it.
	g.exec(`UPDATE afs.spend_counters SET spent_usd = 500 WHERE budget_id = $1 AND period_start = $2`, budID, monthStart(g.now))
	g.reconcile(g.r)
	if marked(teamID) == 0 || !g.iam.Denied[role] {
		t.Fatal("not blocked again")
	}
	g.now = monthStart(g.now).AddDate(0, 1, 0).Add(10 * time.Second)
	g.reconcile(g.leader())
	if marked(teamID) != 0 || g.iam.Denied[role] {
		t.Fatalf("after the period reset: marked %d denied %v", marked(teamID), g.iam.Denied)
	}
}
