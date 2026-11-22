package main

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// FR-R02 through the real pipeline and database: an allowed mutation and a denied one, sent with the same
// socket peer and X-Forwarded-For, record the same source_ip, and it honors AFS_TRUSTED_PROXY_HOPS.
func TestAuditSourceIPMatchesForAllowedAndDeniedMutations(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	for _, tc := range []struct {
		hops int
		want string
	}{{0, "192.0.2.1"}, {1, "10.0.0.5"}, {2, "203.0.113.7"}} {
		c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
			TrustedProxyHops: tc.hops, OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
		if err != nil {
			t.Fatal(err)
		}
		send := func(token, slug string) int {
			body := `{"slug":"` + slug + `","displayName":"IP","kind":"persistent","costCenter":"CC1","ownerEmail":"o@example.com","maxBudgetUsd":"10","firstAdmin":{"subjectType":"group","groupName":"ip-g"},"tags":[]}`
			r := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/teams", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5")
			r.RemoteAddr = "192.0.2.1:4000"
			rec := httptest.NewRecorder()
			c.Handler.ServeHTTP(rec, r)
			return rec.Code
		}
		slug := strings.ToLower("ip"+strings.ReplaceAll(w.TeamA.Slug, "-", "")[:6]) + fmt.Sprint(tc.hops)
		if code := send(w.Tokens[matrix.ServiceAdmin], slug); code != 201 {
			t.Fatalf("hops %d allowed create: %d", tc.hops, code)
		}
		// Keep the shared directory small: later tests list it on one page.
		if _, err := w.Admin.Exec(t.Context(), `DELETE FROM afs.teams WHERE slug = $1`, slug); err != nil {
			t.Logf("cleanup of team %s: %v", slug, err)
		}
		if code := send(w.Tokens[matrix.Viewer], slug+"x"); code != 403 {
			t.Fatalf("hops %d denied create: %d", tc.hops, code)
		}
		for _, outcome := range []string{"allowed", "denied"} {
			var ip string
			if err := w.Admin.QueryRow(t.Context(), `SELECT host(source_ip) FROM afs.audit_log WHERE action = 'CreateTeam' AND outcome = $1 ORDER BY id DESC LIMIT 1`, outcome).Scan(&ip); err != nil {
				t.Fatal(err)
			}
			if ip != tc.want {
				t.Errorf("hops %d: %s record source_ip = %s, want %s", tc.hops, outcome, ip, tc.want)
			}
		}
		c.Close()
	}
}
