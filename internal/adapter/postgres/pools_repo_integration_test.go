package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// M5-B: pool rows, stages, handoffs, bindings, configuration, external system switches and role links
// against the real schema (000012-000015): the guards, the unique keys, the generated role ARN and the
// team scoping.
func TestPoolRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewPoolRepo()
	secrets := postgres.NewSecretRepo()
	admin := db.Admin(t)
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.models (id, provider, display_name) VALUES ('claude-sonnet', 'Anthropic', 'Claude Sonnet') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	swarm, _ := seedSwarm(t, db)
	other, _ := seedSwarm(t, db)
	persistent := db.SeedTeam(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'claude-sonnet') ON CONFLICT DO NOTHING`, persistent.ID)
	tplName := "tpl-" + lower(pgtest.NewID("x")[2:8])
	tplID := seedTemplateForAgents(t, db, tplName)
	now := time.Now().UTC()
	img := "registry.example.com/agents/coder@" + testDigest
	run := func(team string, fn func(ctx context.Context, tx port.Tx)) error {
		t.Helper()
		return inTx(t, db, teamSession(team), func(ctx context.Context, tx port.Tx) error { fn(ctx, tx); return nil })
	}
	newPool := func(team, name string) domain.Pool {
		t.Helper()
		p, err := domain.NewPool(pgtest.NewID("pool"), team, domain.NewPoolParams{Name: name, Template: tplName, Image: img, ModelID: "claude-sonnet", Workers: 2}, now)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Catalog reads, kinds and the shared agent/pool name space.
	seedAgent(t, db, swarm, "taken")
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if k, err := repo.TeamKind(ctx, tx, swarm); err != nil || k != domain.TeamKindEphemeral {
			t.Errorf("TeamKind: %v %v", k, err)
		}
		if _, err := repo.TeamKind(ctx, tx, persistent.ID); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("another team's kind is hidden: %v", err)
		}
		if sets, err := repo.ModelSets(ctx, tx, swarm); err != nil || !slices.Contains(sets.Team, "claude-sonnet") || slices.Contains(sets.Team, "nope") {
			t.Errorf("ModelSets: %+v %v", sets, err)
		}
		if tpl, ok, err := repo.Template(ctx, tx, tplName); err != nil || !ok || tpl.ID != tplID || !tpl.Enabled {
			t.Errorf("Template: %+v %v %v", tpl, ok, err)
		}
		if _, ok, _ := repo.Template(ctx, tx, "missing"); ok {
			t.Error("missing template found")
		}
		if ok, err := repo.AgentNameTaken(ctx, tx, swarm, "taken"); err != nil || !ok {
			t.Errorf("AgentNameTaken: %v %v", ok, err)
		}
		if ok, _ := repo.AgentNameTaken(ctx, tx, swarm, "free"); ok {
			t.Error("free name taken")
		}
	}); err != nil {
		t.Fatal(err)
	}

	// The database refuses a pool named like an agent (the use case checks first) and in a persistent team.
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {}); err != nil {
		t.Fatal(err)
	}
	err := inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.Insert(ctx, tx, newPool(swarm, "taken"), tplID)
	})
	wantKind(t, err, domain.KindConflict)
	err = inTx(t, db, teamSession(persistent.ID), func(ctx context.Context, tx port.Tx) error {
		return repo.Insert(ctx, tx, newPool(persistent.ID, "fixer"), tplID)
	})
	wantKind(t, err, domain.KindValidation)

	// Create, read, list, update, touch.
	p := newPool(swarm, "fixer")
	st, err := domain.NewStage(pgtest.NewID("stg"), swarm, domain.NewStageParams{Name: "triage", Position: 1, Description: "first"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if err := repo.InsertStage(ctx, tx, st); err != nil {
			t.Fatal(err)
		}
		p.StageID = st.ID
		if err := repo.Insert(ctx, tx, p, tplID); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	var cur domain.Pool
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		got, err := repo.Get(ctx, tx, swarm, p.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		cur = got
		if got.Name != "fixer" || got.Workers != 2 || got.StageID != st.ID || got.State != domain.PoolStateEnabled || got.Generation != 1 || got.Version != 1 ||
			got.ScratchSizeGiB != domain.DefaultScratchGiB || got.SharedVolume != "none" || got.Status.Phase != domain.PhasePending || got.Handoffs == nil {
			t.Errorf("pool: %+v", got)
		}
		rows, err := repo.List(ctx, tx, swarm, "", 10)
		if err != nil || len(rows) < 1 {
			t.Errorf("list: %v %v", rows, err)
		}
		found := false
		for _, r := range rows {
			found = found || r.ID == p.ID
		}
		if !found {
			t.Error("the pool is not listed")
		}
		if rows, _ = repo.List(ctx, tx, swarm, p.ID, 10); len(rows) != 0 && rows[0].ID <= p.ID {
			t.Errorf("cursor: %v", rows)
		}
	}); err != nil {
		t.Fatal(err)
	}
	next, err := cur.Apply(domain.PoolPatch{Workers: new(int)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if err := repo.Update(ctx, tx, next, "", false, 99); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale update: %v", err)
		}
		if err := repo.Update(ctx, tx, next, "", false, cur.Version); err != nil {
			t.Fatal(err)
		}
		got, _ := repo.Get(ctx, tx, swarm, p.ID, false)
		if got.Workers != 0 || got.Version != 2 || got.Generation != 2 {
			t.Errorf("after update: %+v", got)
		}
		if err := repo.Touch(ctx, tx, swarm, []string{p.ID}); err != nil {
			t.Fatal(err)
		}
		got, _ = repo.Get(ctx, tx, swarm, p.ID, false)
		if got.Generation != 3 || got.Version != 3 {
			t.Errorf("after touch: %+v", got)
		}
	}); err != nil {
		t.Fatal(err)
	}
	// A template change goes through the catalog guard: a digest the template does not allow is refused.
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		got, err := repo.Get(ctx, tx, swarm, p.ID, true)
		if err != nil {
			return err
		}
		got.Image = domain.ImageRef("registry.example.com/agents/coder@sha256:" + strings.Repeat("b", 64))
		return repo.Update(ctx, tx, got, tplID, true, got.Version)
	}), domain.KindValidation)
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		w, err := repo.Workers(ctx, tx, swarm, p.ID)
		if err != nil || w.Desired != 0 || w.Reporting != 0 {
			t.Errorf("workers: %+v %v", w, err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	// Stages: unique name and position, update, restrict while a pool names it.
	st2, _ := domain.NewStage(pgtest.NewID("stg"), swarm, domain.NewStageParams{Name: "triage", Position: 2}, now)
	st3, _ := domain.NewStage(pgtest.NewID("stg"), swarm, domain.NewStageParams{Name: "fix", Position: 1}, now)
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error { return repo.InsertStage(ctx, tx, st2) }), domain.KindConflict)
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error { return repo.InsertStage(ctx, tx, st3) }), domain.KindConflict)
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		rows, err := repo.ListStages(ctx, tx, swarm, "", 10)
		if err != nil || len(rows) != 1 || rows[0].ID != st.ID {
			t.Errorf("ListStages: %v %v", rows, err)
		}
		got, err := repo.GetStage(ctx, tx, swarm, st.ID, true)
		if err != nil || got.Description != "first" {
			t.Fatalf("GetStage: %+v %v", got, err)
		}
		got.Position = 4
		if err := repo.UpdateStage(ctx, tx, got, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale stage update: %v", err)
		}
		if err := repo.UpdateStage(ctx, tx, domain.Stage{ID: "stg_missing", TeamID: swarm}, 1); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("missing stage update: %v", err)
		}
		if err := repo.UpdateStage(ctx, tx, got, got.Version); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error { return repo.DeleteStage(ctx, tx, swarm, st.ID, 2) }), domain.KindConflict)

	// Handoffs.
	q := newPool(swarm, "reviewer")
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if err := repo.Insert(ctx, tx, q, tplID); err != nil {
			t.Fatal(err)
		}
		if n, err := repo.PoolsExist(ctx, tx, swarm, []string{p.ID, q.ID, "pool_missing"}); err != nil || n != 2 {
			t.Errorf("PoolsExist: %d %v", n, err)
		}
		if err := repo.ReplaceHandoffs(ctx, tx, swarm, p.ID, []string{q.ID}); err != nil {
			t.Fatal(err)
		}
		if to, err := repo.Handoffs(ctx, tx, swarm, p.ID); err != nil || len(to) != 1 || to[0] != q.ID {
			t.Errorf("Handoffs: %v %v", to, err)
		}
		got, _ := repo.Get(ctx, tx, swarm, p.ID, false)
		if len(got.Handoffs) != 1 {
			t.Errorf("handoffs on the pool: %+v", got.Handoffs)
		}
		if err := repo.ReplaceHandoffs(ctx, tx, swarm, p.ID, nil); err != nil {
			t.Fatal(err)
		}
		if to, _ := repo.Handoffs(ctx, tx, swarm, p.ID); len(to) != 0 {
			t.Errorf("cleared: %v", to)
		}
	}); err != nil {
		t.Fatal(err)
	}
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.ReplaceHandoffs(ctx, tx, swarm, p.ID, []string{p.ID})
	}), domain.KindValidation)

	// Secrets and bindings: a pool binds team secrets or its own, never an agent's or another pool's.
	var shared, own, foreign domain.Secret
	mk := func(tx port.Tx, ctx context.Context, poolID, name string) domain.Secret {
		path, err := secrets.Path(ctx, tx, swarm, "", poolID, name)
		if err != nil {
			t.Fatal(err)
		}
		s := domain.Secret{ID: pgtest.NewID("sec"), TeamID: swarm, PoolID: poolID, Name: name, ARN: arnFor(path), VersionID: "v1"}
		if err := secrets.Insert(ctx, tx, s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		shared, own, foreign = mk(tx, ctx, "", "shared-key"), mk(tx, ctx, p.ID, "own-key"), mk(tx, ctx, q.ID, "other-key")
		owners, err := repo.Secrets(ctx, tx, swarm, []string{shared.ID, own.ID, "sec_missing"})
		if err != nil || len(owners) != 2 || owners[own.ID].PoolID != p.ID || owners[shared.ID].PoolID != "" {
			t.Errorf("owners: %+v %v", owners, err)
		}
		bs := []domain.SecretBinding{{SecretID: shared.ID, Delivery: "env", Target: "TOKEN"}, {SecretID: own.ID, Delivery: "file", Target: "/etc/pool/key"}}
		if err := repo.ReplaceBindings(ctx, tx, swarm, p.ID, bs); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Bindings(ctx, tx, swarm, p.ID)
		if err != nil || len(got) != 2 {
			t.Errorf("Bindings: %v %v", got, err)
		}
		bound, err := secrets.Get(ctx, tx, swarm, shared.ID, false)
		if err != nil || len(bound.BoundTo) != 1 || bound.BoundTo[0] != p.ID {
			t.Errorf("a secret reports the pools that use it: %+v %v", bound.BoundTo, err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.ReplaceBindings(ctx, tx, swarm, p.ID, []domain.SecretBinding{{SecretID: foreign.ID, Delivery: "env", Target: "TOKEN"}})
	}), domain.KindValidation)

	// Configuration: literal and namespace link, resolved; a link needs the namespace value.
	exec(`INSERT INTO afs.namespace_config_values (id, team_id, name, config_value) VALUES ($1, $2, 'REGION', 'eu-west-1')`, pgtest.NewID("ncv"), swarm)
	lv := "high"
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if ok, err := repo.NamespaceValueExists(ctx, tx, swarm, "REGION"); err != nil || !ok {
			t.Errorf("NamespaceValueExists: %v %v", ok, err)
		}
		if ok, _ := repo.NamespaceValueExists(ctx, tx, swarm, "GONE"); ok {
			t.Error("missing namespace value exists")
		}
		for _, v := range []domain.PoolConfigValue{
			{ID: pgtest.NewID("pcv"), TeamID: swarm, PoolID: p.ID, Name: "LEVEL", Source: domain.ConfigSourceLiteral, Value: &lv},
			{ID: pgtest.NewID("pcv"), TeamID: swarm, PoolID: p.ID, Name: "REGION", Source: domain.ConfigSourceNamespace},
		} {
			if err := repo.InsertConfig(ctx, tx, v); err != nil {
				t.Fatal(err)
			}
		}
		rows, err := repo.ListConfig(ctx, tx, swarm, p.ID, "", 10)
		if err != nil || len(rows) != 2 || rows[0].Name != "LEVEL" || *rows[0].ResolvedValue != "high" || *rows[1].ResolvedValue != "eu-west-1" || rows[1].Value != nil {
			t.Errorf("ListConfig: %+v %v", rows, err)
		}
		if rows, _ = repo.ListConfig(ctx, tx, swarm, p.ID, "LEVEL", 10); len(rows) != 1 || rows[0].Name != "REGION" {
			t.Errorf("cursor: %+v", rows)
		}
		v, err := repo.GetConfig(ctx, tx, swarm, p.ID, "LEVEL", true)
		if err != nil {
			t.Fatal(err)
		}
		lo := "low"
		v.Value = &lo
		if err := repo.UpdateConfig(ctx, tx, v, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale config update: %v", err)
		}
		if err := repo.UpdateConfig(ctx, tx, v, v.Version); err != nil {
			t.Fatal(err)
		}
		if got, _ := repo.GetConfig(ctx, tx, swarm, p.ID, "LEVEL", false); *got.ResolvedValue != "low" || got.Version != 2 {
			t.Errorf("after update: %+v", got)
		}
		if err := repo.DeleteConfig(ctx, tx, swarm, p.ID, "LEVEL", 9); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale config delete: %v", err)
		}
		if err := repo.DeleteConfig(ctx, tx, swarm, p.ID, "LEVEL", 2); err != nil {
			t.Fatal(err)
		}
		if err := repo.DeleteConfig(ctx, tx, swarm, p.ID, "LEVEL", 2); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("config delete of a missing entry: %v", err)
		}
		if _, err := repo.GetConfig(ctx, tx, swarm, p.ID, "LEVEL", false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("deleted entry: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.InsertConfig(ctx, tx, domain.PoolConfigValue{ID: pgtest.NewID("pcv"), TeamID: swarm, PoolID: p.ID, Name: "GONE", Source: domain.ConfigSourceNamespace})
	}), domain.KindValidation)
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.InsertConfig(ctx, tx, domain.PoolConfigValue{ID: pgtest.NewID("pcv"), TeamID: swarm, PoolID: p.ID, Name: "TOKEN", Source: domain.ConfigSourceLiteral, Value: &lv})
	}), domain.KindConflict) // TOKEN is delivered to the pool as a secret

	// External systems: a pool's switch is reported at the agent level, the contract's enumeration has no pool.
	sys := pgtest.NewID("ext")
	exec(`INSERT INTO afs.external_systems (id, slug, name, category, endpoints) VALUES ($1, $2, 'Pool system', 'other', ARRAY['pool.example.com'])`, sys, "pool-"+lower(sys[len(sys)-8:]))
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		rows, err := repo.ListSystems(ctx, tx, swarm, p.ID, "", 500)
		found := false
		for _, r := range rows {
			found = found || (r.ID == sys && r.On())
		}
		if err != nil || !found {
			t.Fatalf("ListSystems: %v (found %v)", err, found)
		}
		if err := repo.SetSystemOff(ctx, tx, swarm, p.ID, sys, pgtest.NewID("xso"), true); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetSystemOff(ctx, tx, swarm, p.ID, sys, pgtest.NewID("xso"), true); err != nil {
			t.Fatalf("switching off twice is idempotent: %v", err)
		}
		got, err := repo.GetSystem(ctx, tx, swarm, p.ID, sys)
		if err != nil || got.DisabledAt != domain.ExtSystemDisabledAtPool {
			t.Errorf("off: %+v %v", got, err)
		}
		if other, _ := repo.GetSystem(ctx, tx, swarm, q.ID, sys); !other.On() {
			t.Error("another pool's switch changed")
		}
		if err := repo.SetSystemOff(ctx, tx, swarm, p.ID, sys, "", false); err != nil {
			t.Fatal(err)
		}
		if got, _ = repo.GetSystem(ctx, tx, swarm, p.ID, sys); !got.On() {
			t.Errorf("on again: %+v", got)
		}
		if _, err := repo.GetSystem(ctx, tx, swarm, p.ID, "ext_missing"); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("missing system: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	// Role links: one role name per pool, one link per account and profile, the ARN is generated.
	exec(`INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ('111122223333', 'pool-a', 'active'), ('444455556666', 'pool-b', 'active') ON CONFLICT (id) DO UPDATE SET status = 'active'`)
	exec(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['111122223333'] WHERE id = $1`, swarm)
	link := func(role, acct, profile string) domain.PoolRoleLink {
		l, err := domain.NewPoolRoleLink(pgtest.NewID("prl"), swarm, p.ID, "afs-"+strings.Repeat("a", 20)+lower(pgtest.NewID("x")[2:10]),
			domain.NewPoolRoleLinkParams{RoleName: role, AccountID: acct, ProfileName: profile}, now)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	first := link("shared", "111122223333", "one")
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if ok, err := repo.AccountAllowed(ctx, tx, swarm, "111122223333"); err != nil || !ok {
			t.Errorf("AccountAllowed: %v %v", ok, err)
		}
		if ok, _ := repo.AccountAllowed(ctx, tx, swarm, "444455556666"); ok {
			t.Error("an account outside the filter is allowed")
		}
		if role, err := repo.LinkRoleName(ctx, tx, swarm, p.ID); err != nil || role != "" {
			t.Errorf("no links yet: %q %v", role, err)
		}
		if err := repo.InsertLink(ctx, tx, first); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetLink(ctx, tx, swarm, p.ID, first.ID, true)
		if err != nil || got.RoleARN != "arn:aws:iam::111122223333:role/shared" || got.Status != domain.RoleLinkStatusPendingTrust || got.RoleName != "shared" {
			t.Errorf("link: %+v %v", got, err)
		}
		if role, _ := repo.LinkRoleName(ctx, tx, swarm, p.ID); role != "shared" {
			t.Errorf("role name: %q", role)
		}
		if _, err := repo.GetLink(ctx, tx, swarm, q.ID, first.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("a link of another pool: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['111122223333', '444455556666'] WHERE id = $1`, swarm)
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.InsertLink(ctx, tx, link("another", "444455556666", "two"))
	}), domain.KindValidation) // a pool uses one role name in every account
	wantKind(t, inTx(t, db, teamSession(swarm), func(ctx context.Context, tx port.Tx) error {
		return repo.InsertLink(ctx, tx, link("shared", "111122223333", "three"))
	}), domain.KindConflict) // one link per account
	exec(`UPDATE afs.agent_pools SET state = state WHERE id = $1`, p.ID)
	exec(`UPDATE afs.pool_role_links SET status = 'failed', last_error = 'trust missing' WHERE id = $1`, first.ID)
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		rows, err := repo.ListLinks(ctx, tx, swarm, p.ID, "", 10)
		if err != nil || len(rows) != 1 || rows[0].LastError != "trust missing" {
			t.Errorf("ListLinks: %+v %v", rows, err)
		}
		l, err := repo.RequestVerify(ctx, tx, swarm, p.ID, first.ID)
		if err != nil || l.Status != domain.RoleLinkStatusPendingTrust || l.LastError != "" {
			t.Errorf("RequestVerify: %+v %v", l, err)
		}
		if err := repo.DeleteLink(ctx, tx, swarm, p.ID, first.ID, 99); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale link delete: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		l, _ := repo.GetLink(ctx, tx, swarm, p.ID, first.ID, false)
		if err := repo.DeleteLink(ctx, tx, swarm, p.ID, first.ID, l.Version); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	// Stages can go once no pool names them.
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		cur, _ := repo.Get(ctx, tx, swarm, p.ID, true)
		cur.StageID = ""
		if err := repo.Update(ctx, tx, cur, "", false, cur.Version); err != nil {
			t.Fatal(err)
		}
		s, _ := repo.GetStage(ctx, tx, swarm, st.ID, false)
		if err := repo.DeleteStage(ctx, tx, swarm, st.ID, 99); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale stage delete: %v", err)
		}
		if err := repo.DeleteStage(ctx, tx, swarm, st.ID, s.Version); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	// Team scoping: another team sees none of it; a deleted pool is invisible.
	if err := run(other, func(ctx context.Context, tx port.Tx) {
		if _, err := repo.Get(ctx, tx, swarm, p.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("another team's pool: %v", err)
		}
		if rows, _ := repo.List(ctx, tx, swarm, "", 10); len(rows) != 0 {
			t.Errorf("another team's list: %v", rows)
		}
	}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE afs.agent_pools SET state = 'deleted', deleted_at = now() WHERE id = $1`, q.ID)
	if err := run(swarm, func(ctx context.Context, tx port.Tx) {
		if _, err := repo.Get(ctx, tx, swarm, q.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("deleted pool: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
}
