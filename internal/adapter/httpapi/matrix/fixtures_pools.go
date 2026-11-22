package matrix

import (
	"context"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M5-B (stages, pools and their satellites). Pools exist only in ephemeral teams, and the
// matrix world's team A is persistent, so every Prepare turns team A into an ephemeral team for the
// length of its subtest. The database refuses a change of kind (teams_lifecycle_guard), so the guard
// is switched off around the two updates and back on after each; the cleanup restores team A.

// ephemeralTeamA makes team A ephemeral (teardown rule at_expiry, expiry in 30 days) until the end of t.
func (w *World) ephemeralTeamA(t *testing.T) {
	t.Helper()
	flip := func(ctx context.Context, set string) error {
		for _, q := range []string{
			`ALTER TABLE afs.teams DISABLE TRIGGER teams_lifecycle_guard`,
			`UPDATE afs.teams SET ` + set + ` WHERE id = '` + w.TeamA.ID + `'`,
			`ALTER TABLE afs.teams ENABLE TRIGGER teams_lifecycle_guard`,
		} {
			if _, err := w.Admin.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}
	if err := flip(t.Context(), `kind = 'ephemeral', teardown_rule = 'at_expiry', expires_at = now() + interval '30 days'`); err != nil {
		t.Fatalf("matrix seed: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		for _, q := range []string{
			`DELETE FROM afs.external_system_optouts WHERE pool_id IN (SELECT id FROM afs.agent_pools WHERE team_id = $1)`,
			`DELETE FROM afs.agent_pools WHERE team_id = $1`,
			`DELETE FROM afs.swarm_stages WHERE team_id = $1`,
		} {
			if _, err := w.Admin.Exec(ctx, q, w.TeamA.ID); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
		if err := flip(ctx, `kind = 'persistent', teardown_rule = NULL, expires_at = NULL`); err != nil {
			t.Errorf("matrix cleanup: %v", err)
		}
	})
}

// poolCatalog seeds the model and template and, registered after them so it runs first, removes team
// A's pools before the cleanup of the catalog needs the model free.
func (w *World) poolCatalog(t *testing.T) (model, tpl string) {
	t.Helper()
	model, tpl = w.SeedAgentCatalog(t)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		for _, q := range []string{
			`DELETE FROM afs.external_system_optouts WHERE pool_id IN (SELECT id FROM afs.agent_pools WHERE model_id = $1)`,
			`DELETE FROM afs.agent_pools WHERE model_id = $1`,
		} {
			if _, err := w.Admin.Exec(ctx, q, model); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
	})
	return model, tpl
}

// seedPool inserts a pool of team A (in an ephemeral team A) and returns the placeholders a case needs.
func (w *World) seedPool(t *testing.T) map[string]string {
	t.Helper()
	w.ephemeralTeamA(t)
	model, tpl := w.poolCatalog(t)
	id := pgtest.NewID("pool")
	w.exec(t, `INSERT INTO afs.agent_pools (id, team_id, name, template, image, model_id, workers) VALUES ($1, $2, $3, $4, $5, $6, 1)`,
		id, w.TeamA.ID, "mx-"+strings.ToLower(unique()), tpl, mxImage, model)
	return map[string]string{"poolId": id, "model": model, "tpl": tpl,
		"ver": ver(t, w, `SELECT version FROM afs.agent_pools WHERE id = $1`, id)}
}

func poolPrep(t *testing.T, w *World) map[string]string { return w.seedPool(t) }

func (w *World) seedStage(t *testing.T) map[string]string {
	t.Helper()
	w.ephemeralTeamA(t)
	id := pgtest.NewID("stg")
	w.exec(t, `INSERT INTO afs.swarm_stages (id, team_id, name, position) VALUES ($1, $2, $3, 9)`, id, w.TeamA.ID, "mx-"+strings.ToLower(unique()))
	return map[string]string{"stageId": id, "ver": ver(t, w, `SELECT version FROM afs.swarm_stages WHERE id = $1`, id)}
}

func init() {
	ifm := map[string]string{"If-Match": ifMatch}
	// Stages: the contract has no event for them (CD-7).
	Register(Case{Op: "ListStages", Path: "/v1/teams/{teamId}/stages", Prepare: func(t *testing.T, w *World) map[string]string { return w.seedStage(t) }})
	Register(Case{Op: "CreateStage", Path: "/v1/teams/{teamId}/stages", Body: `{"name":"mx-{n}","position":7}`, NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.ephemeralTeamA(t)
			return map[string]string{"n": strings.ToLower(unique())}
		}})
	Register(Case{Op: "UpdateStage", Path: "/v1/teams/{teamId}/stages/{stageId}", Body: `{"description":"matrix"}`, Headers: ifm, NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedStage(t) }})
	Register(Case{Op: "DeleteStage", Path: "/v1/teams/{teamId}/stages/{stageId}", Headers: ifm, NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string { return w.seedStage(t) }})

	// Pools.
	Register(Case{Op: "ListPools", Path: "/v1/teams/{teamId}/pools", Prepare: poolPrep})
	Register(Case{Op: "GetPool", Path: "/v1/teams/{teamId}/pools/{poolId}", Prepare: poolPrep})
	Register(Case{Op: "GetPoolStatus", Path: "/v1/teams/{teamId}/pools/{poolId}/status", Prepare: poolPrep})
	Register(Case{Op: "CreatePool", Path: "/v1/teams/{teamId}/pools",
		Body: `{"name":"mx-{n}","template":"{tpl}","image":"` + mxImage + `","modelId":"{model}","workers":1}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.ephemeralTeamA(t)
			model, tpl := w.poolCatalog(t)
			return map[string]string{"n": strings.ToLower(unique()), "model": model, "tpl": tpl}
		}})
	Register(Case{Op: "UpdatePool", Path: "/v1/teams/{teamId}/pools/{poolId}", Body: `{"description":"matrix"}`, Headers: ifm, Prepare: poolPrep})
	Register(Case{Op: "DeletePool", Path: "/v1/teams/{teamId}/pools/{poolId}", Headers: ifm, Prepare: poolPrep})
	Register(Case{Op: "ScalePool", Path: "/v1/teams/{teamId}/pools/{poolId}:scale", Body: `{"workers":3}`, Prepare: poolPrep})
	Register(Case{Op: "PausePool", Path: "/v1/teams/{teamId}/pools/{poolId}:pause", Prepare: poolPrep})
	Register(Case{Op: "ResumePool", Path: "/v1/teams/{teamId}/pools/{poolId}:resume",
		Prepare: func(t *testing.T, w *World) map[string]string {
			p := w.seedPool(t)
			w.exec(t, `UPDATE afs.agent_pools SET state = 'paused' WHERE id = $1`, p["poolId"])
			return p
		}})

	// Handoffs and secret bindings (pool.updated).
	Register(Case{Op: "ListPoolHandoffs", Path: "/v1/teams/{teamId}/pools/{poolId}/handoffs", Prepare: poolPrep})
	Register(Case{Op: "PutPoolHandoffs", Path: "/v1/teams/{teamId}/pools/{poolId}/handoffs", Body: `{"toPoolIds":["{otherPool}"]}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			p := w.seedPool(t)
			other := pgtest.NewID("pool")
			w.exec(t, `INSERT INTO afs.agent_pools (id, team_id, name, template, image, model_id, workers) VALUES ($1, $2, $3, $4, $5, $6, 1)`,
				other, w.TeamA.ID, "mx-"+strings.ToLower(unique()), p["tpl"], mxImage, p["model"])
			p["otherPool"] = other
			return p
		}})
	Register(Case{Op: "ListPoolSecretBindings", Path: "/v1/teams/{teamId}/pools/{poolId}/secret-bindings", Prepare: poolPrep})
	Register(Case{Op: "PutPoolSecretBindings", Path: "/v1/teams/{teamId}/pools/{poolId}/secret-bindings", Body: `{"items":[]}`, Prepare: poolPrep})

	// Configuration, external systems and role links: no event in the contract (CD-7).
	Register(Case{Op: "ListPoolConfigValues", Path: "/v1/teams/{teamId}/pools/{poolId}/config-values", Prepare: poolPrep})
	Register(Case{Op: "PutPoolConfigValue", Path: "/v1/teams/{teamId}/pools/{poolId}/config-values/MX_PUT", Body: `{"source":"literal","value":"matrix"}`,
		NoOutbox: cdOutbox, Prepare: poolPrep})
	Register(Case{Op: "DeletePoolConfigValue", Path: "/v1/teams/{teamId}/pools/{poolId}/config-values/{name}", Headers: ifm, NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			p, name := w.seedPool(t), cfgName()
			w.exec(t, `INSERT INTO afs.pool_config_values (id, team_id, pool_id, name, source, config_value) VALUES ($1, $2, $3, $4, 'literal', 'x')`,
				pgtest.NewID("pcv"), w.TeamA.ID, p["poolId"], name)
			p["name"] = name
			p["ver"] = ver(t, w, `SELECT version FROM afs.pool_config_values WHERE pool_id = $1 AND name = $2`, p["poolId"], name)
			return p
		}})
	Register(Case{Op: "ListPoolExternalSystems", Path: "/v1/teams/{teamId}/pools/{poolId}/external-systems", Prepare: poolPrep})
	Register(Case{Op: "PutPoolExternalSystem", Path: "/v1/teams/{teamId}/pools/{poolId}/external-systems/{systemId}", Body: `{"enabled":false}`,
		NoOutbox: cdOutbox, Prepare: func(t *testing.T, w *World) map[string]string {
			p := w.seedPool(t)
			p["systemId"], _ = w.seedCatalogSystem(t)
			return p
		}})
	linkReady := func(t *testing.T, w *World) map[string]string {
		p := w.seedPool(t)
		w.exec(t, `INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ($1, 'mx-account', 'active') ON CONFLICT (id) DO UPDATE SET status = 'active'`, mxAccount)
		w.exec(t, `UPDATE afs.teams SET allowed_aws_accounts = ARRAY['*'] WHERE id = $1`, w.TeamA.ID)
		return p
	}
	seedLink := func(t *testing.T, w *World) map[string]string {
		p := linkReady(t, w)
		id := pgtest.NewID("prl")
		w.exec(t, `INSERT INTO afs.pool_role_links (id, team_id, pool_id, account_id, role_name, profile_name, external_id)
VALUES ($1, $2, $3, $4, 'mx-role', 'mx-prof', $5)`, id, w.TeamA.ID, p["poolId"], mxAccount, "afs-"+strings.Repeat("a", 20)+strings.ToLower(unique()))
		p["linkId"] = id
		p["ver"] = ver(t, w, `SELECT version FROM afs.pool_role_links WHERE id = $1`, id)
		return p
	}
	Register(Case{Op: "ListPoolRoleLinks", Path: "/v1/teams/{teamId}/pools/{poolId}/role-links", Prepare: poolPrep})
	Register(Case{Op: "CreatePoolRoleLink", Path: "/v1/teams/{teamId}/pools/{poolId}/role-links", NoOutbox: cdOutbox,
		Body: `{"roleName":"mx-role","accountId":"` + mxAccount + `","profileName":"mx-{n}"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			p := linkReady(t, w)
			p["n"] = strings.ToLower(unique())
			return p
		}})
	Register(Case{Op: "DeletePoolRoleLink", Path: "/v1/teams/{teamId}/pools/{poolId}/role-links/{linkId}", Headers: ifm, NoOutbox: cdOutbox, Prepare: seedLink})
	Register(Case{Op: "VerifyPoolRoleLink", Path: "/v1/teams/{teamId}/pools/{poolId}/role-links/{linkId}:verify", NoOutbox: cdOutbox, Prepare: seedLink})
}
