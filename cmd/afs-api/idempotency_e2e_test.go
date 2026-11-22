package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

func post(t *testing.T, h http.Handler, path, token, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.88:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	b, _ := io.ReadAll(rec.Body)
	return rec.Code, string(b), rec.Header()
}

// RA-US-9.1, RA-US-9.2 through the real pipeline and database: the key is claimed and the response
// stored in the create's own transaction, so a replay writes nothing, a different body conflicts, and a
// replayed CreateApiKey never carries the secret key.
func TestIdempotencyEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	svc, adm := w.Tokens[matrix.ServiceAdmin], w.Tokens[matrix.TeamAdmin]
	count := func(q string, args ...any) int {
		var n int
		if err := w.Admin.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	body := `{"slug":"idem-` + w.TeamA.Slug + `","displayName":"Idem","kind":"persistent","costCenter":"CC1","ownerEmail":"o@example.com","maxBudgetUsd":"1000","firstAdmin":{"subjectType":"group","groupName":"idem-g"},"tags":[]}`
	code, first, _ := post(t, c.Handler, "/v1/teams", svc, body, "Idempotency-Key", "idem-team-1")
	if code != 201 {
		t.Fatalf("create: %d %s", code, first)
	}
	teams, events := count(`SELECT count(*) FROM afs.teams WHERE slug = $1`, "idem-"+w.TeamA.Slug), count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'team.created'`)
	code, again, hdr := post(t, c.Handler, "/v1/teams", svc, body, "Idempotency-Key", "idem-team-1")
	if code != 201 || !sameJSON(again, first) || hdr.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d replayed=%q same=%v", code, hdr.Get("Idempotent-Replayed"), sameJSON(again, first))
	}
	if count(`SELECT count(*) FROM afs.teams WHERE slug = $1`, "idem-"+w.TeamA.Slug) != teams ||
		count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'team.created'`) != events {
		t.Fatal("a replay wrote rows")
	}
	if code, out, _ := post(t, c.Handler, "/v1/teams", svc, strings.Replace(body, "Idem", "Other", 1), "Idempotency-Key", "idem-team-1"); code != 409 || !strings.Contains(out, "idempotency_mismatch") {
		t.Fatalf("different body: %d %s", code, out)
	}

	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	kb := `{"name":"idem","role":"team_viewer","expiresAt":"` + exp + `"}`
	path := "/v1/teams/" + w.TeamA.ID + "/api-keys"
	code, created, _ := post(t, c.Handler, path, adm, kb, "Idempotency-Key", "idem-key-1")
	if code != 201 || !strings.Contains(created, `"secretKey":"afsk_`) {
		t.Fatalf("create key: %d %s", code, created)
	}
	code, replay, hdr := post(t, c.Handler, path, adm, kb, "Idempotency-Key", "idem-key-1")
	if code != 201 || hdr.Get("Idempotent-Replayed") != "true" || strings.Contains(replay, "secretKey") {
		t.Fatalf("key replay: %d %q %s", code, hdr.Get("Idempotent-Replayed"), replay)
	}
	// The stored response never holds the secret, in the database either.
	if n := count(`SELECT count(*) FROM afs.idempotency_keys WHERE response_body::text LIKE '%secretKey%'`); n != 0 {
		t.Fatalf("%d stored responses contain secretKey", n)
	}
}

func sameJSON(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && reflect.DeepEqual(x, y)
}
