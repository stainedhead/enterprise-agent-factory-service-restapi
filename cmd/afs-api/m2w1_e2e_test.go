package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// m2w1 drives the M2-A, M2-D and M2-E operations through the real pipeline and database.
type m2w1 struct {
	t *testing.T
	h http.Handler
	w *matrix.World
}

// call sends a request as the caller and returns the status, the decoded JSON object (nil when the
// body is not an object), the headers and the raw body.
func (m *m2w1) call(method, path string, caller matrix.Caller, body string, hdr ...string) (int, map[string]any, http.Header, string) {
	m.t.Helper()
	r := httptest.NewRequestWithContext(m.t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+m.w.Tokens[caller])
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.90:4000"
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, r)
	raw, _ := io.ReadAll(rec.Body)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	return rec.Code, obj, rec.Header(), string(raw)
}

// expect fails the test unless the call answers want.
func (m *m2w1) expect(want int, method, path string, caller matrix.Caller, body string, hdr ...string) (map[string]any, http.Header) {
	m.t.Helper()
	code, obj, h, raw := m.call(method, path, caller, body, hdr...)
	if code != want {
		m.t.Fatalf("%s %s as %s: %d %s, want %d", method, path, caller, code, raw, want)
	}
	return obj, h
}

func items(t *testing.T, obj map[string]any) []map[string]any {
	t.Helper()
	raw, _ := obj["items"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func find(list []map[string]any, key, value string) map[string]any {
	for _, x := range list {
		if x[key] == value {
			return x
		}
	}
	return nil
}

func newM2W1(t *testing.T) *m2w1 {
	t.Helper()
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &m2w1{t: t, h: c.Handler, w: w}
}

// etag returns the ETag header as the If-Match value of the next request.
func etag(h http.Header) string { return h.Get("ETag") }

func TestM2W1ModelsAndPrices(t *testing.T) {
	m := newM2W1(t)
	id := "e2e-model-" + m.w.TeamA.Slug
	body := `{"id":"` + id + `","provider":"Example","displayName":"E2E","contextWindow":200000,"maxOutputTokens":8192}`
	obj, h := m.expect(201, "POST", "/v1/admin/models", matrix.ServiceAdmin, body, "Idempotency-Key", "e2e-model-1")
	if obj["id"] != id || etag(h) != `"1"` || h.Get("Location") != "/v1/admin/models/"+id || obj["maxOutputTokens"] != float64(8192) {
		t.Fatalf("create: %v %v", obj, h)
	}
	if _, h := m.expect(201, "POST", "/v1/admin/models", matrix.ServiceAdmin, body, "Idempotency-Key", "e2e-model-1"); h.Get("Idempotent-Replayed") != "true" {
		t.Error("the replay is not marked")
	}
	m.expect(409, "POST", "/v1/admin/models", matrix.ServiceAdmin, body)
	m.expect(400, "POST", "/v1/admin/models", matrix.ServiceAdmin, `{"id":"bad id","provider":"x","displayName":"x","contextWindow":10,"maxOutputTokens":5}`)
	// A Bedrock id ends in ":0": it can be created and then addressed (the router splits only real verbs).
	cid := "e2e.anthropic-" + m.w.TeamA.Slug + "-v2:0"
	m.expect(201, "POST", "/v1/admin/models", matrix.ServiceAdmin, `{"id":"`+cid+`","provider":"x","displayName":"x","contextWindow":10,"maxOutputTokens":5}`)
	m.expect(200, "PATCH", "/v1/admin/models/"+cid, matrix.ServiceAdmin, `{"displayName":"Colon"}`, "If-Match", `"1"`)
	m.expect(400, "POST", "/v1/admin/models", matrix.ServiceAdmin, `{"id":"e2e-over","provider":"x","displayName":"x","contextWindow":10,"maxOutputTokens":50}`)

	m.expect(428, "PATCH", "/v1/admin/models/"+id, matrix.ServiceAdmin, `{"displayName":"New"}`)
	m.expect(412, "PATCH", "/v1/admin/models/"+id, matrix.ServiceAdmin, `{"displayName":"New"}`, "If-Match", `"7"`)
	obj, h = m.expect(200, "PATCH", "/v1/admin/models/"+id, matrix.ServiceAdmin, `{"displayName":"New"}`, "If-Match", `"1"`)
	if obj["displayName"] != "New" || etag(h) != `"2"` {
		t.Fatalf("update: %v", obj)
	}

	// Team roles read the enabled models and nothing of the admin side.
	list, _ := m.expect(200, "GET", "/v1/models", matrix.TeamAdmin, "")
	if find(items(t, list), "id", id) == nil {
		t.Error("an enabled model is not listed for a team admin")
	}
	m.expect(403, "GET", "/v1/admin/models", matrix.TeamAdmin, "")
	m.expect(403, "PATCH", "/v1/admin/models/"+id, matrix.TeamAdmin, `{"enabled":false}`, "If-Match", `"2"`)
	m.expect(200, "PATCH", "/v1/admin/models/"+id, matrix.ServiceAdmin, `{"enabled":false}`, "If-Match", `"2"`)
	list, _ = m.expect(200, "GET", "/v1/models", matrix.TeamAdmin, "")
	if find(items(t, list), "id", id) != nil {
		t.Error("a disabled model is listed for a team admin")
	}
	list, _ = m.expect(200, "GET", "/v1/admin/models", matrix.ServiceAdmin, "")
	if find(items(t, list), "id", id) == nil {
		t.Error("a disabled model is missing from the admin list")
	}

	// Prices are effective-dated, immutable and listed newest first.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	price := func(at time.Time, in string) string {
		return `{"effectiveFrom":"` + at.Format(time.RFC3339) + `","inputUsdPerMtok":"` + in + `","outputUsdPerMtok":"15.000000","cacheReadUsdPerMtok":"0.3","cacheWriteUsdPerMtok":"3.75"}`
	}
	pp := "/v1/admin/models/" + id + "/prices"
	for i, in := range []string{"3", "2.5", "2"} {
		m.expect(201, "POST", pp, matrix.ServiceAdmin, price(base.AddDate(0, i, 0), in))
	}
	m.expect(409, "POST", pp, matrix.ServiceAdmin, price(base, "1"))
	m.expect(400, "POST", pp, matrix.ServiceAdmin, `{"effectiveFrom":"`+base.Format(time.RFC3339)+`","inputUsdPerMtok":"-1","outputUsdPerMtok":"1"}`)
	m.expect(404, "POST", "/v1/admin/models/absent/prices", matrix.ServiceAdmin, price(base, "1"))
	m.expect(403, "POST", pp, matrix.TeamAdmin, price(base.AddDate(1, 0, 0), "1"))
	first, _ := m.expect(200, "GET", pp+"?maxResults=2", matrix.ServiceAdmin, "")
	rows := items(t, first)
	if len(rows) != 2 || rows[0]["inputUsdPerMtok"] != "2.00" || first["nextToken"] == nil {
		t.Fatalf("price page 1: %v", first)
	}
	rest, _ := m.expect(200, "GET", pp+"?maxResults=2&nextToken="+first["nextToken"].(string), matrix.ServiceAdmin, "")
	if r := items(t, rest); len(r) != 1 || r[0]["inputUsdPerMtok"] != "3.00" {
		t.Fatalf("price page 2: %v", rest)
	}
	m.expect(403, "GET", pp, matrix.TeamAdmin, "")
}

func TestM2W1StorageClassesAndTemplates(t *testing.T) {
	m := newM2W1(t)
	sc := "/v1/admin/storage-classes/e2e-" + strings.ToLower(m.w.TeamA.Slug)
	obj, h := m.expect(200, "PUT", sc, matrix.ServiceAdmin, `{"description":"fast","volumeType":"gp3"}`)
	if obj["volumeType"] != "gp3" || etag(h) != `"1"` {
		t.Fatalf("put: %v", obj)
	}
	m.expect(400, "PUT", sc, matrix.ServiceAdmin, `{"description":"x","volumeType":"io1"}`)
	m.expect(412, "PUT", sc, matrix.ServiceAdmin, `{"description":"x","volumeType":"io2"}`, "If-Match", `"9"`)
	obj, h = m.expect(200, "PUT", sc, matrix.ServiceAdmin, `{"description":"x","volumeType":"io2","enabled":false}`, "If-Match", `"1"`)
	if obj["volumeType"] != "io2" || obj["enabled"] != false || etag(h) != `"2"` {
		t.Fatalf("update: %v", obj)
	}
	name := sc[strings.LastIndex(sc, "/")+1:]
	list, _ := m.expect(200, "GET", "/v1/storage-classes", matrix.Viewer, "")
	if find(items(t, list), "name", name) != nil {
		t.Error("a disabled storage class is listed for a viewer")
	}
	list, _ = m.expect(200, "GET", "/v1/admin/storage-classes", matrix.ServiceAdmin, "")
	if find(items(t, list), "name", name) == nil {
		t.Error("a disabled storage class is missing from the admin list")
	}
	m.expect(403, "PUT", sc, matrix.TeamAdmin, `{"description":"x","volumeType":"gp3"}`)
	m.expect(403, "DELETE", sc, matrix.TeamAdmin, "", "If-Match", `"2"`)
	m.expect(428, "DELETE", sc, matrix.ServiceAdmin, "")
	m.expect(412, "DELETE", sc, matrix.ServiceAdmin, "", "If-Match", `"1"`)
	m.expect(204, "DELETE", sc, matrix.ServiceAdmin, "", "If-Match", `"2"`)
	m.expect(404, "DELETE", sc, matrix.ServiceAdmin, "", "If-Match", `"2"`)

	digA := "sha256:" + strings.Repeat("a", 64)
	digB := "sha256:" + strings.Repeat("b", 64)
	tp := "/v1/admin/agent-templates/e2e-tpl"
	body := func(cur string) string {
		return `{"description":"d","harness":"hermes","defaultResources":{"cpu":"500m","memory":"1Gi"},"allowedDigests":["` + digA + `","` + digB + `"],"currentDigest":"` + cur + `"}`
	}
	obj, h = m.expect(200, "PUT", tp, matrix.ServiceAdmin, body(digA))
	if obj["currentDigest"] != digA || !strings.HasPrefix(obj["id"].(string), "tpl_") {
		t.Fatalf("template: %v", obj)
	}
	m.expect(400, "PUT", tp, matrix.ServiceAdmin, body("sha256:"+strings.Repeat("c", 64)))
	obj, _ = m.expect(200, "PUT", tp, matrix.ServiceAdmin, body(digB), "If-Match", etag(h))
	if obj["currentDigest"] != digB {
		t.Fatalf("template update: %v", obj)
	}
	m.expect(412, "PUT", tp, matrix.ServiceAdmin, body(digA), "If-Match", etag(h))
	list, _ = m.expect(200, "GET", "/v1/agent-templates", matrix.Viewer, "")
	if find(items(t, list), "name", "e2e-tpl") == nil {
		t.Error("an enabled template is not listed for a viewer")
	}
	m.expect(403, "GET", "/v1/admin/agent-templates", matrix.Viewer, "")
	m.expect(403, "PUT", tp, matrix.TeamAdmin, body(digA))
	m.expect(204, "DELETE", tp, matrix.ServiceAdmin, "", "If-Match", `"2"`)
}

func TestM2W1CatalogSystems(t *testing.T) {
	m := newM2W1(t)
	slug := "e2e-" + strings.ToLower(m.w.TeamA.Slug)
	body := `{"slug":"` + slug + `","name":"E2E","category":"other","endpoints":["e2e.example.com"]}`
	obj, h := m.expect(201, "POST", "/v1/admin/external-systems", matrix.ServiceAdmin, body, "Idempotency-Key", "e2e-sys-1")
	id := obj["id"].(string)
	if obj["builtin"] != false || obj["scope"] != "service" || obj["enabled"] != true || h.Get("Location") != "/v1/admin/external-systems/"+id {
		t.Fatalf("create: %v", obj)
	}
	if _, h := m.expect(201, "POST", "/v1/admin/external-systems", matrix.ServiceAdmin, body, "Idempotency-Key", "e2e-sys-1"); h.Get("Idempotent-Replayed") != "true" {
		t.Error("replay not marked")
	}
	m.expect(409, "POST", "/v1/admin/external-systems", matrix.ServiceAdmin, body)
	m.expect(400, "POST", "/v1/admin/external-systems", matrix.ServiceAdmin, `{"slug":"Bad Slug","name":"x","category":"other","endpoints":["a.example.com"]}`)
	m.expect(403, "POST", "/v1/admin/external-systems", matrix.TeamAdmin, body)
	m.expect(403, "GET", "/v1/admin/external-systems", matrix.Viewer, "")
	m.expect(200, "GET", "/v1/admin/external-systems/"+id, matrix.ServiceAdmin, "")
	m.expect(404, "GET", "/v1/admin/external-systems/ext_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", matrix.ServiceAdmin, "")
	m.expect(428, "PATCH", "/v1/admin/external-systems/"+id, matrix.ServiceAdmin, `{"enabled":false}`)
	obj, h = m.expect(200, "PATCH", "/v1/admin/external-systems/"+id, matrix.ServiceAdmin, `{"enabled":false,"endpoints":["x.example.com","*.example.org"]}`, "If-Match", etag(h))
	if obj["enabled"] != false || etag(h) != `"2"` {
		t.Fatalf("update: %v", obj)
	}
	m.expect(400, "PATCH", "/v1/admin/external-systems/"+id, matrix.ServiceAdmin, `{"endpoints":["not a host"]}`, "If-Match", `"2"`)

	// A built-in entry can be switched off but never deleted.
	list, _ := m.expect(200, "GET", "/v1/admin/external-systems?maxResults=100", matrix.ServiceAdmin, "")
	gh := find(items(t, list), "slug", "github")
	if gh == nil || gh["builtin"] != true {
		t.Fatalf("no built-in github entry: %v", list)
	}
	m.expect(409, "DELETE", "/v1/admin/external-systems/"+gh["id"].(string), matrix.ServiceAdmin, "", "If-Match", fmt.Sprintf(`"%d"`, int(gh["version"].(float64))))

	m.expect(412, "DELETE", "/v1/admin/external-systems/"+id, matrix.ServiceAdmin, "", "If-Match", `"1"`)
	m.expect(204, "DELETE", "/v1/admin/external-systems/"+id, matrix.ServiceAdmin, "", "If-Match", `"2"`)
}

// seedAgent inserts a live agent of team A with the model it needs.
func (m *m2w1) seedAgent() string {
	m.t.Helper()
	exec := func(q string, args ...any) {
		if _, err := m.w.Admin.Exec(m.t.Context(), q, args...); err != nil {
			m.t.Fatal(err)
		}
	}
	exec(`INSERT INTO afs.models (id, provider, display_name) VALUES ('e2e-agent-model', 'Example', 'E2E') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'e2e-agent-model') ON CONFLICT DO NOTHING`, m.w.TeamA.ID)
	id := pgtest.NewID("agt")
	exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, $3, 'omp', $4, 'e2e-agent-model')`,
		id, m.w.TeamA.ID, "e2e-"+strings.ToLower(id[len(id)-8:]), "ghcr.io/example/omp@sha256:"+strings.Repeat("a", 64))
	m.t.Cleanup(func() { // other tests of the shared database must not find the model assignment
		_, _ = m.w.Admin.Exec(t0(m.t), `DELETE FROM afs.agents WHERE id = $1`, id)
		_, _ = m.w.Admin.Exec(t0(m.t), `DELETE FROM afs.team_models WHERE team_id = $1 AND model_id = 'e2e-agent-model'`, m.w.TeamA.ID)
	})
	return id
}

func TestM2W1ConfigValues(t *testing.T) {
	m := newM2W1(t)
	team := "/v1/teams/" + m.w.TeamA.ID
	agent := m.seedAgent()
	ns := team + "/config-values/LOG_LEVEL"

	obj, h := m.expect(200, "PUT", ns, matrix.TeamAdmin, `{"value":"info"}`)
	if obj["value"] != "info" || etag(h) != `"1"` || obj["source"] != "literal" {
		t.Fatalf("put: %v", obj)
	}
	m.expect(400, "PUT", team+"/config-values/log_level", matrix.TeamAdmin, `{"value":"x"}`)
	m.expect(400, "PUT", team+"/config-values/DB_PASSWORD", matrix.TeamAdmin, `{"value":"x"}`)
	m.expect(400, "PUT", team+"/config-values/BIG", matrix.TeamAdmin, `{"value":"`+strings.Repeat("x", 4097)+`"}`)
	m.expect(400, "PUT", team+"/config-values/AFS_OWNED_REPOS", matrix.TeamAdmin, `{"value":"github.com/acme"}`)
	// The wildcard names every acme repository for the shared team A; the world outlives this test, so remove it
	// (it made the owner resolution of the work order tests ambiguous).
	t.Cleanup(func() {
		_, _ = m.w.Admin.Exec(context.WithoutCancel(t.Context()), `DELETE FROM afs.namespace_config_values WHERE team_id = $1 AND name = 'AFS_OWNED_REPOS'`, m.w.TeamA.ID)
	})
	m.expect(200, "PUT", team+"/config-values/AFS_OWNED_REPOS", matrix.TeamAdmin, `{"value":"github.com/acme/*, github.com/acme/tool"}`)
	m.expect(400, "PUT", ns, matrix.TeamAdmin, `{}`)
	m.expect(412, "PUT", ns, matrix.TeamAdmin, `{"value":"debug"}`, "If-Match", `"5"`)
	m.expect(200, "PUT", ns, matrix.TeamAdmin, `{"value":"debug"}`, "If-Match", `"1"`)
	m.expect(403, "PUT", ns, matrix.Viewer, `{"value":"x"}`)
	m.expect(403, "DELETE", ns, matrix.Viewer, "", "If-Match", `"2"`)
	list, _ := m.expect(200, "GET", team+"/config-values", matrix.Viewer, "")
	if v := find(items(t, list), "name", "LOG_LEVEL"); v == nil || v["value"] != "debug" {
		t.Fatalf("list: %v", list)
	}

	// An agent links to the namespace value or holds its own; a link needs the value to exist.
	am := team + "/agents/" + agent + "/config-values/"
	m.expect(400, "PUT", am+"NO_SUCH", matrix.TeamAdmin, `{"source":"namespace"}`)
	m.expect(400, "PUT", am+"LOG_LEVEL", matrix.TeamAdmin, `{"source":"literal"}`)
	m.expect(400, "PUT", am+"LOG_LEVEL", matrix.TeamAdmin, `{"source":"namespace","value":"x"}`)
	obj, h = m.expect(200, "PUT", am+"LOG_LEVEL", matrix.TeamAdmin, `{"source":"namespace"}`)
	if obj["source"] != "namespace" || obj["resolvedValue"] != "debug" || obj["value"] != nil {
		t.Fatalf("link: %v", obj)
	}
	nsList, _ := m.expect(200, "GET", team+"/config-values", matrix.Viewer, "")
	if v := find(items(t, nsList), "name", "LOG_LEVEL"); v == nil || len(v["usedBy"].([]any)) != 1 || v["usedBy"].([]any)[0] != agent {
		t.Errorf("usedBy: %v", nsList)
	}
	obj, _ = m.expect(409, "DELETE", ns, matrix.TeamAdmin, "", "If-Match", `"2"`)
	if obj["code"] != "Conflict" {
		t.Errorf("conflict body: %v", obj)
	}
	m.expect(404, "GET", team+"/agents/agt_0ZZZZZZZZZZZZZZZZZZZZZZZZZ/config-values", matrix.Viewer, "")
	m.expect(200, "PUT", am+"LOG_LEVEL", matrix.TeamAdmin, `{"source":"literal","value":"trace"}`, "If-Match", etag(h))
	agentList, _ := m.expect(200, "GET", team+"/agents/"+agent+"/config-values", matrix.Viewer, "")
	if v := find(items(t, agentList), "name", "LOG_LEVEL"); v == nil || v["source"] != "literal" || v["resolvedValue"] != "trace" {
		t.Fatalf("agent list: %v", agentList)
	}
	m.expect(403, "PUT", am+"LOG_LEVEL", matrix.Viewer, `{"source":"literal","value":"x"}`)
	m.expect(412, "DELETE", am+"LOG_LEVEL", matrix.TeamAdmin, "", "If-Match", `"9"`)
	m.expect(204, "DELETE", am+"LOG_LEVEL", matrix.TeamAdmin, "", "If-Match", `"2"`)
	m.expect(204, "DELETE", ns, matrix.TeamAdmin, "", "If-Match", `"2"`)
	m.expect(404, "DELETE", ns, matrix.TeamAdmin, "", "If-Match", `"2"`)
	// Another team's admin learns nothing of this team.
	m.expect(404, "GET", "/v1/teams/"+m.w.TeamB.ID+"/config-values", matrix.TeamAdmin, "")
}

func TestM2W1ExternalSystems(t *testing.T) {
	m := newM2W1(t)
	team := "/v1/teams/" + m.w.TeamA.ID
	agent := m.seedAgent()
	slug := "e2e-" + strings.ToLower(m.w.TeamA.Slug)
	cat, _ := m.expect(201, "POST", "/v1/admin/external-systems", matrix.ServiceAdmin,
		`{"slug":"`+slug+`","name":"Catalog","category":"other","endpoints":["c.example.com"]}`)
	catID := cat["id"].(string)

	teamList, _ := m.expect(200, "GET", team+"/external-systems?maxResults=100", matrix.Viewer, "")
	if x := find(items(t, teamList), "id", catID); x == nil || x["enabled"] != true || x["teamState"] != "on" || x["disabledAt"] != nil {
		t.Fatalf("team view: %v", x)
	}

	// The team switches the system off; the agent below sees it off at the team level and cannot turn it on.
	m.expect(403, "PUT", team+"/external-systems/"+catID+"/state", matrix.Viewer, `{"enabled":false}`)
	m.expect(400, "PUT", team+"/external-systems/"+catID+"/state", matrix.TeamAdmin, `{}`)
	obj, _ := m.expect(200, "PUT", team+"/external-systems/"+catID+"/state", matrix.TeamAdmin, `{"enabled":false}`)
	if obj["enabled"] != false || obj["teamState"] != "off" || obj["disabledAt"] != "team" {
		t.Fatalf("team off: %v", obj)
	}
	m.expect(200, "PUT", team+"/external-systems/"+catID+"/state", matrix.TeamAdmin, `{"enabled":false}`)
	agentList, _ := m.expect(200, "GET", team+"/agents/"+agent+"/external-systems?maxResults=100", matrix.Viewer, "")
	if x := find(items(t, agentList), "id", catID); x == nil || x["disabledAt"] != "team" || x["enabled"] != false {
		t.Fatalf("agent view under a team that has it off: %v", x)
	}
	as := team + "/agents/" + agent + "/external-systems/" + catID + "/state"
	if obj, _ := m.expect(409, "PUT", as, matrix.TeamAdmin, `{"enabled":true}`); obj["code"] != "Conflict" {
		t.Errorf("conflict body: %v", obj)
	}
	m.expect(200, "PUT", team+"/external-systems/"+catID+"/state", matrix.TeamAdmin, `{"enabled":true}`)
	obj, _ = m.expect(200, "PUT", as, matrix.TeamAdmin, `{"enabled":false}`)
	if obj["disabledAt"] != "agent" || obj["enabled"] != false {
		t.Fatalf("agent off: %v", obj)
	}
	m.expect(403, "PUT", as, matrix.Viewer, `{"enabled":true}`)
	m.expect(404, "PUT", team+"/agents/agt_0ZZZZZZZZZZZZZZZZZZZZZZZZZ/external-systems/"+catID+"/state", matrix.TeamAdmin, `{"enabled":false}`)
	m.expect(404, "PUT", team+"/external-systems/ext_0ZZZZZZZZZZZZZZZZZZZZZZZZZ/state", matrix.TeamAdmin, `{"enabled":false}`)
	// The service-level switch beats both.
	m.expect(200, "PATCH", "/v1/admin/external-systems/"+catID, matrix.ServiceAdmin, `{"enabled":false}`, "If-Match", `"1"`)
	teamList, _ = m.expect(200, "GET", team+"/external-systems?maxResults=100", matrix.Viewer, "")
	if x := find(items(t, teamList), "id", catID); x == nil || x["disabledAt"] != "service" {
		t.Fatalf("team view under a service that has it off: %v", x)
	}
	m.expect(409, "PUT", team+"/external-systems/"+catID+"/state", matrix.TeamAdmin, `{"enabled":true}`)
	m.expect(409, "PUT", as, matrix.TeamAdmin, `{"enabled":true}`)

	// A team may declare its own systems only when a service admin allowed it.
	decl := `{"slug":"` + slug + `-own","name":"Wiki","category":"other","endpoints":["wiki.example.com"],"owner":"platform","purpose":"team docs"}`
	m.w.SetFlags(t, authz.TeamFlags{})
	obj, _ = m.expect(403, "POST", team+"/external-systems", matrix.TeamAdmin, decl)
	if obj["code"] != "AccessDenied" {
		t.Errorf("flag off: %v", obj)
	}
	m.expect(403, "POST", team+"/external-systems", matrix.ServiceAdmin, decl)
	m.w.SetFlags(t, authz.TeamFlags{MayAddExternalSystems: true})
	m.expect(403, "POST", team+"/external-systems", matrix.Viewer, decl)
	own, h := m.expect(201, "POST", team+"/external-systems", matrix.TeamAdmin, decl, "Idempotency-Key", "e2e-decl-1")
	ownID := own["id"].(string)
	if own["scope"] != "team" || own["owner"] != "platform" || h.Get("Location") != team+"/external-systems/"+ownID {
		t.Fatalf("declare: %v", own)
	}
	if _, h := m.expect(201, "POST", team+"/external-systems", matrix.TeamAdmin, decl, "Idempotency-Key", "e2e-decl-1"); h.Get("Idempotent-Replayed") != "true" {
		t.Error("replay not marked")
	}
	m.expect(409, "POST", team+"/external-systems", matrix.TeamAdmin, strings.Replace(decl, slug+"-own", "github", 1))
	m.expect(400, "POST", team+"/external-systems", matrix.TeamAdmin, strings.Replace(decl, `"owner":"platform",`, "", 1))
	// Team-declared: editable by the team; catalog systems are not.
	m.expect(428, "PATCH", team+"/external-systems/"+ownID, matrix.TeamAdmin, `{"purpose":"x"}`)
	obj, _ = m.expect(200, "PATCH", team+"/external-systems/"+ownID, matrix.TeamAdmin, `{"purpose":"better docs"}`, "If-Match", etag(h))
	if obj["purpose"] != "better docs" {
		t.Fatalf("update: %v", obj)
	}
	m.expect(412, "PATCH", team+"/external-systems/"+ownID, matrix.TeamAdmin, `{"purpose":"x"}`, "If-Match", etag(h))
	m.expect(409, "PATCH", team+"/external-systems/"+catID, matrix.TeamAdmin, `{"purpose":"x"}`, "If-Match", `"2"`)
	m.expect(409, "DELETE", team+"/external-systems/"+catID, matrix.TeamAdmin, "", "If-Match", `"2"`)
	m.expect(403, "DELETE", team+"/external-systems/"+ownID, matrix.Viewer, "", "If-Match", `"2"`)
	// Another team does not see this team's declaration.
	m.expect(404, "GET", "/v1/teams/"+m.w.TeamB.ID+"/external-systems", matrix.TeamAdmin, "")
	m.expect(204, "DELETE", team+"/external-systems/"+ownID, matrix.TeamAdmin, "", "If-Match", `"2"`)
	m.expect(404, "DELETE", team+"/external-systems/"+ownID, matrix.TeamAdmin, "", "If-Match", `"2"`)
}

// t0 is a context that outlives the test, for cleanups.
func t0(t *testing.T) context.Context { return context.WithoutCancel(t.Context()) }
