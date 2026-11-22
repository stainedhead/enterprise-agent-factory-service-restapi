package postgres_test

import (
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
)

// M5-E2: the source reads a team's flag, state and namespace and the enabled providers its people sign in
// through: the service-level ones and the team's own, never another team's and never a disabled one.
func TestKubectlSource(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	src := postgres.NewKubectlSource(runner)
	admin := db.Admin(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	idp := func(name string, def, enabled bool) string {
		id := pgtest.NewID("idp")
		exec(`INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, is_default, enabled)
VALUES ($1, 'okta', $2, $3, 'client', $4, $5)`, id, name, "https://"+name+".example.com", def, enabled)
		return id
	}
	// Only one default may exist database-wide: replace any earlier one.
	exec(`UPDATE afs.identity_providers SET is_default = false WHERE is_default`)
	svcDefault := idp("kc-default-"+a.Slug, true, true)
	chosen := idp("kc-chosen-"+a.Slug, false, true)

	// No provider chosen: the service default.
	got, err := src.LoadTeam(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ViewerKubectl || got.State != "active" || got.Slug != a.Slug || got.Namespace != "afs-t-"+a.Slug {
		t.Fatalf("team = %+v", got)
	}
	if want := []kubectl.Provider{{ID: svcDefault, IsDefault: true}}; !slices.Equal(got.Providers, want) {
		t.Errorf("providers = %+v, want the default %+v", got.Providers, want)
	}
	// A chosen provider replaces the default.
	exec(`UPDATE afs.teams SET identity_provider_id = $2 WHERE id = $1`, a.ID, chosen)
	if got, _ = src.LoadTeam(t.Context(), a.ID); !slices.Equal(got.Providers, []kubectl.Provider{{ID: chosen}}) {
		t.Errorf("chosen provider: %+v", got.Providers)
	}
	// No default and no choice: no provider, so the team has no role to map.
	exec(`UPDATE afs.teams SET identity_provider_id = NULL WHERE id = $1`, a.ID)
	exec(`UPDATE afs.identity_providers SET is_default = false WHERE id = $1`, svcDefault)
	if got, _ = src.LoadTeam(t.Context(), a.ID); len(got.Providers) != 0 {
		t.Errorf("a team with no provider has %+v", got.Providers)
	}

	if _, err := src.LoadTeam(t.Context(), "team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ"); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown team: %v", err)
	}

	list := func() []string {
		ts, err := src.ListViewerTeams(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, x := range ts {
			ids = append(ids, x.ID)
		}
		return ids
	}
	if slices.Contains(list(), a.ID) {
		t.Error("a team with the flag off is listed")
	}
	exec(`UPDATE afs.teams SET viewer_kubectl = true WHERE id = $1`, a.ID)
	exec(`UPDATE afs.teams SET viewer_kubectl = true, state = 'deleting' WHERE id = $1`, b.ID)
	ids := list()
	if !slices.Contains(ids, a.ID) {
		t.Errorf("a team with the flag on is not listed: %v", ids)
	}
	if slices.Contains(ids, b.ID) {
		t.Errorf("a deleting team is listed: %v", ids)
	}
	if got, _ := src.LoadTeam(t.Context(), a.ID); !got.ViewerKubectl {
		t.Error("flag not read")
	}
	if got, _ := src.LoadTeam(t.Context(), b.ID); got.State != "deleting" {
		t.Errorf("state = %q", got.State)
	}
}
