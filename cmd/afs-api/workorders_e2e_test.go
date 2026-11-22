package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

const woImage = "registry.example.com/agents/fixer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestWorkOrdersEndToEnd drives M6-B and M6-C through the real router, authentication and database:
// owner resolution from AFS_OWNED_REPOS (exact beats wildcard, ambiguous, unowned, explicit wins,
// deleted teams skipped, unknown explicit team), nothing written on a refusal, the one open work
// order per CVE, a disabled template, visibility (assigned team, owner team, unrelated team),
// repository progress reports, cancel and the template delete rules.
func TestWorkOrdersEndToEnd(t *testing.T) {
	m := newM2W1(t)
	w := m.w
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
	teamA, teamB := w.TeamA.ID, w.TeamB.ID
	teamC, teamD := w.DB.SeedTeam(t), w.DB.SeedTeam(t)
	owned := func(team, value string) {
		exec(`INSERT INTO afs.namespace_config_values (id, team_id, name, config_value) VALUES ($1, $2, 'AFS_OWNED_REPOS', $3)`, pgtest.NewID("ncv"), team, value)
	}
	owned(teamA, "github.com/wosec/*")
	owned(teamB, "github.com/wosec/special GITHUB.com/shared/lib")
	owned(teamC.ID, "github.com/shared/lib")
	owned(teamD.ID, "github.com/gone/*")
	exec(`UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, teamD.ID)

	// People bound to team B and C (owner team and unrelated team).
	bind := func(caller matrix.Caller, team, role string) {
		group := "e2e-" + string(caller)
		exec(`INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, $2, $3, 'group', $4)`, pgtest.NewID("rb"), role, team, group)
		w.Tokens[caller] = w.IDP.Token(t, "sub-"+string(caller), string(caller)+"@example.com", []string{group})
	}
	const ownerB, otherC matrix.Caller = "team-b-admin", "team-c-admin"
	bind(ownerB, teamB, "team_admin")
	bind(otherC, teamC.ID, "team_admin")
	const unrelated matrix.Caller = "team-e-admin"
	bind(unrelated, w.DB.SeedTeam(t).ID, "team_admin")

	// Swarm templates: service admin only for writes; reads include the security operator.
	spec := fmt.Sprintf(`{"stages":[],"pools":[{"name":"fixer","template":"coder","image":%q,"modelId":"e2e-model"}]}`, woImage)
	tplBody := func(name string) string {
		return `{"name":"` + name + `","kind":"cve_remediation","spec":` + spec + `,"budgetUsd":"250.00","ttlHours":48,"teardownRule":"at_work_complete"}`
	}
	m.expect(http.StatusForbidden, "POST", "/v1/admin/swarm-templates", matrix.SecurityOperator, tplBody("denied-tpl"))
	m.expect(http.StatusForbidden, "POST", "/v1/admin/swarm-templates", matrix.TeamAdmin, tplBody("denied-tpl"))
	if count(`SELECT count(*) FROM afs.swarm_templates WHERE name = 'denied-tpl'`) != 0 {
		t.Fatal("a refused create wrote a template")
	}
	obj, h := m.expect(http.StatusCreated, "POST", "/v1/admin/swarm-templates", matrix.ServiceAdmin, tplBody("e2e-cve"), "Idempotency-Key", "tpl-1")
	tpl := obj["id"].(string)
	if obj["budgetUsd"] != "250.00" || obj["enabled"] != true || h.Get("ETag") != `"1"` || !strings.HasPrefix(tpl, "swt_") {
		t.Fatalf("%v %v", obj, h)
	}
	if o, hh := m.expect(http.StatusCreated, "POST", "/v1/admin/swarm-templates", matrix.ServiceAdmin, tplBody("e2e-cve"), "Idempotency-Key", "tpl-1"); o["id"] != tpl || hh.Get("Idempotent-Replayed") != "true" {
		t.Errorf("replay %v %v", o, hh)
	}
	m.expect(http.StatusConflict, "POST", "/v1/admin/swarm-templates", matrix.ServiceAdmin, tplBody("e2e-cve"))
	bad := strings.Replace(tplBody("e2e-bad"), `"modelId":"e2e-model"`, `"modelId":""`, 1)
	m.expect(http.StatusBadRequest, "POST", "/v1/admin/swarm-templates", matrix.ServiceAdmin, bad)
	for _, c := range []matrix.Caller{matrix.SecurityOperator, matrix.ServiceKey} {
		m.expect(http.StatusOK, "GET", "/v1/admin/swarm-templates/"+tpl, c, "")
	}
	m.expect(http.StatusForbidden, "GET", "/v1/admin/swarm-templates/"+tpl, matrix.TeamAdmin, "")

	// Owner resolution.
	repos := func(entries ...string) string { return `[` + strings.Join(entries, ",") + `]` }
	repo := func(host, name string, owners ...string) string {
		s := `{"host":"` + host + `","repo":"` + name + `"`
		for _, o := range owners {
			s += o
		}
		return s + `}`
	}
	create := func(cve string, r string) string {
		return `{"cveId":"` + cve + `","severity":"critical","title":"OpenSSL RCE","slaDueAt":"` + time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339) +
			`","templateId":"` + tpl + `","repos":` + r + `}`
	}
	before := count(`SELECT count(*) FROM afs.work_orders`)
	auditBefore := count(`SELECT count(*) FROM afs.audit_log WHERE action = 'CreateWorkOrder' AND outcome = 'allowed'`)
	obj, _ = m.expect(http.StatusBadRequest, "POST", "/v1/work-orders", matrix.ServiceKey, create("CVE-2026-2001",
		repos(repo("github.com", "shared/lib"), repo("github.com", "gone/x"), repo("github.com", "wosec/api"))))
	details := obj["details"].(map[string]any)
	amb := details["ambiguousRepos"].([]any)[0].(map[string]any)
	ids := amb["teamIds"].([]any)
	if amb["repo"] != "github.com/shared/lib" || len(ids) != 2 || !anyContains(ids, teamB) || !anyContains(ids, teamC.ID) {
		t.Errorf("ambiguous: %v", details)
	}
	if un := details["unownedRepos"].([]any); len(un) != 1 || un[0] != "github.com/gone/x" {
		t.Errorf("unowned (a deleted team is skipped): %v", details)
	}
	obj, _ = m.expect(http.StatusBadRequest, "POST", "/v1/work-orders", matrix.ServiceKey, create("CVE-2026-2001",
		repos(repo("github.com", "wosec/api", `,"owningTeamId":"`+teamD.ID+`"`))))
	if !strings.Contains(fmt.Sprint(obj["details"]), "owningTeamId") {
		t.Errorf("deleted explicit team: %v", obj)
	}
	m.expect(http.StatusBadRequest, "POST", "/v1/work-orders", matrix.ServiceKey, create("CVE-2026-2001",
		repos(repo("github.com", "wosec/api", `,"owningTeamId":"team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ"`))))
	if n := count(`SELECT count(*) FROM afs.work_orders`); n != before {
		t.Fatalf("refused creates wrote %d work orders", n-before)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE action = 'CreateWorkOrder' AND outcome = 'allowed'`) - auditBefore; n != 0 {
		t.Errorf("refused creates wrote %d audit rows", n)
	}

	events0 := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'work_order.created'`)
	obj, h = m.expect(http.StatusCreated, "POST", "/v1/work-orders", matrix.ServiceKey, create("CVE-2026-2001", repos(
		repo("github.com", "wosec/api"),                                            // wildcard: team A
		repo("github.com", "wosec/special"),                                        // exact beats the wildcard: team B
		repo("github.com", "wosec/contact", `,"owningContact":"lead@example.com"`), // explicit contact
		repo("github.com", "wosec/both", `,"owningTeamId":"`+teamC.ID+`"`),         // explicit wins over config (A)
	)), "Idempotency-Key", "wo-1")
	wo := obj["id"].(string)
	got := map[string]map[string]any{}
	for _, r := range obj["repos"].([]any) {
		rm := r.(map[string]any)
		got[rm["repo"].(string)] = rm
	}
	if got["wosec/api"]["owningTeamId"] != teamA || got["wosec/api"]["ownerSource"] != "team_config" ||
		got["wosec/special"]["owningTeamId"] != teamB || got["wosec/special"]["ownerSource"] != "team_config" ||
		got["wosec/contact"]["owningContact"] != "lead@example.com" || got["wosec/contact"]["ownerSource"] != "explicit" ||
		got["wosec/both"]["owningTeamId"] != teamC.ID || got["wosec/both"]["ownerSource"] != "explicit" {
		t.Fatalf("owners: %v", got)
	}
	if obj["status"] != "open" || obj["budgetUsd"] != "250.00" || obj["slaState"] != "on_track" || obj["teamId"] != nil ||
		obj["source"] != "api" || h.Get("ETag") != `"1"` {
		t.Fatalf("%v", obj)
	}
	if c := obj["progress"].(map[string]any); c["repos"] != float64(4) || c["remaining"] != float64(4) {
		t.Errorf("progress %v", c)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'work_order.created'`); n != events0+1 {
		t.Errorf("work_order.created events: %d", n-events0)
	}
	// Stored without ownerSource; Get shows the resolved owners.
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo, matrix.ServiceAdmin, "")
	for _, r := range obj["repos"].([]any) {
		if _, has := r.(map[string]any)["ownerSource"]; has {
			t.Errorf("ownerSource is not stored: %v", r)
		}
	}
	// Replay, second open work order for the CVE, another CVE.
	_, h = m.expect(http.StatusCreated, "POST", "/v1/work-orders", matrix.ServiceKey, create("CVE-2026-2001", repos(
		repo("github.com", "wosec/api"), repo("github.com", "wosec/special"),
		repo("github.com", "wosec/contact", `,"owningContact":"lead@example.com"`), repo("github.com", "wosec/both", `,"owningTeamId":"`+teamC.ID+`"`))), "Idempotency-Key", "wo-1")
	if h.Get("Idempotent-Replayed") != "true" {
		t.Error("not a replay")
	}
	m.expect(http.StatusConflict, "POST", "/v1/work-orders", matrix.SecurityOperator, create("CVE-2026-2001", repos(repo("github.com", "wosec/api"))))
	for _, c := range []matrix.Caller{matrix.TeamAdmin, matrix.Viewer, matrix.TeamKeyAdmin} {
		m.expect(http.StatusForbidden, "POST", "/v1/work-orders", c, create("CVE-2026-2002", repos(repo("github.com", "wosec/api"))))
	}

	// A disabled template refuses new work orders and changes nothing else.
	m.expect(http.StatusForbidden, "POST", "/v1/admin/swarm-templates/"+tpl+":disable", matrix.SecurityOperator, "")
	obj, _ = m.expect(http.StatusOK, "POST", "/v1/admin/swarm-templates/"+tpl+":disable", matrix.ServiceAdmin, "")
	if obj["enabled"] != false {
		t.Fatalf("%v", obj)
	}
	obj, _ = m.expect(http.StatusBadRequest, "POST", "/v1/work-orders", matrix.ServiceAdmin, create("CVE-2026-2003", repos(repo("github.com", "wosec/api"))))
	if !strings.Contains(fmt.Sprint(obj["details"]), "disabled") {
		t.Errorf("%v", obj)
	}
	m.expect(http.StatusOK, "POST", "/v1/admin/swarm-templates/"+tpl+":enable", matrix.ServiceAdmin, "")

	// Assign to team A (the reconciler's step).
	exec(`UPDATE afs.work_orders SET team_id = $2, status = 'assigned', assigned_at = now() WHERE id = $1`, wo, teamA)

	// Visibility: assigned team sees all repos; owner team B sees only its own; unrelated C sees nothing.
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo, matrix.TeamAdmin, "")
	if len(obj["repos"].([]any)) != 4 || obj["teamId"] != teamA {
		t.Errorf("assigned team: %v", obj)
	}
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo, ownerB, "")
	if r := obj["repos"].([]any); len(r) != 1 || r[0].(map[string]any)["repo"] != "wosec/special" {
		t.Errorf("owner team sees only its repositories: %v", obj["repos"])
	}
	m.expect(http.StatusNotFound, "GET", "/v1/work-orders/"+wo, unrelated, "")
	m.expect(http.StatusNotFound, "GET", "/v1/work-orders/"+wo+"/repos", unrelated, "")
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo, otherC, "")
	if r := obj["repos"].([]any); len(r) != 1 || r[0].(map[string]any)["repo"] != "wosec/both" {
		t.Errorf("explicit owner sees its repository: %v", obj["repos"])
	}
	m.expect(http.StatusNotFound, "PATCH", "/v1/work-orders/"+wo+"/repos/github.com/wosec/api", unrelated, `{"status":"in_progress"}`, "If-Match", `"1"`)
	m.expect(http.StatusNotFound, "GET", "/v1/work-orders/wo_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", matrix.TeamAdmin, "")
	list, _ := m.expect(http.StatusOK, "GET", "/v1/work-orders", unrelated, "")
	if len(items(t, list)) != 0 {
		t.Errorf("unrelated team lists %v", list)
	}
	for _, c := range []matrix.Caller{matrix.TeamAdmin, ownerB, matrix.Viewer} {
		list, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders", c, "")
		if find(items(t, list), "id", wo) == nil {
			t.Errorf("%s does not list the work order", c)
		}
	}
	for _, c := range []matrix.Caller{matrix.SecurityOperator, matrix.ServiceKey, matrix.ServiceAdmin} {
		list, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders?teamId="+teamA+"&status=assigned&severity=critical", c, "")
		if find(items(t, list), "id", wo) == nil {
			t.Errorf("%s: filtered list misses the work order", c)
		}
	}
	list, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders?teamId="+teamC.ID, matrix.SecurityOperator, "")
	if len(items(t, list)) != 1 { // team C owns wosec/both explicitly
		t.Errorf("teamId filter matches owner teams too: %v", list)
	}
	m.expect(http.StatusBadRequest, "GET", "/v1/work-orders?status=weird", matrix.SecurityOperator, "")
	m.expect(http.StatusBadRequest, "GET", "/v1/work-orders?teamId=x", matrix.SecurityOperator, "")
	list, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders?slaState=on_track", matrix.SecurityOperator, "")
	if find(items(t, list), "id", wo) == nil {
		t.Error("slaState filter")
	}
	list, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders?slaState=breached", matrix.SecurityOperator, "")
	if find(items(t, list), "id", wo) != nil {
		t.Error("slaState filter matches the wrong state")
	}
	// Team route.
	list, _ = m.expect(http.StatusOK, "GET", "/v1/teams/"+teamA+"/work-orders", matrix.TeamAdmin, "")
	if find(items(t, list), "id", wo) == nil {
		t.Error("ListTeamWorkOrders")
	}
	m.expect(http.StatusForbidden, "GET", "/v1/teams/"+teamA+"/work-orders", matrix.SecurityOperator, "")
	m.expect(http.StatusForbidden, "GET", "/v1/teams/"+teamA+"/work-orders", matrix.ServiceKey, "")
	m.expect(http.StatusNotFound, "GET", "/v1/teams/"+teamA+"/work-orders", unrelated, "")
	// CSV.
	csvReq := func() string {
		_, _, hh, raw := m.call("GET", "/v1/work-orders", matrix.SecurityOperator, "", "Accept", "text/csv")
		if !strings.HasPrefix(hh.Get("Content-Type"), "text/csv") {
			t.Errorf("content type %q", hh.Get("Content-Type"))
		}
		return raw
	}
	if raw := csvReq(); !strings.HasPrefix(raw, "id,cveId,severity,title,status,slaState,teamId,slaDueAt") || !strings.Contains(raw, wo+",CVE-2026-2001,critical,OpenSSL RCE,assigned,on_track,"+teamA) {
		t.Errorf("csv:\n%s", raw)
	}

	// Repository reports.
	rp := "/v1/work-orders/" + wo + "/repos/github.com/wosec/api"
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo+"/repos", matrix.TeamAdmin, "")
	api := find(items(t, obj), "repo", "wosec/api")
	rver := fmt.Sprint(int64(api["version"].(float64)))
	m.expect(http.StatusForbidden, "PATCH", rp, matrix.Viewer, `{"status":"in_progress"}`, "If-Match", `"`+rver+`"`)
	m.expect(http.StatusForbidden, "PATCH", rp, matrix.TeamKeyViewer, `{"status":"in_progress"}`, "If-Match", `"`+rver+`"`)
	m.expect(http.StatusForbidden, "PATCH", rp, ownerB, `{"status":"in_progress"}`, "If-Match", `"`+rver+`"`) // owner, not assigned
	m.expect(http.StatusBadRequest, "PATCH", rp, matrix.TeamAdmin, `{"status":"pr_opened"}`, "If-Match", `"`+rver+`"`)
	m.expect(http.StatusBadRequest, "PATCH", rp, matrix.TeamAdmin, `{"status":"pr_opened","prUrl":"http://x"}`, "If-Match", `"`+rver+`"`)
	m.expect(http.StatusPreconditionRequired, "PATCH", rp, matrix.TeamAdmin, `{"status":"in_progress"}`)
	m.expect(http.StatusPreconditionFailed, "PATCH", rp, matrix.TeamAdmin, `{"status":"in_progress"}`, "If-Match", `"99"`)
	ev0 := count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type IN ('work_order.repo_updated', 'work_order.pr_opened')`, wo)
	obj, h = m.expect(http.StatusOK, "PATCH", rp, matrix.TeamKeyAdmin, `{"status":"pr_opened","prUrl":"https://github.com/wosec/api/pull/7","prNumber":7,"detail":"bumped openssl"}`, "If-Match", `"`+rver+`"`)
	if obj["status"] != "pr_opened" || obj["prNumber"] != float64(7) || h.Get("ETag") == `"`+rver+`"` {
		t.Fatalf("%v", obj)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type IN ('work_order.repo_updated', 'work_order.pr_opened')`, wo); n != ev0+2 {
		t.Errorf("repo_updated and pr_opened events: %d", n-ev0)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE action = 'UpdateWorkOrderRepo' AND outcome = 'allowed' AND resource_id = $1`, wo); n != 1 {
		t.Errorf("audit rows %d", n)
	}
	// The security operator reports too; merged is terminal.
	nv := fmt.Sprint(int64(obj["version"].(float64)))
	obj, _ = m.expect(http.StatusOK, "PATCH", rp, matrix.SecurityOperator, `{"status":"merged"}`, "If-Match", `"`+nv+`"`)
	if obj["mergedAt"] == nil {
		t.Errorf("%v", obj)
	}
	m.expect(http.StatusConflict, "PATCH", rp, matrix.SecurityOperator, `{"status":"failed"}`, "If-Match", `"`+fmt.Sprint(int64(obj["version"].(float64)))+`"`)
	m.expect(http.StatusNotFound, "PATCH", "/v1/work-orders/"+wo+"/repos/github.com/wosec/none", matrix.TeamAdmin, `{"status":"in_progress"}`, "If-Match", `"1"`)
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo, matrix.ServiceAdmin, "")
	if p := obj["progress"].(map[string]any); p["prsMerged"] != float64(1) || p["remaining"] != float64(3) {
		t.Errorf("progress %v", p)
	}
	// Notifications written by the reconciler show on the repository.
	exec(`INSERT INTO afs.work_order_notifications (work_order_id, host, repo, kind, recipient) VALUES ($1, 'github.com', 'wosec/api', 'pr_opened', 'lead@example.com')`, wo)
	obj, _ = m.expect(http.StatusOK, "GET", "/v1/work-orders/"+wo, matrix.ServiceAdmin, "")
	if n := find(items(t, map[string]any{"items": obj["repos"]}), "repo", "wosec/api")["notifications"].([]any); len(n) != 1 {
		t.Errorf("notifications %v", n)
	}

	// Update: title, SLA, cap, added repositories (resolved like create); write holders only.
	ver := obj["version"].(float64)
	ifm := `"` + fmt.Sprint(int64(ver)) + `"`
	m.expect(http.StatusForbidden, "PATCH", "/v1/work-orders/"+wo, matrix.TeamAdmin, `{"title":"x"}`, "If-Match", ifm)
	m.expect(http.StatusBadRequest, "PATCH", "/v1/work-orders/"+wo, matrix.SecurityOperator, `{}`, "If-Match", ifm)
	m.expect(http.StatusBadRequest, "PATCH", "/v1/work-orders/"+wo, matrix.SecurityOperator, `{"addRepos":[{"repo":"nobody/owns"}]}`, "If-Match", ifm)
	obj, _ = m.expect(http.StatusOK, "PATCH", "/v1/work-orders/"+wo, matrix.SecurityOperator,
		`{"title":"Renamed","budgetUsd":"400.00","addRepos":[{"repo":"wosec/added"}]}`, "If-Match", ifm)
	if obj["title"] != "Renamed" || obj["budgetUsd"] != "400.00" || obj["progress"].(map[string]any)["repos"] != float64(5) {
		t.Errorf("%v", obj)
	}
	m.expect(http.StatusPreconditionFailed, "PATCH", "/v1/work-orders/"+wo, matrix.SecurityOperator, `{"title":"x"}`, "If-Match", ifm)

	// Delete rules: an open work order uses the template.
	_, h = m.expect(http.StatusOK, "GET", "/v1/admin/swarm-templates/"+tpl, matrix.ServiceAdmin, "")
	tver := h.Get("ETag")
	m.expect(http.StatusConflict, "DELETE", "/v1/admin/swarm-templates/"+tpl, matrix.ServiceAdmin, "", "If-Match", tver)
	m.expect(http.StatusForbidden, "DELETE", "/v1/admin/swarm-templates/"+tpl, matrix.SecurityOperator, "", "If-Match", tver)

	// Cancel.
	m.expect(http.StatusForbidden, "POST", "/v1/work-orders/"+wo+":cancel", matrix.TeamAdmin, `{"reason":"x"}`)
	m.expect(http.StatusBadRequest, "POST", "/v1/work-orders/"+wo+":cancel", matrix.SecurityOperator, `{"reason":""}`)
	obj, _ = m.expect(http.StatusOK, "POST", "/v1/work-orders/"+wo+":cancel", matrix.SecurityOperator, `{"reason":"fixed elsewhere"}`)
	if obj["status"] != "cancelled" || obj["cancelReason"] != "fixed elsewhere" || obj["completedAt"] == nil || obj["slaState"] != "met" {
		t.Fatalf("%v", obj)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'work_order.cancelled'`, wo); n != 1 {
		t.Errorf("cancelled events %d", n)
	}
	m.expect(http.StatusConflict, "POST", "/v1/work-orders/"+wo+":cancel", matrix.SecurityOperator, `{"reason":"again"}`)
	m.expect(http.StatusConflict, "PATCH", "/v1/work-orders/"+wo, matrix.SecurityOperator, `{"title":"x"}`, "If-Match", `"`+fmt.Sprint(int64(obj["version"].(float64)))+`"`)
	m.expect(http.StatusConflict, "PATCH", rp, matrix.TeamAdmin, `{"status":"in_progress"}`, "If-Match", `"1"`)
	// The CVE is free again; a closed work order still names its template, so delete refuses.
	m.expect(http.StatusCreated, "POST", "/v1/work-orders", matrix.SecurityOperator, create("CVE-2026-2001", repos(repo("github.com", "wosec/api"))))
	m.expect(http.StatusConflict, "DELETE", "/v1/admin/swarm-templates/"+tpl, matrix.ServiceAdmin, "", "If-Match", tver)
	// An unused template deletes; update and the rest.
	obj, h = m.expect(http.StatusCreated, "POST", "/v1/admin/swarm-templates", matrix.ServiceAdmin, tplBody("e2e-spare"))
	spare := obj["id"].(string)
	m.expect(http.StatusBadRequest, "PATCH", "/v1/admin/swarm-templates/"+spare, matrix.ServiceAdmin, `{"ttlHours":0}`, "If-Match", h.Get("ETag"))
	m.expect(http.StatusBadRequest, "PATCH", "/v1/admin/swarm-templates/"+spare, matrix.ServiceAdmin, `{}`, "If-Match", h.Get("ETag"))
	obj, h = m.expect(http.StatusOK, "PATCH", "/v1/admin/swarm-templates/"+spare, matrix.ServiceAdmin, `{"ttlHours":72,"description":"d"}`, "If-Match", h.Get("ETag"))
	if obj["ttlHours"] != float64(72) || obj["version"] != float64(2) {
		t.Errorf("%v", obj)
	}
	m.expect(http.StatusPreconditionFailed, "DELETE", "/v1/admin/swarm-templates/"+spare, matrix.ServiceAdmin, "", "If-Match", `"1"`)
	m.expect(http.StatusNoContent, "DELETE", "/v1/admin/swarm-templates/"+spare, matrix.ServiceAdmin, "", "If-Match", h.Get("ETag"))
	m.expect(http.StatusNotFound, "GET", "/v1/admin/swarm-templates/"+spare, matrix.ServiceAdmin, "")
	list, _ = m.expect(http.StatusOK, "GET", "/v1/admin/swarm-templates", matrix.SecurityOperator, "")
	if find(items(t, list), "id", tpl) == nil || find(items(t, list), "id", spare) != nil {
		t.Errorf("template list %v", list)
	}
}

// TestWorkOrderSLAMatchesTheDatabaseView proves domain.SLAStateAt agrees with afs.work_order_progress
// (the reconciler reads the view) for open, done and cancelled work orders around the due time and the
// warning window, and that the API's filter and response agree with both.
func TestWorkOrderSLAMatchesTheDatabaseView(t *testing.T) {
	m := newM2W1(t)
	w := m.w
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	tpl := pgtest.NewID("swt")
	exec(`INSERT INTO afs.swarm_templates (id, name, kind, spec, budget_usd, ttl_hours) VALUES ($1, $2, 'cve_remediation', '{"pools":[]}', 10, 1)`, tpl, "sla-"+strings.ToLower(pgtest.NewID("x")[2:10]))
	now := time.Now().UTC()
	cases := []struct {
		status string
		due    time.Duration
		done   time.Duration // completion relative to now, 0: none
	}{
		{"open", 100 * time.Hour, 0}, {"assigned", 47 * time.Hour, 0}, {"in_progress", -time.Hour, 0}, {"failed", -time.Hour, 0},
		{"done", 24 * time.Hour, -time.Hour}, {"done", -2 * time.Hour, -time.Hour}, {"done", -2 * time.Hour, 0}, {"cancelled", time.Hour, -time.Minute},
	}
	for i, c := range cases {
		id := pgtest.NewID("wo")
		var team any
		if c.status != "open" && c.status != "cancelled" {
			team = w.TeamA.ID
		}
		var completed any
		if c.done != 0 {
			completed = now.Add(c.done)
		}
		exec(`INSERT INTO afs.work_orders (id, team_id, kind, external_ref, title, sla_due_at, status, severity, template_id, completed_at)
VALUES ($1, $2, 'cve_remediation', $3, 'sla', $4, $5, 'low', $6, $7)`, id, team, fmt.Sprintf("CVE-2026-%d", 700000+i), now.Add(c.due), c.status, tpl, completed)
		exec(`INSERT INTO afs.work_order_repos (work_order_id, host, repo, owning_contact) VALUES ($1, 'github.com', 'a/b', 'o@example.com')`, id)
	}
	obj, _ := m.expect(http.StatusOK, "GET", "/v1/work-orders?maxResults=50", matrix.ServiceAdmin, "")
	rows := items(t, obj)
	if len(rows) < len(cases) {
		t.Fatalf("%d rows", len(rows))
	}
	checked := 0
	for _, r := range rows {
		if !strings.HasPrefix(fmt.Sprint(r["cveId"]), "CVE-2026-7000") {
			continue
		}
		var view string
		if err := w.Admin.QueryRow(t.Context(), `SELECT sla_state FROM afs.work_order_progress WHERE work_order_id = $1`, r["id"]).Scan(&view); err != nil {
			t.Fatal(err)
		}
		if r["slaState"] != view {
			t.Errorf("%v (%v): API says %v, the view says %v", r["cveId"], r["status"], r["slaState"], view)
		}
		if !domain.SLAState(view).Valid() {
			t.Errorf("view state %q", view)
		}
		f, _ := m.expect(http.StatusOK, "GET", "/v1/work-orders?slaState="+view, matrix.ServiceAdmin, "")
		if find(items(t, f), "id", r["id"].(string)) == nil {
			t.Errorf("filter slaState=%s misses %v", view, r["id"])
		}
		checked++
	}
	if checked != len(cases) {
		t.Errorf("checked %d of %d", checked, len(cases))
	}
}

func anyContains(list []any, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
