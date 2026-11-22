package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// D-4 (FR-R07 AC3): two replicas built from the same settings redeem each other's nextToken; a replica
// with a different key refuses it.
func TestPageTokenSurvivesReplicas(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	mk := func(key string, owner string) *components {
		t.Helper()
		c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: owner, APIReplicas: 2,
			PageTokenKey: []byte(key), IdempotencyHMACKey: []byte(key), OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		return c
	}
	key := strings.Repeat("a", 32)
	r1, r2, other := mk(key, db.URLs["afs_owner"]), mk(key, ""), mk(strings.Repeat("b", 32), "")
	tok := w.Tokens[matrix.ServiceAdmin]

	code, out, _ := agentCall(t, r1.Handler, http.MethodGet, "/v1/teams?maxResults=1", tok, "")
	var page struct {
		NextToken string `json:"nextToken"`
	}
	if err := json.Unmarshal([]byte(out), &page); code != 200 || err != nil || page.NextToken == "" {
		t.Fatalf("first page: %d %s", code, out)
	}
	next := "/v1/teams?maxResults=1&nextToken=" + url.QueryEscape(page.NextToken)
	if code, out, _ := agentCall(t, r2.Handler, http.MethodGet, next, tok, ""); code != 200 {
		t.Fatalf("replica 2 refused replica 1's token: %d %s", code, out)
	}
	if code, out, _ := agentCall(t, other.Handler, http.MethodGet, next, tok, ""); code != 400 {
		t.Fatalf("a replica with another key accepted the token: %d %s", code, out)
	}
}
