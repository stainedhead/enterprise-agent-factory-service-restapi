package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
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
