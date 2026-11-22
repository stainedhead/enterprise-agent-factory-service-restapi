package postgres_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

type egressWorld struct {
	team       pgtest.Team
	agtA, agtB string
	sysGH      string
	slugGH     string
	sysSlack   string
}

// seedEgress creates a team with two agents, a service-level system and a team-declared one.
func seedEgress(t *testing.T, db *pgtest.DB) egressWorld {
	t.Helper()
	admin, ctx := db.Admin(t), t.Context()
	w := egressWorld{team: db.SeedTeam(t), agtA: pgtest.NewID("agt"), agtB: pgtest.NewID("agt"), sysGH: pgtest.NewID("ext"), sysSlack: pgtest.NewID("ext")}
	w.slugGH = "gh" + strings.ToLower(w.sysGH[len(w.sysGH)-8:])
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO afs.models (id, provider, display_name) VALUES ('claude-sonnet', 'Anthropic', 'Claude Sonnet') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'claude-sonnet')`, w.team.ID)
	for i, id := range []string{w.agtA, w.agtB} {
		exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, $3, 'omp', 'ghcr.io/x/omp@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'claude-sonnet')`,
			id, w.team.ID, []string{"alpha", "beta"}[i])
	}
	exec(`INSERT INTO afs.external_systems (id, slug, name, category, endpoints) VALUES ($1, $2, 'GH', 'dev_tools', ARRAY['api.github.com', '*.githubusercontent.com:443'])`, w.sysGH, w.slugGH)
	return w
}

// M2-H1: the source reports, per agent, which systems are on and the level that switched one off
// (service, team, agent), and only for the team's own view.
func TestEgressSource(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	src := postgres.NewEgressSource(runner)
	w := seedEgress(t, db)
	other := db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	slugGH := w.slugGH

	find := func(d egress.Desired, agent string) map[string]string {
		for _, a := range d.Agents {
			if a.ID != agent {
				continue
			}
			m := map[string]string{}
			for _, s := range a.Systems {
				m[s.Slug] = s.DisabledAt
			}
			return m
		}
		t.Fatalf("agent %s missing in %+v", agent, d)
		return nil
	}
	d, err := src.LoadEgress(ctx, w.team.ID)
	if err != nil || d.TeamID != w.team.ID || d.State != "active" || d.Namespace == "" || len(d.Agents) != 2 {
		t.Fatalf("load: %+v %v", d, err)
	}
	if got, ok := find(d, w.agtA)[slugGH]; !ok || got != "" {
		t.Errorf("system must be on for agent A: %q %v", got, ok)
	}
	var eps []string
	for _, s := range d.Agents[0].Systems {
		if s.Slug == slugGH {
			eps = s.Endpoints
		}
	}
	if len(eps) != 2 || eps[0] != "api.github.com" {
		t.Errorf("endpoints = %v", eps)
	}

	step := func(name, q string, args []any, wantA, wantB string) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d, err := src.LoadEgress(ctx, w.team.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a, b := find(d, w.agtA)[slugGH], find(d, w.agtB)[slugGH]; a != wantA || b != wantB {
			t.Errorf("%s: disabled_at = %q/%q, want %q/%q", name, a, b, wantA, wantB)
		}
	}
	step("agent opt-out", `INSERT INTO afs.external_system_optouts (id, team_id, agent_id, system_id) VALUES ($1, $2, $3, $4)`,
		[]any{pgtest.NewID("xso"), w.team.ID, w.agtB, w.sysGH}, "", "agent")
	step("team opt-out", `INSERT INTO afs.external_system_optouts (id, team_id, system_id) VALUES ($1, $2, $3)`,
		[]any{pgtest.NewID("xso"), w.team.ID, w.sysGH}, "team", "team")
	step("service off", `UPDATE afs.external_systems SET enabled = false WHERE id = $1`, []any{w.sysGH}, "service", "service")

	// Another team sees the service-level system too, but none of this team's agents.
	od, err := src.LoadEgress(ctx, other.ID)
	if err != nil || len(od.Agents) != 0 {
		t.Fatalf("other team: %+v %v", od, err)
	}
	if _, err := src.LoadEgress(ctx, pgtest.NewID("team")); !isNotFound(err) {
		t.Errorf("unknown team: %v", err)
	}
}

// M2-H1: EgressPolicyReady is merged onto the agent's status without changing its phase, keeps the
// transition time while the status stays, and does nothing for an agent that has no status yet.
func TestAgentConditions(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	w := seedEgress(t, db)
	admin := db.Admin(t)
	clock := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	ac := postgres.NewAgentConditions(runner, func() time.Time { return clock })

	if err := ac.SetCondition(t.Context(), w.agtA, domain.Condition{Type: domain.CondEgressPolicyReady, Status: domain.ConditionTrue, Reason: "PolicyApplied"}); err != nil {
		t.Fatalf("no row must be skipped: %v", err)
	}
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase, conditions, observed_generation)
VALUES ('agent', $1, $2, 'Degraded', '[{"type":"VolumeReady","status":"True","reason":"Bound","lastTransitionTime":"2026-11-22T09:00:00Z"}]', 3)`, w.agtA, w.team.ID); err != nil {
		t.Fatal(err)
	}
	read := func() (string, int64, []domain.Condition) {
		var (
			phase string
			gen   int64
			raw   []byte
		)
		if err := admin.QueryRow(t.Context(), `SELECT phase, observed_generation, conditions FROM afs.resource_status WHERE resource_kind='agent' AND resource_id=$1`, w.agtA).Scan(&phase, &gen, &raw); err != nil {
			t.Fatal(err)
		}
		return phase, gen, decodeConditions(t, raw)
	}
	set := func(status domain.ConditionStatus, reason string) {
		t.Helper()
		if err := ac.SetCondition(t.Context(), w.agtA, domain.Condition{Type: domain.CondEgressPolicyReady, Status: status, Reason: reason}); err != nil {
			t.Fatal(err)
		}
	}
	set(domain.ConditionTrue, "PolicyApplied")
	phase, gen, conds := read()
	if phase != "Degraded" || gen != 3 || len(conds) != 2 {
		t.Fatalf("after first set: %s %d %+v", phase, gen, conds)
	}
	first := condOf(conds, domain.CondEgressPolicyReady)
	if first.Status != domain.ConditionTrue || !first.LastTransitionTime.Equal(clock) {
		t.Errorf("condition = %+v", first)
	}
	clock = clock.Add(time.Minute)
	set(domain.ConditionTrue, "PolicyApplied2")
	if c := condOf(second(read()), domain.CondEgressPolicyReady); !c.LastTransitionTime.Equal(first.LastTransitionTime) || c.Reason != "PolicyApplied2" {
		t.Errorf("same status must keep the transition time: %+v", c)
	}
	clock = clock.Add(time.Minute)
	set(domain.ConditionFalse, "ApplyFailed")
	if c := condOf(second(read()), domain.CondEgressPolicyReady); c.Status != domain.ConditionFalse || !c.LastTransitionTime.Equal(clock) {
		t.Errorf("a changed status must move the transition time: %+v", c)
	}
	if vr := condOf(second(read()), "VolumeReady"); vr.Status != domain.ConditionTrue {
		t.Errorf("other conditions must survive: %+v", vr)
	}
}

func second(_ string, _ int64, c []domain.Condition) []domain.Condition { return c }

func condOf(cs []domain.Condition, typ string) domain.Condition {
	for _, c := range cs {
		if c.Type == typ {
			return c
		}
	}
	return domain.Condition{}
}

// M2-H1: the lister adds one egress resource per live team and leaves the base kinds to the base.
func TestTeamLister(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	base := logListerBase{}
	l := postgres.NewTeamLister(&base, db.Admin(t), egress.Kind)
	refs, err := l.List(t.Context(), []engine.Kind{engine.KindTeam, egress.Kind})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range refs {
		found = found || (r == engine.NewRef(egress.Kind, tm.ID, tm.ID))
	}
	if !found || len(base.listed) != 1 || base.listed[0] != engine.KindTeam {
		t.Fatalf("refs=%v listed=%v", refs, base.listed)
	}
	if stale, err := l.Stale(t.Context(), []engine.Kind{egress.Kind}); err != nil || len(stale) != 0 || base.stale != 0 {
		t.Fatalf("stale: %v %v (%d base calls)", stale, err, base.stale)
	}
	if _, err := l.Stale(t.Context(), []engine.Kind{engine.KindTeam, egress.Kind}); err != nil || base.stale != 1 {
		t.Fatalf("stale passes the base kinds: %v %d", err, base.stale)
	}
}

func decodeConditions(t *testing.T, raw []byte) []domain.Condition {
	t.Helper()
	var cs []domain.Condition
	if err := json.Unmarshal(raw, &cs); err != nil {
		t.Fatal(err)
	}
	return cs
}
