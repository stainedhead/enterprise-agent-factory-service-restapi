package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// M2-B: agent rows, bindings, catalog checks, snapshots, and the template guard (RA-US-4.5).
func TestAgentRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewAgentRepo()
	tm, other := db.SeedTeam(t), db.SeedTeam(t)
	model := seedModel(t, db, tm.ID)
	tplName := "tpl-" + lower(pgtest.NewID("x")[2:8])
	tplID := seedTemplateForAgents(t, db, tplName)
	now := time.Now().UTC()
	img := domain.ImageRef("registry.example.com/agents/coder@" + testDigest)

	admin := db.Admin(t)

	a, err := domain.NewAgent(pgtest.NewID("agt"), tm.ID, domain.NewAgentParams{Name: "coder", Template: tplName, Image: string(img), ModelID: model,
		Resources: domain.AgentResources{CPU: "500m", Memory: "1Gi"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		if ok, err := repo.ModelEnabled(ctx, tx, tm.ID, model); err != nil || !ok {
			t.Errorf("ModelEnabled: %v %v", ok, err)
		}
		if ok, _ := repo.ModelEnabled(ctx, tx, tm.ID, "nope"); ok {
			t.Error("unknown model enabled")
		}
		if ok, err := repo.StorageClassEnabled(ctx, tx, "gp3"); err != nil || !ok {
			t.Errorf("StorageClassEnabled: %v %v", ok, err)
		}
		if ok, _ := repo.StorageClassEnabled(ctx, tx, "nope"); ok {
			t.Error("unknown class enabled")
		}
		tpl, ok, err := repo.Template(ctx, tx, tplName)
		if err != nil || !ok || tpl.ID != tplID || !tpl.Enabled || len(tpl.AllowedDigests) != 1 {
			t.Errorf("Template: %+v %v %v", tpl, ok, err)
		}
		if _, ok, _ := repo.Template(ctx, tx, "missing"); ok {
			t.Error("missing template found")
		}
		return repo.Insert(ctx, tx, a, tplID)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		got, err := repo.Get(ctx, tx, tm.ID, a.ID, true)
		if err != nil {
			return err
		}
		if got.Name != "coder" || got.State != domain.AgentEnabled || got.Generation != 1 || got.Version != 1 || got.Volume.SizeGiB != 20 ||
			got.Volume.StorageClass != "gp3" || got.Resources.CPU != "500m" || got.Status.Phase != domain.PhasePending || got.Bindings == nil {
			t.Errorf("agent: %+v", got)
		}
		rows, err := repo.List(ctx, tx, tm.ID, "", "", 10)
		if err != nil || len(rows) != 1 || rows[0].ID != a.ID {
			t.Errorf("list: %v %v", rows, err)
		}
		// Update that does not touch the template, after the template is disabled (RA-US-4.5).
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bindings are stored, replaced and read back; secret owners are reported.
	secrets := postgres.NewSecretRepo()
	var sec domain.Secret
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		path, err := secrets.Path(ctx, tx, tm.ID, "", "", "api-key")
		if err != nil {
			return err
		}
		sec = domain.Secret{ID: pgtest.NewID("sec"), TeamID: tm.ID, Name: "api-key", ARN: arnFor(path), VersionID: "v1"}
		if err := secrets.Insert(ctx, tx, sec); err != nil {
			return err
		}
		owners, err := repo.Secrets(ctx, tx, tm.ID, []string{sec.ID, "sec_missing"})
		if err != nil || len(owners) != 1 || owners[sec.ID] != (agents.SecretInfo{ManagedBy: "user"}) {
			t.Errorf("owners: %+v %v", owners, err)
		}
		cur, err := repo.Get(ctx, tx, tm.ID, a.ID, true)
		if err != nil {
			return err
		}
		bs := []domain.SecretBinding{{SecretID: sec.ID, Delivery: "env", Target: "API_KEY"}, {SecretID: sec.ID, Delivery: "file", Target: "/etc/key"}}
		next, err := cur.Apply(domain.AgentPatch{Bindings: &bs}, now)
		if err != nil {
			return err
		}
		if err := repo.Update(ctx, tx, next, "", false, cur.Version); err != nil {
			return err
		}
		got, err := repo.Get(ctx, tx, tm.ID, a.ID, false)
		if err != nil || len(got.Bindings) != 2 {
			t.Errorf("bindings: %+v %v", got.Bindings, err)
		}
		none := []domain.SecretBinding{}
		n2, _ := got.Apply(domain.AgentPatch{Bindings: &none}, now)
		if err := repo.Update(ctx, tx, n2, "", false, got.Version); err != nil {
			return err
		}
		got, _ = repo.Get(ctx, tx, tm.ID, a.ID, false)
		if len(got.Bindings) != 0 {
			t.Errorf("cleared: %+v", got.Bindings)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agent_templates SET enabled=false WHERE id=$1`, tplID); err != nil {
		t.Fatal(err)
	}
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		cur, err := repo.Get(ctx, tx, tm.ID, a.ID, true)
		if err != nil {
			return err
		}
		d := "changed"
		next, err := cur.Apply(domain.AgentPatch{Description: &d, Volume: &domain.VolumeSpec{SizeGiB: 40}}, now)
		if err != nil {
			return err
		}
		if err := repo.Update(ctx, tx, next, "", false, cur.Version); err != nil {
			t.Errorf("update with disabled template: %v", err)
			return err
		}
		got, err := repo.Get(ctx, tx, tm.ID, a.ID, false)
		if err != nil || got.Description != "changed" || got.Volume.SizeGiB != 40 || got.Generation != cur.Generation+1 || got.Version != cur.Version+1 {
			t.Errorf("after update: %+v %v", got, err)
		}
		// A stale version is a precondition failure.
		wantKind(t, repo.Update(ctx, tx, next, "", false, cur.Version), domain.KindPreconditionFailed)
		// Naming a template (disabled now) with an image change is refused by the database guard.
		next.Image = domain.ImageRef("registry.example.com/agents/coder@sha256:" + strings.Repeat("a", 64))
		wantKind(t, repo.Update(ctx, tx, next, tplID, true, got.Version), domain.KindValidation)
		return context.Canceled
	})
	if err == nil {
		t.Fatal("expected rollback")
	}

	// Snapshots, state change and deleted agents hide.
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		for i := 0; i < 3; i++ {
			if err := repo.InsertSnapshot(ctx, tx, tm.ID, domain.Snapshot{ID: pgtest.NewID("vsnap"), AgentID: a.ID, Reason: "manual"}); err != nil {
				return err
			}
		}
		all, err := repo.Snapshots(ctx, tx, tm.ID, a.ID, "", 10)
		if err != nil || len(all) != 3 || all[0].State != "requested" || all[0].Reason != "manual" {
			t.Errorf("snapshots: %+v %v", all, err)
		}
		page, _ := repo.Snapshots(ctx, tx, tm.ID, a.ID, all[0].ID, 10)
		if len(page) != 2 {
			t.Errorf("after: %d", len(page))
		}
		cur, _ := repo.Get(ctx, tx, tm.ID, a.ID, true)
		del, _, _ := cur.MarkDeleting(now)
		if err := repo.Update(ctx, tx, del, "", false, cur.Version); err != nil {
			return err
		}
		got, err := repo.Get(ctx, tx, tm.ID, a.ID, false)
		if err != nil || got.State != domain.AgentDeleting {
			t.Errorf("deleting: %+v %v", got, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another team sees nothing.
	err = inTx(t, db, teamSession(other.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := repo.Get(ctx, tx, tm.ID, a.ID, false)
		wantKind(t, err, domain.KindNotFound)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agents SET state='deleted', deleted_at=now() WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	_ = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := repo.Get(ctx, tx, tm.ID, a.ID, false)
		wantKind(t, err, domain.KindNotFound)
		return nil
	})
}
