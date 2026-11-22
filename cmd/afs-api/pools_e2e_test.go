package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

const poolDigest = "sha256:4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"

// TestPoolsEndToEnd drives ephemeral team creation (M5-A) and the pool operations (M5-B) through the
// real router, authentication and database: the create rules, the persistent-team 409 before any
// write, the shared agent and pool name space, scale bounds, handoffs, role link rules, idempotent
// creates and the viewer's 403.
func TestPoolsEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := wire(context.Background(), quiet, settings{
		DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	call := func(caller matrix.Caller, method, path, body string, hdr ...string) (int, map[string]any, http.Header) {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+w.Tokens[caller])
		r.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		rec := httptest.NewRecorder()
		c.Handler.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Header()
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := w.Admin.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	expect := func(what string, got, want int, body map[string]any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: %d, want %d: %v", what, got, want, body)
		}
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }

	// M5-A: creating an ephemeral team.
	soon := time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339)
	teamBody := func(slug, expires, rule, budget string) string {
		b := fmt.Sprintf(`{"slug":%q,"displayName":"Swarm","costCenter":"CC1","ownerEmail":"owner@example.com","maxBudgetUsd":"1000","kind":"ephemeral","firstAdmin":{"subjectType":"group","groupName":"m5-g"},"tags":[]`, slug)
		if expires != "" {
			b += `,"expiresAt":"` + expires + `"`
		}
		if rule != "" {
			b += `,"teardownRule":"` + rule + `"`
		}
		if budget != "" {
			b += `,"budget":{"amountUsd":"` + budget + `","period":"lifetime"}`
		}
		return b + `}`
	}
	for _, tc := range []struct{ name, body, field string }{
		{"no expiry", teamBody("sw-a", "", "at_expiry", "50"), "expiresAt"},
		{"expiry in the past", teamBody("sw-b", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "at_expiry", "50"), "expiresAt"},
		{"expiry beyond the maximum", teamBody("sw-c", time.Now().UTC().Add(200*24*time.Hour).Format(time.RFC3339), "at_expiry", "50"), "expiresAt"},
		{"no teardown rule", teamBody("sw-d", soon, "", "50"), "teardownRule"},
		{"unknown teardown rule", teamBody("sw-e", soon, "whenever", "50"), "teardownRule"},
		{"no lifetime budget", teamBody("sw-f", soon, "at_expiry", ""), "budget"},
		{"budget above the ceiling", teamBody("sw-g", soon, "at_expiry", "1000.01"), "budget.amountUsd"},
	} {
		st, b, _ := call(matrix.ServiceAdmin, http.MethodPost, "/v1/teams", tc.body)
		expect(tc.name, st, http.StatusBadRequest, b)
		if d, _ := b["details"].(map[string]any); !strings.Contains(fmt.Sprint(d["fields"]), "field:"+tc.field+" ") {
			t.Errorf("%s: the error is not about %s: %v", tc.name, tc.field, b)
		}
	}
	if n := count(`SELECT count(*) FROM afs.teams WHERE slug LIKE 'sw-%'`); n != 0 {
		t.Fatalf("%d teams were written by refused creates", n)
	}
	st, b, _ := call(matrix.TeamAdmin, http.MethodPost, "/v1/teams", teamBody("sw-h", soon, "at_expiry", "50"))
	expect("a team-admin creates a team", st, http.StatusForbidden, b)

	audit0 := count(`SELECT count(*) FROM afs.audit_log WHERE action = 'CreateTeam' AND outcome = 'allowed'`)
	st, b, th := call(matrix.ServiceAdmin, http.MethodPost, "/v1/teams", teamBody("sw-ok", soon, "at_expiry", "50"))
	expect("an ephemeral team", st, http.StatusCreated, b)
	swarm := str(b, "id")
	if b["kind"] != "ephemeral" || b["teardownRule"] != "at_expiry" || b["expiresAt"] == nil || swarm == "" {
		t.Fatalf("%v", b)
	}
	if n := count(`SELECT count(*) FROM afs.budgets WHERE team_id = $1 AND period = 'lifetime' AND on_exhaust = 'block'`, swarm); n != 1 {
		t.Errorf("lifetime blocking budget rows: %d", n)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE action = 'CreateTeam' AND outcome = 'allowed'`); n != audit0+1 {
		t.Errorf("CreateTeam audit records: %d, want %d", n, audit0+1)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'team.created' AND team_id = $1`, swarm); n != 1 {
		t.Errorf("team.created events: %d", n)
	}

	// M5-A2: an ephemeral team cannot become persistent or lose its lifetime budget.
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPatch, "/v1/teams/"+swarm, `{"kind":"persistent"}`, "If-Match", th.Get("ETag"))
	expect("ephemeral to persistent", st, http.StatusBadRequest, b)
	for _, body := range []string{`{"period":"monthly","amountUsd":"50"}`, `{"period":"lifetime","amountUsd":"50","onExhaust":"notify"}`} {
		st, b, _ = call(matrix.ServiceAdmin, http.MethodPut, "/v1/teams/"+swarm+"/budget", body)
		expect("budget that is not a blocking lifetime budget: "+body, st, http.StatusBadRequest, b)
	}
	if n := count(`SELECT count(*) FROM afs.budgets WHERE team_id = $1 AND period = 'lifetime' AND on_exhaust = 'block'`, swarm); n != 1 {
		t.Errorf("the lifetime budget changed: %d", n)
	}

	// Catalog for the pools: a model for the swarm and an enabled template that allows the digest.
	model, tpl := "m5.model-"+strings.ToLower(pgtest.NewID("x")[2:10]), "m5-"+strings.ToLower(pgtest.NewID("x")[2:10])
	image := "registry.example.com/agents/coder@" + poolDigest
	exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'M5', 4096)`, model)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2)`, swarm, model)
	exec(`INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest) VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`,
		pgtest.NewID("tpl"), tpl, poolDigest)
	pools := "/v1/teams/" + swarm + "/pools"
	poolBody := func(name string, workers int) string {
		return fmt.Sprintf(`{"name":%q,"template":%q,"image":%q,"modelId":%q,"workers":%d}`, name, tpl, image, model, workers)
	}

	// Pools: a persistent team is a 409 before anything is written; a viewer cannot write at all.
	persistentPools := "/v1/teams/" + w.TeamA.ID + "/pools"
	st, b, _ = call(matrix.TeamAdmin, http.MethodPost, persistentPools, poolBody("fixer", 1))
	expect("a pool in a persistent team", st, http.StatusConflict, b)
	st, b, _ = call(matrix.Viewer, http.MethodPost, persistentPools, poolBody("fixer", 1))
	expect("a viewer creates a pool", st, http.StatusForbidden, b)
	if n := count(`SELECT count(*) FROM afs.agent_pools WHERE team_id = $1`, w.TeamA.ID); n != 0 {
		t.Fatalf("pools in a persistent team: %d", n)
	}

	// Create rules.
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, poolBody("fixer", 201))
	expect("201 workers", st, http.StatusBadRequest, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, strings.Replace(poolBody("fixer", 1), tpl, "no-such-template", 1))
	expect("unknown template", st, http.StatusBadRequest, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, strings.Replace(poolBody("fixer", 1), poolDigest[7:15], "bbbbbbbb", 1))
	expect("digest the template does not allow", st, http.StatusBadRequest, b)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, 'taken', $3, $4, $5)`, pgtest.NewID("agt"), swarm, tpl, image, model)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, poolBody("taken", 1))
	expect("a pool named like an agent", st, http.StatusConflict, b)

	st, b, h := call(matrix.ServiceAdmin, http.MethodPost, pools, poolBody("fixer", 2), "Idempotency-Key", "k-pool-1")
	expect("create", st, http.StatusCreated, b)
	pid := str(b, "id")
	if h.Get("ETag") != `"1"` || b["state"] != "enabled" || b["workers"] != float64(2) || b["scratchSizeGiB"] != float64(20) || b["sharedVolume"] != "none" {
		t.Fatalf("%v %v", h, b)
	}
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, poolBody("fixer", 2), "Idempotency-Key", "k-pool-1")
	expect("replay", st, http.StatusCreated, b)
	if str(b, "id") != pid || count(`SELECT count(*) FROM afs.agent_pools WHERE team_id = $1`, swarm) != 1 {
		t.Fatalf("the replay created another pool: %v", b)
	}
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, poolBody("fixer", 2))
	expect("duplicate name", st, http.StatusConflict, b)
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'pool.created' AND team_id = $1`, swarm); n != 1 {
		t.Errorf("pool.created events: %d", n)
	}
	one := pools + "/" + pid

	// Scale: 0 to 200, with an event only when the count changes.
	for _, tc := range []struct {
		body string
		want int
	}{{`{"workers":201}`, 400}, {`{"workers":-1}`, 400}, {`{}`, 400}, {`{"workers":200}`, 200}, {`{"workers":0}`, 200}} {
		st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, one+":scale", tc.body)
		expect("scale "+tc.body, st, tc.want, b)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'pool.scaled' AND team_id = $1`, swarm); n != 2 {
		t.Errorf("pool.scaled events: %d", n)
	}
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, one+":pause", "")
	expect("pause", st, http.StatusOK, b)
	if b["state"] != "paused" {
		t.Fatalf("%v", b)
	}
	st, b, _ = call(matrix.ServiceAdmin, http.MethodGet, one+"/status", "")
	expect("status", st, http.StatusOK, b)
	if wk, _ := b["workers"].(map[string]any); wk["desired"] != float64(0) {
		t.Fatalf("a paused pool desires no workers: %v", b)
	}
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, one+":resume", "")
	expect("resume", st, http.StatusOK, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, one+":scale", `{"workers":3}`)
	expect("scale again", st, http.StatusOK, b)

	// Update and delete need If-Match.
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPatch, one, `{"description":"x"}`)
	expect("update without If-Match", st, http.StatusPreconditionRequired, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPatch, one, `{"description":"x"}`, "If-Match", `"1"`)
	expect("update with a stale If-Match", st, http.StatusPreconditionFailed, b)
	st, b, h = call(matrix.ServiceAdmin, http.MethodGet, one, "")
	expect("get", st, http.StatusOK, b)
	ver := h.Get("ETag")
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPatch, one, `{"description":"x","workers":4}`, "If-Match", ver)
	expect("update", st, http.StatusOK, b)
	if b["description"] != "x" || b["workers"] != float64(4) {
		t.Fatalf("%v", b)
	}

	// Handoffs between two pools; to itself is a 400.
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, pools, poolBody("reviewer", 1))
	expect("second pool", st, http.StatusCreated, b)
	rid := str(b, "id")
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPut, one+"/handoffs", `{"toPoolIds":["`+pid+`"]}`)
	expect("handoff to itself", st, http.StatusBadRequest, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPut, one+"/handoffs", `{"toPoolIds":["`+rid+`"]}`)
	expect("handoff", st, http.StatusOK, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodGet, one, "")
	expect("get with handoffs", st, http.StatusOK, b)
	if hs, _ := b["handoffs"].([]any); len(hs) != 1 || hs[0] != rid {
		t.Fatalf("%v", b)
	}

	// Role links: one role name, accounts inside the team's allowed accounts.
	exec(`INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ('111122223333', 'm5-a', 'active'), ('444455556666', 'm5-b', 'active') ON CONFLICT (id) DO UPDATE SET status = 'active'`)
	exec(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['111122223333'] WHERE id = $1`, swarm)
	links := one + "/role-links"
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, links, `{"roleName":"shared","accountId":"444455556666","profileName":"b"}`)
	expect("account outside the allowed accounts", st, http.StatusBadRequest, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, links, `{"roleName":"shared","accountId":"111122223333","profileName":"a"}`)
	expect("link", st, http.StatusCreated, b)
	if b["roleArn"] != "arn:aws:iam::111122223333:role/shared" || b["status"] != "pending_trust" || str(b, "externalId") == "" {
		t.Fatalf("%v", b)
	}
	exec(`UPDATE afs.teams SET allowed_aws_accounts = ARRAY['*'] WHERE id = $1`, swarm)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPost, links, `{"roleName":"other","accountId":"444455556666","profileName":"b"}`)
	expect("a role name other than the pool's", st, http.StatusBadRequest, b)

	// Delete: 202 in state deleting; the pool is invisible once the reconciler finalizes it.
	st, b, h = call(matrix.ServiceAdmin, http.MethodGet, one, "")
	expect("get before delete", st, http.StatusOK, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodDelete, one, "", "If-Match", h.Get("ETag"))
	expect("delete", st, http.StatusAccepted, b)
	if b["state"] != "deleting" {
		t.Fatalf("%v", b)
	}
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPut, one+"/handoffs", `{"toPoolIds":[]}`)
	expect("a pool being deleted cannot change", st, http.StatusConflict, b)
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'pool.deleted' AND team_id = $1`, swarm); n != 1 {
		t.Errorf("pool.deleted events: %d", n)
	}
	exec(`UPDATE afs.agent_pools SET state = 'deleted', deleted_at = now() WHERE id = $1`, pid)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodGet, one, "")
	expect("a deleted pool", st, http.StatusNotFound, b)
}
