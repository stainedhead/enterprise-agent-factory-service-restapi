package matrix

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
)

// ClientID is the audience of the matrix identity provider.
const ClientID = "afs-matrix"

// IDP is a fake OIDC identity provider on a TLS test server with one RSA key.
type IDP struct {
	Server *httptest.Server
	key    *rsa.PrivateKey
}

// NewIDP starts a fake identity provider; it stops when t ends.
func NewIDP(t testing.TB) *IDP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &IDP{key: k}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": p.Server.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	p.Server = httptest.NewTLSServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Token signs an access token for subject with the given groups.
func (p *IDP) Token(t testing.TB, sub, email string, groups []string) string {
	t.Helper()
	claims := map[string]any{"iss": p.Server.URL, "sub": sub, "aud": ClientID, "exp": time.Now().Add(time.Hour).Unix(),
		"email": email, "name": sub, "groups": groups}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := s.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// World is the seeded database the matrix runs against: two teams, a default identity provider, and
// one credential per caller kind.
type World struct {
	DB    *pgtest.DB
	Admin *pgx.Conn
	IDP   *IDP
	// TeamA is the team the cases target; TeamB is another team nobody in the matrix is bound to.
	TeamA, TeamB pgtest.Team
	// Tokens maps a caller to the bearer token it sends; a caller without one is skipped.
	Tokens     map[Caller]string
	ProviderID string
}

// NewWorld seeds db. The caller kinds are: three people through group bindings (viewer and team admin
// on team A, service admin), a security operator by group, an unbound person, team keys of each role
// on team A and a service-level security operator key. The workload caller has no token until workload
// authentication exists (M6-D2).
func NewWorld(t testing.TB, db *pgtest.DB) *World {
	t.Helper()
	w := &World{DB: db, Admin: db.Admin(t), IDP: NewIDP(t), Tokens: map[Caller]string{}}
	w.TeamA, w.TeamB = db.SeedTeam(t), db.SeedTeam(t)
	w.ProviderID = pgtest.NewID("idp")
	w.exec(t, `INSERT INTO afs.identity_providers (id, kind, name, issuer_url, client_id, is_default) VALUES ($1, 'okta', 'Matrix IdP', $2, $3, true)`,
		w.ProviderID, w.IDP.Server.URL, ClientID)
	admin := w.Admin
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.identity_providers WHERE id = $1`, w.ProviderID)
	})

	groups := map[Caller]struct{ name, role, team string }{
		Viewer:           {"mx-viewers-" + w.TeamA.Slug, "team_viewer", w.TeamA.ID},
		TeamAdmin:        {"mx-admins-" + w.TeamA.Slug, "team_admin", w.TeamA.ID},
		ServiceAdmin:     {"mx-service-admins-" + w.TeamA.Slug, "service_admin", ""},
		SecurityOperator: {"mx-security-" + w.TeamA.Slug, "security_operator", ""},
	}
	for caller, g := range groups {
		var team any
		if g.team != "" {
			team = g.team
		}
		w.exec(t, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, $2, $3, 'group', $4)`,
			pgtest.NewID("rb"), g.role, team, g.name)
		w.Tokens[caller] = w.IDP.Token(t, "sub-"+string(caller)+"-"+w.TeamA.Slug, string(caller)+"@"+w.TeamA.Slug+".example.com", []string{g.name})
	}
	w.Tokens[Unbound] = w.IDP.Token(t, "sub-unbound-"+w.TeamA.Slug, "unbound@"+w.TeamA.Slug+".example.com", []string{"mx-nothing"})

	key := func(caller Caller, team any, role string) {
		secret := "afsk_matrix_" + string(caller) + "_" + w.TeamA.Slug
		sum := auth.HashAPIKey(secret)
		w.exec(t, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at) VALUES ($1, $2, $3, $4, 'afsk_mx', $5, now() + interval '60 days')`,
			pgtest.NewID("key"), team, "mx-"+string(caller), role, sum[:])
		w.Tokens[caller] = secret
	}
	key(TeamKeyViewer, w.TeamA.ID, "team_viewer")
	key(TeamKeyAdmin, w.TeamA.ID, "team_admin")
	key(ServiceKey, nil, "security_operator")
	return w
}

func (w *World) exec(t testing.TB, q string, args ...any) {
	t.Helper()
	if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
		t.Fatalf("matrix seed: %v\n%s", err, q)
	}
}

// OIDCClient trusts the matrix identity provider's certificate; pass it to the server's OIDC settings.
func (w *World) OIDCClient() *http.Client { return w.IDP.Server.Client() }

// SetFlags sets the guardrail flags of team A, which conditional grants read.
func (w *World) SetFlags(t testing.TB, f authz.TeamFlags) {
	t.Helper()
	w.exec(t, `UPDATE afs.teams SET may_manage_aws_accounts = $2, viewer_kubectl = $3, may_add_external_systems = $4 WHERE id = $1`,
		w.TeamA.ID, f.MayManageAwsAccounts, f.ViewerKubectl, f.MayAddExternalSystems)
}
