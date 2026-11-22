package postgres_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func seedProvider(t *testing.T, db *pgtest.DB, isDefault, enabled bool) string {
	t.Helper()
	id := pgtest.NewID("idp")
	issuer := "https://" + strings.ToLower(id[4:12]) + ".example.com"
	admin := db.Admin(t)
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, is_default, enabled)
VALUES ($1, 'okta', $2, $3, 'client-'||$1, $4, $5)`, id, "idp "+id, issuer, isDefault, enabled); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.identity_providers WHERE id = $1`, id)
	})
	return id
}

func seedUser(t *testing.T, db *pgtest.DB, email string) string {
	t.Helper()
	id := pgtest.NewID("usr")
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.users (id, oidc_issuer, oidc_subject, email, display_name) VALUES ($1, 'https://seed.example.com', $1, $2, 'Seed')`, id, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func repoOf(t *testing.T, db *pgtest.DB) *postgres.PrincipalRepo {
	t.Helper()
	return postgres.NewPrincipalRepo(postgres.NewTxRunner(appPool(t, db)), nil)
}

// RA-US-2.1: only enabled providers are listed.
func TestEnabledProviders(t *testing.T) {
	db := pgtest.Get(t)
	def := seedProvider(t, db, true, true)
	second := seedProvider(t, db, false, true)
	off := seedProvider(t, db, false, false)
	ps, err := repoOf(t, db).EnabledProviders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]auth.Provider{}
	for _, p := range ps {
		got[p.ID] = p
	}
	if _, ok := got[off]; ok {
		t.Fatal("a disabled provider was listed")
	}
	if !got[def].IsDefault || got[second].IsDefault || got[def].GroupsClaim != "groups" {
		t.Fatalf("providers = %+v", got)
	}
	if ps[0].ID != def {
		t.Fatal("the default provider is not first")
	}
}

func TestSignInCreatesRefreshesAndRefuses(t *testing.T) {
	db := pgtest.Get(t)
	r := repoOf(t, db)
	admin := db.Admin(t)
	now := time.Now().UTC().Truncate(time.Second)
	u := auth.UserClaims{Issuer: "https://signin.example.com", Subject: "sub-" + pgtest.NewID("x"), Email: pgtest.NewID("e") + "@example.com", Name: "Ada"}

	id, err := r.SignIn(t.Context(), u, now)
	if err != nil || !strings.HasPrefix(id, "usr_") {
		t.Fatalf("first sign-in: %q %v", id, err)
	}
	var email, name string
	var login *time.Time
	read := func() {
		t.Helper()
		if err := admin.QueryRow(t.Context(), `SELECT email, display_name, last_login_at FROM afs.users WHERE id = $1`, id).Scan(&email, &name, &login); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if email != u.Email || name != "Ada" || login == nil || !login.Equal(now) {
		t.Fatalf("row = %s %s %v", email, name, login)
	}

	// Same person again: same id, no rewrite inside the refresh window.
	if again, err := r.SignIn(t.Context(), u, now.Add(time.Minute)); err != nil || again != id {
		t.Fatalf("second sign-in: %q %v", again, err)
	}
	read()
	if !login.Equal(now) {
		t.Fatalf("last_login_at rewritten inside the window: %v", login)
	}
	// A changed name, or an old last login, refreshes the row.
	u.Name = "Ada L"
	if _, err := r.SignIn(t.Context(), u, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	read()
	if name != "Ada L" || !login.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("row = %s %v", name, login)
	}
	if _, err := r.SignIn(t.Context(), u, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	read()
	if !login.Equal(now.Add(time.Hour)) {
		t.Fatalf("stale last_login_at not refreshed: %v", login)
	}

	// A disabled person is refused.
	if _, err := admin.Exec(t.Context(), `UPDATE afs.users SET disabled_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SignIn(t.Context(), u, now); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("disabled user: %v", err)
	}

	// The same email through another provider is refused, not a 500.
	other := auth.UserClaims{Issuer: "https://other.example.com", Subject: "s2", Email: strings.ToUpper(u.Email), Name: "Imposter"}
	if _, err := r.SignIn(t.Context(), other, now); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("duplicate email: %v", err)
	}
}

func TestSignInConcurrentFirstRequests(t *testing.T) {
	db := pgtest.Get(t)
	r := repoOf(t, db)
	u := auth.UserClaims{Issuer: "https://race.example.com", Subject: "s-" + pgtest.NewID("x"), Email: pgtest.NewID("r") + "@example.com", Name: "Race"}
	ids := make([]string, 6)
	errs := make([]error, 6)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() { ids[i], errs[i] = r.SignIn(t.Context(), u, time.Now()) })
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("racing first sign-ins: %v %v", ids, errs)
		}
	}
}

// RA-US-2.2 (adapter half): bindings come back with the provider facts of the team and the binding.
func TestBindings(t *testing.T) {
	db := pgtest.Get(t)
	a, b, gone := db.SeedTeam(t), db.SeedTeam(t), db.SeedTeam(t)
	idp := seedProvider(t, db, false, true)
	admin := db.Admin(t)
	ctx := t.Context()
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET identity_provider_id = $1 WHERE id = $2`, idp, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	u := seedUser(t, db, pgtest.NewID("b")+"@example.com")
	group := "grp-" + strings.ToLower(pgtest.NewID("g")[4:10])
	if _, err := admin.Exec(ctx, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, user_id) VALUES
		($1, 'team_admin', $2, 'user', $3), ($4, 'team_viewer', $5, 'user', $3), ($6, 'team_viewer', $7, 'user', $3)`,
		pgtest.NewID("rb"), a.ID, u, pgtest.NewID("rb"), b.ID, pgtest.NewID("rb"), gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name, identity_provider_id) VALUES
		($1, 'team_viewer', $2, 'group', $3, NULL), ($4, 'service_admin', NULL, 'group', $3, $5),
		($6, 'team_viewer', $2, 'group', 'unrelated-group', NULL)`,
		pgtest.NewID("rb"), b.ID, group, pgtest.NewID("rb"), idp, pgtest.NewID("rb")); err != nil {
		t.Fatal(err)
	}
	rows, err := repoOf(t, db).Bindings(ctx, u, []string{group})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]auth.BindingRow{}
	for _, r := range rows {
		byKey[string(r.Role)+"/"+r.TeamID+"/"+r.Group] = r
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %+v (the deleted team and the unrelated group must be absent)", rows)
	}
	if r := byKey["team_admin/"+a.ID+"/"]; r.TeamProviderID != idp {
		t.Fatalf("team provider not carried: %+v", r)
	}
	if r := byKey["team_viewer/"+b.ID+"/"+group]; r.BindingProviderID != "" || r.TeamProviderID != "" {
		t.Fatalf("group binding: %+v", r)
	}
	if r, ok := byKey["service_admin//"+group]; !ok || r.TeamID != "" || r.BindingProviderID != idp {
		t.Fatalf("service group binding missing: %+v", rows)
	}
	// Nothing else is returned for a person with no bindings.
	none, err := repoOf(t, db).Bindings(ctx, seedUser(t, db, pgtest.NewID("n")+"@example.com"), nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("no bindings: %v %v", none, err)
	}
}

// RA-US-3a.1 (adapter half): flags and slugs under row-level security.
func TestMeRepoTeams(t *testing.T) {
	db := pgtest.Get(t)
	a, b, gone := db.SeedTeam(t), db.SeedTeam(t), db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET viewer_kubectl = true, may_manage_aws_accounts = true WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	r := postgres.NewMeRepo(postgres.NewTxRunner(appPool(t, db)))
	s := port.Session{Actor: user, ActorType: port.ActorUser, TeamIDs: []string{a.ID, b.ID, gone.ID}}
	teams, err := r.Teams(ctx, s, s.TeamIDs)
	if err != nil || len(teams) != 2 {
		t.Fatalf("teams = %+v %v (deleted team must be skipped)", teams, err)
	}
	for _, x := range teams {
		switch x.ID {
		case a.ID:
			if x.Slug != a.Slug || !x.Flags.ViewerKubectl || !x.Flags.MayManageAwsAccounts || x.Flags.MayAddExternalSystems {
				t.Fatalf("flags of A: %+v", x)
			}
		case b.ID:
			if x.Flags != (authz.TeamFlags{}) {
				t.Fatalf("flags of B: %+v", x)
			}
		default:
			t.Fatalf("unexpected team %s", x.ID)
		}
	}
	// Row-level security: a session that does not name the team sees nothing.
	if got, _ := r.Teams(ctx, port.Session{Actor: user, ActorType: port.ActorUser}, []string{a.ID}); len(got) != 0 {
		t.Fatalf("unscoped session saw %+v", got)
	}
	if f, ok, err := r.TeamFlags(ctx, s, a.ID); err != nil || !ok || !f.ViewerKubectl {
		t.Fatalf("TeamFlags: %+v %v %v", f, ok, err)
	}
	if _, ok, err := r.TeamFlags(ctx, s, gone.ID); err != nil || ok {
		t.Fatalf("deleted team: %v %v", ok, err)
	}
}

// RA-US-2.4: through auth.Service on a real database.
func TestAPIKeyAuthEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	a := db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	pool := appPool(t, db)
	runner := postgres.NewTxRunner(pool)
	svc := &auth.Service{Keys: postgres.NewCredentials(runner)}

	good, expired, deleted := "afsk_good_"+a.ID, "afsk_exp_"+a.ID, "afsk_del_"+a.ID
	for i, k := range []string{good, expired, deleted} {
		sum := auth.HashAPIKey(k)
		if _, err := admin.Exec(ctx, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES
			($1, $2, $3, 'team_viewer', 'afsk_xx', $4, now() + interval '1 day')`, pgtest.NewID("key"), a.ID, "k"+string(rune('a'+i)), sum[:]); err != nil {
			t.Fatal(err)
		}
	}
	// expires_at must stay after created_at, so the key expires by letting a short life pass.
	sum := auth.HashAPIKey(expired)
	if _, err := admin.Exec(ctx, `UPDATE afs.api_keys SET expires_at = now() + interval '300 milliseconds' WHERE key_sha256 = $1`, sum[:]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(600 * time.Millisecond)
	sum = auth.HashAPIKey(deleted)
	if _, err := admin.Exec(ctx, `UPDATE afs.api_keys SET revoked_at = now() WHERE key_sha256 = $1`, sum[:]); err != nil {
		t.Fatal(err)
	}

	p, err := svc.Authenticate(ctx, good)
	if err != nil || p.Type != authz.PrincipalAPIKey || len(p.Bindings) != 1 || p.Bindings[0] != (authz.Binding{Role: authz.RoleTeamViewer, TeamID: a.ID}) {
		t.Fatalf("good key: %+v %v", p, err)
	}
	var used *time.Time
	sum = auth.HashAPIKey(good)
	if err := admin.QueryRow(ctx, `SELECT last_used_at FROM afs.api_keys WHERE key_sha256 = $1`, sum[:]).Scan(&used); err != nil || used == nil {
		t.Fatalf("lastUsedAt did not advance: %v %v", used, err)
	}
	for name, k := range map[string]string{"expired": expired, "deleted": deleted, "unknown": "afsk_unknown"} {
		if p, err := svc.Authenticate(ctx, k); p != nil || domain.KindOf(err) != domain.KindUnauthenticated {
			t.Fatalf("%s key: %v %v", name, p, err)
		}
	}
}
