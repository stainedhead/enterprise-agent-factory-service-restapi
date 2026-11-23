package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/audit"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/revisions"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

func adminSession() port.Session {
	return port.Session{Actor: user, ActorType: port.ActorUser, ServiceAdmin: true, RequestID: "req_test"}
}

func inTx(t *testing.T, db *pgtest.DB, s port.Session, fn func(ctx context.Context, tx port.Tx) error) error {
	t.Helper()
	return postgres.NewTxRunner(appPool(t, db)).InTx(t.Context(), s, fn)
}

func wantKind(t *testing.T, err error, k domain.ErrorKind) {
	t.Helper()
	if domain.KindOf(err) != k {
		t.Errorf("want %s, got %v", k, err)
	}
}

func newTeam(t *testing.T, slug string) domain.Team {
	t.Helper()
	tm, err := domain.NewTeam(pgtest.NewID("team"), domain.NewTeamParams{
		Slug: strings.ToLower(slug), DisplayName: "Team " + slug, CostCenter: "CC100", OwnerEmail: "owner@example.com",
		Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("500.00"), IsolationTier: domain.IsolationTierShared,
		AllowedAwsAccounts: []string{"111122223333"},
	}, time.Now(), 30)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

// M1-B: team rows round trip, optimistic concurrency, RLS visibility.
func TestTeamRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewTeamRepo()
	tm := newTeam(t, "alpha"+pgtest.NewID("x")[2:6])
	other := newTeam(t, "bravo"+pgtest.NewID("x")[2:6])
	err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		if err := repo.Insert(ctx, tx, tm); err != nil {
			return err
		}
		return repo.Insert(ctx, tx, other)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Insert(ctx, tx, tm), domain.KindConflict)
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}

	var v domain.Version
	err = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		got, err := repo.Get(ctx, tx, tm.ID, true)
		if err != nil {
			return err
		}
		if got.Slug != tm.Slug || got.MaxBudget.String() != "500.000000" && got.MaxBudget.String() != "500.00" {
			t.Errorf("team = %+v", got)
		}
		if got.Status.Phase != domain.PhasePending || len(got.AllowedAwsAccounts) != 1 {
			t.Errorf("status/accounts = %+v %v", got.Status, got.AllowedAwsAccounts)
		}
		v = got.Version
		got.DisplayName = "Renamed"
		got.Generation++
		upd, err := repo.Update(ctx, tx, got, v)
		if err != nil {
			return err
		}
		if upd.DisplayName != "Renamed" || upd.Version <= v {
			t.Errorf("update = %+v", upd)
		}
		_, err = repo.Update(ctx, tx, got, v)
		wantKind(t, err, domain.KindPreconditionFailed)
		missing := got
		missing.ID = pgtest.NewID("team")
		_, err = repo.Update(ctx, tx, missing, 1)
		wantKind(t, err, domain.KindNotFound)
		list, err := repo.List(ctx, tx, teams.ListFilter{}, "", 10)
		if err != nil || len(list) < 2 {
			t.Errorf("list = %d, %v", len(list), err)
		}
		list, _ = repo.List(ctx, tx, teams.ListFilter{State: domain.TeamStateSuspended}, "", 10)
		if len(list) != 0 {
			t.Errorf("suspended filter returned %d", len(list))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A caller bound to another team does not see this one; a denied read is NotFound.
	err = inTx(t, db, userSession(other.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := repo.Get(ctx, tx, tm.ID, false)
		wantKind(t, err, domain.KindNotFound)
		list, err := repo.List(ctx, tx, teams.ListFilter{}, "", 10)
		if err != nil || len(list) != 1 || list[0].ID != other.ID {
			t.Errorf("scoped list = %v, %v", list, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// M1-B: models and tags sets, their versions, and the service-admin-only model writes.
func TestTeamModelsAndTags(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewTeamRepo()
	tm := newTeam(t, "models"+pgtest.NewID("x")[2:6])
	modelID := "anthropic.test-" + pgtest.NewID("m")[2:8]
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.models (id, provider, display_name, context_window) VALUES ($1,'anthropic','Test',200000)`, modelID); err != nil {
		t.Fatal(err)
	}
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error { return repo.Insert(ctx, tx, tm) }); err != nil {
		t.Fatal(err)
	}
	err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		m0, err := repo.Models(ctx, tx, tm.ID)
		v0 := m0.Version
		if err != nil || len(m0.ModelIDs) != 0 {
			t.Errorf("models = %v, %v", m0, err)
			return errors.New("assertion failed")
		}
		found, err := repo.ModelsByID(ctx, tx, []string{modelID, "nope"})
		if err != nil || len(found) != 1 || found[modelID].ContextWindow != 200000 {
			t.Errorf("by id = %v, %v", found, err)
			return errors.New("assertion failed")
		}
		if err := repo.ReplaceModels(ctx, tx, tm.ID, []string{modelID}); err != nil {
			return err
		}
		m1, err := repo.Models(ctx, tx, tm.ID)
		v1 := m1.Version
		if err != nil || len(m1.ModelIDs) != 1 || v1 <= v0 {
			t.Errorf("after replace: %v v0=%d v1=%d %v", m1, v0, v1, err)
		}
		if err := repo.ReplaceModels(ctx, tx, tm.ID, []string{modelID}); err != nil {
			return err
		}
		if m2, _ := repo.Models(ctx, tx, tm.ID); m2.Version != v1 {
			t.Errorf("unchanged replace moved the version %d -> %d", v1, m2.Version)
		}

		_, tv0, err := repo.TeamTags(ctx, tx, tm.ID)
		if err != nil {
			return err
		}
		if err := repo.ReplaceTeamTags(ctx, tx, tm.ID, []domain.TagValue{{Key: "project", Value: "a"}, {Key: "env", Value: "dev"}}); err != nil {
			return err
		}
		if err := repo.ReplaceTeamTags(ctx, tx, tm.ID, []domain.TagValue{{Key: "project", Value: "b"}}); err != nil {
			return err
		}
		tags, tv1, err := repo.TeamTags(ctx, tx, tm.ID)
		if err != nil || len(tags) != 1 || tags[0].Value != "b" || tv1 <= tv0 {
			t.Errorf("tags = %v v0=%d v1=%d %v", tags, tv0, tv1, err)
		}
		if _, err := repo.ServiceTags(ctx, tx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A team admin cannot change the team's models (database contract: service admin only).
	_ = inTx(t, db, userSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		return repo.ReplaceModels(ctx, tx, tm.ID, nil)
	})
	err = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		m, err := repo.Models(ctx, tx, tm.ID)
		if err == nil && len(m.ModelIDs) != 1 {
			t.Errorf("a team admin changed the team's models: %v", m)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// M1-C: members and API keys.
func TestMemberAndAPIKeyRepos(t *testing.T) {
	db := pgtest.Get(t)
	teamRepo := postgres.NewTeamRepo()
	members := postgres.NewMemberRepo()
	keys := postgres.NewAPIKeyRepo()
	tm := newTeam(t, "mem"+pgtest.NewID("x")[2:6])
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error { return teamRepo.Insert(ctx, tx, tm) }); err != nil {
		t.Fatal(err)
	}
	userID := pgtest.NewID("usr")
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.users (id, oidc_issuer, oidc_subject, email, display_name)
VALUES ($1, 'https://idp.example.com', $1, 'Pat@Example.com', 'Pat')`, userID); err != nil {
		t.Fatal(err)
	}
	var memberID string
	err := inTx(t, db, userSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		id, ok, err := members.ResolveUser(ctx, tx, "", "pat@example.com")
		if err != nil || !ok || id != userID {
			t.Errorf("resolve = %q %v %v", id, ok, err)
			return errors.New("assertion failed")
		}
		if _, ok, _ := members.ResolveUser(ctx, tx, "", "nobody@example.com"); ok {
			t.Error("resolved a missing user")
		}
		memberID = pgtest.NewID("rb")
		m := domain.Member{ID: memberID, TeamID: tm.ID, Role: domain.TeamRoleTeamViewer, SubjectType: domain.SubjectTypeUser, UserID: userID}
		if err := members.Insert(ctx, tx, m); err != nil {
			return err
		}
		m.ID = pgtest.NewID("rb")
		wantKind(t, members.Insert(ctx, tx, m), domain.KindConflict)
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	err = inTx(t, db, userSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		memberID = pgtest.NewID("rb")
		if err := members.Insert(ctx, tx, domain.Member{ID: memberID, TeamID: tm.ID, Role: domain.TeamRoleTeamViewer, SubjectType: domain.SubjectTypeUser, UserID: userID}); err != nil {
			return err
		}
		got, err := members.Get(ctx, tx, tm.ID, memberID, true)
		if err != nil || got.Email != "Pat@Example.com" || got.Role != domain.TeamRoleTeamViewer {
			t.Errorf("get = %+v %v", got, err)
			return errors.New("assertion failed")
		}
		up, err := members.UpdateRole(ctx, tx, tm.ID, memberID, domain.TeamRoleTeamAdmin, got.Version)
		if err != nil || up.Role != domain.TeamRoleTeamAdmin || up.Version <= got.Version {
			t.Errorf("update = %+v %v", up, err)
			return errors.New("assertion failed")
		}
		_, err = members.UpdateRole(ctx, tx, tm.ID, memberID, domain.TeamRoleTeamViewer, got.Version)
		wantKind(t, err, domain.KindPreconditionFailed)
		_, err = members.UpdateRole(ctx, tx, tm.ID, pgtest.NewID("rb"), domain.TeamRoleTeamViewer, 1)
		wantKind(t, err, domain.KindNotFound)
		list, err := members.List(ctx, tx, tm.ID, "", 10)
		if err != nil || len(list) != 1 {
			t.Errorf("list = %v %v", list, err)
		}
		wantKind(t, members.Delete(ctx, tx, tm.ID, memberID, got.Version), domain.KindPreconditionFailed)
		if err := members.Delete(ctx, tx, tm.ID, memberID, up.Version); err != nil {
			return err
		}
		wantKind(t, members.Delete(ctx, tx, tm.ID, memberID, up.Version), domain.KindNotFound)

		now := time.Now().UTC().Truncate(time.Second)
		raw, err := domain.GenerateAPIKey(zeroReader{})
		if err != nil {
			return err
		}
		k := domain.APIKey{ID: pgtest.NewID("key"), TeamID: tm.ID, Name: "ci", Role: domain.TeamRoleTeamViewer,
			KeyPrefix: raw.Prefix, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now}
		sum := domain.HashAPIKey(raw.Secret)
		out, err := keys.Insert(ctx, tx, k, sum)
		if err != nil || out.Name != "ci" {
			t.Errorf("insert key = %+v %v", out, err)
			return errors.New("assertion failed")
		}
		k2 := k
		k2.ID = pgtest.NewID("key")
		_, err = keys.Insert(ctx, tx, k2, [32]byte{1})
		wantKind(t, err, domain.KindConflict)
		return errors.New("rollback after duplicate")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i + 1)
	}
	return len(p), nil
}

// M1-C/E: key list and delete, settings, service tags, identity providers, audit and revisions.
func TestAdminRepos(t *testing.T) {
	db := pgtest.Get(t)
	teamRepo := postgres.NewTeamRepo()
	keys := postgres.NewAPIKeyRepo()
	sets := postgres.NewSettingsRepo()
	stags := postgres.NewServiceTagRepo()
	idps := postgres.NewIdPRepo()
	revs := postgres.NewRevisionRepo()
	aud := postgres.NewAuditRepo()
	tm := newTeam(t, "adm"+pgtest.NewID("x")[2:6])

	err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		if err := teamRepo.Insert(ctx, tx, tm); err != nil {
			return err
		}
		now := time.Now().UTC()
		raw, _ := domain.GenerateAPIKey(zeroReader{})
		kid := pgtest.NewID("key")
		if _, err := keys.Insert(ctx, tx, domain.APIKey{ID: kid, TeamID: tm.ID, Name: "k1", Role: domain.TeamRoleTeamAdmin,
			KeyPrefix: raw.Prefix, ExpiresAt: now.Add(time.Hour), CreatedAt: now}, domain.HashAPIKey(raw.Secret)); err != nil {
			return err
		}
		list, err := keys.List(ctx, tx, tm.ID, "", 10)
		if err != nil || len(list) != 1 || list[0].KeyPrefix != raw.Prefix {
			t.Errorf("keys = %v %v", list, err)
		}
		del, err := keys.Delete(ctx, tx, tm.ID, kid)
		if err != nil || del.ID != kid {
			t.Errorf("delete = %+v %v", del, err)
		}
		_, err = keys.Delete(ctx, tx, tm.ID, kid)
		wantKind(t, err, domain.KindNotFound)

		// settings
		st, err := sets.Put(ctx, tx, "budget.ephemeral_max_days", json.RawMessage(`30`), nil)
		if err != nil {
			t.Errorf("put setting: %v", err)
			return errors.New("assertion failed")
		}
		v := st.Version
		st2, err := sets.Put(ctx, tx, "budget.ephemeral_max_days", json.RawMessage(`45`), &v)
		if err != nil || st2.Version <= v {
			t.Errorf("put with expect: %+v %v", st2, err)
			return errors.New("assertion failed")
		}
		_, err = sets.Put(ctx, tx, "budget.ephemeral_max_days", json.RawMessage(`46`), &v)
		wantKind(t, err, domain.KindPreconditionFailed)
		missing := domain.Version(1)
		_, err = sets.Put(ctx, tx, "no.such_key", json.RawMessage(`1`), &missing)
		wantKind(t, err, domain.KindNotFound)
		all, err := sets.All(ctx, tx)
		if err != nil || string(all["budget.ephemeral_max_days"].Value) != "45" {
			t.Errorf("all = %v %v", all, err)
		}

		// service tags
		val := "fin"
		tag := domain.ServiceTag{Key: "cost-line", Mode: domain.ServiceTagModeFixed, Value: &val, Description: "d"}
		if err := stags.Insert(ctx, tx, tag); err != nil {
			return err
		}
		got, err := stags.Get(ctx, tx, "cost-line", true)
		if err != nil || got.Value == nil || *got.Value != "fin" {
			t.Errorf("tag = %+v %v", got, err)
			return errors.New("assertion failed")
		}
		other := "ops"
		got.Value = &other
		up, err := stags.Update(ctx, tx, got, got.Version)
		if err != nil || *up.Value != "ops" {
			t.Errorf("tag update = %+v %v", up, err)
			return errors.New("assertion failed")
		}
		_, err = stags.Update(ctx, tx, got, got.Version)
		wantKind(t, err, domain.KindPreconditionFailed)
		page, err := stags.List(ctx, tx, "", 10)
		if err != nil || len(page) != 1 {
			t.Errorf("tags = %v %v", page, err)
		}
		wantKind(t, stags.Delete(ctx, tx, "cost-line", got.Version), domain.KindPreconditionFailed)
		if err := stags.Delete(ctx, tx, "cost-line", up.Version); err != nil {
			return err
		}
		wantKind(t, stags.Delete(ctx, tx, "cost-line", up.Version), domain.KindNotFound)

		// identity providers
		p := domain.IdentityProvider{ID: pgtest.NewID("idp"), Kind: domain.IdPKindOkta, Name: "Okta", IssuerURL: "https://example.okta.com",
			ConsoleClientID: "console", GroupsClaim: "groups", IsDefault: true, Enabled: true}
		if err := idps.Insert(ctx, tx, p); err != nil {
			return err
		}
		q := p
		q.ID = pgtest.NewID("idp")
		q.IsDefault = false
		wantKind(t, idps.Insert(ctx, tx, q), domain.KindConflict)
		return errors.New("rollback after conflict")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}

	err = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		p := domain.IdentityProvider{ID: pgtest.NewID("idp"), Kind: domain.IdPKindEntraId, Name: "Entra", IssuerURL: "https://login.example.com/t/v2.0",
			ConsoleClientID: "console", GroupsClaim: "groups", Enabled: true}
		if err := idps.Insert(ctx, tx, p); err != nil {
			return err
		}
		got, err := idps.Get(ctx, tx, p.ID, true)
		if err != nil || got.Name != "Entra" || got.CLIClientID != "" {
			t.Errorf("idp = %+v %v", got, err)
			return errors.New("assertion failed")
		}
		got.Name = "Entra 2"
		up, err := idps.Update(ctx, tx, got, got.Version)
		if err != nil || up.Name != "Entra 2" {
			t.Errorf("idp update = %+v %v", up, err)
			return errors.New("assertion failed")
		}
		_, err = idps.Update(ctx, tx, got, got.Version)
		wantKind(t, err, domain.KindPreconditionFailed)
		if list, err := idps.List(ctx, tx, "", 10); err != nil || len(list) == 0 {
			t.Errorf("idp list = %v %v", list, err)
		}
		if err := idps.ClearDefault(ctx, tx, p.ID); err != nil {
			t.Error(err)
		}
		if err := teamRepo.Insert(ctx, tx, newTeam(t, "idp"+pgtest.NewID("x")[2:6])); err != nil {
			return err
		}
		if refs, err := idps.TeamsUsing(ctx, tx, p.ID); err != nil || len(refs) != 0 {
			t.Errorf("teams using = %v %v", refs, err)
		}
		wantKind(t, idps.Delete(ctx, tx, p.ID, got.Version), domain.KindPreconditionFailed)
		if err := idps.Delete(ctx, tx, p.ID, up.Version); err != nil {
			return err
		}
		wantKind(t, idps.Delete(ctx, tx, p.ID, up.Version), domain.KindNotFound)

		// revisions of the team row written by the insert above are readable
		ok, err := revs.TeamExists(ctx, tx, tm.ID)
		if err != nil || ok {
			t.Errorf("team exists after rollback = %v %v", ok, err)
		}
		_, err = revs.Get(ctx, tx, tm.ID, 1<<40)
		wantKind(t, err, domain.KindNotFound)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Revisions and audit of a committed team.
	tm2 := newTeam(t, "rev"+pgtest.NewID("x")[2:6])
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		if err := teamRepo.Insert(ctx, tx, tm2); err != nil {
			return err
		}
		pt, _ := postgres.Unwrap(tx)
		_, err := pt.Exec(ctx, `INSERT INTO afs.audit_log (actor_type, actor_id, action, outcome, resource_kind, resource_id, team_id, request_id)
VALUES ('user', $1, 'CreateTeam', 'allowed', 'teams', $2, $2, 'req_1')`, user, tm2.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = inTx(t, db, userSession(tm2.ID), func(ctx context.Context, tx port.Tx) error {
		ok, err := revs.TeamExists(ctx, tx, tm2.ID)
		if err != nil || !ok {
			t.Errorf("exists = %v %v", ok, err)
			return errors.New("assertion failed")
		}
		list, err := revs.List(ctx, tx, tm2.ID, revisionsFilter("teams", ""), 0, 10)
		if err != nil || len(list) != 1 || list[0].Operation != "INSERT" || list[0].Snapshot != nil {
			t.Errorf("revisions = %+v %v", list, err)
			return errors.New("assertion failed")
		}
		one, err := revs.Get(ctx, tx, tm2.ID, list[0].Revision)
		if err != nil || len(one.Snapshot) == 0 {
			t.Errorf("revision = %+v %v", one, err)
		}
		events, err := aud.List(ctx, tx, audit.Filter{TeamID: tm2.ID, Action: "CreateTeam"}, 0, 10)
		if err != nil || len(events) != 1 || events[0].RequestID != "req_1" {
			t.Errorf("audit = %+v %v", events, err)
		}
		events, _ = aud.List(ctx, tx, audit.Filter{TeamID: tm2.ID, Outcome: "denied"}, 0, 10)
		if len(events) != 0 {
			t.Errorf("denied filter matched %d", len(events))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func revisionsFilter(kind, id string) revisions.Filter {
	return revisions.Filter{ResourceKind: kind, ResourceID: id}
}

// The status the engine writes under engine.KindTeam is the status the teams repository reads (the
// contract says resource_kind "team").
func TestTeamStatusKindMatchesTheEngine(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewTeamRepo()
	tm := newTeam(t, "stk"+pgtest.NewID("x")[2:6])
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error { return repo.Insert(ctx, tx, tm) }); err != nil {
		t.Fatal(err)
	}
	if teams.StatusKind != string(engine.KindTeam) {
		t.Fatalf("StatusKind = %q, engine kind %q", teams.StatusKind, engine.KindTeam)
	}
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase, conditions, observed_generation)
VALUES ($1, $2, $2, 'Ready', '[]', 1)`, string(engine.KindTeam), tm.ID); err != nil {
		t.Fatal(err)
	}
	err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		got, err := repo.Get(ctx, tx, tm.ID, false)
		if err == nil && got.Status.Phase != domain.PhaseReady {
			t.Errorf("phase = %q, want Ready", got.Status.Phase)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// RA-US-21: WithoutOrchestration lists the live teams with no agent and stops listing a team once it has one.
func TestTeamsWithoutOrchestration(t *testing.T) {
	db := pgtest.Get(t)
	db.SeedOrchestrationRuntime(t)
	repo := postgres.NewTeamRepo()
	id := pgtest.NewID("team")
	slug := "bf" + strings.ToLower(id[len(id)-8:])
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd, orchestration_required)
VALUES ($1, $2, $2, 'CC1', 'o@example.com', 1000, false)`, id, slug); err != nil {
		t.Fatal(err)
	}
	ids := func() (list []string, has bool) {
		err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
			var err error
			if list, err = repo.WithoutOrchestration(ctx, tx, 100000); err != nil {
				return err
			}
			has, err = repo.HasOrchestration(ctx, tx, id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return list, has
	}
	list, has := ids()
	if has || !slices.Contains(list, id) {
		t.Fatalf("before: has=%v listed=%v", has, slices.Contains(list, id))
	}
	err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		return repo.InsertOrchestration(ctx, tx, teams.OrchestrationAgent{TeamID: id, AgentID: pgtest.NewID("agt"), Name: domain.OrchestratorName,
			Models: domain.AgentModels{Access: domain.ModelAccessAll, DefaultModelID: pgtest.OrchestrationModel}})
	})
	if err != nil {
		t.Fatal(err)
	}
	list, has = ids()
	if !has || slices.Contains(list, id) {
		t.Fatalf("after: has=%v listed=%v", has, slices.Contains(list, id))
	}
}
