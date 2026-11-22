package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// RA-US-17.2, RA-US-17.3, RA-US-15.6 through the real pipeline and database: a team admin manages
// notification channels with credentials only as a secret reference; a viewer lists and describes
// but cannot create (403 and one denied audit record); a test reports the reconciler's result;
// service-level channels need admin:settings and one named by work_orders.notification_channel_id
// cannot be deleted.
func TestNotificationChannelsEndToEnd(t *testing.T) {
	channelTestWait = 5 * time.Second
	defer func() { channelTestWait = 20 * time.Millisecond }()
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	adm, view, svc := w.Tokens[matrix.TeamAdmin], w.Tokens[matrix.Viewer], w.Tokens[matrix.ServiceAdmin]
	base := "/v1/teams/" + w.TeamA.ID + "/notification-channels"
	count := func(q string, args ...any) int {
		var n int
		if err := w.Admin.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	newSecret := func(team, name string) string {
		id := pgtest.NewID("sec")
		var path string
		if err := w.Admin.QueryRow(t.Context(), `SELECT afs.secret_path($1, NULL, $2)`, team, name).Scan(&path); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Admin.Exec(t.Context(), `INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin) VALUES ($1, $2, $3, $4, 'managed')`,
			id, team, name, "arn:aws:secretsmanager:us-east-1:111122223333:secret:"+path+"-AbCdEf"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	secretID, otherSecret := newSecret(w.TeamA.ID, "slack"), newSecret(w.TeamB.ID, "slack")

	email := `{"name":"ops","kind":"email","config":{"to":["ops@example.com"]},"eventTypes":["budget.*"],"enabled":true}`
	code, out, hdr := ldCall(t, c.Handler, http.MethodPost, base, adm, email, "Idempotency-Key", "nch-1")
	if code != 201 || hdr.Get("ETag") != `"1"` || !strings.Contains(out, `"secretId":null`) || !strings.Contains(out, `"phase":"Pending"`) ||
		!strings.Contains(out, `"healthy":null`) {
		t.Fatalf("create: %d %s %v", code, out, hdr)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(out), &created)
	if code, again, h2 := ldCall(t, c.Handler, http.MethodPost, base, adm, email, "Idempotency-Key", "nch-1"); code != 201 || h2.Get("Idempotent-Replayed") != "true" || !sameJSON(again, out) {
		t.Fatalf("replay: %d %s", code, again)
	}
	if n := count(`SELECT count(*) FROM afs.notification_channels WHERE team_id = $1`, w.TeamA.ID); n != 1 {
		t.Fatalf("channels = %d after a replay", n)
	}

	for name, body := range map[string]string{ //nolint:gosec // request bodies with fake credentials, to prove they are refused
		"inline token":        `{"name":"s","kind":"slack","config":{"channel":"#a1"},"token":"xoxb-1"}`,
		"inline webhook url":  `{"name":"s","kind":"slack","config":{"channel":"#a1","webhookUrl":"https://hooks.example.com/x"},"secretId":"` + secretID + `"}`,
		"slack needs secret":  `{"name":"s","kind":"slack","config":{"channel":"#a1"}}`,
		"other team's secret": `{"name":"s","kind":"slack","config":{"channel":"#a1"},"secretId":"` + otherSecret + `"}`,
		"unknown secret":      `{"name":"s","kind":"slack","config":{"channel":"#a1"},"secretId":"` + pgtest.NewID("sec") + `"}`,
		"unknown event type":  `{"name":"s","kind":"email","config":{"to":["a@example.com"]},"eventTypes":["nope.*"]}`,
		"unknown kind":        `{"name":"s","kind":"sms","config":{}}`,
	} {
		if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, body); code != 400 || !strings.Contains(out, "ValidationError") {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	slack := `{"name":"slack","kind":"slack","config":{"channel":"#payments-alerts"},"secretId":"` + secretID + `","eventTypes":["budget.*","agent.status_changed"]}`
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, slack); code != 201 || !strings.Contains(out, secretID) {
		t.Fatalf("slack with a team secret: %d %s", code, out)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, email); code != 409 {
		t.Fatalf("duplicate name: %d", code)
	}
	if n := count(`SELECT count(*) FROM afs.notification_channels WHERE team_id = $1`, w.TeamA.ID); n != 2 {
		t.Fatalf("channels = %d", n)
	}

	// A viewer lists and describes, never writes: one denied audit record per create.
	if code, out, _ := ldCall(t, c.Handler, http.MethodGet, base, view, ""); code != 200 || !strings.Contains(out, created.ID) {
		t.Fatalf("viewer list: %d %s", code, out)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, base+"/"+created.ID, view, ""); code != 200 {
		t.Fatalf("viewer get: %d", code)
	}
	denied := func() int {
		return count(`SELECT count(*) FROM afs.audit_log WHERE team_id = $1 AND outcome = 'denied'`, w.TeamA.ID)
	}
	before := denied()
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, view, email); code != 403 || !strings.Contains(out, "notifications:write") {
		t.Fatalf("viewer create: %d %s", code, out)
	}
	if got := denied(); got != before+1 {
		t.Fatalf("viewer create wrote %d denied audit records, want 1", got-before)
	}
	one := base + "/" + created.ID
	for _, rq := range []struct{ method, path, body string }{{http.MethodPatch, one, `{"enabled":false}`}, {http.MethodDelete, one, ""}, {http.MethodPost, one + ":test", ""}} {
		if code, _, _ := ldCall(t, c.Handler, rq.method, rq.path, view, rq.body, "If-Match", `"1"`); code != 403 {
			t.Errorf("viewer %s %s: %d", rq.method, rq.path, code)
		}
	}

	// Update and delete.
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"enabled":false}`); code != 428 {
		t.Fatalf("update without If-Match: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"enabled":false}`, "If-Match", `"9"`); code != 412 {
		t.Fatalf("stale update: %d", code)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"secretId":"`+secretID+`"}`, "If-Match", `"1"`); code != 400 {
		t.Fatalf("secret on email: %d %s", code, out)
	}
	code, out, hdr = ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"enabled":false,"eventTypes":["team.*"]}`, "If-Match", `"1"`)
	if code != 200 || hdr.Get("ETag") != `"2"` || !strings.Contains(out, `"enabled":false`) || !strings.Contains(out, `"team.*"`) {
		t.Fatalf("update: %d %s %v", code, out, hdr)
	}

	// RA-US-17.3: the test returns the reconciler's result. Play the reconciler: record a failure for the
	// test id found in the outbox.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			var tid string
			err := w.Admin.QueryRow(t.Context(), `SELECT payload #>> '{data,testId}' FROM afs.outbox_events WHERE event_type = 'notification.test_requested' AND aggregate_id = $1 ORDER BY id DESC LIMIT 1`, created.ID).Scan(&tid)
			if err == nil && tid != "" {
				detail, _ := json.Marshal(domain.DeliveryDetail{LastTestID: tid, TestDelivered: new(bool), TestError: "slack: HTTP 404 no_service"})
				conds, _ := json.Marshal([]domain.Condition{{Type: domain.CondDeliveryHealthy, Status: domain.ConditionFalse, LastTransitionTime: time.Now()},
					{Type: domain.CondDeliveryDetail, Status: domain.ConditionTrue, Message: string(detail), LastTransitionTime: time.Now()}})
				_, _ = w.Admin.Exec(t.Context(), `INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase, conditions) VALUES ('notification_channel', $1, $2, 'Degraded', $3)`, created.ID, w.TeamA.ID, conds)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	code, out, _ = ldCall(t, c.Handler, http.MethodPost, one+":test", adm, "")
	<-done
	if code != 200 || !strings.Contains(out, `"delivered":false`) || !strings.Contains(out, "no_service") || !strings.Contains(out, `"testId":"ntest_`) {
		t.Fatalf("test: %d %s", code, out)
	}
	if code, out, _ = ldCall(t, c.Handler, http.MethodGet, one, adm, ""); code != 200 || !strings.Contains(out, `"healthy":false`) || !strings.Contains(out, `"lastTestId":"ntest_`) || !strings.Contains(out, `"lastError":null`) {
		t.Fatalf("channel shows the failure: %d %s", code, out)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'notification.test_requested' AND team_id = $1`, w.TeamA.ID); n != 1 {
		t.Fatalf("test events = %d", n)
	}

	code, out, _ = ldCall(t, c.Handler, http.MethodDelete, one, adm, "", "If-Match", `"2"`)
	if code != 202 || !strings.Contains(out, `"phase":"Deleting"`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, one, adm, ""); code != 404 {
		t.Fatalf("get after delete: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/teams/"+w.TeamB.ID+"/notification-channels", adm, ""); code != 404 {
		t.Fatalf("another team's list: %d", code)
	}

	// Service-level channels: admin:settings only, no secret, 409 while the work order setting names one.
	sbase := "/v1/admin/notification-channels"
	svcBody := `{"name":"wo-mail","kind":"email","config":{"to":["owners@example.com"]},"enabled":true}`
	if code, _, _ := ldCall(t, c.Handler, http.MethodPost, sbase, adm, svcBody); code != 403 {
		t.Fatalf("team admin creates a service channel: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, sbase, view, ""); code != 403 {
		t.Fatalf("viewer lists service channels: %d", code)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, sbase, svc, slack); code != 400 {
		t.Fatalf("service slack: %d %s", code, out)
	}
	code, out, _ = ldCall(t, c.Handler, http.MethodPost, sbase, svc, svcBody)
	if code != 201 {
		t.Fatalf("service create: %d %s", code, out)
	}
	var sc struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(out), &sc)
	if code, out, _ := ldCall(t, c.Handler, http.MethodGet, sbase, svc, ""); code != 200 || !strings.Contains(out, sc.ID) {
		t.Fatalf("service list: %d %s", code, out)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, base+"/"+sc.ID, adm, ""); code != 404 {
		t.Fatalf("service channel through a team route: %d", code)
	}
	if _, err := w.Admin.Exec(t.Context(), `INSERT INTO afs.service_settings (key, value) VALUES ('work_orders.notification_channel_id', to_jsonb($1::text))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, sc.ID); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodDelete, sbase+"/"+sc.ID, svc, "", "If-Match", `"1"`); code != 409 || !strings.Contains(out, "work_orders.notification_channel_id") {
		t.Fatalf("delete of the named channel: %d %s", code, out)
	}
	if _, err := w.Admin.Exec(t.Context(), `UPDATE afs.service_settings SET value = 'null'::jsonb WHERE key = 'work_orders.notification_channel_id'`); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodPatch, sbase+"/"+sc.ID, svc, `{"enabled":false}`, "If-Match", `"1"`); code != 200 {
		t.Fatalf("service update: %d %s", code, out)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodDelete, sbase+"/"+sc.ID, svc, "", "If-Match", `"2"`); code != 202 {
		t.Fatalf("service delete: %d %s", code, out)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE action LIKE '%NotificationChannel' AND outcome = 'allowed'`); n < 6 {
		t.Fatalf("allowed audit records = %d", n)
	}
}
