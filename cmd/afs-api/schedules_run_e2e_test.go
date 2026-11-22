package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// BI-US-3.1 through the real database: a due schedule is run by the billing role, its CSV equals the
// on-demand CSV of the same query, the report.delivered event names the channel and carries a
// download URL, next_run_at advances; a failure records failed with the error and report.failed, keeps
// the schedule due, and the next cycle delivers.
func TestScheduledReportEqualsTheOnDemandReport(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	view := w.Tokens[matrix.Viewer]
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
	now := time.Now().UTC()
	for i, tag := range []any{ts, ts, nil} {
		exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd, tag_set_id)
VALUES ($1, $2, $3, 'model-x', $4, 'ok', 100, 10, 1.5, $5)`, now.Add(-time.Duration(i+1)*time.Hour), w.TeamA.ID, pgtest.NewID("agt"), pgtest.NewID("req"), tag)
	}
	exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd)
VALUES ($1, $2, $3, 'model-x', $4, 'ok', 1, 1, 7)`, now.Add(-time.Hour), w.TeamB.ID, pgtest.NewID("agt"), pgtest.NewID("req"))

	ch := pgtest.NewID("nch")
	exec(`INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, $2, 'ops', 'email', '{"to":["ops@example.com"]}')`, ch, w.TeamA.ID)
	sid := pgtest.NewID("rsc")
	due := now.Add(-time.Hour).Truncate(time.Second)
	exec(`INSERT INTO afs.report_schedules (id, team_id, name, report, query, cadence, channel_id, next_run_at)
VALUES ($1, $2, 'daily', 'usage', '{"range":"24h","groupBy":["agent","tag:business-unit"]}', 'daily', $3, $4)`, sid, w.TeamA.ID, ch, due)

	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_billing"], Service: "afs-billing", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	objs := &fakes.ObjectStore{Bucket: "example-afs-reports"}
	run := &schedules.Runner{Tx: postgres.NewTxRunner(pool), Store: postgres.NewReportSchedulesRepo(), Reports: postgres.NewReportsRepo(), Objects: objs,
		Outbox: postgres.NewOutboxWriter(), Clock: m1w2Clock{}, IDs: m1w2IDs{rand.Reader}, Render: httpapi.ReportCSV,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	// First cycle: the store is down, so the schedule fails and stays due.
	objs.PutErr = errors.New("AccessDenied: s3:PutObject on example-afs-reports")
	sum, err := run.RunDue(t.Context())
	if err != nil || sum.Due != 1 || sum.Failed != 1 {
		t.Fatalf("first cycle: %+v %v", sum, err)
	}
	var status, lastErr string
	var next time.Time
	row := func() {
		t.Helper()
		if err := w.Admin.QueryRow(t.Context(), `SELECT coalesce(last_status, ''), coalesce(last_error, ''), next_run_at FROM afs.report_schedules WHERE id = $1`, sid).Scan(&status, &lastErr, &next); err != nil {
			t.Fatal(err)
		}
	}
	row()
	if status != "failed" || lastErr == "" || !next.Equal(due) {
		t.Fatalf("after a failure: %q %q %s", status, lastErr, next)
	}
	events := func(typ string) []map[string]any {
		t.Helper()
		rows, err := w.Admin.Query(t.Context(), `SELECT payload FROM afs.outbox_events WHERE event_type = $1 AND aggregate_id = $2 ORDER BY id`, typ, sid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var p struct {
				TeamID string         `json:"teamId"`
				Actor  string         `json:"actor"`
				Data   map[string]any `json:"data"`
			}
			if err := json.Unmarshal(raw, &p); err != nil || p.TeamID != w.TeamA.ID || p.Actor != "system:billing" {
				t.Fatalf("payload %s: %v", raw, err)
			}
			out = append(out, p.Data)
		}
		return out
	}
	if f := events("report.failed"); len(f) != 1 || f[0]["channelId"] != ch || f[0]["error"] == "" || len(events("report.delivered")) != 0 {
		t.Fatalf("failure events: %v", f)
	}

	// Second cycle: the cause is gone; the schedule delivers.
	objs.PutErr = nil
	if sum, err = run.RunDue(t.Context()); err != nil || sum.Delivered != 1 {
		t.Fatalf("second cycle: %+v %v", sum, err)
	}
	row()
	if status != "sent" || lastErr != "" || !next.After(now) || next.Hour() != 6 {
		t.Fatalf("after delivery: %q %q %s", status, lastErr, next)
	}
	d := events("report.delivered")
	if len(d) != 1 || d[0]["channelId"] != ch || d[0]["scheduleId"] != sid {
		t.Fatalf("delivered events: %v", d)
	}
	key := schedules.ObjectKey(domain.ReportSchedule{ID: sid, TeamID: w.TeamA.ID, Report: domain.ReportKindUsage}, due)
	got, err := objs.Body(key)
	if err != nil {
		t.Fatalf("object %s: %v (have %v)", key, err, objs.Objects)
	}
	if u, _ := d[0]["downloadUrl"].(string); u == "" || d[0]["s3Uri"] != "s3://example-afs-reports/"+key {
		t.Errorf("event data: %v", d[0])
	}

	// The on-demand report for the same query, as a viewer of the team asks for it.
	code, onDemand, _ := ldCall(t, c.Handler, "GET", "/v1/teams/"+w.TeamA.ID+"/reports/usage?range=24h&groupBy=agent,tag:business-unit", view, "", "Accept", "text/csv")
	if code != 200 || onDemand != string(got) {
		t.Fatalf("on-demand CSV (%d):\n%s\nscheduled CSV:\n%s", code, onDemand, got)
	}
	if len(got) == 0 {
		t.Fatal("empty CSV")
	}

	// The team B row never reaches team A's report.
	if want := 3; countLines(string(got)) != want+1 {
		t.Errorf("lines = %d, want header and %d rows:\n%s", countLines(string(got)), want, got)
	}
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}
