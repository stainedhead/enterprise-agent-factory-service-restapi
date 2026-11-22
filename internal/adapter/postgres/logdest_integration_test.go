package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
)

func teamSession(teamID string) port.Session {
	return port.Session{Actor: user, ActorType: port.ActorUser, TeamIDs: []string{teamID}, RequestID: "req_test"}
}

func teamSecret(t *testing.T, db *pgtest.DB, teamID, name string) string {
	t.Helper()
	admin := db.Admin(t)
	id := pgtest.NewID("sec")
	var path string
	if err := admin.QueryRow(t.Context(), `SELECT afs.secret_path($1, NULL, $2)`, teamID, name).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin) VALUES ($1, $2, $3, $4, 'managed')`,
		id, teamID, name, "arn:aws:secretsmanager:us-east-1:111122223333:secret:"+path+"-AbCdEf"); err != nil {
		t.Fatal(err)
	}
	return id
}

// M2-I1: log destination rows round trip, optimistic concurrency, unique names, team isolation, the
// team-secret rule, and the reconciler's view of them (M2-I3).
func TestLogDestRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewLogDestRepo()
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	secA := teamSecret(t, db, a.ID, "hec")
	secB := teamSecret(t, db, b.ID, "hec")
	now := time.Now()
	hec := domain.NewLogDestinationParams{Name: "hec", Kind: domain.LogSplunkHEC, Enabled: true, SecretID: secA,
		Config: map[string]string{"endpoint": "https://splunk.example.com:8088"}}
	cw := domain.NewLogDestinationParams{Name: "cw", Kind: domain.LogCloudWatch, Enabled: true, Config: map[string]string{"logGroup": "/afs/x", "region": "us-east-1"}}
	hecD, _ := hec.New(pgtest.NewID("log"), a.ID, now)
	cwD, _ := cw.New(pgtest.NewID("log"), a.ID, now)
	foreign, _ := cw.New(pgtest.NewID("log"), b.ID, now)

	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		for _, d := range []domain.LogDestination{hecD, cwD, foreign} {
			if err := repo.Insert(ctx, tx, d); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A duplicate name in the team is a Conflict; the same name in another team is fine.
	dup, _ := cw.New(pgtest.NewID("log"), a.ID, now)
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Insert(ctx, tx, dup), domain.KindConflict)
		return context.Canceled
	})

	// A secret of another team cannot be referenced (composite foreign key).
	cross, _ := hec.New(pgtest.NewID("log"), a.ID, now)
	cross.Name, cross.SecretID = "cross", secB
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		if err := repo.Insert(ctx, tx, cross); err == nil {
			t.Error("another team's secret accepted")
		}
		return context.Canceled
	})

	// Reads: the status is Pending until the reconciler writes one, generation follows the version, and
	// a caller of another team sees nothing.
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		rows, err := repo.List(ctx, tx, a.ID, "", 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("list: %d %v", len(rows), err)
		}
		got, err := repo.Get(ctx, tx, a.ID, hecD.ID, false)
		if err != nil || got.SecretID != secA || got.Status.Phase != domain.PhasePending || got.Generation != 1 || got.Config["endpoint"] == "" {
			t.Fatalf("get: %+v %v", got, err)
		}
		ok, err := repo.SecretExists(ctx, tx, a.ID, secA)
		if err != nil || !ok {
			t.Fatalf("own secret: %v %v", ok, err)
		}
		if ok, _ := repo.SecretExists(ctx, tx, a.ID, secB); ok {
			t.Fatal("another team's secret is visible")
		}
		_, err = repo.Get(ctx, tx, a.ID, foreign.ID, false)
		wantKind(t, err, domain.KindNotFound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := inTx(t, db, teamSession(b.ID), func(ctx context.Context, tx port.Tx) error {
		rows, _ := repo.List(ctx, tx, a.ID, "", 10)
		if len(rows) != 0 {
			t.Fatalf("team B sees %d destinations of team A", len(rows))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Update with the right and a stale version; the trigger bumps the version.
	off := false
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		next, _ := cwD.Apply(domain.LogDestinationPatch{Enabled: &off}, now)
		_, err := repo.Update(ctx, tx, next, 9)
		wantKind(t, err, domain.KindPreconditionFailed)
		up, err := repo.Update(ctx, tx, next, 1)
		if err != nil || up.Version != 2 || up.Enabled || up.Generation != 2 {
			t.Fatalf("update: %+v %v", up, err)
		}
		missing := next
		missing.ID = pgtest.NewID("log")
		_, err = repo.Update(ctx, tx, missing, 1)
		wantKind(t, err, domain.KindNotFound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The reconciler reads the team, the destinations and the secret's ARN (not a value).
	rr := reconcilerPool(t, db)
	d, err := postgres.NewLogSource(rr).LoadLogs(t.Context(), a.ID)
	if err != nil || d.Namespace == "" || len(d.Destinations) != 2 {
		t.Fatalf("source: %+v %v", d, err)
	}
	byName := map[string]logs.Destination{}
	for _, x := range d.Destinations {
		byName[x.Name] = x
	}
	if byName["hec"].SecretARN == "" || byName["hec"].Kind != domain.LogSplunkHEC || byName["cw"].Enabled || byName["cw"].Version != 2 {
		t.Fatalf("destinations: %+v", byName)
	}
	if _, err := postgres.NewLogSource(rr).LoadLogs(t.Context(), pgtest.NewID("team")); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown team: %v", err)
	}

	// Delete: stale refused, then removed; the reconciler sees the change feed's input gone.
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Delete(ctx, tx, a.ID, hecD.ID, 7), domain.KindPreconditionFailed)
		if err := repo.Delete(ctx, tx, a.ID, hecD.ID, 1); err != nil {
			t.Fatal(err)
		}
		wantKind(t, repo.Delete(ctx, tx, a.ID, hecD.ID, 1), domain.KindNotFound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// M2-I3: the lister adds one log delivery resource per live team to the base lister's answer.
func TestLogLister(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	rr := reconcilerPool(t, db)
	base := logListerBase{}
	l := postgres.NewLogLister(&base, db.Admin(t))
	refs, err := l.List(t.Context(), []engine.Kind{engine.KindTeam, logs.Kind})
	if err != nil {
		t.Fatal(err)
	}
	var found, baseOnly bool
	for _, r := range refs {
		if r.Kind == logs.Kind && r.ID == tm.ID && r.TeamID == tm.ID {
			found = true
		}
		if r.Kind == engine.KindTeam && r.ID == "base" {
			baseOnly = true
		}
	}
	if !found || !baseOnly || len(base.listed) != 1 || base.listed[0] != engine.KindTeam {
		t.Fatalf("refs=%v listed=%v", refs, base.listed)
	}
	stale, err := l.Stale(t.Context(), []engine.Kind{logs.Kind})
	if err != nil || len(stale) != 0 || base.stale != 0 {
		t.Fatalf("stale: %v %v (%d base calls)", stale, err, base.stale)
	}
	_ = rr
}

type logListerBase struct {
	listed []engine.Kind
	stale  int
}

func (f *logListerBase) List(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	f.listed = append(f.listed, kinds...)
	return []engine.Ref{engine.NewRef(engine.KindTeam, "base", "base")}, nil
}

func (f *logListerBase) Stale(context.Context, []engine.Kind) ([]engine.Ref, error) {
	f.stale++
	return nil, nil
}
