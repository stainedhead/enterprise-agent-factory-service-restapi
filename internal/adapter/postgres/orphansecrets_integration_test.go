package postgres_test

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// M2-C5: the sweep's database side, as the afs_reconciler login.
func TestOrphanSecretRows(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	rows := postgres.NewOrphanSecretRows(runner, postgres.NewAuditWriter(runner))
	admin := db.Admin(t)
	tm := db.SeedTeam(t)
	var ns string
	if err := admin.QueryRow(t.Context(), `SELECT namespace FROM afs.teams WHERE id = $1`, tm.ID).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	arn := arnFor("afs/" + ns + "/_shared/known")
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin) VALUES ($1, $2, 'known', $3, 'managed')`,
		pgtest.NewID("sec"), tm.ID, arn); err != nil {
		t.Fatal(err)
	}
	if p, err := rows.PathPrefix(t.Context()); err != nil || p != "afs" {
		t.Fatalf("prefix %q %v", p, err)
	}
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.service_settings (key, value) VALUES ('secrets.path_prefix', '"acme"') ON CONFLICT (key) DO UPDATE SET value = '"acme"'`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(t.Context(), `DELETE FROM afs.service_settings WHERE key = 'secrets.path_prefix'`)
	}()
	if p, err := rows.PathPrefix(t.Context()); err != nil || p != "acme" {
		t.Fatalf("prefix after setting %q %v", p, err)
	}
	known, err := rows.KnownARNs(t.Context(), []string{arn, arnFor("afs/" + ns + "/_shared/other")})
	if err != nil || len(known) != 1 || !known[arn] {
		t.Fatalf("known %v %v", known, err)
	}
	if err := rows.RecordSwept(t.Context(), "arn:x", "orphan", "no database row"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'SweepOrphanSecret' AND actor_id = 'system:afs-reconciler' AND resource_id = 'arn:x'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows %d %v", n, err)
	}
}
