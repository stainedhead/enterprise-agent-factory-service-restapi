package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// RA-US-7a.1, RA-US-7a.2, RA-US-15.6 (part) through the real pipeline and database: a team admin
// schedules a weekly report on the team's own channel; a service-level channel and another team's
// channel are a 400; a viewer reads but cannot write (403, one denied audit record); a replayed
// create writes once; updates and deletes need If-Match; a channel a schedule uses cannot be deleted.
func TestReportSchedulesEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	adm, view := w.Tokens[matrix.TeamAdmin], w.Tokens[matrix.Viewer]
	base := "/v1/teams/" + w.TeamA.ID + "/report-schedules"
	count := func(q string, args ...any) int {
		var n int
		if err := w.Admin.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	channel := func(team any) string {
		id := pgtest.NewID("nch")
		if _, err := w.Admin.Exec(t.Context(), `INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, $2, $3, 'email', '{"to":["ops@example.com"]}')`,
			id, team, "ch-"+id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	own, svcCh, otherCh := channel(w.TeamA.ID), channel(nil), channel(w.TeamB.ID)

	body := func(name, ch string) string {
		return `{"name":"` + name + `","report":"costs","cadence":"weekly","channelId":"` + ch + `","query":{"range":"30d","groupBy":["model","tag:business-unit"],"filters":{"tag.env":"prod"}}}`
	}
	code, out, hdr := ldCall(t, c.Handler, http.MethodPost, base, adm, body("weekly-costs", own), "Idempotency-Key", "rsc-1")
	if code != 201 || hdr.Get("ETag") != `"1"` || !strings.HasPrefix(hdr.Get("Location"), base+"/rsc_") {
		t.Fatalf("create: %d %s %v", code, out, hdr)
	}
	var made struct {
		ID         string  `json:"id"`
		NextRunAt  string  `json:"nextRunAt"`
		LastStatus *string `json:"lastStatus"`
		Format     string  `json:"format"`
		Enabled    bool    `json:"enabled"`
		Query      struct {
			Range   string            `json:"range"`
			GroupBy []string          `json:"groupBy"`
			Filters map[string]string `json:"filters"`
		} `json:"query"`
	}
	if err := json.Unmarshal([]byte(out), &made); err != nil {
		t.Fatal(err)
	}
	next, perr := time.Parse(time.RFC3339, made.NextRunAt)
	if perr != nil || !next.After(time.Now()) || next.Weekday() != time.Monday || next.Hour() != 6 || made.LastStatus != nil || made.Format != "csv" || !made.Enabled ||
		made.Query.Range != "30d" || len(made.Query.GroupBy) != 2 || made.Query.Filters["tag.env"] != "prod" {
		t.Fatalf("created schedule: %s", out)
	}
	if code, again, h2 := ldCall(t, c.Handler, http.MethodPost, base, adm, body("weekly-costs", own), "Idempotency-Key", "rsc-1"); code != 201 || h2.Get("Idempotent-Replayed") != "true" || !sameJSON(again, out) {
		t.Fatalf("replay: %d %s", code, again)
	}
	if n := count(`SELECT count(*) FROM afs.report_schedules WHERE team_id = $1`, w.TeamA.ID); n != 1 {
		t.Fatalf("schedules = %d after a replay", n)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, body("weekly-costs", own)); code != 409 {
		t.Fatalf("duplicate name: %d", code)
	}

	// RA-US-7a.1, RA-US-15.6: only the team's own channel; the query must be a valid report query.
	for name, b := range map[string]string{
		"service-level channel": body("a1", svcCh),
		"another team's":        body("a2", otherCh),
		"unknown channel":       body("a3", pgtest.NewID("nch")),
		"bad groupBy":           `{"name":"a4","report":"usage","cadence":"daily","channelId":"` + own + `","query":{"groupBy":["bogus"]}}`,
		"another team's data":   `{"name":"a5","report":"usage","cadence":"daily","channelId":"` + own + `","query":{"filters":{"teamId":"` + w.TeamB.ID + `"}}}`,
		"bad cadence":           `{"name":"a6","report":"usage","cadence":"hourly","channelId":"` + own + `"}`,
		"unknown field":         `{"name":"a7","report":"usage","cadence":"daily","channelId":"` + own + `","recipients":["x"]}`,
	} {
		if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, b); code != 400 || !strings.Contains(out, "ValidationError") {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if n := count(`SELECT count(*) FROM afs.report_schedules WHERE team_id = $1`, w.TeamA.ID); n != 1 {
		t.Fatalf("a refused create wrote a row: %d", n)
	}

	// RA-US-7a.2: a viewer reads but cannot write.
	if code, out, _ := ldCall(t, c.Handler, http.MethodGet, base, view, ""); code != 200 || !strings.Contains(out, made.ID) {
		t.Fatalf("viewer list: %d %s", code, out)
	}
	denied := count(`SELECT count(*) FROM afs.audit_log WHERE outcome = 'denied' AND action = 'CreateReportSchedule'`)
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, view, body("v", own)); code != 403 || !strings.Contains(out, "reports:schedule") {
		t.Fatalf("viewer create: %d %s", code, out)
	}
	if got := count(`SELECT count(*) FROM afs.audit_log WHERE outcome = 'denied' AND action = 'CreateReportSchedule'`); got != denied+1 {
		t.Errorf("denied audit rows %d, want %d", got, denied+1)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, base+"/"+made.ID, view, `{"enabled":false}`, "If-Match", `"1"`); code != 403 {
		t.Errorf("viewer update: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodDelete, base+"/"+made.ID, view, "", "If-Match", `"1"`); code != 403 {
		t.Errorf("viewer delete: %d", code)
	}

	// Update: If-Match required, stale is 412, the channel rule applies, cadence changes the next run.
	patch := base + "/" + made.ID
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, patch, adm, `{"enabled":false}`); code != 428 {
		t.Errorf("update without If-Match: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, patch, adm, `{"enabled":false}`, "If-Match", `"9"`); code != 412 {
		t.Errorf("stale update: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, patch, adm, `{"channelId":"`+svcCh+`"}`, "If-Match", `"1"`); code != 400 {
		t.Errorf("update to a service channel: %d", code)
	}
	code, out, hdr = ldCall(t, c.Handler, http.MethodPatch, patch, adm, `{"cadence":"monthly","enabled":false}`, "If-Match", `"1"`)
	if code != 200 || hdr.Get("ETag") != `"2"` || !strings.Contains(out, `"cadence":"monthly"`) || !strings.Contains(out, `"enabled":false`) {
		t.Fatalf("update: %d %s", code, out)
	}

	// The runner's outcome shows with the API's vocabulary.
	if _, err := w.Admin.Exec(t.Context(), `UPDATE afs.report_schedules SET last_status = 'sent', last_run_at = now() WHERE id = $1`, made.ID); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodGet, base, adm, ""); code != 200 || !strings.Contains(out, `"lastStatus":"succeeded"`) || !strings.Contains(out, `"lastError":null`) {
		t.Fatalf("list after a run: %d %s", code, out)
	}
	if _, err := w.Admin.Exec(t.Context(), `UPDATE afs.report_schedules SET last_status = 'failed', last_error = 'AccessDenied' WHERE id = $1`, made.ID); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodGet, base, adm, ""); code != 200 || !strings.Contains(out, `"lastStatus":"failed"`) || !strings.Contains(out, `"lastError":"AccessDenied"`) {
		t.Fatalf("list after a failure: %d %s", code, out)
	}

	// Another team's admin cannot see the team's schedules; a channel in use cannot be deleted.
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/teams/"+w.TeamB.ID+"/report-schedules", adm, ""); code != 404 {
		t.Errorf("list through another team: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodDelete, "/v1/teams/"+w.TeamA.ID+"/notification-channels/"+own, adm, "", "If-Match", `"1"`); code != 409 {
		t.Errorf("delete of a channel a schedule uses: %d, want 409", code)
	}

	// Delete: 204, then gone. The runner's updates (-db 000018) did not raise the version, so the ETag
	// the update returned is still current; a stale one is refused.
	var v int
	if err := w.Admin.QueryRow(t.Context(), `SELECT version FROM afs.report_schedules WHERE id = $1`, made.ID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("version after the runner's updates = %d, want 2 (a run must not raise it)", v)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodDelete, patch, adm, "", "If-Match", `"1"`); code != 412 {
		t.Fatalf("a delete with a stale version: %d, want 412", code)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodDelete, patch, adm, "", "If-Match", `"`+strconv.Itoa(v)+`"`); code != 204 || out != "" {
		t.Fatalf("delete: %d %q", code, out)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodDelete, patch, adm, "", "If-Match", `"`+strconv.Itoa(v)+`"`); code != 404 {
		t.Errorf("second delete: %d", code)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_kind = 'report_schedules' AND resource_id = $1 AND outcome = 'allowed'`, made.ID); n != 3 {
		t.Errorf("audit rows for the schedule = %d, want create, update, delete", n)
	}
}
