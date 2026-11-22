package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

func ldCall(t *testing.T, h http.Handler, method, path, token, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.89:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b), rec.Header()
}

// RA-US-17.1 and RA-US-17.2 through the real pipeline and database: a team admin manages log
// destinations with credentials only as a secret reference; an inline credential is a 400; a viewer
// can list and describe but not create (403 and one denied audit record).
func TestLogDestinationsEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	adm, view := w.Tokens[matrix.TeamAdmin], w.Tokens[matrix.Viewer]
	base := "/v1/teams/" + w.TeamA.ID + "/log-destinations"
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
	secretID, otherSecret := newSecret(w.TeamA.ID, "hec"), newSecret(w.TeamB.ID, "hec")

	// Create: CloudWatch needs no credential.
	cw := `{"name":"cw","kind":"cloudwatch","config":{"logGroup":"/afs/payments","region":"us-east-1"},"enabled":true}`
	code, out, hdr := ldCall(t, c.Handler, http.MethodPost, base, adm, cw, "Idempotency-Key", "ld-1")
	if code != 201 || hdr.Get("ETag") != `"1"` || !strings.Contains(out, `"secretId":null`) || !strings.Contains(out, `"phase":"Pending"`) {
		t.Fatalf("create: %d %s %v", code, out, hdr)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(out), &created)
	if code, again, h2 := ldCall(t, c.Handler, http.MethodPost, base, adm, cw, "Idempotency-Key", "ld-1"); code != 201 || h2.Get("Idempotent-Replayed") != "true" || !sameJSON(again, out) {
		t.Fatalf("replay: %d %s", code, again)
	}
	if n := count(`SELECT count(*) FROM afs.log_destinations WHERE team_id = $1`, w.TeamA.ID); n != 1 {
		t.Fatalf("destinations = %d after a replay", n)
	}

	// A credential is a reference, never inline.
	for name, body := range map[string]string{
		"top-level token":     `{"name":"sp","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088"},"token":"abc123"}`,
		"top-level value":     `{"name":"sp","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088"},"value":"abc123"}`,
		"config token":        `{"name":"sp","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088","hecToken":"abc123"},"secretId":"` + secretID + `"}`,
		"credential in URL":   `{"name":"sp","kind":"otlp","config":{"endpoint":"https://user:pw@otel.example.com"}}`,
		"secret required":     `{"name":"sp","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088"}}`,
		"other team's secret": `{"name":"sp","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088"},"secretId":"` + otherSecret + `"}`,
		"unknown secret":      `{"name":"sp","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088"},"secretId":"` + pgtest.NewID("sec") + `"}`,
	} {
		if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, body); code != 400 || !strings.Contains(out, "ValidationError") {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if n := count(`SELECT count(*) FROM afs.log_destinations WHERE team_id = $1`, w.TeamA.ID); n != 1 {
		t.Fatalf("a refused create left %d destinations", n)
	}
	hec := `{"name":"hec","kind":"splunk_hec","config":{"endpoint":"https://splunk.example.com:8088","index":"afs"},"secretId":"` + secretID + `"}`
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, hec); code != 201 || !strings.Contains(out, secretID) {
		t.Fatalf("splunk with a team secret: %d %s", code, out)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, adm, cw); code != 409 {
		t.Fatalf("duplicate name: %d %s", code, out)
	}

	// A viewer lists and describes, never creates, updates or deletes: one denied audit record each.
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
	if code, out, _ := ldCall(t, c.Handler, http.MethodPost, base, view, cw); code != 403 || !strings.Contains(out, "logs:write") {
		t.Fatalf("viewer create: %d %s", code, out)
	}
	if got := denied(); got != before+1 {
		t.Fatalf("viewer create wrote %d denied audit records, want 1", got-before)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, base+"/"+created.ID, view, `{"enabled":false}`, "If-Match", `"1"`); code != 403 {
		t.Fatalf("viewer update: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodDelete, base+"/"+created.ID, view, "", "If-Match", `"1"`); code != 403 {
		t.Fatalf("viewer delete: %d", code)
	}

	// Update: If-Match required, stale refused, a patch is validated as a whole.
	one := base + "/" + created.ID
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"enabled":false}`); code != 428 {
		t.Fatalf("update without If-Match: %d", code)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"enabled":false}`, "If-Match", `"9"`); code != 412 {
		t.Fatalf("stale update: %d", code)
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"secretId":"`+secretID+`"}`, "If-Match", `"1"`); code != 400 {
		t.Fatalf("secret on cloudwatch: %d %s", code, out)
	}
	code, out, hdr = ldCall(t, c.Handler, http.MethodPatch, one, adm, `{"enabled":false,"config":{"logGroup":"/afs/other","region":"eu-west-1"}}`, "If-Match", `"1"`)
	if code != 200 || hdr.Get("ETag") != `"2"` || !strings.Contains(out, `"enabled":false`) || !strings.Contains(out, `"generation":2`) {
		t.Fatalf("update: %d %s %v", code, out, hdr)
	}

	// Delete: 202 with the destination in phase Deleting; the row is gone.
	if code, _, _ := ldCall(t, c.Handler, http.MethodDelete, one, adm, ""); code != 428 {
		t.Fatalf("delete without If-Match: %d", code)
	}
	code, out, _ = ldCall(t, c.Handler, http.MethodDelete, one, adm, "", "If-Match", `"2"`)
	if code != 202 || !strings.Contains(out, `"phase":"Deleting"`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, one, adm, ""); code != 404 {
		t.Fatalf("get after delete: %d", code)
	}
	// Audit records exist for the allowed mutations, and none carries a credential value.
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE team_id = $1 AND action LIKE '%LogDestination' AND outcome = 'allowed'`, w.TeamA.ID); n != 4 {
		t.Fatalf("allowed audit records = %d, want 4 (create x2, update, delete)", n)
	}
	// A destination in team B is invisible through team A's path.
	if code, _, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/teams/"+w.TeamB.ID+"/log-destinations", adm, ""); code != 404 {
		t.Fatalf("another team's list: %d", code)
	}
}
