package postgres_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/exports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/export"
)

type liveIDs struct{}

func (liveIDs) NewID(p domain.IDPrefix) (domain.ID, error) { return domain.NewID(p, time.Now(), nil) }

type liveClock struct{}

func (liveClock) Now() time.Time { return time.Now() }

type memBlob struct{ objects map[string][]byte }

func (b *memBlob) Put(_ context.Context, key string, body []byte) (string, error) {
	b.objects[key] = body
	return "s3://exports-bucket/" + key, nil
}

func (b *memBlob) PresignGet(_ context.Context, key string, ttl time.Duration) (string, time.Time, error) {
	return "https://exports-bucket.s3.example/" + key + "?X-Amz-Signature=EXAMPLE", time.Now().Add(ttl), nil
}

type exportRig struct {
	db      *pgtest.DB
	team    seededAgent
	runner  *postgres.TxRunner
	deps    acting.Deps
	exports *exports.Service
	imports *imports.Service
	builder *export.Builder
	blob    *memBlob
}

func (r *exportRig) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := r.db.Admin(t).Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (r *exportRig) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := r.db.Admin(t).QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newExportRig(t *testing.T) *exportRig {
	t.Helper()
	db := pgtest.Get(t)
	r := &exportRig{db: db, blob: &memBlob{objects: map[string][]byte{}}}
	r.team = seedFullAgent(t, db)
	// Other tests assert on the shared outbox, so the events these tests publish are removed after.
	admin := db.Admin(t)
	var firstOutbox int64
	_ = admin.QueryRow(t.Context(), `SELECT coalesce(max(id), 0) FROM afs.outbox_events`).Scan(&firstOutbox)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.WithoutCancel(t.Context()), `DELETE FROM afs.outbox_events WHERE id > $1`, firstOutbox)
	})
	r.runner = postgres.NewTxRunner(appPool(t, db))
	r.deps = acting.Deps{Tx: r.runner, Audit: postgres.NewAuditWriter(r.runner), Outbox: postgres.NewOutboxWriter(), Idem: postgres.NewIdempotencyStore(),
		Clock: liveClock{}, IDs: liveIDs{}}
	r.exports = &exports.Service{Deps: r.deps, Repo: postgres.NewExportRepo()}
	r.imports = &imports.Service{Deps: r.deps, Repo: postgres.NewImportRepo(liveIDs{})}
	rr, raudit := reconcilerRunner(t, db)
	r.builder = &export.Builder{Store: postgres.NewExportJobs(rr, raudit, postgres.NewOutboxWriter(), liveIDs{}), Blob: r.blob}

	// What seedFullAgent does not cover: tags, a budget, members, an opt-out, an identity, a role link in an
	// allowed account, and a completed volume snapshot.
	tm := r.team.team.ID
	r.exec(t, `UPDATE afs.teams SET allowed_aws_accounts = ARRAY['*'], description = 'payments team' WHERE id = $1`, tm)
	r.exec(t, `INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ('111122223333', 'source', 'active'), ('444455556666', 'target', 'active') ON CONFLICT DO NOTHING`)
	r.exec(t, `INSERT INTO afs.team_tags (team_id, key, value) VALUES ($1, 'env', 'prod')`, tm)
	r.exec(t, `INSERT INTO afs.budgets (id, team_id, period, amount_usd) VALUES ($1, $2, 'monthly', 400)`, pgtest.NewID("bud"), tm)
	r.exec(t, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, 'team_admin', $2, 'group', 'payments-admins')`, pgtest.NewID("rb"), tm)
	r.exec(t, `INSERT INTO afs.external_system_optouts (id, team_id, agent_id, system_id, reason) VALUES ($1, $2, $3, $4, 'not needed')`,
		pgtest.NewID("xso"), tm, r.team.agentID, r.team.extID)
	r.exec(t, `INSERT INTO afs.agent_identities (id, team_id, agent_id, kind, external_ref, status) VALUES ($1, $2, $3, 'aws_iam_role', 'arn:aws:iam::111122223333:role/afs-reviewer', 'active')`,
		pgtest.NewID("aid"), tm, r.team.agentID)
	r.exec(t, `INSERT INTO afs.agent_role_links (id, team_id, agent_id, account_id, role_arn, profile_name, external_id, status)
VALUES ($1, $2, $3, '111122223333', 'arn:aws:iam::111122223333:role/deploy', 'deploy', 'afs-'||substr(md5($1), 1, 24), 'verified')`, pgtest.NewID("arl"), tm, r.team.agentID)
	r.exec(t, `INSERT INTO afs.agent_volume_snapshots (id, team_id, agent_id, reason, state, ebs_snapshot_id, size_gib, completed_at)
VALUES ($1, $2, $3, 'manual', 'completed', 'snap-0123456789abcdef0', 30, now())`, pgtest.NewID("vsnap"), tm, r.team.agentID)
	return r
}

var exportAdmin = acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser, ServiceAdmin: true}, RequestID: "req_export"}

func (r *exportRig) build(t *testing.T) (domain.Export, []byte) {
	t.Helper()
	res, err := r.exports.Create(t.Context(), exportAdmin, []string{r.team.team.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Export.Status != domain.ExportStatusPending {
		t.Fatalf("%+v", res.Export)
	}
	if err := r.builder.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	e, err := r.exports.Get(t.Context(), exportAdmin, res.Export.ID)
	if err != nil {
		t.Fatal(err)
	}
	return e, r.blob.objects[export.Key(e.ID)]
}

// wipeTeam removes the team and everything under it, as an operator restoring into an empty environment would
// see it.
func (r *exportRig) wipeTeam(t *testing.T) {
	t.Helper()
	tm := r.team.team.ID
	admin := r.db.Admin(t)
	rows, err := admin.Query(t.Context(), `SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'afs' AND column_name = 'team_id'
		AND table_name NOT IN ('audit_log', 'outbox_events', 'config_revisions', 'idempotency_keys')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	// Delete in repeated passes so foreign keys between the tables sort themselves out.
	for pass := 0; pass < 6; pass++ {
		for _, n := range tables {
			_, _ = admin.Exec(t.Context(), `DELETE FROM afs.`+n+` WHERE team_id = $1`, tm)
		}
	}
	r.exec(t, `DELETE FROM afs.teams WHERE id = $1`, tm)
}

func (r *exportRig) importBundle(t *testing.T, doc []byte, dry bool, m map[string]string) imports.Report {
	t.Helper()
	res, err := r.imports.Run(t.Context(), exportAdmin, imports.Input{Bundle: doc, AccountMap: m, DryRun: dry}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Report
}

// teamsPart is the canonical JSON of a bundle's teams, without the snapshot reference (the snapshot rows go with
// the wiped agent) and the secret ARNs' account when mapped.
func teamsPart(t *testing.T, doc []byte) string {
	t.Helper()
	b, err := domain.ParseBundle(doc)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b.Teams {
		for j := range b.Teams[i].Agents {
			b.Teams[i].Agents[j].LatestSnapshot = nil
		}
	}
	out, err := domain.CanonicalJSON(b.Teams)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// M4-F3: a succeeded export has the checksum, size, URI and a 7-day link; the stored object verifies and carries
// no secret value, key hash or client secret.
func TestExportBuildsABundleWithoutSecrets(t *testing.T) {
	r := newExportRig(t)
	// A team API key and an identity provider with a client secret ARN exist, and none may show up.
	r.exec(t, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, $2, 'ci', 'team_admin', 'afsk_xx', decode(repeat('ab', 32), 'hex'), now() + interval '30 days')`,
		pgtest.NewID("key"), r.team.team.ID)
	e, doc := r.build(t)
	if e.Status != domain.ExportStatusSucceeded || e.SHA256 == nil || e.SizeBytes == nil || *e.SizeBytes != int64(len(doc)) || e.S3URI == nil || e.DownloadURL == nil || e.Error != nil {
		t.Fatalf("%+v", e)
	}
	if left := time.Until(*e.DownloadExpiresAt); left < 7*24*time.Hour-time.Minute || left > 7*24*time.Hour+time.Minute {
		t.Fatalf("link valid for %v", left)
	}
	b, err := domain.ParseBundle(doc)
	if err != nil || b.SHA256 != *e.SHA256 {
		t.Fatalf("the stored object does not verify: %v", err)
	}
	for _, banned := range []string{"key_sha256", "keySha256", "abababab", "client_secret", "password"} {
		if bytes.Contains(doc, []byte(banned)) {
			t.Errorf("the bundle contains %q", banned)
		}
	}
	if !bytes.Contains(doc, []byte("arn:aws:secretsmanager")) || !bytes.Contains(doc, []byte("snap-0123456789abcdef0")) || !bytes.Contains(doc, []byte("afs-")) {
		t.Error("the bundle misses secret references, the snapshot reference or the role link external id")
	}
	if r.count(t, `SELECT count(*) FROM afs.audit_log WHERE action = 'ExportCompleted' AND resource_id = $1`, e.ID) != 1 ||
		r.count(t, `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'export.completed' AND aggregate_id = $1`, e.ID) != 1 {
		t.Error("export.completed or its audit record is missing")
	}
}

func TestExportFailureRecordsNoChecksum(t *testing.T) {
	r := newExportRig(t)
	res, err := r.exports.Create(t.Context(), exportAdmin, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr, raudit := reconcilerRunner(t, r.db)
	r.builder.Store = postgres.NewExportJobs(rr, raudit, postgres.NewOutboxWriter(), liveIDs{})
	r.builder.Blob = failingBlob{}
	if err := r.builder.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	e, _ := r.exports.Get(t.Context(), exportAdmin, res.Export.ID)
	if e.Status != domain.ExportStatusFailed || e.SHA256 != nil || e.DownloadURL != nil || e.Error == nil || e.FinishedAt == nil {
		t.Fatalf("%+v", e)
	}
	if r.count(t, `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'export.failed' AND aggregate_id = $1`, e.ID) != 1 {
		t.Error("export.failed is missing")
	}
}

type failingBlob struct{}

func (failingBlob) Put(context.Context, string, []byte) (string, error) {
	return "", context.DeadlineExceeded
}

func (failingBlob) PresignGet(context.Context, string, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}

// M4-F4, M4-F5: export a team, import it unchanged (skipped), wipe it, import it as a dry run (nothing written),
// then apply (created), and export again: the team's bundle content is identical.
func TestExportImportRoundTrip(t *testing.T) {
	r := newExportRig(t)
	e1, doc1 := r.build(t)
	same := map[string]string{"111122223333": "111122223333"}

	rep := r.importBundle(t, doc1, false, same)
	if len(rep.Teams) != 1 || rep.Teams[0].Result != imports.ResultSkipped {
		t.Fatalf("importing an unchanged team: %+v", rep.Teams)
	}

	r.wipeTeam(t)
	dry := r.importBundle(t, doc1, true, same)
	if !dry.DryRun || dry.Teams[0].Result != imports.ResultCreated || dry.Teams[0].Error != nil {
		t.Fatalf("dry run: %+v", dry.Teams)
	}
	if r.count(t, `SELECT count(*) FROM afs.teams WHERE slug = $1`, r.team.team.Slug) != 0 {
		t.Fatal("a dry run wrote the team")
	}
	if !strings.Contains(strings.Join(dry.Teams[0].Changes, "\n"), "put the value again") {
		t.Fatalf("the plan does not list the secrets that need values: %v", dry.Teams[0].Changes)
	}

	auditBefore := r.count(t, `SELECT count(*) FROM afs.audit_log WHERE action = 'ImportResource'`)
	applied := r.importBundle(t, doc1, false, same)
	if applied.Teams[0].Result != imports.ResultCreated || applied.Teams[0].Error != nil {
		t.Fatalf("apply: %+v", applied.Teams)
	}
	var teamID string
	if err := r.db.Admin(t).QueryRow(t.Context(), `SELECT id FROM afs.teams WHERE slug = $1`, r.team.team.Slug).Scan(&teamID); err != nil {
		t.Fatal(err)
	}
	if n := r.count(t, `SELECT count(*) FROM afs.audit_log WHERE action = 'ImportResource'`) - auditBefore; n < 12 {
		t.Errorf("one audit record per resource: %d", n)
	}
	if r.count(t, `SELECT count(*) FROM afs.agent_role_links WHERE team_id = $1 AND status = 'pending_trust'`, teamID) != 1 {
		t.Error("an imported role link must be pending_trust")
	}
	if r.count(t, `SELECT count(*) FROM afs.agent_secret_bindings WHERE team_id = $1`, teamID) != 2 ||
		r.count(t, `SELECT count(*) FROM afs.agent_config_values WHERE team_id = $1`, teamID) != 2 ||
		r.count(t, `SELECT count(*) FROM afs.external_system_optouts WHERE team_id = $1`, teamID) != 1 {
		t.Error("bindings, configuration values or opt-outs missing after the import")
	}
	if r.count(t, `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'team.created' AND team_id = $1`, teamID) != 1 ||
		r.count(t, `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'import.applied' AND payload ->> 'id' IS NOT NULL AND aggregate_id = $1`, applied.ID) != 1 {
		t.Error("team.created or import.applied missing")
	}
	if r.count(t, `SELECT count(*) FROM afs.export_bundles WHERE id = $1 AND direction = 'import' AND s3_uri IS NULL AND NOT dry_run`, applied.ID) != 1 {
		t.Error("the import row is missing or has an S3 object")
	}

	r.team.team.ID = teamID
	_, doc2 := r.build(t)
	if teamsPart(t, doc1) != teamsPart(t, doc2) {
		t.Fatalf("the re-exported team differs from the first export:\n%s\n%s", teamsPart(t, doc1), teamsPart(t, doc2))
	}
	_ = e1
}

// M4-F5: the account map rewrites secret ARNs, identities, role links and their ARNs; a missing account fails the
// team with nothing written for it, and the other teams are applied.
func TestImportAccountMap(t *testing.T) {
	r := newExportRig(t)
	_, doc := r.build(t)
	r.wipeTeam(t)

	rep := r.importBundle(t, doc, false, map[string]string{"999900001111": "444455556666"})
	if rep.Teams[0].Result != imports.ResultFailed || rep.Teams[0].Error == nil || !strings.Contains(*rep.Teams[0].Error, "111122223333") || len(rep.Teams[0].Changes) != 0 {
		t.Fatalf("%+v", rep.Teams[0])
	}
	if r.count(t, `SELECT count(*) FROM afs.teams WHERE slug = $1`, r.team.team.Slug) != 0 {
		t.Fatal("a failed team left rows behind")
	}

	// Two teams in one bundle: the broken one (a model the target does not have) fails alone.
	b, _ := domain.ParseBundle(doc)
	good := b.Teams[0]
	bad := b.Teams[0]
	bad.Team = domain.Row{}
	for k, v := range good.Team {
		bad.Team[k] = v
	}
	bad.Team["slug"] = "ztwo" + good.Slug()[1:]
	bad.Models = []string{"no-such-model"}
	bad.Agents, bad.Secrets = nil, nil
	b.Teams = []domain.TeamBundle{bad, good}
	_, two, err := b.Seal()
	if err != nil {
		t.Fatal(err)
	}
	rep = r.importBundle(t, two, false, map[string]string{"111122223333": "444455556666"})
	if rep.Teams[0].Result != imports.ResultFailed || rep.Teams[1].Result != imports.ResultCreated {
		t.Fatalf("%+v", rep.Teams)
	}
	if r.count(t, `SELECT count(*) FROM afs.teams WHERE slug = $1`, bad.Slug()) != 0 {
		t.Fatal("the failed team was written")
	}
	if r.count(t, `SELECT count(*) FROM afs.agent_role_links WHERE account_id = '444455556666' AND role_arn = 'arn:aws:iam::444455556666:role/deploy'`) != 1 ||
		r.count(t, `SELECT count(*) FROM afs.secrets s JOIN afs.teams t ON t.id = s.team_id WHERE t.slug = $1 AND s.secrets_manager_arn LIKE 'arn:aws:secretsmanager:us-east-1:444455556666:%'`, good.Slug()) != 2 ||
		r.count(t, `SELECT count(*) FROM afs.agent_identities WHERE external_ref = 'arn:aws:iam::444455556666:role/afs-reviewer'`) != 1 {
		t.Error("the account map was not applied")
	}
}

// M4-F4: a bundle with a wrong checksum is refused and nothing is written.
func TestImportRefusesATamperedBundle(t *testing.T) {
	r := newExportRig(t)
	_, doc := r.build(t)
	r.wipeTeam(t)
	tampered := bytes.Replace(doc, []byte(`"payments team"`), []byte(`"payments team!"`), 1)
	_, err := r.imports.Run(t.Context(), exportAdmin, imports.Input{Bundle: tampered, DryRun: false}, nil)
	if domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("%v", err)
	}
	if r.count(t, `SELECT count(*) FROM afs.teams WHERE slug = $1`, r.team.team.Slug) != 0 {
		t.Fatal("a refused bundle wrote a team")
	}
}

// RA-US-14.7 at the database: only service admins may read or write export rows.
func TestExportRowsAreForServiceAdmins(t *testing.T) {
	r := newExportRig(t)
	if _, err := r.exports.Create(t.Context(), exportAdmin, nil, nil); err != nil {
		t.Fatal(err)
	}
	var seen int
	err := r.runner.ReadTx(t.Context(), port.Session{Actor: "usr_t", ActorType: port.ActorUser, TeamIDs: []string{r.team.team.ID}}, func(ctx context.Context, tx port.Tx) error {
		rows, err := postgres.NewExportRepo().List(ctx, tx, "", 10)
		seen = len(rows)
		return err
	})
	if err != nil || seen != 0 {
		t.Fatalf("a team member sees %d export rows (%v)", seen, err)
	}
}
