package postgres_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// MEG-API-5: the route members of an agent (pools share the same helpers) are stored, read back with the effective routes and
// the route environment, and checked against the team's route set.
func TestAgentRoutesAgainstTheSchema(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	model := seedModel(t, db, tm.ID)
	tplName := "tpl-" + lower(pgtest.NewID("x")[2:8])
	tplID := seedTemplateForAgents(t, db, tplName)
	admin := db.Admin(t)
	ctx := t.Context()

	// Two free service endpoints; the team opts in to both; one model each.
	slugs := []string{mepSlug("ra"), mepSlug("rb")}
	for _, s := range slugs {
		id := pgtest.NewID("mep")
		mustExec(t, admin, `INSERT INTO afs.model_endpoints (id, slug, display_name, location, base_url, auth_kind, pricing_mode, enabled)
VALUES ($1, $2, 'Routes', 'external', 'https://llm.example.com/v1', 'none', 'free', true)`, id, s)
		mustExec(t, admin, `INSERT INTO afs.endpoint_models (endpoint_id, model_name, max_output_tokens) VALUES ($1, 'm1', 100)`, id)
		mustExec(t, admin, `INSERT INTO afs.team_model_endpoints (team_id, endpoint_id) VALUES ($1, $2)`, tm.ID, id)
	}

	repo := postgres.NewAgentRepo()
	var sets domain.ModelSets
	if err := inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		var err error
		sets, err = repo.ModelSets(ctx, tx, tm.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range slugs {
		if !slices.Equal(sets.Routes[s], []string{"m1"}) {
			t.Fatalf("team routes = %v", sets.Routes)
		}
	}
	def := slugs[1] + "/m1"
	sel := []string{slugs[1]}
	m, err := domain.ResolveNewModels(&domain.ModelsInput{DefaultModelID: &def, Routes: &domain.RoutesInput{Routes: &sel}}, "", sets)
	if err != nil {
		t.Fatal(err)
	}
	a, err := domain.NewAgent(pgtest.NewID("agt"), tm.ID, domain.NewAgentParams{Name: "router", Template: tplName,
		Image: "registry.example.com/agents/coder@" + testDigest, ModelID: model, Resources: domain.AgentResources{CPU: "500m", Memory: "1Gi"}}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	a.Models = m
	a.ModelID = m.DefaultRef()
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
		if err := repo.Insert(ctx, tx, a, tplID); err != nil {
			return err
		}
		got, err := repo.Get(ctx, tx, tm.ID, a.ID, true)
		if err != nil {
			return err
		}
		list := []domain.Agent{got}
		if err := repo.EffectiveModels(ctx, tx, tm.ID, list); err != nil {
			return err
		}
		g := list[0].Models
		if g.RouteAccess != domain.ModelAccessSelected || !slices.Equal(g.Routes, sel) || g.DefaultRoute != slugs[1] ||
			g.DefaultModelID != "m1" || list[0].ModelID != def || !slices.Equal(g.EffectiveRoutes, sel) {
			t.Errorf("models = %+v modelId %s", g, list[0].ModelID)
		}
		env := map[string]string{}
		for _, e := range g.Env {
			env[e.Name] = e.Value
		}
		if env["AFS_LLM_ROUTES"] != slugs[1] || env["AFS_LLM_DEFAULT_ROUTE"] != slugs[1] || env["AFS_LLM_DEFAULT_MODEL"] != "m1" ||
			env["OPENAI_API_KEY"] != domain.GatewayPlaceholderKey || env[domain.RouteEnvName(slugs[1])] != domain.RouteBaseURL(slugs[1]) {
			t.Errorf("env = %v", env)
		}
		// Back to all routes with a Bedrock default: the selection is dropped.
		next := got
		next.Models.RouteAccess, next.Models.Routes, next.Models.DefaultRoute, next.Models.DefaultModelID = domain.ModelAccessAll, []string{}, "", model
		next.ModelID = model
		if err := repo.Update(ctx, tx, next, tplID, false, got.Version); err != nil {
			return err
		}
		got, err = repo.Get(ctx, tx, tm.ID, a.ID, true)
		if err != nil {
			return err
		}
		list = []domain.Agent{got}
		if err := repo.EffectiveModels(ctx, tx, tm.ID, list); err != nil {
			return err
		}
		if list[0].Models.RouteAccess != domain.ModelAccessAll || len(list[0].Models.Routes) != 0 || list[0].Models.DefaultRoute != "" ||
			list[0].ModelID != model || len(list[0].Models.EffectiveRoutes) != 2 {
			t.Errorf("after update: %+v", list[0].Models)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.agent_model_endpoints WHERE agent_id = $1`, a.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("selections = %d %v", n, err)
	}

	// A default route outside the selection is refused by the database too.
	bad := cloneAgent(a)
	bad.ID = pgtest.NewID("agt")
	bad.Name = "router-two"
	bad.Models.Routes = []string{slugs[0]}
	err = inTx(t, db, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error { return repo.Insert(ctx, tx, bad, tplID) })
	if domain.KindOf(err) != domain.KindValidation {
		t.Errorf("default route outside selection: %v", err)
	}
}

func cloneAgent(a domain.Agent) domain.Agent {
	c := a
	c.Models.Routes = slices.Clone(a.Models.Routes)
	return c
}

func mustExec(t *testing.T, conn *pgx.Conn, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), q, args...); err != nil {
		t.Fatal(err)
	}
}
