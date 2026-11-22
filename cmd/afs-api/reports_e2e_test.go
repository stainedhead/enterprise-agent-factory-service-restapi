package main

import (
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// RA-US-7.1, 7.3, 7.4, 7.6 through the real pipeline: a viewer reads the team's report as JSON and as
// CSV with the same rows, filters and groups by tag, gets 404 for another team and 403 for service
// reports; a service admin reads the service report and a deleted team's.
func TestReportsEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	view, svc := w.Tokens[matrix.Viewer], w.Tokens[matrix.ServiceAdmin]
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	var ts string
	if err := w.Admin.QueryRow(t.Context(), `SELECT afs.tag_set_for($1, '{"business-unit":"retail"}'::jsonb)`, w.TeamA.ID).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	agent := pgtest.NewID("agt")
	now := time.Now().UTC()
	for i, tag := range []any{ts, ts, nil} {
		exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd, tag_set_id)
VALUES ($1, $2, $3, 'model-x', $4, 'ok', 100, 10, 1.5, $5)`, now.Add(-time.Duration(i+1)*time.Hour), w.TeamA.ID, agent, pgtest.NewID("req"), tag)
	}
	exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd)
VALUES ($1, $2, $3, 'model-x', $4, 'ok', 1, 1, 7)`, now.Add(-time.Hour), w.TeamB.ID, pgtest.NewID("agt"), pgtest.NewID("req"))

	base := "/v1/teams/" + w.TeamA.ID + "/reports/usage?range=24h"
	code, body, _ := ldCall(t, c.Handler, "GET", base+"&groupBy=model", view, "")
	var rep struct {
		Kind    string
		GroupBy []string
		Rows    []map[string]any
		Totals  map[string]any
		Notes   []string
	}
	if code != 200 || json.Unmarshal([]byte(body), &rep) != nil || rep.Kind != "usage" || len(rep.Rows) != 1 ||
		rep.Totals["calls"] != float64(3) || rep.Totals["costUsd"] != "4.50" || rep.Rows[0]["model"] != "model-x" || len(rep.Notes) == 0 {
		t.Fatalf("%d %s", code, body)
	}

	// CSV: the same rows as JSON.
	code, csvBody, h := ldCall(t, c.Handler, "GET", base+"&groupBy=model", view, "", "Accept", "text/csv")
	recs, err := csv.NewReader(strings.NewReader(csvBody)).ReadAll()
	if code != 200 || err != nil || !strings.HasPrefix(h.Get("Content-Type"), "text/csv") || len(recs) != 2 ||
		strings.Join(recs[0], ",") != "model,calls,inputTokens,outputTokens,costUsd" || strings.Join(recs[1], ",") != "model-x,3,300,30,4.50" {
		t.Fatalf("%d %v %s", code, h, csvBody)
	}

	// Tags: one row per value plus (untagged); a filter keeps one.
	code, body, _ = ldCall(t, c.Handler, "GET", base+"&groupBy=tag:business-unit", view, "")
	_ = json.Unmarshal([]byte(body), &rep)
	got := map[string]any{}
	for _, r := range rep.Rows {
		got[r["tag:business-unit"].(string)] = r["calls"]
	}
	if code != 200 || len(got) != 2 || got["retail"] != float64(2) || got["(untagged)"] != float64(1) {
		t.Fatalf("%d %s", code, body)
	}
	code, body, _ = ldCall(t, c.Handler, "GET", base+"&tag.business-unit=retail", view, "")
	_ = json.Unmarshal([]byte(body), &rep)
	if code != 200 || rep.Totals["calls"] != float64(2) {
		t.Fatalf("tag filter: %d %s", code, body)
	}

	// Validation: a bad range, an unknown groupBy, a foreign page token.
	for _, q := range []string{"?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", "?range=24h&groupBy=planet", "?from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", "?nextToken=zz"} {
		if code, body, _ := ldCall(t, c.Handler, "GET", "/v1/teams/"+w.TeamA.ID+"/reports/usage"+q, view, ""); code != 400 {
			t.Errorf("%s: %d %s", q, code, body)
		}
	}
	// A report kind that does not exist.
	if code, _, _ := ldCall(t, c.Handler, "GET", "/v1/teams/"+w.TeamA.ID+"/reports/nothing", view, ""); code != 404 {
		t.Errorf("unknown kind = %d", code)
	}

	// GetUsage has the Usage shape with the key object.
	code, body, _ = ldCall(t, c.Handler, "GET", "/v1/teams/"+w.TeamA.ID+"/usage?from="+now.Add(-24*time.Hour).Format(time.RFC3339)+"&groupBy=agent", view, "")
	var us struct {
		Rows   []struct{ Key map[string]string }
		Totals struct{ Calls float64 }
	}
	if code != 200 || json.Unmarshal([]byte(body), &us) != nil || len(us.Rows) != 1 || us.Rows[0].Key["agent"] != agent || us.Totals.Calls != 3 {
		t.Fatalf("usage: %d %s", code, body)
	}

	// RA-US-7.6: another team is 404, service reports are 403.
	if code, _, _ := ldCall(t, c.Handler, "GET", "/v1/teams/"+w.TeamB.ID+"/reports/usage", view, ""); code != 404 {
		t.Errorf("team B = %d", code)
	}
	if code, body, _ := ldCall(t, c.Handler, "GET", "/v1/reports/usage", view, ""); code != 403 || !strings.Contains(body, "admin:reports") {
		t.Errorf("service report = %d %s", code, body)
	}

	// A service admin reads the service report, filtered by team, and a deleted team's report.
	code, body, _ = ldCall(t, c.Handler, "GET", "/v1/reports/usage?range=24h&groupBy=team&teamId="+w.TeamB.ID, svc, "")
	_ = json.Unmarshal([]byte(body), &rep)
	if code != 200 || len(rep.Rows) != 1 || rep.Totals["costUsd"] != "7.00" {
		t.Fatalf("service: %d %s", code, body)
	}
	exec(`UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, w.TeamB.ID)
	code, body, _ = ldCall(t, c.Handler, "GET", "/v1/teams/"+w.TeamB.ID+"/reports/usage?range=24h", svc, "")
	_ = json.Unmarshal([]byte(body), &rep)
	if code != 200 || rep.Totals["costUsd"] != "7.00" {
		t.Fatalf("deleted team: %d %s", code, body)
	}
}
