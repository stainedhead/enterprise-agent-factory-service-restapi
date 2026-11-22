package main

import (
	"context"
	"encoding/json"
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

// TestBudgetsEndToEnd drives GetBudget and PutBudget through the real router, authentication and
// database as the callers of RA-US-13: the ceiling, the unexpired override, notify by role and team
// kind, If-Match, the position (spent, reserved, forecast, projected cap) and the audit trail.
func TestBudgetsEndToEnd(t *testing.T) {
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
			t.Fatal(err)
		}
	}
	path := "/v1/teams/" + w.TeamA.ID + "/budget"
	expect := func(what string, got, want int, body map[string]any) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: %d, want %d: %v", what, got, want, body)
		}
	}

	st, b, _ := call(matrix.TeamAdmin, http.MethodGet, path, "")
	expect("no budget yet", st, http.StatusNotFound, b)

	// RA-US-13.1: a team-admin above the ceiling (1000) is a 400 and nothing is stored.
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"1000.01"}`)
	expect("above the ceiling", st, http.StatusBadRequest, b)
	if b["code"] != "ValidationError" {
		t.Fatalf("%v", b)
	}
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"amountUsd":"10"}`)
	expect("period is required", st, http.StatusBadRequest, b)

	// Create at the ceiling, then replace with If-Match.
	st, b, h := call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"1000.00"}`)
	expect("at the ceiling", st, http.StatusOK, b)
	if h.Get("ETag") != `"1"` || b["onExhaust"] != "block" || b["spentUsd"] != "0.00" || b["forecastUsd"] != "0.00" || b["projectedCapAt"] != nil {
		t.Fatalf("%v %v", h, b)
	}
	if th, _ := b["thresholds"].([]any); len(th) != 3 {
		t.Fatalf("default thresholds: %v", b["thresholds"])
	}
	st, b, h = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"800","thresholds":[80]}`, "If-Match", `"1"`)
	expect("replace", st, http.StatusOK, b)
	if h.Get("ETag") != `"2"` || b["amountUsd"] != "800.00" {
		t.Fatalf("%v %v", h, b)
	}
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"700"}`, "If-Match", `"1"`)
	expect("stale If-Match", st, http.StatusPreconditionFailed, b)

	// RA-US-13.3: notify by a team-admin is 403 with a denied audit record, by a service admin 200; a
	// viewer cannot write at all.
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"800","onExhaust":"notify"}`)
	expect("notify by a team-admin", st, http.StatusForbidden, b)
	if d, _ := b["details"].(map[string]any); d["requiredPermission"] != "teams:set-guardrails" {
		t.Fatalf("%v", b)
	}
	var denied int
	if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'PutBudget' AND outcome = 'denied' AND team_id = $1`, w.TeamA.ID).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("denied audit records: %d %v", denied, err)
	}
	st, b, _ = call(matrix.Viewer, http.MethodPut, path, `{"period":"monthly","amountUsd":"100"}`)
	expect("viewer writes", st, http.StatusForbidden, b)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"800","onExhaust":"notify"}`)
	expect("notify by a service admin", st, http.StatusOK, b)
	if b["onExhaust"] != "notify" {
		t.Fatalf("%v", b)
	}

	// RA-US-13.2: the override (a service admin's guardrail) lifts the ceiling for a team-admin until it expires.
	exec(`UPDATE afs.teams SET ceiling_override_usd = 3000, ceiling_override_expires_at = now() + interval '1 day' WHERE id = $1`, w.TeamA.ID)
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"3000"}`)
	expect("up to the override", st, http.StatusOK, b)
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"3000.01"}`)
	expect("above the override", st, http.StatusBadRequest, b)
	// The reconciler lowers a budget above the base ceiling before it clears an expired override (M3-F4).
	exec(`UPDATE afs.budgets SET amount_usd = 800 WHERE team_id = $1`, w.TeamA.ID)
	exec(`UPDATE afs.teams SET ceiling_override_expires_at = now() - interval '1 second' WHERE id = $1`, w.TeamA.ID)
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"3000"}`)
	expect("the override expired, the ceiling is back", st, http.StatusBadRequest, b)

	// A notify budget on an ephemeral team is a 400, by a service admin too.
	ephID := pgtest.NewID("team")
	exec(`BEGIN;
INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd, kind, expires_at, teardown_rule)
VALUES ('` + ephID + `', 'eph` + strings.ToLower(ephID[len(ephID)-8:]) + `', 'Eph', 'CC100', 'owner@example.com', 500, 'ephemeral', now() + interval '10 days', 'at_expiry');
INSERT INTO afs.budgets (id, team_id, period, amount_usd) VALUES ('` + pgtest.NewID("bud") + `', '` + ephID + `', 'lifetime', 100);
COMMIT;`)
	st, b, _ = call(matrix.ServiceAdmin, http.MethodPut, "/v1/teams/"+ephID+"/budget", `{"period":"lifetime","amountUsd":"100","onExhaust":"notify"}`)
	expect("notify on an ephemeral team", st, http.StatusBadRequest, b)

	// The position: a counter for the current month shows up as spent, reserved, percent and forecast.
	exec(`UPDATE afs.teams SET ceiling_override_usd = NULL, ceiling_override_expires_at = NULL WHERE id = $1`, w.TeamA.ID)
	st, b, _ = call(matrix.TeamAdmin, http.MethodPut, path, `{"period":"monthly","amountUsd":"1000"}`)
	expect("reset to 1000", st, http.StatusOK, b)
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	exec(`INSERT INTO afs.spend_counters (budget_id, period_start, limit_usd, spent_usd, reserved_usd) VALUES ($1, $2, 1000, 250, 3.2)`, b["id"], start)
	st, b, h = call(matrix.Viewer, http.MethodGet, path, "")
	expect("viewer reads the position", st, http.StatusOK, b)
	if b["spentUsd"] != "250.00" || b["reservedUsd"] != "3.20" || b["percentUsed"] != 25.0 || h.Get("ETag") == "" {
		t.Fatalf("%v", b)
	}
	if now.Sub(start) > time.Minute { // forecast is spent scaled to the whole month, so it is at least the spend
		if f, _ := b["forecastUsd"].(string); f == "" || f == "0.00" {
			t.Fatalf("forecast: %v", b["forecastUsd"])
		}
	}

	// Another team's admin cannot tell the budget exists, and an unbound person gets the same 404.
	st, b, _ = call(matrix.Unbound, http.MethodGet, path, "")
	expect("unbound", st, http.StatusNotFound, b)
	var puts int
	if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'PutBudget' AND outcome = 'allowed' AND team_id = $1`, w.TeamA.ID).Scan(&puts); err != nil || puts < 5 {
		t.Fatalf("allowed audit records: %d %v", puts, err)
	}
}
