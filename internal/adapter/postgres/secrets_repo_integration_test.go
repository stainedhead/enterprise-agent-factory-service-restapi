package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// M2-C: secret rows hold ARNs and metadata; the database derives the path.
func TestSecretRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewSecretRepo()
	tm, other := db.SeedTeam(t), db.SeedTeam(t)
	agent := seedAgent(t, db, tm.ID, "coder")
	now := time.Now().UTC()
	var teamSec, agentSec domain.Secret

	err := inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		path, err := repo.Path(ctx, tx, tm.ID, "", "", "github-pat")
		if err != nil {
			return err
		}
		if !strings.HasSuffix(path, "/_shared/github-pat") {
			t.Errorf("path %q", path)
		}
		teamSec = domain.Secret{ID: pgtest.NewID("sec"), TeamID: tm.ID, Name: "github-pat", Description: "d", ARN: arnFor(path),
			VersionID: "v1", LastChangedAt: &now, Rotation: domain.Rotation{Enabled: true, Days: 90}}
		if err := repo.Insert(ctx, tx, teamSec); err != nil {
			return err
		}
		apath, err := repo.Path(ctx, tx, tm.ID, agent, "", "agent-key")
		if err != nil {
			return err
		}
		if !strings.Contains(apath, "/coder/agent-key") {
			t.Errorf("agent path %q", apath)
		}
		agentSec = domain.Secret{ID: pgtest.NewID("sec"), TeamID: tm.ID, AgentID: agent, Name: "agent-key", ARN: arnFor(apath), VersionID: "v1"}
		return repo.Insert(ctx, tx, agentSec)
	})
	if err != nil {
		t.Fatal(err)
	}

	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		got, err := repo.Get(ctx, tx, tm.ID, teamSec.ID, true)
		if err != nil {
			return err
		}
		if got.Scope != domain.SecretScopeTeam || got.Rotation.Days != 90 || got.Version != 1 || got.VersionID != "v1" || got.BoundTo == nil || got.Path == "" {
			t.Errorf("team secret: %+v", got)
		}
		g2, err := repo.Get(ctx, tx, tm.ID, agentSec.ID, false)
		if err != nil || g2.Scope != domain.SecretScopeAgent || g2.AgentID != agent || g2.Rotation.Enabled {
			t.Errorf("agent secret: %+v %v", g2, err)
		}
		rows, err := repo.List(ctx, tx, tm.ID, "", 10)
		if err != nil || len(rows) != 2 {
			t.Errorf("list: %v %v", rows, err)
		}
		if taken, _ := repo.NameTaken(ctx, tx, tm.ID, "github-pat"); !taken {
			t.Error("NameTaken")
		}
		wantKind(t, repo.Insert(ctx, tx, domain.Secret{ID: pgtest.NewID("sec"), TeamID: tm.ID, Name: "github-pat", ARN: strings.Replace(teamSec.ARN, "AbCdEf", "ZzZzZz", 1)}), domain.KindConflict)
		return context.Canceled // roll back the failed insert's transaction
	})
	if err == nil {
		t.Fatal("expected rollback")
	}

	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		upd := teamSec
		upd.Description, upd.Rotation = "changed", domain.Rotation{}
		if err := repo.Update(ctx, tx, upd, 1); err != nil {
			return err
		}
		wantKind(t, repo.Update(ctx, tx, upd, 1), domain.KindPreconditionFailed)
		if err := repo.SetCurrentVersion(ctx, tx, tm.ID, teamSec.ID, "v2", now); err != nil {
			return err
		}
		got, _ := repo.Get(ctx, tx, tm.ID, teamSec.ID, false)
		if got.Description != "changed" || got.Rotation.Enabled || got.VersionID != "v2" || got.Version != 3 {
			t.Errorf("after update: %+v", got)
		}
		wantKind(t, repo.SetCurrentVersion(ctx, tx, tm.ID, "sec_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", "v", now), domain.KindNotFound)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another team neither sees nor changes the rows (RLS).
	_ = inTx(t, db, teamSession(other.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := repo.Get(ctx, tx, tm.ID, teamSec.ID, false)
		wantKind(t, err, domain.KindNotFound)
		rows, _ := repo.List(ctx, tx, tm.ID, "", 10)
		if len(rows) != 0 {
			t.Errorf("cross-team list: %v", rows)
		}
		wantKind(t, repo.Delete(ctx, tx, tm.ID, teamSec.ID), domain.KindNotFound)
		return nil
	})

	// A bound secret lists its agents and cannot be deleted from the database.
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.agent_secret_bindings (team_id, agent_id, secret_id, delivery, target) VALUES ($1, $2, $3, 'env', 'PAT')`,
		tm.ID, agent, teamSec.ID); err != nil {
		t.Fatal(err)
	}
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		got, _ := repo.Get(ctx, tx, tm.ID, teamSec.ID, false)
		if len(got.BoundTo) != 1 || got.BoundTo[0] != agent {
			t.Errorf("BoundTo = %v", got.BoundTo)
		}
		wantKind(t, repo.Delete(ctx, tx, tm.ID, teamSec.ID), domain.KindConflict)
		return context.Canceled
	})
	_ = err
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		return repo.Delete(ctx, tx, tm.ID, agentSec.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Unknown agent: the path function fails, as a validation error.
	_ = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := repo.Path(ctx, tx, tm.ID, pgtest.NewID("agt"), "", "x")
		wantKind(t, err, domain.KindValidation)
		return context.Canceled
	})
}
