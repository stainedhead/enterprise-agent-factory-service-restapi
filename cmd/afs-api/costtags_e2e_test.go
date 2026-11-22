package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// RA-US-18.1 and RA-US-18.2 through the real pipeline and database. A service admin lists the tag keys with
// seenInCur and awsStatus (null timestamps for a key the CUR has not shown), in pages; a team-admin gets
// 403 and one denied audit record. PutSetting billing.tag_activation_acknowledged = true by a service admin
// is stored and audited, a non-boolean value is a 400 that writes nothing, and a team-admin gets 403.
func TestCostTagStatusAndTheAcknowledgedSetting(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	admin, teamAdmin := w.Tokens[matrix.ServiceAdmin], w.Tokens[matrix.TeamAdmin]
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DELETE FROM afs.cost_allocation_tag_status`)
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	exec(`INSERT INTO afs.cost_allocation_tag_status (key, seen_in_cur, first_seen_at, last_seen_at, aws_status, checked_at) VALUES
('afs-team-id', true, $1, $2, 'active', $2), ('business-unit', false, NULL, NULL, 'inactive', $2), ('cost-center', false, NULL, NULL, 'unknown', $2)`,
		day, day.Add(20*24*time.Hour))

	code, out, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/admin/cost-tag-status?maxResults=2", admin, "")
	var page struct {
		Items []struct {
			Key         string  `json:"key"`
			SeenInCur   bool    `json:"seenInCur"`
			FirstSeenAt *string `json:"firstSeenAt"`
			LastSeenAt  *string `json:"lastSeenAt"`
			AwsStatus   string  `json:"awsStatus"`
			CheckedAt   string  `json:"checkedAt"`
		} `json:"items"`
		NextToken string `json:"nextToken"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil || code != 200 {
		t.Fatalf("list: %d %s", code, out)
	}
	if len(page.Items) != 2 || page.NextToken == "" || page.Items[0].Key != "afs-team-id" || !page.Items[0].SeenInCur ||
		page.Items[0].FirstSeenAt == nil || *page.Items[0].FirstSeenAt != "2026-11-01T00:00:00Z" || page.Items[0].AwsStatus != "active" ||
		page.Items[1].Key != "business-unit" || page.Items[1].SeenInCur || page.Items[1].FirstSeenAt != nil || page.Items[1].LastSeenAt != nil || page.Items[1].AwsStatus != "inactive" {
		t.Fatalf("first page: %s", out)
	}
	if !strings.Contains(out, `"firstSeenAt":null`) {
		t.Errorf("an unseen key must show null timestamps: %s", out)
	}
	code, out, _ = ldCall(t, c.Handler, http.MethodGet, "/v1/admin/cost-tag-status?maxResults=2&nextToken="+page.NextToken, admin, "")
	if code != 200 || !strings.Contains(out, `"cost-center"`) || strings.Contains(out, `"nextToken"`) {
		t.Fatalf("second page: %d %s", code, out)
	}

	denied := func() int {
		var n int
		if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'ListCostTagStatus' AND outcome = 'denied'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := denied()
	if code, out, _ := ldCall(t, c.Handler, http.MethodGet, "/v1/admin/cost-tag-status", teamAdmin, ""); code != 403 || !strings.Contains(out, "admin:cost-tag-status") {
		t.Errorf("team admin: %d %s", code, out)
	}
	if got := denied(); got != before+1 {
		t.Errorf("denied audit records %d -> %d", before, got)
	}

	// The banner: acknowledging it is a PutSetting, stored and audited.
	const settingPath = "/v1/admin/settings/billing.tag_activation_acknowledged"
	auditCount := func() int {
		var n int
		if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'PutSetting' AND resource_id = 'billing.tag_activation_acknowledged' AND outcome = 'allowed'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec(`DELETE FROM afs.service_settings WHERE key = 'billing.tag_activation_acknowledged'`)
	audits := auditCount()
	for _, bad := range []string{`{"value":"yes"}`, `{"value":1}`, `{"value":null}`, `{"value":[true]}`, `{}`} {
		if code, out, _ := ldCall(t, c.Handler, http.MethodPut, settingPath, admin, bad); code != 400 {
			t.Errorf("PutSetting %s: %d %s, want 400", bad, code, out)
		}
	}
	var n int
	if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.service_settings WHERE key = 'billing.tag_activation_acknowledged'`).Scan(&n); err != nil || n != 0 || auditCount() != audits {
		t.Fatalf("a refused value wrote a setting (%d rows) or an audit record (%d -> %d)", n, audits, auditCount())
	}
	if code, out, _ := ldCall(t, c.Handler, http.MethodPut, settingPath, teamAdmin, `{"value":true}`); code != 403 {
		t.Errorf("team admin PutSetting: %d %s, want 403", code, out)
	}
	code, out, _ = ldCall(t, c.Handler, http.MethodPut, settingPath, admin, `{"value":true}`)
	if code != 200 || !strings.Contains(out, `"value":true`) {
		t.Fatalf("acknowledge: %d %s", code, out)
	}
	var stored string
	if err := w.Admin.QueryRow(t.Context(), `SELECT value::text FROM afs.service_settings WHERE key = 'billing.tag_activation_acknowledged'`).Scan(&stored); err != nil || stored != "true" {
		t.Errorf("stored value %q %v", stored, err)
	}
	if got := auditCount(); got != audits+1 {
		t.Errorf("PutSetting audit records %d -> %d, want one more", audits, got)
	}
}
