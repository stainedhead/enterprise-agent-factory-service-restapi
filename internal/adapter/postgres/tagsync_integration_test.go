package postgres_test

import (
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// M6-X2: the tag sync reads the team's secrets with their owners' names, its live pools and its tags.
func TestTagSyncSource(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	src := postgres.NewTagSyncSource(runner)
	admin := db.Admin(t)
	sp := seedPool(t, db) // an ephemeral team with pools triage and fixers
	var ns, slug string
	if err := admin.QueryRow(t.Context(), `SELECT namespace FROM afs.teams WHERE id = $1`, sp.teamID).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT slug FROM afs.teams WHERE id = $1`, sp.teamID).Scan(&slug); err != nil {
		t.Fatal(err)
	}
	tm := pgtest.Team{ID: sp.teamID, Slug: slug}
	live := sp.poolID
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	exec(`UPDATE afs.agent_pools SET state = 'deleting' WHERE id = $1`, sp.otherID)
	arnShared, arnPool := arnFor("afs/"+ns+"/_shared/db"), arnFor("afs/"+ns+"/triage/key")
	exec(`INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin, path) VALUES ($1, $2, 'db', $3, 'managed', $4)`, pgtest.NewID("sec"), tm.ID, arnShared, "afs/"+ns+"/_shared/db")
	exec(`INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin, pool_id, path) VALUES ($1, $2, 'key', $3, 'managed', $4, $5)`, pgtest.NewID("sec"), tm.ID, arnPool, live, "afs/"+ns+"/triage/key")

	d, err := src.LoadTagSync(t.Context(), tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.TeamID != tm.ID || d.Slug != tm.Slug || d.State != "active" {
		t.Errorf("team %+v", d)
	}
	if len(d.Secrets) != 2 {
		t.Fatalf("secrets %+v", d.Secrets)
	}
	for _, s := range d.Secrets {
		switch s.ARN {
		case arnShared:
			if s.AgentID != "" || s.PoolID != "" {
				t.Errorf("shared secret %+v", s)
			}
		case arnPool:
			if s.PoolID != live || s.PoolName != "triage" || s.AgentID != "" {
				t.Errorf("pool secret %+v", s)
			}
		default:
			t.Errorf("unexpected %+v", s)
		}
	}
	if len(d.Pools) != 1 || d.Pools[0].ID != live || d.Pools[0].Name != "triage" {
		t.Errorf("pools %+v", d.Pools)
	}
	if _, err := src.LoadTagSync(t.Context(), pgtest.NewID("team")); !domainNotFound(err) {
		t.Errorf("missing team: %v", err)
	}
}

func domainNotFound(err error) bool {
	var de *domain.Error
	return err != nil && errors.As(err, &de) && de.Kind == domain.KindNotFound
}
