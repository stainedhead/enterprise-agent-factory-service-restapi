package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
)

// M4-A: role links and identities under the database guards, and the account listings.
func TestIdentityRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo, accts := postgres.NewIdentityRepo(), postgres.NewAccountsRepo()
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	agent, shared := seedAgent(t, db, a.ID, "dedic"), seedAgent(t, db, a.ID, "shared")
	admin := db.Admin(t)
	run := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	run(`UPDATE afs.agents SET iam_identity = 'dedicated' WHERE id = $1`, agent)
	run(`INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ('111122223333', 'payments-prod', 'active'), ('222233334444', 'sandbox', 'active'), ('333344445555', 'old', 'suspended') ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, status = EXCLUDED.status`)
	run(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['payments-*', '3333*'] WHERE id = $1`, a.ID)

	now := time.Now()
	good, _ := domain.NewRoleLink(pgtest.NewID("arl"), a.ID, agent, "afs-"+pgtest.NewID("x")[2:22]+"aaaaaaaaaaaaaaaaaaaa", domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/deployer", ProfileName: "prod"}, now)
	outside, _ := domain.NewRoleLink(pgtest.NewID("arl"), a.ID, agent, "afs-bbbbbbbbbbbbbbbbbbbbbbbb", domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::222233334444:role/x", ProfileName: "sbx"}, now)
	onShared, _ := domain.NewRoleLink(pgtest.NewID("arl"), a.ID, shared, "afs-cccccccccccccccccccccccc", domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/y", ProfileName: "p"}, now)

	s := teamSession(a.ID)
	if err := inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		ok, err := repo.AccountAllowed(ctx, tx, a.ID, "111122223333")
		if err != nil || !ok {
			t.Errorf("allowed: %v %v", ok, err)
		}
		if ok, _ = repo.AccountAllowed(ctx, tx, a.ID, "222233334444"); ok {
			t.Error("outside the filter")
		}
		if ok, _ = repo.AccountAllowed(ctx, tx, a.ID, "333344445555"); ok {
			t.Error("suspended account")
		}
		info, err := repo.Agent(ctx, tx, a.ID, agent)
		if err != nil || info.IAMIdentity != domain.IAMIdentityDedicated || info.Namespace == "" {
			t.Errorf("agent: %+v %v", info, err)
		}
		if _, err := repo.Agent(ctx, tx, b.ID, agent); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("agent of another team: %v", err)
		}
		return repo.InsertLink(ctx, tx, good)
	}); err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]domain.RoleLink{"outside the filter": outside, "shared agent": onShared} {
		_ = inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
			if k := domain.KindOf(repo.InsertLink(ctx, tx, l)); k != domain.KindValidation {
				t.Errorf("%s: %v", name, k)
			}
			return context.Canceled
		})
	}
	dup, _ := domain.NewRoleLink(pgtest.NewID("arl"), a.ID, agent, "afs-dddddddddddddddddddddddd", domain.NewRoleLinkParams{RoleARN: "arn:aws:iam::111122223333:role/deployer", ProfileName: "other"}, now)
	_ = inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.InsertLink(ctx, tx, dup), domain.KindConflict)
		return context.Canceled
	})

	if err := inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		rows, err := repo.ListLinks(ctx, tx, a.ID, agent, "", 10)
		if err != nil || len(rows) != 1 || rows[0].Status != domain.RoleLinkStatusPendingTrust || rows[0].MaxSessionSeconds != 3600 {
			t.Errorf("list: %+v %v", rows, err)
		}
		if _, err := repo.GetLink(ctx, tx, b.ID, agent, good.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("link of another team: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run(`UPDATE afs.agent_role_links SET status = 'failed', last_error = 'AccessDenied' WHERE id = $1`, good.ID)
	if err := inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		l, err := repo.RequestVerify(ctx, tx, a.ID, agent, good.ID)
		if err != nil || l.Status != domain.RoleLinkStatusPendingTrust || l.LastError != "" || l.Version != 3 {
			t.Errorf("verify: %+v %v", l, err)
		}
		wantKind(t, repo.DeleteLink(ctx, tx, a.ID, agent, good.ID, 1), domain.KindPreconditionFailed)
		return repo.DeleteLink(ctx, tx, a.ID, agent, good.ID, l.Version)
	}); err != nil {
		t.Fatal(err)
	}

	// Identities.
	ident, _ := domain.NewAgentIdentity(pgtest.NewID("aid"), a.ID, agent, domain.NewAgentIdentityParams{Kind: domain.IdentityGitHubApp, ExternalRef: "42"}, now)
	if err := inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		if err := repo.InsertIdentity(ctx, tx, ident); err != nil {
			return err
		}
		got, err := repo.GetIdentity(ctx, tx, a.ID, agent, ident.ID, true)
		if err != nil || got.Kind != domain.IdentityGitHubApp || got.Status != domain.IdentityPending || got.SecretID != "" {
			t.Errorf("identity: %+v %v", got, err)
		}
		if ok, _ := repo.SecretInTeam(ctx, tx, a.ID, "sec_x"); ok {
			t.Error("unknown secret")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	again := ident
	again.ID = pgtest.NewID("aid")
	_ = inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.InsertIdentity(ctx, tx, again), domain.KindConflict)
		return context.Canceled
	})
	if err := inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		if err := repo.DeleteIdentity(ctx, tx, b.ID, agent, ident.ID, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("other team delete: %v", err)
		}
		return repo.DeleteIdentity(ctx, tx, a.ID, agent, ident.ID, 1)
	}); err != nil {
		t.Fatal(err)
	}

	// Account listings: the team sees the active accounts its filter matches.
	if err := inTx(t, db, s, func(ctx context.Context, tx port.Tx) error {
		rows, err := accts.ListAllowed(ctx, tx, a.ID, "", 10)
		if err != nil || len(rows) != 1 || rows[0].ID != "111122223333" {
			t.Errorf("allowed: %+v %v", rows, err)
		}
		all, err := accts.ListOrg(ctx, tx, "", 10)
		if err != nil || len(all) < 3 {
			t.Errorf("org: %+v %v", all, err)
		}
		if ok, _ := accts.TeamExists(ctx, tx, a.ID); !ok {
			t.Error("team exists")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// M4-A1: the mirror upserts, deletes what left the organization and retires what a link still uses.
func TestOrgMirrorStore(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	store := postgres.NewOrgMirrorStore(runner)
	admin := db.Admin(t)
	tm := db.SeedTeam(t)
	agent := seedAgent(t, db, tm.ID, "mirror")
	// Clean slate for this test's accounts.
	if _, err := admin.Exec(t.Context(), `DELETE FROM afs.org_aws_accounts WHERE id IN ('555566667777', '666677778888', '777788889999')`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agents SET iam_identity = 'dedicated' WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	keep := domain.OrgAccount{ID: "555566667777", Name: "keep", Status: domain.OrgAccountActive}
	used := domain.OrgAccount{ID: "666677778888", Name: "used", Status: domain.OrgAccountActive}
	gone := domain.OrgAccount{ID: "777788889999", Name: "gone", Status: domain.OrgAccountActive}
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := store.Mirror(t.Context(), []domain.OrgAccount{keep, used, gone}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.teams SET allowed_aws_accounts = ARRAY['*'] WHERE id = $1`, tm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.agent_role_links (id, team_id, agent_id, account_id, role_arn, profile_name, external_id)
VALUES ($1, $2, $3, '666677778888', 'arn:aws:iam::666677778888:role/r', 'p', 'afs-eeeeeeeeeeeeeeeeeeeeeeee')`, pgtest.NewID("arl"), tm.ID, agent); err != nil {
		t.Fatal(err)
	}
	// Only the live accounts of this test are listed; every other mirrored account is "removed" too, which is the point.
	res, err := store.Mirror(t.Context(), []domain.OrgAccount{{ID: "555566667777", Name: "renamed", Status: domain.OrgAccountActive}}, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	has := func(ids []string, id string) bool {
		for _, x := range ids {
			if x == id {
				return true
			}
		}
		return false
	}
	if !has(res.Removed, "777788889999") || !has(res.Retired, "666677778888") || has(res.Removed, "666677778888") {
		t.Fatalf("result: %+v", res)
	}
	var name, status string
	if err := admin.QueryRow(t.Context(), `SELECT name FROM afs.org_aws_accounts WHERE id = '555566667777'`).Scan(&name); err != nil || name != "renamed" {
		t.Fatalf("upsert: %q %v", name, err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT status FROM afs.org_aws_accounts WHERE id = '666677778888'`).Scan(&status); err != nil || status != "suspended" {
		t.Fatalf("retired: %q %v", status, err)
	}
	var n int
	_ = admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.org_aws_accounts WHERE id = '777788889999'`).Scan(&n)
	if n != 0 {
		t.Fatal("removed account still mirrored")
	}
}

// M4-A4/A5: the links store reads the team's links with the filter verdict and writes status, audit and
// the event in one transaction, never over a status that changed meanwhile.
func TestLinksStore(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	audit := postgres.NewAuditWriter(runner)
	store := postgres.NewLinksStore(runner, audit, postgres.NewOutboxWriter(), budgetIDs{})
	admin := db.Admin(t)
	tm := db.SeedTeam(t)
	agent := seedAgent(t, db, tm.ID, "links")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE afs.agents SET iam_identity = 'dedicated' WHERE id = $1`, agent)
	exec(`INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ('111122223333', 'payments', 'active') ON CONFLICT (id) DO UPDATE SET status = 'active'`)
	exec(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['111122223333'] WHERE id = $1`, tm.ID)
	link := pgtest.NewID("arl")
	exec(`INSERT INTO afs.agent_role_links (id, team_id, agent_id, account_id, role_arn, profile_name, external_id)
VALUES ($1, $2, $3, '111122223333', 'arn:aws:iam::111122223333:role/r', 'p', 'afs-ffffffffffffffffffffffff')`, link, tm.ID, agent)

	d, err := store.LoadLinks(t.Context(), tm.ID)
	if err != nil || len(d.Links) != 1 || !d.Links[0].Allowed || !d.Links[0].AgentDedicated || !d.Links[0].AgentLive ||
		d.Links[0].Status != domain.RoleLinkStatusPendingTrust || d.Links[0].Namespace == "" || d.State != "active" {
		t.Fatalf("load: %+v %v", d, err)
	}
	if _, err := store.LoadLinks(t.Context(), "team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ"); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown team: %v", err)
	}
	at := time.Now().UTC()
	ch := links.Change{TeamID: tm.ID, AgentID: agent, LinkID: link, From: domain.RoleLinkStatusPendingTrust, To: domain.RoleLinkStatusVerified,
		VerifiedAt: &at, AccountID: "111122223333", RoleARN: "arn:aws:iam::111122223333:role/r", Event: domain.EventRoleLinkVerified}
	if err := store.Record(t.Context(), ch); err != nil {
		t.Fatal(err)
	}
	var status string
	var verified *time.Time
	var events, audits int
	row := func() {
		if err := admin.QueryRow(t.Context(), `SELECT status, last_verified_at FROM afs.agent_role_links WHERE id = $1`, link).Scan(&status, &verified); err != nil {
			t.Fatal(err)
		}
		_ = admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type LIKE 'role_link.%'`, link).Scan(&events)
		_ = admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = $2`, link, postgres.AuditLinkStatus).Scan(&audits)
	}
	row()
	if status != "verified" || verified == nil || events != 1 || audits != 1 {
		t.Fatalf("after verify: %s %v events=%d audits=%d", status, verified, events, audits)
	}
	// A stale pass (it saw pending_trust, the link is verified now) changes nothing.
	stale := ch
	stale.To, stale.Event, stale.Error, stale.VerifiedAt = domain.RoleLinkStatusFailed, domain.EventRoleLinkFailed, "AccessDenied", nil
	if err := store.Record(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	row()
	if status != "verified" || events != 1 || audits != 1 {
		t.Fatalf("stale write applied: %s events=%d audits=%d", status, events, audits)
	}
	// Narrowing the filter: the verdict flips and a disable is recorded without an event.
	exec(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['999999999999'] WHERE id = $1`, tm.ID)
	if d, _ = store.LoadLinks(t.Context(), tm.ID); d.Links[0].Allowed {
		t.Fatal("narrowed filter must disallow the account")
	}
	off := links.Change{TeamID: tm.ID, AgentID: agent, LinkID: link, From: domain.RoleLinkStatusVerified, To: domain.RoleLinkStatusDisabled, Error: "AccountNotAllowed"}
	if err := store.Record(t.Context(), off); err != nil {
		t.Fatal(err)
	}
	var lastErr string
	if err := admin.QueryRow(t.Context(), `SELECT status, coalesce(last_error, '') FROM afs.agent_role_links WHERE id = $1`, link).Scan(&status, &lastErr); err != nil || status != "disabled" || lastErr != "AccountNotAllowed" {
		t.Fatalf("disabled: %s %q %v", status, lastErr, err)
	}
}
