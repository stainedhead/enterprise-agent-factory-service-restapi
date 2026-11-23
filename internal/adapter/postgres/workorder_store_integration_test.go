package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/teardown"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

const woDigest = "ghcr.io/x/omp@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type woRig struct {
	t     *testing.T
	db    *pgtest.DB
	admin *pgx.Conn
	store *postgres.WorkOrderStore
	tpl   string
	model string
}

func newWORig(t *testing.T) *woRig {
	t.Helper()
	db := pgtest.Get(t)
	db.SeedOrchestrationRuntime(t)
	runner, audit := reconcilerRunner(t, db)
	r := &woRig{t: t, db: db, admin: db.Admin(t), model: "anthropic.claude-wo-test"}
	r.store = postgres.NewWorkOrderStore(runner, audit, postgres.NewOutboxWriter(), budgetIDs{}, time.Now)
	r.exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'WO test', 4096) ON CONFLICT DO NOTHING`, r.model)
	r.exec(`INSERT INTO afs.service_settings (key, value) VALUES ('work_orders.cost_center', '"CC-SEC"'), ('work_orders.owner_email', '"secops@example.com"')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	r.tpl = r.template("cve-"+strings.ToLower(pgtest.NewID("x")[2:10]), true)
	return r
}

func (r *woRig) exec(sql string, args ...any) {
	r.t.Helper()
	if _, err := r.admin.Exec(r.t.Context(), sql, args...); err != nil {
		r.t.Fatalf("%.70s: %v", sql, err)
	}
}

func (r *woRig) count(sql string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.admin.QueryRow(r.t.Context(), sql, args...).Scan(&n); err != nil {
		r.t.Fatalf("%.70s: %v", sql, err)
	}
	return n
}

func (r *woRig) str(sql string, args ...any) string {
	r.t.Helper()
	var s string
	if err := r.admin.QueryRow(r.t.Context(), sql, args...).Scan(&s); err != nil {
		r.t.Fatalf("%.70s: %v", sql, err)
	}
	return s
}

// template inserts a swarm template with two pools in two stages, a handoff, a config value and two secrets.
func (r *woRig) template(name string, enabled bool) string {
	r.t.Helper()
	id := pgtest.NewID("swt")
	spec := map[string]any{
		"tags":   map[string]string{"purpose": "cve"},
		"stages": []any{map[string]any{"name": "triage", "position": 1}, map[string]any{"name": "fix", "position": 2}},
		"pools": []any{
			map[string]any{"name": "triage", "stage": "triage", "template": "omp", "image": woDigest, "modelId": r.model, "workers": 2, "scratchSizeGiB": 30},
			map[string]any{"name": "fixers", "stage": "fix", "template": "omp", "image": woDigest, "modelId": r.model, "workers": 3},
		},
		"handoffs":     []any{map[string]any{"from": "triage", "to": "fixers"}},
		"configValues": []any{map[string]any{"pool": "fixers", "name": "SCAN_TARGET", "value": "libfoo"}},
		"secrets": []any{
			map[string]any{"pool": "triage", "name": "scanner-token", "description": "scanner API token"},
			map[string]any{"pool": "fixers", "name": "deploy-key", "delivery": "file", "target": "/etc/afs/deploy-key"},
		},
	}
	b, _ := json.Marshal(spec)
	r.exec(`INSERT INTO afs.swarm_templates (id, name, kind, spec, budget_usd, ttl_hours, enabled) VALUES ($1, $2, 'cve_remediation', $3, 40, 72, $4)`, id, name, b, enabled)
	return id
}

// owner makes a team that owns the repositories listed (AFS_OWNED_REPOS).
func (r *woRig) owner(entries string) string {
	r.t.Helper()
	tm := r.db.SeedTeam(r.t)
	r.exec(`INSERT INTO afs.namespace_config_values (id, team_id, name, config_value) VALUES ($1, $2, 'AFS_OWNED_REPOS', $3)`, pgtest.NewID("ncv"), tm.ID, entries)
	return tm.ID
}

var woSeq int

func (r *woRig) event(repos ...workorder.RepoInput) workorder.NewWorkOrder {
	woSeq++
	return workorder.NewWorkOrder{
		SourceEventID: "evt-" + strings.ToLower(pgtest.NewID("e")), Kind: "cve_remediation", CVEID: fmt.Sprintf("CVE-2026-%d%04d", time.Now().UnixNano()%1000000, woSeq),
		Severity: "high", Title: "Patch libfoo", SLADueAt: time.Now().Add(7 * 24 * time.Hour).UTC(), TemplateID: r.tpl, Repos: repos,
	}
}

func repoIn(host, repo, team, contact string) workorder.RepoInput {
	return workorder.RepoInput{Host: host, Repo: repo, OwningTeamID: team, OwningContact: contact}
}

func wantRejection(t *testing.T, err error, reason string) *workorder.Rejection {
	t.Helper()
	var rej *workorder.Rejection
	if !errors.As(err, &rej) || rej.Reason != reason {
		t.Fatalf("err = %v, want a %s refusal", err, reason)
	}
	return rej
}

// M6-E1: one work order per source event id, even when delivered twice and even when delivered at once; owners
// resolve through afs.resolve_repo_owners (D38) and explicit owners win.
func TestWorkOrderIntake(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	host := "wo" + strings.ToLower(pgtest.NewID("h")[2:8]) + ".example.com"
	exact, wild, other := r.owner(host+"/acme/api"), r.owner(host+"/acme/*"), r.owner(host+"/other/lib")
	deleted := r.owner(host + "/acme/legacy")
	r.exec(`UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, deleted)

	ev := r.event(
		repoIn(host, "acme/api", "", ""),                     // the exact entry beats the wildcard
		repoIn(host, "acme/web", "", ""),                     // only the wildcard
		repoIn(host, "acme/legacy", "", ""),                  // the exact owner is deleted: the wildcard answers
		repoIn(host, "other/lib", other, ""),                 // explicit team
		repoIn(host, "acme/vendored", "", "dev@example.com"), // explicit contact, no resolution
		repoIn(host, "acme/both", wild, "both@example.com"),  // both
	)
	ev.Budget = domain.MustMoney("12.50")
	res, err := r.store.CreateFromEvent(ctx, ev, time.Now())
	if err != nil || res.Duplicate || !strings.HasPrefix(res.ID, "wo_") {
		t.Fatalf("create: %+v, %v", res, err)
	}
	owners := map[string]string{}
	rows, err := r.admin.Query(ctx, `SELECT repo, coalesce(owning_team_id, '') || '|' || coalesce(owning_contact, '') FROM afs.work_order_repos WHERE work_order_id = $1`, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var repo, o string
		if err := rows.Scan(&repo, &o); err != nil {
			t.Fatal(err)
		}
		owners[repo] = o
	}
	rows.Close()
	want := map[string]string{"acme/api": exact + "|", "acme/web": wild + "|", "acme/legacy": wild + "|", "other/lib": other + "|",
		"acme/vendored": "|dev@example.com", "acme/both": wild + "|both@example.com"}
	for k, v := range want {
		if owners[k] != v {
			t.Errorf("%s owner = %q, want %q", k, owners[k], v)
		}
	}
	if got := r.str(`SELECT status || '|' || source || '|' || source_event_id || '|' || budget_usd::text || '|' || template_id || '|' || severity FROM afs.work_orders WHERE id = $1`, res.ID); got != "open|eventbridge|"+ev.SourceEventID+"|12.50|"+r.tpl+"|high" {
		t.Errorf("work order = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'CreateWorkOrder' AND team_id IS NULL`, res.ID); n != 1 {
		t.Errorf("audit rows = %d", n)
	}
	var payload string
	if err := r.admin.QueryRow(ctx, `SELECT payload::text FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.created'`, res.ID).Scan(&payload); err != nil || !strings.Contains(payload, `"workOrderId"`) || !strings.Contains(payload, `"occurredAt"`) {
		t.Errorf("work_order.created payload = %s, %v", payload, err)
	}

	// Delivered again: the same work order, nothing written.
	again, err := r.store.CreateFromEvent(ctx, ev, time.Now())
	if err != nil || !again.Duplicate || again.ID != res.ID {
		t.Fatalf("duplicate: %+v, %v", again, err)
	}
	if n := r.count(`SELECT count(*) FROM afs.work_orders WHERE source_event_id = $1`, ev.SourceEventID); n != 1 {
		t.Errorf("%d work orders for one event", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.created'`, res.ID); n != 1 {
		t.Errorf("%d created events", n)
	}

	// Another event for the same CVE while the first is open.
	dup := r.event(repoIn(host, "acme/web", "", ""))
	dup.CVEID = ev.CVEID
	rej := wantRejection(t, func() error { _, err := r.store.CreateFromEvent(ctx, dup, time.Now()); return err }(), "open_work_order")
	if rej.Detail["workOrderId"] != res.ID {
		t.Errorf("detail = %v", rej.Detail)
	}
	// Once the first is finished the CVE may be taken again.
	r.exec(`UPDATE afs.work_orders SET status = 'cancelled' WHERE id = $1`, res.ID)
	if res2, err := r.store.CreateFromEvent(ctx, dup, time.Now()); err != nil || res2.Duplicate {
		t.Errorf("after cancel: %+v, %v", res2, err)
	}
}

func TestWorkOrderIntakeRefusalsWriteNothing(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	host := "wo" + strings.ToLower(pgtest.NewID("h")[2:8]) + ".example.com"
	a, b := r.owner(host+"/shared/*"), r.owner(host+"/shared/*")
	r.owner(host + "/mine/*")
	before := r.count(`SELECT count(*) FROM afs.work_orders`)

	// Two teams at the best match.
	rej := wantRejection(t, func() error {
		_, err := r.store.CreateFromEvent(ctx, r.event(repoIn(host, "shared/x", "", ""), repoIn(host, "nobody/x", "", "")), time.Now())
		return err
	}(), "ambiguous_repos")
	amb := rej.Detail["ambiguousRepos"].([]map[string]any)
	ids := amb[0]["teamIds"].([]string)
	if len(amb) != 1 || amb[0]["repo"] != host+"/shared/x" || len(ids) != 2 || (ids[0] != a || ids[1] != b) && (ids[0] != b || ids[1] != a) {
		t.Errorf("ambiguous = %v", amb)
	}
	if un, _ := rej.Detail["unownedRepos"].([]string); len(un) != 1 || un[0] != host+"/nobody/x" {
		t.Errorf("unowned = %v: every failing repository is listed", rej.Detail)
	}
	// Nobody owns it.
	rej = wantRejection(t, func() error {
		_, err := r.store.CreateFromEvent(ctx, r.event(repoIn(host, "mine/ok", "", ""), repoIn(host, "nobody/x", "", "")), time.Now())
		return err
	}(), "unowned_repos")
	if un := rej.Detail["unownedRepos"].([]string); len(un) != 1 || un[0] != host+"/nobody/x" {
		t.Errorf("unowned = %v", un)
	}
	// An explicit team that does not exist, or is deleted.
	gone := r.db.SeedTeam(t)
	r.exec(`UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, gone.ID)
	for _, id := range []string{pgtest.NewID("team"), gone.ID} {
		_ = wantRejection(t, func() error {
			_, err := r.store.CreateFromEvent(ctx, r.event(repoIn(host, "mine/ok", id, "")), time.Now())
			return err
		}(), "unknown_owner")
	}
	// A disabled and an unknown template.
	off := r.template("off-"+strings.ToLower(pgtest.NewID("x")[2:8]), false)
	for _, id := range []string{off, pgtest.NewID("swt")} {
		ev := r.event(repoIn(host, "mine/ok", "", ""))
		ev.TemplateID = id
		_ = wantRejection(t, func() error { _, err := r.store.CreateFromEvent(ctx, ev, time.Now()); return err }(), "template_unavailable")
	}
	if after := r.count(`SELECT count(*) FROM afs.work_orders`); after != before {
		t.Errorf("work orders went from %d to %d: a refused event writes nothing", before, after)
	}
	// The refusal itself is audited, with the reason and no event body.
	id := "evt-refused"
	if err := r.store.Reject(ctx, id, &workorder.Rejection{Reason: "unowned_repos", Detail: map[string]any{"unownedRepos": []string{"x"}}}); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT outcome || '|' || (after ->> 'reason') FROM afs.audit_log WHERE resource_id = $1 AND action = 'work_order.intake_refused'`, id); got != "failed|unowned_repos" {
		t.Errorf("audit = %s", got)
	}
}

func TestWorkOrderIntakeConcurrentDeliveries(t *testing.T) {
	r := newWORig(t)
	host := "wo" + strings.ToLower(pgtest.NewID("h")[2:8]) + ".example.com"
	r.owner(host + "/acme/*")
	ev := r.event(repoIn(host, "acme/api", "", ""))
	var wg sync.WaitGroup
	results := make([]workorder.CreateResult, 6)
	errs := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = r.store.CreateFromEvent(context.Background(), ev, time.Now())
		}()
	}
	wg.Wait()
	created := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
		if !results[i].Duplicate {
			created++
		}
		if results[i].ID != results[0].ID {
			t.Errorf("delivery %d got %s, want %s", i, results[i].ID, results[0].ID)
		}
	}
	if n := r.count(`SELECT count(*) FROM afs.work_orders WHERE source_event_id = $1`, ev.SourceEventID); n != 1 {
		t.Errorf("%d work orders", n)
	}
	if created < 1 {
		t.Errorf("created = %d", created)
	}
}

// intake makes one open work order with two repositories owned by two teams.
func (r *woRig) intake(budget string) (id, teamA, teamB string) {
	r.t.Helper()
	teamA, teamB = r.db.SeedTeam(r.t).ID, r.db.SeedTeam(r.t).ID
	ev := r.event(repoIn("github.com", "acme/api", teamA, ""), repoIn("github.com", "acme/web", teamB, "sec@example.com"))
	if budget != "" {
		ev.Budget = domain.MustMoney(budget)
	}
	res, err := r.store.CreateFromEvent(r.t.Context(), ev, time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	return res.ID, teamA, teamB
}

// M6-E2: the remediation team comes from the template: lifetime budget = the cap, expiry = the TTL, rule
// at_work_complete, stages, pools, handoffs and configuration; work_order.assigned.
func TestWorkOrderTeamInstantiation(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	id, _, _ := r.intake("25.00")

	ids, err := r.store.Unassigned(ctx)
	if err != nil || !contains(ids, id) {
		t.Fatalf("unassigned = %v, %v", ids, err)
	}
	a := &workorder.Assigner{Store: r.store, Now: time.Now}
	if err := a.Run(ctx); err != nil {
		t.Fatalf("assign: %v", err)
	}
	var team, status string
	if err := r.admin.QueryRow(ctx, `SELECT team_id, status FROM afs.work_orders WHERE id = $1`, id).Scan(&team, &status); err != nil || status != "assigned" || team == "" {
		t.Fatalf("work order = %s %s, %v", team, status, err)
	}
	if got := r.str(`SELECT kind || '|' || teardown_rule || '|' || max_budget_usd::text || '|' || (expires_at > now() + interval '71 hours' AND expires_at < now() + interval '73 hours')::text FROM afs.teams WHERE id = $1`, team); got != "ephemeral|at_work_complete|25.00|true" {
		t.Errorf("team = %s: cap 25 from the work order, TTL 72 h from the template", got)
	}
	if got := r.str(`SELECT period || '|' || amount_usd::text || '|' || on_exhaust FROM afs.budgets WHERE team_id = $1`, team); got != "lifetime|25.00|block" {
		t.Errorf("budget = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM afs.agent_pools WHERE team_id = $1`, team); n != 2 {
		t.Errorf("pools = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.swarm_stages WHERE team_id = $1`, team); n != 2 {
		t.Errorf("stages = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.pool_handoffs WHERE team_id = $1`, team); n != 1 {
		t.Errorf("handoffs = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.pool_config_values WHERE team_id = $1 AND name = 'SCAN_TARGET' AND config_value = 'libfoo'`, team); n != 1 {
		t.Errorf("config values = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.assigned' AND team_id = $2`, id, team); n != 1 {
		t.Errorf("work_order.assigned events = %d", n)
	}
	// Idempotent: nothing is unassigned, and asking again returns the same team.
	if again, err := r.store.Instantiate(ctx, id); err != nil || again != team {
		t.Errorf("again = %s, %v", again, err)
	}
	if ids, _ := r.store.Unassigned(ctx); contains(ids, id) {
		t.Error("an assigned work order is still unassigned")
	}

	// Without a cap the template's applies; a template disabled after the work order was created is an error that
	// leaves the work order open (the Assigner then backs off).
	id2, _, _ := r.intake("")
	if tm, err := r.store.Instantiate(ctx, id2); err != nil || r.str(`SELECT max_budget_usd::text FROM afs.teams WHERE id = $1`, tm) != "40.00" {
		t.Errorf("template cap: %v", err)
	}
	id3, _, _ := r.intake("")
	r.exec(`UPDATE afs.swarm_templates SET enabled = false WHERE id = $1`, r.tpl)
	if _, err := r.store.Instantiate(ctx, id3); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled template: %v", err)
	}
	if r.str(`SELECT status FROM afs.work_orders WHERE id = $1`, id3) != "open" {
		t.Error("the work order must stay open")
	}
	r.exec(`UPDATE afs.swarm_templates SET enabled = true WHERE id = $1`, r.tpl)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

type fakeSM struct {
	mu      sync.Mutex
	created []string
	tags    []map[string]string
}

func (f *fakeSM) CreateEmpty(_ context.Context, path string, tags map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, path)
	f.tags = append(f.tags, tags)
	return "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + path + "-AbCdEf", nil
}

// RC-12: the template's secrets exist empty, bound to their pools, once, through -db's
// afs.record_work_order_secret (000019).
func TestWorkOrderTemplateSecrets(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	id, _, _ := r.intake("")
	team, err := r.store.Instantiate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cr := &fakeSM{}
	job := &workorder.Secrets{Store: r.store, Creator: cr}

	pending, err := r.store.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mine := 0
	for _, p := range pending {
		if p.TeamID == team {
			mine++
			if !strings.HasPrefix(p.Path, "afs/") || !strings.Contains(p.Path, "/"+p.Pool+"/"+p.Name) {
				t.Errorf("path = %s: the database's afs.secret_path, under the pool", p.Path)
			}
		}
	}
	if mine != 2 {
		t.Fatalf("pending for the team = %d (%+v)", mine, pending)
	}
	// Other teams' pending secrets (other tests' work orders) are served in the same pass; only this team is checked.
	for pass := 0; pass < 2; pass++ {
		if err := job.Run(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if n := r.count(`SELECT count(*) FROM afs.secrets WHERE team_id = $1 AND origin = 'managed' AND pool_id IS NOT NULL`, team); n != 2 {
		t.Errorf("secrets = %d", n)
	}
	if got := r.str(`SELECT b.delivery || '|' || b.target FROM afs.pool_secret_bindings b JOIN afs.secrets s ON s.id = b.secret_id WHERE s.team_id = $1 AND s.name = 'scanner-token'`, team); got != "env|SCANNER_TOKEN" {
		t.Errorf("env binding = %s", got)
	}
	if got := r.str(`SELECT b.delivery || '|' || b.target FROM afs.pool_secret_bindings b JOIN afs.secrets s ON s.id = b.secret_id WHERE s.team_id = $1 AND s.name = 'deploy-key'`, team); got != "file|/etc/afs/deploy-key" {
		t.Errorf("file binding = %s", got)
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE team_id = $1 AND event_type = 'secret.created'`, team); n != 2 {
		t.Errorf("secret.created events = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.audit_log WHERE team_id = $1 AND action = 'RecordWorkOrderSecret'`, team); n != 2 {
		t.Errorf("audit rows = %d", n)
	}
	tagged := 0
	for i, p := range cr.created {
		if cr.tags[i][workorder.TagTeamID] == team {
			tagged++
			if !strings.HasSuffix(p, "scanner-token") && !strings.HasSuffix(p, "deploy-key") {
				t.Errorf("created %s", p)
			}
		}
	}
	if tagged != 2 {
		t.Errorf("secrets created in Secrets Manager for the team = %d: once each, tagged with the team", tagged)
	}
	// The function's refusals map to domain errors (P0002, 23503, 23514, 23505, 55000).
	good := func(ts workorder.TemplateSecret) workorder.TemplateSecret {
		ts.WorkOrderID, ts.Name, ts.Delivery, ts.Target = id, "x-token", "env", "X_TOKEN"
		return ts
	}
	poolID := r.str(`SELECT id FROM afs.agent_pools WHERE team_id = $1 ORDER BY name LIMIT 1`, team)
	triageID := r.str(`SELECT b.pool_id FROM afs.pool_secret_bindings b JOIN afs.secrets s ON s.id = b.secret_id WHERE s.team_id = $1 AND s.name = 'scanner-token'`, team)
	arnFor2 := func(name, pool string) string {
		return "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + r.str(`SELECT afs.secret_path($1, NULL, $2, $3)`, team, pool, name) + "-AbCdEf"
	}
	arnFor := func(name string) string { return arnFor2(name, poolID) }
	kindOf := func(err error) domain.ErrorKind {
		var de *domain.Error
		if !errors.As(err, &de) {
			return ""
		}
		return de.Kind
	}
	for _, c := range []struct {
		name string
		ts   workorder.TemplateSecret
		arn  string
		want domain.ErrorKind
	}{
		{"unknown work order (P0002)", workorder.TemplateSecret{WorkOrderID: "wo_" + strings.Repeat("0", 26), PoolID: poolID, Name: "x-token", Delivery: "env", Target: "X_TOKEN"}, arnFor("x-token"), domain.KindNotFound},
		{"pool of another team (23503)", good(workorder.TemplateSecret{PoolID: pgtest.NewID("pool")}), arnFor("x-token"), domain.KindValidation},
		{"ARN not at the secret path (23514)", good(workorder.TemplateSecret{PoolID: poolID}), "arn:aws:secretsmanager:us-east-1:111122223333:secret:elsewhere-AbCdEf", domain.KindValidation},
		{"relative file target (23514)", workorder.TemplateSecret{WorkOrderID: id, PoolID: poolID, Name: "x-token", Delivery: "file", Target: "relative"}, arnFor("x-token"), domain.KindValidation},
		{"target already bound (23505)", workorder.TemplateSecret{WorkOrderID: id, PoolID: triageID, Name: "y-token", Delivery: "env", Target: "SCANNER_TOKEN"}, arnFor2("y-token", triageID), domain.KindConflict},
	} {
		if err := r.store.Record(ctx, c.ts, c.arn); kindOf(err) != c.want {
			t.Errorf("%s: err = %v (kind %q), want %s", c.name, err, kindOf(err), c.want)
		}
	}
	// Recording a secret the team already has is a no-op (the function is idempotent on team and name).
	if err := r.store.Record(ctx, workorder.TemplateSecret{WorkOrderID: id, PoolID: poolID, Name: "scanner-token", Delivery: "env", Target: "SCANNER_TOKEN"}, arnFor("scanner-token")); err != nil {
		t.Errorf("idempotent record: %v", err)
	}
	// A work order that is not live (cancelled) has no live remediation team (55000).
	r.exec(`UPDATE afs.work_orders SET status = 'cancelled' WHERE id = $1`, id)
	if err := r.store.Record(ctx, good(workorder.TemplateSecret{PoolID: poolID}), arnFor("x-token")); kindOf(err) != domain.KindConflict {
		t.Errorf("not live: err = %v, want a Conflict", err)
	}
}

// M6-E4: the active list, a merge from GitHub, completion, and the hand-over to teardown.
func TestWorkOrderTrackingAndCompletion(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	id, teamA, teamB := r.intake("")
	team, err := r.store.Instantiate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// Agents report PRs through the API (here: by SQL as the admin).
	r.exec(`UPDATE afs.work_order_repos SET status = 'pr_opened', pr_url = 'https://github.com/acme/api/pull/7', pr_number = 7 WHERE work_order_id = $1 AND repo = 'acme/api'`, id)
	r.exec(`UPDATE afs.work_order_repos SET status = 'not_affected' WHERE work_order_id = $1 AND repo = 'acme/web'`, id)

	var wo workorder.WorkOrder
	list, err := r.store.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if w.ID == id {
			wo = w
		}
	}
	if wo.ID == "" || wo.TeamID != team || wo.Status != "assigned" || wo.TeamTornDown || wo.SLAState != "on_track" || wo.ExternalRef == "" || len(wo.Repos) != 2 {
		t.Fatalf("active = %+v", wo)
	}
	if rp := wo.Repos[0]; rp.Repo != "acme/api" || rp.Status != "pr_opened" || rp.PRNumber != 7 || rp.OwningTeamID != teamA {
		t.Errorf("repo = %+v", rp)
	}
	if rp := wo.Repos[1]; rp.OwningTeamID != teamB || rp.OwningContact != "sec@example.com" {
		t.Errorf("repo = %+v", rp)
	}

	// The tracker settles the status to pr_opened (every remaining repository has a PR).
	gh := &stubGitHub{state: workorder.PRState{State: "open"}}
	tr := &workorder.Tracker{Store: r.store, GitHub: gh, Now: time.Now}
	if err := tr.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT status FROM afs.work_orders WHERE id = $1`, id); got != "pr_opened" {
		t.Fatalf("status = %s", got)
	}

	// A stale change (the agent re-reported another PR meanwhile) is not applied.
	stale := workorder.RepoChange{WorkOrderID: id, TeamID: team, Host: "github.com", Repo: "acme/api", FromStatus: "pr_opened", FromPRURL: "https://github.com/acme/api/pull/6",
		To: "merged", MergedAt: time.Now()}
	if ok, err := r.store.ApplyRepo(ctx, stale); err != nil || ok {
		t.Fatalf("stale apply = %v, %v", ok, err)
	}

	// GitHub says merged: the repository merges, the work order completes.
	gh.state = workorder.PRState{State: "closed", Merged: true, MergedAt: time.Date(2026, 11, 22, 9, 30, 0, 0, time.UTC)}
	if err := tr.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT status || '|' || merged_at::text FROM afs.work_order_repos WHERE work_order_id = $1 AND repo = 'acme/api'`, id); got != "merged|2026-11-22 09:30:00+00" {
		t.Errorf("repo = %s", got)
	}
	if got := r.str(`SELECT status || '|' || (completed_at IS NOT NULL)::text FROM afs.work_orders WHERE id = $1`, id); got != "done|true" {
		t.Errorf("work order = %s", got)
	}
	for _, typ := range []string{"work_order.repo_updated", "work_order.merged", "work_order.completed"} {
		if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = $2 AND team_id = $3`, id, typ, team); n != 1 {
			t.Errorf("%s events = %d", typ, n)
		}
	}
	if n := r.count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action IN ('work_order.repo_updated', 'work_order.status_changed') AND team_id = $2`, id, team); n != 3 {
		t.Errorf("audit rows = %d", n)
	}
	// Idempotent: a further pass writes nothing.
	if err := tr.Poll(ctx); err != nil || tr.Settle(ctx) != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.completed'`, id); n != 1 {
		t.Errorf("completed events = %d", n)
	}

	// Done makes the team due for teardown (rule at_work_complete), and the teardown job starts it.
	if got := r.str(`SELECT coalesce(reason, '') FROM afs.teams_due_for_teardown WHERE team_id = $1`, team); got != "work_complete" {
		t.Fatalf("due reason = %q", got)
	}
	runner, audit := reconcilerRunner(t, r.db)
	job := &teardown.Job{Store: postgres.NewTeardownStore(runner, audit, postgres.NewOutboxWriter(), budgetIDs{}), Now: time.Now}
	if err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT state || '|' || teardown_reason FROM afs.teams WHERE id = $1`, team); got != "deleting|work_complete" {
		t.Errorf("team = %s", got)
	}
}

func TestWorkOrderTeamTornDownWithRepositoriesRemaining(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	id, _, _ := r.intake("")
	team, err := r.store.Instantiate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE afs.work_order_repos SET status = 'merged', pr_url = 'https://github.com/acme/api/pull/7', merged_at = now() WHERE work_order_id = $1 AND repo = 'acme/api'`, id)
	r.exec(`UPDATE afs.teams SET state = 'deleting', torn_down_at = now(), teardown_reason = 'expired' WHERE id = $1`, team)

	tr := &workorder.Tracker{Store: r.store, Now: time.Now}
	if err := tr.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT status FROM afs.work_orders WHERE id = $1`, id); got != "failed" {
		t.Fatalf("status = %s: the team went with a repository remaining", got)
	}
	if got := r.str(`SELECT payload -> 'data' ->> 'reason' FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.failed'`, id); !strings.Contains(got, "1 repositories") {
		t.Errorf("failed event reason = %q", got)
	}
}

// M6-E5: once per event and recipient; team notifications are outbox events carrying the owner team's id
// (RC-15 routes by it); contacts get a row; the work order's own events carry the remediation team.
func TestWorkOrderNotifications(t *testing.T) {
	r := newWORig(t)
	ctx := t.Context()
	id, teamA, _ := r.intake("")
	team, err := r.store.Instantiate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE afs.work_order_repos SET status = 'pr_opened', pr_url = 'https://github.com/acme/api/pull/7', pr_number = 7 WHERE work_order_id = $1 AND repo = 'acme/api'`, id)

	r.exec(`UPDATE afs.service_settings SET value = 'null' WHERE key = 'work_orders.notification_channel_id'`)
	if ch, err := r.store.NotificationChannel(ctx); err != nil || ch != "" {
		t.Fatalf("channel = %q, %v: nothing is set", ch, err)
	}
	var wo workorder.WorkOrder
	list, _ := r.store.Active(ctx)
	for _, w := range list {
		if w.ID == id {
			wo = w
		}
	}
	mailer := &recordingMailer{}
	n := &workorder.Notifier{Store: r.store, Mailer: mailer}
	for pass := 0; pass < 2; pass++ {
		if err := n.Notify(ctx, []workorder.WorkOrder{wo}); err != nil {
			t.Fatal(err)
		}
	}
	// Team A: assigned (work order) and pr_opened (its repository), once each, as events for team A.
	for _, kind := range []string{"work_order.assigned", "work_order.pr_opened"} {
		if c := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = $2 AND team_id = $3 AND payload -> 'data' ->> 'notification' = 'true'`, id, kind, teamA); c != 1 {
			t.Errorf("%s notifications to team A = %d", kind, c)
		}
	}
	if c := r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1 AND recipient = $2`, id, teamA); c != 2 {
		t.Errorf("team A rows = %d", c)
	}
	// No channel: the contact is not emailed and nothing is recorded.
	if len(mailer.sent) != 0 || r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1 AND recipient = 'sec@example.com'`, id) != 0 {
		t.Errorf("emails sent without a channel: %v", mailer.sent)
	}

	// A disabled channel, a team channel and a non-email channel are not the service channel; an enabled
	// service email channel is.
	ch := pgtest.NewID("nch")
	r.exec(`INSERT INTO afs.notification_channels (id, name, kind, config, enabled) VALUES ($1, $2, 'email', '{"recipients":["ops@example.com"]}', false)`, ch, "svc-"+strings.ToLower(ch[4:12]))
	r.exec(`INSERT INTO afs.service_settings (key, value) VALUES ('work_orders.notification_channel_id', to_jsonb($1::text)) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, ch)
	if got, _ := r.store.NotificationChannel(ctx); got != "" {
		t.Errorf("a disabled channel resolved to %q", got)
	}
	r.exec(`UPDATE afs.notification_channels SET enabled = true WHERE id = $1`, ch)
	if got, _ := r.store.NotificationChannel(ctx); got != ch {
		t.Fatalf("channel = %q, want %q", got, ch)
	}
	t.Cleanup(func() {
		_, _ = r.admin.Exec(context.Background(), `UPDATE afs.service_settings SET value = 'null' WHERE key = 'work_orders.notification_channel_id'`)
	})
	for pass := 0; pass < 2; pass++ {
		if err := n.Notify(ctx, []workorder.WorkOrder{wo}); err != nil {
			t.Fatal(err)
		}
	}
	if len(mailer.sent) == 0 {
		t.Fatal("the contact was not emailed once the channel was set")
	}
	if c := r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1 AND recipient = 'sec@example.com' AND channel_id = $2`, id, ch); c != len(mailer.sent) {
		t.Errorf("contact rows = %d for %d emails", c, len(mailer.sent))
	}
	sent := r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1`, id)

	// SLA: the work order's own event carries the remediation team, once.
	wo.SLAState = workorder.SLADueSoon
	for pass := 0; pass < 2; pass++ {
		if err := n.Notify(ctx, []workorder.WorkOrder{wo}); err != nil {
			t.Fatal(err)
		}
	}
	if c := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.sla_warning' AND team_id = $2`, id, team); c != 1 {
		t.Errorf("own sla_warning events = %d", c)
	}
	if c := r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1 AND kind = 'sla_warning' AND recipient = $2`, id, workorder.SentinelRecipient); c != 1 {
		t.Errorf("sentinel rows = %d", c)
	}
	// A breached SLA writes work_order.sla_breached once, for the same team.
	wo.SLAState = workorder.SLABreached
	for pass := 0; pass < 2; pass++ {
		if err := n.Notify(ctx, []workorder.WorkOrder{wo}); err != nil {
			t.Fatal(err)
		}
	}
	if c := r.count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.sla_breached' AND team_id = $2`, id, team); c != 1 {
		t.Errorf("own sla_breached events = %d", c)
	}
	if after := r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1`, id); after <= sent {
		t.Error("the SLA warning recorded nothing")
	}
	// Sent reports what is recorded.
	sentSet, err := r.store.Sent(ctx, []string{id})
	if err != nil || len(sentSet[id]) != r.count(`SELECT count(*) FROM afs.work_order_notifications WHERE work_order_id = $1`, id) {
		t.Errorf("sent = %d, %v", len(sentSet[id]), err)
	}
}

type recordingMailer struct{ sent []string }

func (m *recordingMailer) Send(_ context.Context, _, to, _, _ string) error {
	m.sent = append(m.sent, to)
	return nil
}

type stubGitHub struct{ state workorder.PRState }

func (g *stubGitHub) PullRequest(context.Context, workorder.PRRef) (workorder.PRState, error) {
	return g.state, nil
}
