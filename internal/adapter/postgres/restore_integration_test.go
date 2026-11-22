package postgres_test

import (
	"errors"
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
	svc := &restore.Service{Deps: r.deps, Repo: postgres.NewRestoreRepo()}
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
