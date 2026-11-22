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

func agentCall(t *testing.T, h http.Handler, method, path, token, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.89:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b), rec.Header()
}

// M2-B6 (RA-US-4.5) and the agent lifecycle through the real pipeline and database: an agent whose
// template is disabled later is still editable and pausable (the API does not touch it), while a move
// to the disabled template is refused with a 400 naming it.
func TestAgentsEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	adm, viewer := w.Tokens[matrix.TeamAdmin], w.Tokens[matrix.Viewer]
	model, tpl := w.SeedAgentCatalog(t)
	base := "/v1/teams/" + w.TeamA.ID + "/agents"
	image := "registry.example.com/agents/coder@sha256:" + strings.Repeat("4f", 32)

	body := `{"name":"e2e-` + strings.ToLower(pgtest.NewID("x")[2:8]) + `","template":"` + tpl + `","image":"` + image + `","modelId":"` + model + `"}`
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, base, viewer, body); code != 403 {
		t.Fatalf("viewer create: %d %s", code, out)
	}
	code, out, hdr := agentCall(t, c.Handler, http.MethodPost, base, adm, body, "Idempotency-Key", "agent-e2e-1")
	if code != 201 || hdr.Get("Location") == "" || hdr.Get("ETag") == "" {
		t.Fatalf("create: %d %s", code, out)
	}
	var a struct {
		ID         string `json:"id"`
		Generation int64  `json:"generation"`
		Paused     bool   `json:"paused"`
		Volume     struct {
			SizeGiB int `json:"sizeGiB"`
		} `json:"volume"`
	}
	if err := json.Unmarshal([]byte(out), &a); err != nil || a.Generation != 1 || a.Volume.SizeGiB != 20 {
		t.Fatalf("created body: %v %s", err, out)
	}
	if code, again, h := agentCall(t, c.Handler, http.MethodPost, base, adm, body, "Idempotency-Key", "agent-e2e-1"); code != 201 || h.Get("Idempotent-Replayed") != "true" || !sameJSON(again, out) {
		t.Fatalf("replay: %d %s", code, again)
	}
	one := base + "/" + a.ID

	// The template is disabled afterwards.
	if _, err := w.Admin.Exec(t.Context(), `UPDATE afs.agent_templates SET enabled = false WHERE name = $1`, tpl); err != nil {
		t.Fatal(err)
	}
	code, out, hdr = agentCall(t, c.Handler, http.MethodGet, one, adm, "")
	if code != 200 {
		t.Fatalf("get: %d %s", code, out)
	}
	code, out, hdr = agentCall(t, c.Handler, http.MethodPatch, one, adm, `{"description":"still editable","volume":{"sizeGiB":30}}`, "If-Match", hdr.Get("ETag"))
	if code != 200 || !strings.Contains(out, `"still editable"`) || !strings.Contains(out, `"sizeGiB":30`) {
		t.Fatalf("update with a disabled template: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPatch, one, adm, `{"volume":{"sizeGiB":10}}`, "If-Match", hdr.Get("ETag")); code != 400 || !strings.Contains(out, "sizeGiB") {
		t.Fatalf("shrink: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPatch, one, adm, `{"image":"registry.example.com/agents/coder@sha256:`+strings.Repeat("ab", 32)+`"}`, "If-Match", hdr.Get("ETag")); code != 400 || !strings.Contains(out, tpl) {
		t.Fatalf("image change on a disabled template: %d %s", code, out)
	}
	if code, _, _ := agentCall(t, c.Handler, http.MethodPatch, one, adm, `{"description":"x"}`); code != 428 {
		t.Fatalf("missing If-Match: %d", code)
	}

	// Lifecycle.
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, one+":pause", adm, ""); code != 200 || !strings.Contains(out, `"paused":true`) {
		t.Fatalf("pause: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, one+":restart", adm, ""); code != 409 {
		t.Fatalf("restart paused: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, one+":resume", adm, ""); code != 200 || !strings.Contains(out, `"paused":false`) {
		t.Fatalf("resume: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, one+":restart", adm, ""); code != 202 {
		t.Fatalf("restart: %d %s", code, out)
	}
	// The reconciler reads the restart nonce from this event (M2-F, M2-G3): one per RestartAgent, per agent.
	var restarts int
	if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'agent.restarted' AND aggregate_id = $1`, a.ID).Scan(&restarts); err != nil || restarts != 1 {
		t.Fatalf("agent.restarted events = %d, %v", restarts, err)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, one+"/snapshots", adm, `{"description":"before delete"}`); code != 202 || !strings.Contains(out, `"state":"requested"`) {
		t.Fatalf("snapshot: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodGet, one+"/snapshots", viewer, ""); code != 200 || !strings.Contains(out, `"reason":"manual"`) {
		t.Fatalf("snapshots: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodGet, one+"/status", viewer, ""); code != 200 || !strings.Contains(out, `"kind":"agent"`) {
		t.Fatalf("status: %d %s", code, out)
	}
	_, _, hdr = agentCall(t, c.Handler, http.MethodGet, one, adm, "")
	if code, _, _ := agentCall(t, c.Handler, http.MethodDelete, one, viewer, "", "If-Match", hdr.Get("ETag")); code != 403 {
		t.Fatalf("viewer delete: %d", code)
	}
	code, out, _ = agentCall(t, c.Handler, http.MethodDelete, one, adm, "", "If-Match", hdr.Get("ETag"))
	if code != 202 || !strings.Contains(out, `"phase":"Deleting"`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	// The API only marks: the reconciler's completer sets deleted and deleted_at (M2-G4).
	var state, onDelete string
	if err := w.Admin.QueryRow(t.Context(), `SELECT state, storage_on_delete FROM afs.agents WHERE id = $1`, a.ID).Scan(&state, &onDelete); err != nil || state != "deleting" || onDelete == "" {
		t.Fatalf("after delete: state %q on-delete %q, %v", state, onDelete, err)
	}
	// Other teams do not see it.
	if code, _, _ := agentCall(t, c.Handler, http.MethodGet, "/v1/teams/"+w.TeamB.ID+"/agents/"+a.ID, w.Tokens[matrix.ServiceAdmin], ""); code != 404 {
		t.Fatalf("cross team get: %d", code)
	}
}
