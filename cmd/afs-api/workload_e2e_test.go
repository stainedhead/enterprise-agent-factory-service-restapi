package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// workloadTeam is an ephemeral team with one pool and one agent, the namespace they run in and the
// ids behind their ServiceAccounts.
type workloadTeam struct {
	id, namespace string
	poolID, agent string
}

// seedWorkloadTeam inserts an ephemeral team (pools exist only there) with the pool triage and the agent
// writer, on a model enabled for it.
func (g secopsRig) seedWorkloadTeam(t *testing.T) workloadTeam {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := g.w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	id, slug := pgtest.NewID("team"), "w"+strings.ToLower(pgtest.NewID("x")[2:10])
	// An ephemeral team needs its lifetime budget in the same transaction (a deferred constraint).
	exec(`WITH t AS (INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd, kind, teardown_rule, expires_at)
VALUES ($1, $2, $2, 'CC100', 'owner@example.com', 1000, 'ephemeral', 'at_expiry', now() + interval '30 days') RETURNING id)
INSERT INTO afs.budgets (id, team_id, period, amount_usd, on_exhaust) SELECT $3, id, 'lifetime', 100, 'block' FROM t`, id, slug, pgtest.NewID("bud"))
	model, tpl := "wl.model-"+slug, "wl-"+slug
	digest := "sha256:" + strings.Repeat("4f", 32)
	exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'Workload', 4096)`, model)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2)`, id, model)
	exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`,
		pgtest.NewID("tpl"), tpl, digest)
	wt := workloadTeam{id: id, namespace: "afs-t-" + slug, poolID: pgtest.NewID("pool"), agent: pgtest.NewID("agt")}
	image := "registry.example.com/agents/coder@" + digest
	exec(`INSERT INTO afs.agent_pools (id, team_id, name, template, image, model_id, workers) VALUES ($1, $2, 'triage', $3, $4, $5, 2)`, wt.poolID, id, tpl, image, model)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, 'writer', $3, $4, $5)`, wt.agent, id, tpl, image, model)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM afs.work_order_repos WHERE work_order_id IN (SELECT id FROM afs.work_orders WHERE team_id = $1)`,
			`DELETE FROM afs.work_orders WHERE team_id = $1`,
			`DELETE FROM afs.agent_pools WHERE team_id = $1`, `DELETE FROM afs.agents WHERE team_id = $1`,
		} {
			_, _ = g.w.Admin.Exec(t.Context(), q, id)
		}
	})
	return wt
}

const podName = "pool-triage-5d8c7b9f4-x2k4q"

// RA-US-2b.1, RA-US-2b.2, RA-US-3a.1 (part), M6-D2: a pool's and an agent's projected tokens become
// workload principals; they may call GetMe and nothing else on their own team or on service routes, get
// 404 on another team's routes, and every refusal is audited with the pool and the worker pod.
func TestWorkloadPrincipalEndToEnd(t *testing.T) {
	g := newSecopsRig(t)
	wt := g.seedWorkloadTeam(t)
	pool := g.w.Cluster.ClusterToken(t, wt.namespace, "pool-triage", podName)
	agent := g.w.Cluster.ClusterToken(t, wt.namespace, "agent-writer", "agent-writer-0")

	for name, tc := range map[string]struct{ token, id, typ string }{
		"pool": {pool, wt.poolID, "workload"}, "agent": {agent, wt.agent, "workload"},
	} {
		var me meBody
		if r := g.do(t, "GET", "/v1/me", tc.token, ""); r.code != 200 || json.Unmarshal([]byte(r.body), &me) != nil {
			t.Fatalf("%s GetMe: %d %s", name, r.code, r.body)
		}
		if me.Principal.ID != tc.id || me.Principal.Type != tc.typ || me.ServiceAdmin || me.SecurityOperator || len(me.ServicePermissions) != 0 ||
			len(me.Teams) != 1 || me.Teams[0].TeamID != wt.id || me.Teams[0].Role != "workload" ||
			len(me.Teams[0].Permissions) != 1 || me.Teams[0].Permissions[0] != "work-orders:report" {
			t.Fatalf("%s GetMe: %+v", name, me)
		}
	}

	// 403 on every other route of its own team and on service routes; 404 on any other team's routes.
	own, other := "/v1/teams/"+wt.id, "/v1/teams/"+g.w.TeamB.ID
	for i, tc := range []struct {
		path string
		want int
	}{
		{own, 403}, {own + "/agents", 403}, {own + "/pools", 403}, {own + "/members", 403}, {own + "/api-keys", 403},
		{"/v1/admin/settings", 403}, {"/v1/admin/api-keys", 403}, {"/v1/team-directory", 403}, {"/v1/teams", 403},
		{"/v1/work-orders", 403}, // work-orders:read is not held
		{other, 404}, {other + "/agents", 404}, {other + "/pools", 404}, {other + "/api-keys", 404},
		{"/v1/teams/team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", 404},
	} {
		rid := "wl-" + wt.id[len(wt.id)-6:] + "-" + string(rune('a'+i))
		r := g.do(t, "GET", tc.path, pool, "", "X-Request-Id", rid)
		if r.code != tc.want {
			t.Errorf("pool GET %s: %d, want %d", tc.path, r.code, tc.want)
		}
		var rows int
		_ = g.w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE request_id = $1 AND outcome = 'denied'`, rid).Scan(&rows)
		if (tc.want == 403) != (rows == 1) {
			t.Errorf("pool GET %s: %d denied audit rows for a %d", tc.path, rows, tc.want)
		}
	}
	// The denied record names the pool and the worker pod; an agent's names the agent only.
	var actorType, actorID, poolID, pod string
	err := g.w.Admin.QueryRow(t.Context(), `SELECT actor_type, actor_id, coalesce(pool_id, ''), coalesce(worker_pod, '') FROM afs.audit_log
WHERE request_id = $1 AND outcome = 'denied'`, "wl-"+wt.id[len(wt.id)-6:]+"-a").Scan(&actorType, &actorID, &poolID, &pod)
	if err != nil || actorType != "pool" || actorID != wt.poolID || poolID != wt.poolID || pod != podName {
		t.Fatalf("pool audit: %q %q %q %q %v", actorType, actorID, poolID, pod, err)
	}
	if r := g.do(t, "GET", own, agent, "", "X-Request-Id", "wl-agent-"+wt.id[len(wt.id)-6:]); r.code != 403 {
		t.Fatalf("agent on its team: %d", r.code)
	}
	err = g.w.Admin.QueryRow(t.Context(), `SELECT actor_type, actor_id, coalesce(pool_id, ''), coalesce(worker_pod, '') FROM afs.audit_log
WHERE request_id = $1 AND outcome = 'denied'`, "wl-agent-"+wt.id[len(wt.id)-6:]).Scan(&actorType, &actorID, &poolID, &pod)
	if err != nil || actorType != "agent" || actorID != wt.agent || poolID != "" || pod != "" {
		t.Fatalf("agent audit: %q %q %q %q %v", actorType, actorID, poolID, pod, err)
	}

	// Mutations: nothing the workload may reach changes anything, whatever it sends.
	if r := g.do(t, "POST", own+"/pools/"+wt.poolID+":pause", pool, "", "Idempotency-Key", "wl-pause"); r.code != 403 {
		t.Fatalf("pool pausing itself: %d", r.code)
	}
	if r := g.do(t, "DELETE", own, pool, "", "If-Match", `"1"`); r.code != 403 {
		t.Fatalf("pool deleting its team: %d", r.code)
	}

	// A suspended team's workloads no longer authenticate.
	if _, err := g.w.Admin.Exec(t.Context(), `UPDATE afs.teams SET state = 'suspended' WHERE id = $1`, wt.id); err != nil {
		t.Fatal(err)
	}
	if r := g.do(t, "GET", "/v1/me", pool, ""); r.code != 401 {
		t.Fatalf("suspended team: %d", r.code)
	}
}

// RA-US-2b.3: tokens the API does not accept are 401, whatever they are for.
func TestWorkloadTokensAreValidated(t *testing.T) {
	g := newSecopsRig(t)
	wt := g.seedWorkloadTeam(t)
	c := g.w.Cluster
	now := time.Now()
	claims := func(extra map[string]any) string {
		base := map[string]any{"iss": c.Server.URL, "sub": "system:serviceaccount:" + wt.namespace + ":pool-triage", "aud": []string{"afs-api"},
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
		for k, v := range extra {
			base[k] = v
		}
		return c.Sign(t, base)
	}
	elsewhere := pgtest.NewID("x") // another namespace: no such pool there
	for name, token := range map[string]string{
		"audience afs-gateway":       claims(map[string]any{"aud": []string{"afs-gateway"}}),
		"api server audience":        claims(map[string]any{"aud": []string{"https://kubernetes.default.svc"}}),
		"expired":                    claims(map[string]any{"iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix()}),
		"two hours of life":          claims(map[string]any{"iat": nil, "exp": now.Add(2 * time.Hour).Unix()}),
		"extended token (one year)":  claims(map[string]any{"iat": now.Unix(), "exp": now.Add(365 * 24 * time.Hour).Unix()}),
		"another cluster's issuer":   g.w.IDP.Sign(t, map[string]any{"iss": g.w.IDP.Server.URL, "sub": "system:serviceaccount:" + wt.namespace + ":pool-triage", "aud": []string{"afs-api"}, "exp": now.Add(time.Hour).Unix()}),
		"no such pool":               claims(map[string]any{"sub": "system:serviceaccount:" + wt.namespace + ":pool-nothing"}),
		"a pool name in a namespace": claims(map[string]any{"sub": "system:serviceaccount:afs-t-" + strings.ToLower(elsewhere[2:8]) + ":pool-triage"}),
		"not a workload service":     claims(map[string]any{"sub": "system:serviceaccount:" + wt.namespace + ":default"}),
		"a node":                     claims(map[string]any{"sub": "system:node:ip-10-0-0-1"}),
	} {
		if r := g.do(t, "GET", "/v1/me", token, ""); r.code != 401 {
			t.Errorf("%s: %d %s, want 401", name, r.code, r.body)
		}
	}
	// The same token is fine when it is right.
	if r := g.do(t, "GET", "/v1/me", claims(nil), ""); r.code != 200 {
		t.Fatalf("a valid token: %d %s", r.code, r.body)
	}
	// A token the cluster did sign for a team that was never created (a pool name of no team).
	if r := g.do(t, "GET", "/v1/me", claims(map[string]any{"sub": "system:serviceaccount:afs-t-nosuchteam:pool-triage"}), ""); r.code != 401 {
		t.Fatalf("unknown namespace: %d", r.code)
	}
}

// RA-US-2b.1, RA-US-2b.2, M6-D3: a pool token of team A reports on a work order assigned to team A, gets
// 404 on one assigned to another team, and cannot read or create work orders. The work order handlers
// are M6-C: until they are bound this test checks authorization only and says so.
func TestWorkloadReportsOnItsAssignedWorkOrder(t *testing.T) {
	g := newSecopsRig(t)
	wt := g.seedWorkloadTeam(t)
	pool := g.w.Cluster.ClusterToken(t, wt.namespace, "pool-triage", podName)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := g.w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	mine, theirs := pgtest.NewID("wo"), pgtest.NewID("wo")
	for _, wo := range []struct{ id, team string }{{mine, wt.id}, {theirs, g.w.TeamB.ID}} {
		exec(`INSERT INTO afs.work_orders (id, team_id, kind, title, status, payload) VALUES ($1, $2, 'general', 'Matrix work order', 'assigned', '{}')`, wo.id, wo.team)
		exec(`INSERT INTO afs.work_order_repos (work_order_id, repo, host, owning_team_id) VALUES ($1, 'acme/app', 'github.com', $2)`, wo.id, wo.team)
	}
	t.Cleanup(func() {
		_, _ = g.w.Admin.Exec(t.Context(), `DELETE FROM afs.work_order_repos WHERE work_order_id = ANY($1)`, []string{mine, theirs})
		_, _ = g.w.Admin.Exec(t.Context(), `DELETE FROM afs.work_orders WHERE id = ANY($1)`, []string{mine, theirs})
	})
	// patch sends the current repository version, as a team admin that can read it would.
	patch := func(id, body string) result {
		v := int64(1) // an unknown work order has no row
		_ = g.w.Admin.QueryRow(t.Context(), `SELECT version FROM afs.work_order_repos WHERE work_order_id = $1`, id).Scan(&v)
		return g.do(t, "PATCH", "/v1/work-orders/"+id+"/repos/github.com/acme/app", pool, body, "If-Match", fmt.Sprintf(`"%d"`, v))
	}

	r := patch(mine, `{"status":"in_progress"}`)
	if r.code != 200 {
		t.Fatalf("pool reports on its team's work order: %d %s", r.code, r.body)
	}
	if got := func() string {
		var s string
		_ = g.w.Admin.QueryRow(t.Context(), `SELECT status FROM afs.work_order_repos WHERE work_order_id = $1`, mine).Scan(&s)
		return s
	}(); got != "in_progress" {
		t.Fatalf("status = %q", got)
	}
	if r = patch(mine, `{"status":"pr_opened"}`); r.code != 400 {
		t.Fatalf("pr_opened without a PR link: %d %s", r.code, r.body)
	}
	if r = patch(mine, `{"status":"pr_opened","prUrl":"https://github.com/acme/app/pull/7","prNumber":7}`); r.code != 200 {
		t.Fatalf("pr_opened: %d %s", r.code, r.body)
	}
	// CLI-CD-1: the workload cannot read the repository version, so it may omit If-Match (current version); a
	// header it does send is still compared (412 when stale) and a malformed one is refused.
	noHeader := func(id, body string) result {
		return g.do(t, "PATCH", "/v1/work-orders/"+id+"/repos/github.com/acme/app", pool, body)
	}
	if r = noHeader(mine, `{"status":"in_progress","detail":"again"}`); r.code != 200 {
		t.Fatalf("workload without If-Match: %d %s", r.code, r.body)
	}
	if r = g.do(t, "PATCH", "/v1/work-orders/"+mine+"/repos/github.com/acme/app", pool, `{"status":"in_progress"}`, "If-Match", `"1"`); r.code != 412 {
		t.Fatalf("a stale If-Match from a workload: %d %s", r.code, r.body)
	}
	if r = g.do(t, "PATCH", "/v1/work-orders/"+mine+"/repos/github.com/acme/app", pool, `{"status":"in_progress"}`, "If-Match", "junk"); r.code != 400 {
		t.Fatalf("a malformed If-Match from a workload: %d %s", r.code, r.body)
	}
	if r = noHeader(theirs, `{"status":"in_progress"}`); r.code != 404 {
		t.Fatalf("no If-Match does not reveal another team's work order: %d %s", r.code, r.body)
	}
	if r = patch(theirs, `{"status":"in_progress"}`); r.code != 404 {
		t.Fatalf("another team's work order: %d %s", r.code, r.body)
	}
	if r = patch("wo_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", `{"status":"in_progress"}`); r.code != 404 {
		t.Fatalf("an unknown work order: %d", r.code)
	}
	for _, p := range []struct{ method, path, body string }{
		{"POST", "/v1/work-orders", `{}`}, {"PATCH", "/v1/work-orders/" + mine, `{}`}, {"POST", "/v1/work-orders/" + mine + ":cancel", ``},
		{"GET", "/v1/work-orders/" + mine + "/repos", ``}, {"GET", "/v1/work-orders/" + mine, ``},
	} {
		if r = g.do(t, p.method, p.path, pool, p.body, "If-Match", `"1"`, "Idempotency-Key", "wl-wo"); r.code != 403 && r.code != 404 {
			t.Errorf("pool %s %s: %d, want 403 or 404", p.method, p.path, r.code)
		}
	}
}

// M6-D3 at the database: the session of a workload principal (its own team in afs.team_ids, nothing
// else) may update the progress of the repositories of its team's work orders and nothing of another
// team's, and may not read or write the work orders themselves (row-level security, 000012).
func TestWorkloadSessionOnWorkOrderRows(t *testing.T) {
	g := newSecopsRig(t)
	wt := g.seedWorkloadTeam(t)
	mine, theirs := pgtest.NewID("wo"), pgtest.NewID("wo")
	for _, wo := range []struct{ id, team string }{{mine, wt.id}, {theirs, g.w.TeamB.ID}} {
		if _, err := g.w.Admin.Exec(t.Context(), `INSERT INTO afs.work_orders (id, team_id, kind, title, status) VALUES ($1, $2, 'general', 'Matrix work order', 'assigned')`, wo.id, wo.team); err != nil {
			t.Fatal(err)
		}
		if _, err := g.w.Admin.Exec(t.Context(), `INSERT INTO afs.work_order_repos (work_order_id, repo, host, owning_team_id) VALUES ($1, 'acme/app', 'github.com', $2)`, wo.id, wo.team); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = g.w.Admin.Exec(t.Context(), `DELETE FROM afs.work_order_repos WHERE work_order_id = ANY($1)`, []string{mine, theirs})
		_, _ = g.w.Admin.Exec(t.Context(), `DELETE FROM afs.work_orders WHERE id = ANY($1)`, []string{mine, theirs})
	})
	app, err := pgx.Connect(t.Context(), g.w.DB.URLs["afs_app"])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(t.Context()) }()
	tx, err := app.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('afs.actor', $1, true), set_config('afs.service_admin', 'off', true),
set_config('afs.team_ids', $2, true), set_config('afs.security_operator', 'off', true)`, wt.poolID, wt.id); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int64{mine: 1, theirs: 0} {
		tag, err := tx.Exec(t.Context(), `UPDATE afs.work_order_repos SET status = 'in_progress', detail = 'working' WHERE work_order_id = $1`, id)
		if err != nil || tag.RowsAffected() != want {
			t.Errorf("progress update of %s: %d rows, want %d (%v)", id, tag.RowsAffected(), want, err)
		}
	}
	var visible int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM afs.work_orders WHERE id = ANY($1)`, []string{mine, theirs}).Scan(&visible); err != nil || visible != 1 {
		t.Errorf("work orders visible: %d (%v), want only its team's", visible, err)
	}
	// The work order itself is not writable, and neither is the repository's owner.
	if tag, err := tx.Exec(t.Context(), `UPDATE afs.work_orders SET status = 'done' WHERE id = $1`, mine); err == nil && tag.RowsAffected() != 0 {
		t.Error("a workload changed a work order")
	}
}
