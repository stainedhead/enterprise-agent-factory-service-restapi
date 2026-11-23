package postgres_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

const (
	guardD1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	guardD2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// guardRig is a team with one catalog template, one model and the real restore service.
type guardRig struct {
	db     *pgtest.DB
	team   string
	tpl    string // template name
	tplID  string
	model  string
	svc    *restore.Service
	admin  acting.Caller
	exec   func(sql string, args ...any)
	scalar func(sql string, args ...any) string
}

func newGuardRig(t *testing.T) *guardRig {
	t.Helper()
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	r := &guardRig{db: db, team: tm.ID, tplID: pgtest.NewID("tpl"), model: seedModel(t, db, tm.ID)}
	r.tpl = "tpl-" + lower(r.tplID[4:14])
	a := db.Admin(t)
	r.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := a.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	r.scalar = func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := a.QueryRow(t.Context(), sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	r.exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, $2, 'hermes', ARRAY[$3, $4]::text[], $3)`,
		r.tplID, r.tpl, guardD1, guardD2)
	runner := postgres.NewTxRunner(appPool(t, db))
	deps := acting.Deps{Tx: runner, Audit: postgres.NewAuditWriter(runner), Outbox: postgres.NewOutboxWriter(), Idem: postgres.NewIdempotencyStore(), Clock: liveClock{}, IDs: liveIDs{}}
	r.svc = &restore.Service{Deps: deps, Repo: postgres.NewRestoreRepo(), Guards: postgres.NewAgentRepo()}
	r.admin = acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: tm.ID}}}, RequestID: "req_restore_guard"}
	return r
}

func (r *guardRig) image(d string) string { return "registry.example.com/agents/coder@" + d }

func (r *guardRig) newAgent(name, d string) string {
	id := pgtest.NewID("agt")
	r.exec(`INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, resources, storage_size_gib, iam_identity, persona)
VALUES ($1, $2, $3, $4, $5, $6, $7, '{"cpu":"500m","memory":"1Gi"}', 30, 'dedicated', 'original')`, id, r.team, name, r.tpl, r.tplID, r.image(d), r.model)
	return id
}

func (r *guardRig) lastRevision() int64 {
	n, err := strconv.ParseInt(r.scalar(`SELECT max(id) FROM afs.config_revisions WHERE team_id = $1`, r.team), 10, 64)
	if err != nil {
		panic(err)
	}
	return n
}

func (r *guardRig) audits() string {
	return r.scalar(`SELECT count(*) FROM afs.audit_log WHERE action = 'RestoreRevision' AND team_id = $1`, r.team)
}

// FR-R01 AC1: create on D1, a service admin retires D1 and the agent moves to D2, a restore to the earlier
// revision leaves the agent on D2 and reports the agent in skipped, naming the template guard.
func TestRestoreHonorsTheTemplateDigestGuard(t *testing.T) {
	r := newGuardRig(t)
	agent := r.newAgent("coder", guardD1)
	rev := r.lastRevision()
	r.exec(`UPDATE afs.agent_templates SET allowed_image_digests = ARRAY[$2]::text[], current_image_digest = $2 WHERE id = $1`, r.tplID, guardD2)
	r.exec(`UPDATE afs.agents SET image = $2 WHERE id = $1`, agent, r.image(guardD2))
	r.exec(`UPDATE afs.agents SET persona = 'drifted' WHERE id = $1`, agent)
	gen := r.scalar(`SELECT generation FROM afs.agents WHERE id = $1`, agent)

	t.Run("dry run reports the skip and writes nothing", func(t *testing.T) {
		res, err := r.svc.Run(t.Context(), r.admin, r.team, rev, true, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertAgentSkipped(t, res, agent, "template guard")
		if got := r.scalar(`SELECT image FROM afs.agents WHERE id = $1`, agent); got != r.image(guardD2) {
			t.Fatalf("image = %s", got)
		}
		if n := r.audits(); n != "0" {
			t.Fatalf("a dry run audited %s", n)
		}
	})
	t.Run("apply keeps D2, skips the agent and counts it in the audit record", func(t *testing.T) {
		res, err := r.svc.Run(t.Context(), r.admin, r.team, rev, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertAgentSkipped(t, res, agent, "template guard")
		if got := r.scalar(`SELECT image FROM afs.agents WHERE id = $1`, agent); got != r.image(guardD2) {
			t.Fatalf("the restore put a retired digest back: %s", got)
		}
		if got := r.scalar(`SELECT persona FROM afs.agents WHERE id = $1`, agent); got != "drifted" {
			t.Errorf("a skipped agent step still changed persona to %q", got)
		}
		if got := r.scalar(`SELECT generation FROM afs.agents WHERE id = $1`, agent); got != gen {
			t.Errorf("agent generation moved from %s to %s", gen, got)
		}
		if got := r.scalar(`SELECT coalesce(after ->> 'skipped', '') FROM afs.audit_log WHERE action = 'RestoreRevision' AND team_id = $1 ORDER BY id DESC LIMIT 1`, r.team); got != "1" {
			t.Fatalf("audit skipped = %q", got)
		}
	})
}

// FR-R01 AC2: a disabled template and a model no longer enabled for the team are not restored either.
func TestRestoreHonorsTemplateEnabledAndModelGuards(t *testing.T) {
	t.Run("disabled template", func(t *testing.T) {
		r := newGuardRig(t)
		agent := r.newAgent("coder", guardD1)
		rev := r.lastRevision()
		other := pgtest.NewID("tpl")
		otherName := "tpl-" + lower(other[4:14])
		r.exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`, other, otherName, guardD2)
		r.exec(`UPDATE afs.agents SET template = $2, template_id = $3, image = $4 WHERE id = $1`, agent, otherName, other, r.image(guardD2))
		r.exec(`UPDATE afs.agent_templates SET enabled = false WHERE id = $1`, r.tplID)
		res, err := r.svc.Run(t.Context(), r.admin, r.team, rev, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertAgentSkipped(t, res, agent, "template guard")
		if got := r.scalar(`SELECT template FROM afs.agents WHERE id = $1`, agent); got != otherName {
			t.Fatalf("template = %s", got)
		}
	})
	t.Run("model not enabled for the team", func(t *testing.T) {
		r := newGuardRig(t)
		agent := r.newAgent("coder", guardD1)
		rev := r.lastRevision()
		r.exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ('anthropic.claude-rf-a', 'anthropic', 'rf-a', 4096) ON CONFLICT DO NOTHING`)
		r.exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'anthropic.claude-rf-a')`, r.team)
		r.exec(`UPDATE afs.agents SET model_id = 'anthropic.claude-rf-a' WHERE id = $1`, agent)
		// Under the default service access (all) a disabled model still passes, so restrict the team (000022).
		r.exec(`DELETE FROM afs.team_models WHERE team_id = $1 AND model_id <> 'anthropic.claude-rf-a'`, r.team)
		r.exec(`UPDATE afs.teams SET model_access = 'selected' WHERE id = $1`, r.team)
		res, err := r.svc.Run(t.Context(), r.admin, r.team, rev, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertAgentSkipped(t, res, agent, "model guard")
		if got := r.scalar(`SELECT model_id FROM afs.agents WHERE id = $1`, agent); got != "anthropic.claude-rf-a" {
			t.Fatalf("model = %s", got)
		}
	})
}

// FR-R01 AC4: fields with no guard still restore, and a revision that passes the guards restores in full.
func TestRestoreStillAppliesUnguardedFields(t *testing.T) {
	r := newGuardRig(t)
	agent := r.newAgent("coder", guardD1)
	rev := r.lastRevision()
	r.exec(`UPDATE afs.agents SET persona = 'drifted', description = 'drifted', snapshot_retain_count = 3 WHERE id = $1`, agent)
	res, err := r.svc.Run(t.Context(), r.admin, r.team, rev, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("skipped %+v", res.Skipped)
	}
	if got := r.scalar(`SELECT persona || '/' || snapshot_retain_count FROM afs.agents WHERE id = $1`, agent); got != "original/7" {
		t.Fatalf("restored = %s", got)
	}
}

// countingRepo counts the catalog queries a restore makes.
type countingRepo struct {
	*postgres.AgentRepo
	templates, models atomic.Int32
}

func (c *countingRepo) Template(ctx context.Context, tx port.Tx, n string) (agents.TemplateInfo, bool, error) {
	c.templates.Add(1)
	return c.AgentRepo.Template(ctx, tx, n)
}

func (c *countingRepo) ModelEnabled(ctx context.Context, tx port.Tx, t, m string) (bool, error) {
	c.models.Add(1)
	return c.AgentRepo.ModelEnabled(ctx, tx, t, m)
}

// FR-R01 NFR: restoring 200 agents validates from one lookup per template and model, and finishes inside
// the 30 s statement timeout.
func TestRestoreOf200AgentsValidatesWithoutPerAgentQueries(t *testing.T) {
	r := newGuardRig(t)
	for i := 0; i < 200; i++ {
		r.newAgent(fmt.Sprintf("agent-%03d", i), guardD1)
	}
	rev := r.lastRevision()
	r.exec(`UPDATE afs.agents SET persona = 'drifted', image = $2 WHERE team_id = $1`, r.team, r.image(guardD2))
	c := &countingRepo{AgentRepo: postgres.NewAgentRepo()}
	r.svc.Guards = c
	start := time.Now()
	res, err := r.svc.Run(t.Context(), r.admin, r.team, rev, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("took %s", d)
	}
	if len(res.Changes) != 200 || len(res.Skipped) != 0 {
		t.Fatalf("changes %d skipped %d", len(res.Changes), len(res.Skipped))
	}
	if c.templates.Load() != 1 || c.models.Load() != 0 {
		t.Fatalf("catalog queries: templates %d models %d, want 1 and 0 (model unchanged)", c.templates.Load(), c.models.Load())
	}
}

func assertAgentSkipped(t *testing.T, res restore.Result, agent, guard string) {
	t.Helper()
	for _, s := range res.Skipped {
		if s.ResourceKind == "agents" && s.ResourceID == agent {
			if !strings.Contains(s.Reason, guard) {
				t.Fatalf("reason %q does not name the %s", s.Reason, guard)
			}
			return
		}
	}
	t.Fatalf("agent %s not in skipped: %+v (changes %+v)", agent, res.Skipped, res.Changes)
}
