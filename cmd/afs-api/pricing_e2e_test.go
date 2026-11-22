package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// M4-D2 [e2e] (RA-US-8.1, RA-US-8.2, RA-7): a service admin approves a proposal through the real
// pipeline and database; the key replays; the second decision is a 409; a team admin gets a 403; the
// price row is written from max(effectiveFrom, approval) and the gateway's store reads it.
func TestPriceProposalsEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sa, ta := w.Tokens[matrix.ServiceAdmin], w.Tokens[matrix.TeamAdmin]

	admin := db.Admin(t)
	model := "anthropic.e2e-" + strings.ToLower(pgtest.NewID("m")[2:]) + "-v1:0"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO afs.models (id, provider, display_name, context_window, max_output_tokens) VALUES ($1, 'Example', 'E2E', 1000, 100)`, model)
	exec(`INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, source) VALUES ($1, now() - interval '30 days', 3, 15, 'manual')`, model)
	id := pgtest.NewID("mpp")
	exec(`INSERT INTO afs.model_price_proposals (id, model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, cache_read_usd_per_mtok, cache_write_usd_per_mtok)
VALUES ($1, $2, now() - interval '2 days', 3.3, 16.5, 0.33, 4.125)`, id, model)

	// The list: service admins only, filtered by status.
	if code, out, _ := agentCall(t, c.Handler, http.MethodGet, "/v1/admin/model-price-proposals?status=pending", ta, ""); code != 403 {
		t.Fatalf("team admin list: %d %s", code, out)
	}
	code, out, _ := agentCall(t, c.Handler, http.MethodGet, "/v1/admin/model-price-proposals?status=pending&maxResults=100", sa, "")
	if code != 200 || !strings.Contains(out, id) {
		t.Fatalf("list: %d %s", code, out)
	}

	path := "/v1/admin/model-price-proposals/" + id
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, path+":approve", ta, `{}`, "Idempotency-Key", "p-1"); code != 403 {
		t.Fatalf("team admin approve: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, path+":approve", sa, `{"unknown":1}`, "Idempotency-Key", "p-0"); code != 400 {
		t.Fatalf("unknown field: %d %s", code, out)
	}

	before := time.Now().Add(-time.Minute)
	code, out, hdr := agentCall(t, c.Handler, http.MethodPost, path+":approve", sa, `{"note":"matches the AWS price list"}`, "Idempotency-Key", "p-2")
	if code != 200 || hdr.Get("ETag") != `"2"` {
		t.Fatalf("approve: %d %s etag %q", code, out, hdr.Get("ETag"))
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatal(err)
	}
	if p["id"] != id || p["status"] != "approved" || p["note"] != "matches the AWS price list" || p["inputUsdPerMtok"] != "3.30" ||
		p["cacheWriteUsdPerMtok"] != "4.125" || p["decidedBy"] == nil || p["decidedAt"] == nil || p["version"] != float64(2) {
		t.Fatalf("body = %v", p)
	}
	if code, again, h := agentCall(t, c.Handler, http.MethodPost, path+":approve", sa, `{"note":"matches the AWS price list"}`, "Idempotency-Key", "p-2"); code != 200 || h.Get("Idempotent-Replayed") != "true" || !sameJSON(again, out) {
		t.Fatalf("replay: %d %s", code, again)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, path+":reject", sa, `{}`, "Idempotency-Key", "p-3"); code != 409 {
		t.Fatalf("reject after approve: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, "/v1/admin/model-price-proposals/mpp_00000000000000000000000000:approve", sa, `{}`); code != 404 {
		t.Fatalf("unknown id: %d %s", code, out)
	}

	// One price row, effective from the approval (the proposal's effectiveFrom was in the past), and
	// one event and one audit record: the replay and the refusals added none.
	var n int
	var eff time.Time
	if err := admin.QueryRow(t.Context(), `SELECT count(*), max(effective_from) FROM afs.model_prices WHERE model_id = $1 AND source = 'aws_price_list'`, model).Scan(&n, &eff); err != nil || n != 1 {
		t.Fatalf("price rows = %d, %v", n, err)
	}
	if eff.Before(before) {
		t.Fatalf("effective %v is before the approval (%v): earlier usage would be repriced", eff, before)
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'pricing.proposal_decided' AND aggregate_id = $1`:                 1,
		`SELECT count(*) FROM afs.audit_log WHERE action = 'ApproveModelPriceProposal' AND resource_id = $1 AND outcome = 'allowed'`: 1,
	} {
		if err := admin.QueryRow(t.Context(), q, id).Scan(&n); err != nil || n != want {
			t.Fatalf("%s = %d, %v", q, n, err)
		}
	}

	// The gateway reads the new price: the old one before the effective time, the new one after.
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_gateway"], Service: "afs-gateway", SystemActor: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ps, err := postgres.NewGatewayStore(pool).Prices(t.Context(), model)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := gateway.EffectivePrice(ps, eff.Add(-time.Second)); !ok || got.Input.String() != "3.00" {
		t.Fatalf("before = %+v", got)
	}
	if got, ok := gateway.EffectivePrice(ps, time.Now().Add(time.Second)); !ok || got.Input.String() != "3.30" || got.CacheWrite.String() != "4.125" {
		t.Fatalf("after = %+v", got)
	}
}
