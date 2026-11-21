package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
)

// fakeIDP is an OIDC provider on a TLS test server with one RSA key.
type fakeIDP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIDP{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIDP) token(t *testing.T, sub, email string, groups []string, extra map[string]any) string {
	t.Helper()
	claims := map[string]any{"iss": p.srv.URL, "sub": sub, "aud": "afs-test", "exp": time.Now().Add(time.Hour).Unix(),
		"email": email, "name": "Test " + sub, "groups": groups}
	for k, v := range extra {
		claims[k] = v
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := obj.CompactSerialize()
	return tok
}

type result struct {
	code int
	body string
	hdr  http.Header
}

func call(t *testing.T, h http.Handler, method, path, token string, hdr ...string) result {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(""))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.77:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return result{rec.Code, rec.Body.String(), rec.Header()}
}

type meBody struct {
	Principal struct {
		ID, Type, Email, Name string
	} `json:"principal"`
	ServiceAdmin       bool     `json:"serviceAdmin"`
	SecurityOperator   bool     `json:"securityOperator"`
	ServicePermissions []string `json:"servicePermissions"`
	Teams              []struct {
		TeamID      string   `json:"teamId"`
		Slug        string   `json:"slug"`
		Role        string   `json:"role"`
		Permissions []string `json:"permissions"`
	} `json:"teams"`
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// M1-A and M1-D end to end: a fake identity provider, a real database, the real wiring.
func TestAuthenticationEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	ctx := t.Context()
	admin := db.Admin(t)
	idp := newFakeIDP(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	idpID := pgtest.NewID("idp")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, is_default) VALUES ($1, 'okta', 'Test IdP', $2, 'afs-test', true)`, idpID, idp.srv.URL)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.identity_providers WHERE id = $1`, idpID)
	})
	exec(`UPDATE afs.teams SET viewer_kubectl = true WHERE id = $1`, a.ID)
	groupAdmin, groupViewer, groupOps := "a-admins-"+a.Slug, "a-viewers-"+a.Slug, "ops-"+a.Slug
	exec(`INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES
		($1, 'team_admin', $2, 'group', $3), ($4, 'team_viewer', $2, 'group', $5), ($6, 'service_admin', NULL, 'group', $7)`,
		pgtest.NewID("rb"), a.ID, groupAdmin, pgtest.NewID("rb"), groupViewer, pgtest.NewID("rb"), groupOps)
	exec(`INSERT INTO afs.service_settings (key, value) VALUES ('api.cors_origins', '["https://console.example.com"]') ON CONFLICT (key) DO UPDATE SET value = excluded.value`)

	keys := map[string]string{"viewer": "afsk_viewer_" + a.ID, "admin": "afsk_admin_" + a.ID, "svc": "afsk_svc_" + a.ID}
	for name, role := range map[string]string{"viewer": "team_viewer", "admin": "team_admin"} {
		sum := auth.HashAPIKey(keys[name])
		exec(`INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, $2, $3, $4, 'afsk_xx', $5, now() + interval '30 days')`,
			pgtest.NewID("key"), a.ID, name, role, sum[:])
	}
	sum := auth.HashAPIKey(keys["svc"])
	exec(`INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, NULL, 'scanner', 'security_operator', 'afsk_sv', $2, now() + interval '30 days')`, pgtest.NewID("key"), sum[:])

	c, err := wire(ctx, quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1, OIDCHTTP: idp.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h := c.Handler
	clock := &testClock{t: time.Now()}
	c.Auth.Clock = clock

	t.Run("sign-in list is public and carries no secret (RA-US-2.5)", func(t *testing.T) {
		r := call(t, h, "GET", "/v1/identity-providers", "")
		if r.code != 200 || !strings.Contains(r.body, idpID) || !strings.Contains(r.body, `"consoleClientId":"afs-test"`) || !strings.Contains(r.body, `"cliClientId":"afs-test"`) ||
			strings.Contains(strings.ToLower(r.body), "secret") {
			t.Fatalf("%d %s", r.code, r.body)
		}
	})

	t.Run("401 for no credential, a bad token, an expired token, the wrong audience", func(t *testing.T) {
		for name, tok := range map[string]string{
			"none": "", "garbage": "not-a-token", "unknown key": "afsk_nope",
			"expired":  idp.token(t, "u9", "u9@example.com", nil, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}),
			"audience": idp.token(t, "u9", "u9@example.com", nil, map[string]any{"aud": "other"}),
		} {
			r := call(t, h, "GET", "/v1/me", tok)
			if r.code != 401 || r.hdr.Get("X-Afs-Contract-Version") == "" || r.hdr.Get("X-Request-Id") == "" {
				t.Fatalf("%s: %d %s", name, r.code, r.body)
			}
		}
	})

	var adminMe meBody
	t.Run("GetMe for a team admin: JIT user, group binding, flag-dependent permissions (RA-US-3a.1)", func(t *testing.T) {
		tok := idp.token(t, "sub-admin", "admin@"+a.Slug+".example.com", []string{groupAdmin}, nil)
		r := call(t, h, "GET", "/v1/me", tok)
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		if err := json.Unmarshal([]byte(r.body), &adminMe); err != nil {
			t.Fatal(err)
		}
		if adminMe.Principal.Type != "user" || !strings.HasPrefix(adminMe.Principal.ID, "usr_") || adminMe.Principal.Email != "admin@"+a.Slug+".example.com" ||
			adminMe.ServiceAdmin || adminMe.SecurityOperator || len(adminMe.Teams) != 1 {
			t.Fatalf("%s", r.body)
		}
		team := adminMe.Teams[0]
		if team.TeamID != a.ID || team.Slug != a.Slug || team.Role != "team_admin" || !contains(team.Permissions, "agents:write") ||
			!contains(team.Permissions, "teams:get-kubeconfig") || contains(team.Permissions, "teams:set-aws-accounts") || contains(team.Permissions, "teams:create") {
			t.Fatalf("permissions = %v", team.Permissions)
		}
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.users WHERE id = $1 AND oidc_subject = 'sub-admin'`, adminMe.Principal.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("users row not created: %d %v", n, err)
		}
	})

	t.Run("GetMe for a viewer, a service admin and an unbound person", func(t *testing.T) {
		var me meBody
		r := call(t, h, "GET", "/v1/me", idp.token(t, "sub-viewer", "viewer@"+a.Slug+".example.com", []string{groupViewer}, nil))
		if err := json.Unmarshal([]byte(r.body), &me); err != nil || r.code != 200 || len(me.Teams) != 1 || me.Teams[0].Role != "team_viewer" || contains(me.Teams[0].Permissions, "agents:write") {
			t.Fatalf("viewer: %d %s", r.code, r.body)
		}
		me = meBody{}
		r = call(t, h, "GET", "/v1/me", idp.token(t, "sub-ops", "ops@"+a.Slug+".example.com", []string{groupOps}, nil))
		if err := json.Unmarshal([]byte(r.body), &me); err != nil || !me.ServiceAdmin || len(me.ServicePermissions) < 50 {
			t.Fatalf("service admin: %d %s", r.code, r.body)
		}
		me = meBody{}
		r = call(t, h, "GET", "/v1/me", idp.token(t, "sub-nobody", "nobody@"+a.Slug+".example.com", []string{"no-such-group"}, nil))
		if err := json.Unmarshal([]byte(r.body), &me); err != nil || r.code != 200 || me.ServiceAdmin || len(me.Teams) != 0 {
			t.Fatalf("unbound: %d %s", r.code, r.body)
		}
	})

	t.Run("API keys (RA-US-2.4, RA-US-3a.1)", func(t *testing.T) {
		var me meBody
		r := call(t, h, "GET", "/v1/me", keys["admin"])
		if err := json.Unmarshal([]byte(r.body), &me); err != nil || me.Principal.Type != "api_key" || len(me.Teams) != 1 || me.Teams[0].Role != "team_admin" {
			t.Fatalf("team key: %d %s", r.code, r.body)
		}
		me = meBody{}
		r = call(t, h, "GET", "/v1/me", keys["svc"])
		if err := json.Unmarshal([]byte(r.body), &me); err != nil || !me.SecurityOperator || len(me.Teams) != 0 || !contains(me.ServicePermissions, "work-orders:write") {
			t.Fatalf("service key: %d %s", r.code, r.body)
		}
	})

	t.Run("route authorization with real team flags: 404, 403 with a denied audit row, allowed", func(t *testing.T) {
		adminTok := idp.token(t, "sub-admin", "admin@"+a.Slug+".example.com", []string{groupAdmin}, nil)
		// Allowed callers reach the placeholder handler (501): authorization let them through.
		if r := call(t, h, "GET", "/v1/teams/"+a.ID, adminTok); r.code != 501 {
			t.Fatalf("admin on own team: %d %s", r.code, r.body)
		}
		// A person bound to team A only: team B does not exist for them.
		if r := call(t, h, "GET", "/v1/teams/"+b.ID, adminTok); r.code != 404 {
			t.Fatalf("admin on another team: %d %s", r.code, r.body)
		}
		// A viewer writing: 403 and one denied audit row naming the permission.
		before := auditCount(t, admin, "CreateApiKey", a.ID)
		r := call(t, h, "POST", "/v1/teams/"+a.ID+"/api-keys", keys["viewer"], "Idempotency-Key", "k1", "X-Request-Id", "req-e2e-denied")
		if r.code != 403 || !strings.Contains(r.body, `"requiredPermission":"api-keys:write"`) {
			t.Fatalf("viewer create key: %d %s", r.code, r.body)
		}
		if n := auditCount(t, admin, "CreateApiKey", a.ID); n != before+1 {
			t.Fatalf("denied audit rows: %d, want %d", n, before+1)
		}
		var outcome, actor, reqID string
		if err := admin.QueryRow(ctx, `SELECT outcome, actor_id, request_id FROM afs.audit_log WHERE request_id = 'req-e2e-denied'`).Scan(&outcome, &actor, &reqID); err != nil ||
			outcome != "denied" || !strings.HasPrefix(actor, "key_") {
			t.Fatalf("audit row: %s %s %v", outcome, actor, err)
		}
		// The security operator key is denied every team route, and the denial is audited although the
		// team is not visible to it (X-DB-6).
		before = auditCount(t, admin, "GetTeam", a.ID)
		if r := call(t, h, "GET", "/v1/teams/"+a.ID, keys["svc"]); r.code != 403 {
			t.Fatalf("security operator on a team route: %d %s", r.code, r.body)
		}
		if n := auditCount(t, admin, "GetTeam", a.ID); n != before+1 {
			t.Fatalf("security operator denial not audited: %d", n)
		}
		// A service-route denial with no team at all (X-DB-6).
		if r := call(t, h, "POST", "/v1/teams", adminTok); r.code != 403 {
			t.Fatalf("team admin creating a team: %d", r.code)
		}
		if n := auditCount(t, admin, "CreateTeam", ""); n < 1 {
			t.Fatal("denied audit on a service route missing")
		}
	})

	t.Run("a disabled provider stops authenticating once the provider cache expires (RA-US-2.1)", func(t *testing.T) {
		tok := idp.token(t, "sub-admin", "admin@"+a.Slug+".example.com", []string{groupAdmin}, nil)
		if r := call(t, h, "GET", "/v1/me", tok); r.code != 200 {
			t.Fatalf("%d", r.code)
		}
		exec(`UPDATE afs.identity_providers SET is_default = false WHERE id = $1`, idpID)
		exec(`UPDATE afs.identity_providers SET enabled = false WHERE id = $1`, idpID)
		clock.advance(time.Minute)
		if r := call(t, h, "GET", "/v1/me", tok); r.code != 401 {
			t.Fatalf("disabled provider: %d %s", r.code, r.body)
		}
		if r := call(t, h, "GET", "/v1/identity-providers", ""); r.code != 200 || strings.Contains(r.body, idpID) {
			t.Fatalf("disabled provider still offered: %s", r.body)
		}
	})

	t.Run("CORS follows api.cors_origins (RA-US-15.2)", func(t *testing.T) {
		r := call(t, h, "OPTIONS", "/v1/teams", "", "Origin", "https://console.example.com", "Access-Control-Request-Method", "GET")
		if r.code != 204 || r.hdr.Get("Access-Control-Allow-Origin") != "https://console.example.com" || r.hdr.Get("X-Request-Id") == "" {
			t.Fatalf("listed origin preflight: %d %v", r.code, r.hdr)
		}
		r = call(t, h, "GET", "/v1/identity-providers", "", "Origin", "https://evil.example.com")
		if r.hdr.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("unlisted origin got CORS headers: %v", r.hdr)
		}
	})
}

func auditCount(t *testing.T, q *pgx.Conn, op, team string) int {
	t.Helper()
	var n int
	if err := q.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = $1 AND outcome = 'denied' AND coalesce(team_id, '') = $2`, op, team).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }
