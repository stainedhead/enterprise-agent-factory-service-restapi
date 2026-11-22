package postgres_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

// TestRestoreRevision covers M4-G: dry run, apply, audit, generation, If-Match, and the role check.
func TestRestoreRevision(t *testing.T) {
	r := newExportRig(t)
	svc := &restore.Service{Deps: r.deps, Repo: postgres.NewRestoreRepo(), Guards: postgres.NewAgentRepo()}
	tm := r.team.team.ID
	admin := acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: tm}}}, RequestID: "req_restore"}
	viewer := acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: tm}}}, RequestID: "req_restore"}

	rev := int64(r.count(t, `SELECT max(id) FROM afs.config_revisions WHERE team_id = $1`, tm))
	// Drift after the revision: a changed description, a removed tag and an added tag.
	r.exec(t, `UPDATE afs.teams SET description = 'drifted' WHERE id = $1`, tm)
	r.exec(t, `DELETE FROM afs.team_tags WHERE team_id = $1 AND key = 'env'`, tm)
	r.exec(t, `INSERT INTO afs.team_tags (team_id, key, value) VALUES ($1, 'extra', 'x')`, tm)
	desc := func() string {
		var d string
		if err := r.db.Admin(t).QueryRow(t.Context(), `SELECT description FROM afs.teams WHERE id = $1`, tm).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	gen := func() int { return r.count(t, `SELECT generation FROM afs.teams WHERE id = $1`, tm) }
	g0 := gen()
	audits := func() int {
		return r.count(t, `SELECT count(*) FROM afs.audit_log WHERE action = 'RestoreRevision' AND team_id = $1 AND (after ->> 'revision') IS NOT NULL`, tm)
	}

	t.Run("a viewer is refused", func(t *testing.T) {
		_, err := svc.Run(t.Context(), viewer, tm, rev, false, nil, nil)
		var de *domain.Error
		if !errors.As(err, &de) || de.Kind != domain.KindAccessDenied {
			t.Fatalf("err = %v", err)
		}
		if desc() != "drifted" {
			t.Fatal("a refused restore changed the team")
		}
	})
	t.Run("an unknown revision is not found", func(t *testing.T) {
		if _, err := svc.Run(t.Context(), admin, tm, rev+1_000_000, false, nil, nil); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("a stale If-Match is refused", func(t *testing.T) {
		stale := domain.Version(1)
		if _, err := svc.Run(t.Context(), admin, tm, rev, false, &stale, nil); err == nil {
			t.Fatal("want a precondition failure")
		}
		if desc() != "drifted" {
			t.Fatal("a refused restore changed the team")
		}
	})
	t.Run("dry run reports and writes nothing", func(t *testing.T) {
		res, err := svc.Run(t.Context(), admin, tm, rev, true, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Changes) == 0 {
			t.Fatalf("dry run reported no changes: %+v", res)
		}
		if desc() != "drifted" || gen() != g0 {
			t.Fatal("a dry run wrote")
		}
		if n := audits(); n != 0 {
			t.Fatalf("dry run audited %d", n)
		}
	})
	t.Run("apply restores, bumps generation once and audits", func(t *testing.T) {
		res, err := svc.Run(t.Context(), admin, tm, rev, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Changes) == 0 {
			t.Fatalf("no changes: %+v", res)
		}
		if desc() != "payments team" {
			t.Fatalf("description = %q", desc())
		}
		if n := r.count(t, `SELECT count(*) FROM afs.team_tags WHERE team_id = $1 AND key = 'env' AND value = 'prod'`, tm); n != 1 {
			t.Fatal("the removed tag was not restored")
		}
		if n := r.count(t, `SELECT count(*) FROM afs.team_tags WHERE team_id = $1 AND key = 'extra'`, tm); n != 0 {
			t.Fatal("the added tag was not removed")
		}
		if gen() != g0+1 {
			t.Fatalf("generation = %d, want %d", gen(), g0+1)
		}
		if n := audits(); n != 1 {
			t.Fatalf("audit records = %d", n)
		}
	})
	t.Run("restoring again changes nothing", func(t *testing.T) {
		res, err := svc.Run(t.Context(), admin, tm, rev, true, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Changes) != 0 {
			t.Fatalf("second plan = %+v", res.Changes)
		}
	})
}

// M4-G gap closed by -db 000018: agent secret bindings have a revision trigger, so a restore rebuilds
// them: a deleted binding comes back, a changed one is put back, one added since goes, and the agent's
// generation goes up so the reconciler renders its pod again.
func TestRestoreRebuildsSecretBindings(t *testing.T) {
	r := newExportRig(t)
	svc := &restore.Service{Deps: r.deps, Repo: postgres.NewRestoreRepo(), Guards: postgres.NewAgentRepo()}
	tm, agent := r.team.team.ID, r.team.agentID
	admin := acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: tm}}}, RequestID: "req_restore_bindings"}
	rev := int64(r.count(t, `SELECT max(id) FROM afs.config_revisions WHERE team_id = $1`, tm))
	bindings := func() string {
		var s string
		if err := r.db.Admin(t).QueryRow(t.Context(), `SELECT coalesce(string_agg(target || '=' || secret_id || ':' || delivery, ',' ORDER BY target), '') FROM afs.agent_secret_bindings WHERE team_id = $1`, tm).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	want := bindings()
	if !strings.Contains(want, "GH_TOKEN=") || !strings.Contains(want, "/etc/okta/token=") {
		t.Fatalf("fixture bindings = %q", want)
	}
	agentGen := func() int { return r.count(t, `SELECT generation FROM afs.agents WHERE id = $1`, agent) }
	g0 := agentGen()

	r.exec(t, `DELETE FROM afs.agent_secret_bindings WHERE team_id = $1 AND target = 'GH_TOKEN'`, tm)
	r.exec(t, `UPDATE afs.agent_secret_bindings SET secret_id = (SELECT id FROM afs.secrets WHERE team_id = $1 AND name = 'gh') WHERE team_id = $1 AND target = '/etc/okta/token'`, tm)
	r.exec(t, `INSERT INTO afs.agent_secret_bindings (team_id, agent_id, secret_id, delivery, target) VALUES ($1, $2, (SELECT id FROM afs.secrets WHERE team_id = $1 AND name = 'gh'), 'env', 'EXTRA')`, tm, agent)
	if bindings() == want {
		t.Fatal("the drift did not change the bindings")
	}

	dry, err := svc.Run(t.Context(), admin, tm, rev, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]domain.RestoreAction{}
	for _, c := range dry.Changes {
		if c.ResourceKind == "agent_secret_bindings" {
			actions[c.ResourceID] = c.Action
		}
	}
	if len(actions) != 3 || bindings() == want {
		t.Fatalf("dry run: actions %v, bindings written: %v", actions, bindings() == want)
	}
	applied, err := svc.Run(t.Context(), admin, tm, rev, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := bindings(); got != want {
		t.Fatalf("bindings after restore = %q, want %q (changes %+v skipped %+v)", got, want, applied.Changes, applied.Skipped)
	}
	if agentGen() <= g0 {
		t.Fatalf("agent generation = %d, want above %d", agentGen(), g0)
	}
	if res, err := svc.Run(t.Context(), admin, tm, rev, true, nil, nil); err != nil || len(res.Changes) != 0 {
		t.Fatalf("second plan = %+v, %v", res.Changes, err)
	}

	// A binding whose secret is gone is skipped with a reason, and the rest still restores.
	r.exec(t, `DELETE FROM afs.agent_secret_bindings WHERE team_id = $1 AND target = 'GH_TOKEN'`, tm)
	r.exec(t, `DELETE FROM afs.agent_secret_bindings WHERE team_id = $1`, tm)
	r.exec(t, `DELETE FROM afs.secrets WHERE team_id = $1 AND name = 'gh'`, tm)
	res, err := svc.Run(t.Context(), admin, tm, rev, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, c := range res.Skipped {
		if c.ResourceKind == "agent_secret_bindings" && c.Reason != "" {
			skipped++
		}
	}
	if skipped == 0 {
		t.Fatalf("no skipped binding for the missing secret: %+v skipped %+v", res.Changes, res.Skipped)
	}
}
