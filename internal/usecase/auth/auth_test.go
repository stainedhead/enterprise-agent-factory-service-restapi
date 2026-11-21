package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

var (
	defIDP  = Provider{ID: "idp_default", Kind: "okta", IssuerURL: "https://default.example", ClientID: "c1", IsDefault: true}
	teamIDP = Provider{ID: "idp_teamA", TeamID: teamA, Kind: "entra_id", IssuerURL: "https://team.example", ClientID: "c2"}
)

func ident(p Provider, groups ...string) Identity {
	return Identity{Provider: p, Subject: "s1", Email: "ada@example.com", Name: "Ada", Groups: groups}
}

// RA-US-2.2: a group binding on team A applies only when the token's issuer is team A's provider (or
// the default provider when A has none).
func TestResolveGroupBindings(t *testing.T) {
	cases := []struct {
		name  string
		id    Identity
		rows  []BindingRow
		teams []authz.Binding
		admin bool
		secOp bool
	}{
		{"user binding counts whatever the provider", ident(teamIDP),
			[]BindingRow{{Role: authz.RoleTeamAdmin, TeamID: teamA}}, []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}, false, false},
		{"group binding, team has own provider, token from it", ident(teamIDP, "eng"),
			[]BindingRow{{Role: authz.RoleTeamViewer, TeamID: teamA, Group: "eng", TeamProviderID: "idp_teamA"}},
			[]authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamA}}, false, false},
		{"group binding, team has own provider, token from the default", ident(defIDP, "eng"),
			[]BindingRow{{Role: authz.RoleTeamViewer, TeamID: teamA, Group: "eng", TeamProviderID: "idp_teamA"}}, nil, false, false},
		{"group binding, team has no provider, token from the default", ident(defIDP, "eng"),
			[]BindingRow{{Role: authz.RoleTeamViewer, TeamID: teamB, Group: "eng"}}, []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamB}}, false, false},
		{"group binding, team has no provider, token from another provider", ident(teamIDP, "eng"),
			[]BindingRow{{Role: authz.RoleTeamViewer, TeamID: teamB, Group: "eng"}}, nil, false, false},
		{"group not in the token", ident(defIDP, "ops"),
			[]BindingRow{{Role: authz.RoleTeamViewer, TeamID: teamB, Group: "eng"}}, nil, false, false},
		{"binding names a different provider", ident(defIDP, "eng"),
			[]BindingRow{{Role: authz.RoleTeamViewer, TeamID: teamB, Group: "eng", BindingProviderID: "idp_teamA"}}, nil, false, false},
		{"service admin group via the default provider", ident(defIDP, "admins"),
			[]BindingRow{{Role: authz.RoleServiceAdmin, Group: "admins"}}, nil, true, false},
		{"service admin group via a non-default provider is ignored", ident(teamIDP, "admins"),
			[]BindingRow{{Role: authz.RoleServiceAdmin, Group: "admins"}}, nil, false, false},
		{"service group naming its provider", ident(teamIDP, "sec"),
			[]BindingRow{{Role: authz.RoleSecurityOperator, Group: "sec", BindingProviderID: "idp_teamA"}}, nil, false, true},
		{"service admin user binding", ident(defIDP),
			[]BindingRow{{Role: authz.RoleServiceAdmin}}, nil, true, false},
		{"duplicates collapse", ident(defIDP),
			[]BindingRow{{Role: authz.RoleTeamAdmin, TeamID: teamB}, {Role: authz.RoleTeamAdmin, TeamID: teamB}},
			[]authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamB}}, false, false},
		{"a team role without a team is ignored", ident(defIDP), []BindingRow{{Role: authz.RoleTeamAdmin}}, nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Resolve(c.id, "usr_1", c.rows, "idp_default")
			if len(p.Bindings) != len(c.teams) || p.ServiceAdmin != c.admin || p.SecurityOperator != c.secOp {
				t.Fatalf("principal = %+v", p)
			}
			for i, b := range c.teams {
				if p.Bindings[i] != b {
					t.Fatalf("binding %d = %+v, want %+v", i, p.Bindings[i], b)
				}
			}
			if p.ID != "usr_1" || p.Type != authz.PrincipalUser || p.Email != "ada@example.com" {
				t.Fatalf("principal = %+v", p)
			}
		})
	}
}

type fakeVerifier struct {
	id    Identity
	err   error
	seen  []Provider
	calls int
}

func (f *fakeVerifier) Verify(_ context.Context, _ string, ps []Provider) (Identity, error) {
	f.calls++
	f.seen = ps
	return f.id, f.err
}

type fakeProviders struct {
	ps    []Provider
	err   error
	calls int
}

func (f *fakeProviders) EnabledProviders(context.Context) ([]Provider, error) {
	f.calls++
	return f.ps, f.err
}

type fakeStore struct {
	signIn   UserClaims
	signErr  error
	rows     []BindingRow
	bindErr  error
	gotGroup []string
}

func (f *fakeStore) SignIn(_ context.Context, u UserClaims, _ time.Time) (string, error) {
	f.signIn = u
	return "usr_1", f.signErr
}

func (f *fakeStore) Bindings(_ context.Context, _ string, groups []string) ([]BindingRow, error) {
	f.gotGroup = groups
	return f.rows, f.bindErr
}

func newService() (*Service, *fakeVerifier, *fakeProviders, *fakeStore, *fakes.Credentials, *fakes.Clock) {
	v := &fakeVerifier{id: ident(defIDP, "eng")}
	ps := &fakeProviders{ps: []Provider{defIDP}}
	st := &fakeStore{rows: []BindingRow{{Role: authz.RoleTeamAdmin, TeamID: teamA}}}
	cr := &fakes.Credentials{Keys: map[[32]byte]port.APIKeyRecord{}}
	ck := &fakes.Clock{T: time.Unix(1_800_000_000, 0)}
	return &Service{Verifier: v, Providers: ps, Store: st, Keys: cr, Clock: ck}, v, ps, st, cr, ck
}

func TestAuthenticateUser(t *testing.T) {
	s, v, ps, st, _, ck := newService()
	p, err := s.Authenticate(t.Context(), "jwt-token")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "usr_1" || p.Type != authz.PrincipalUser || len(p.Bindings) != 1 || p.Bindings[0].TeamID != teamA {
		t.Fatalf("principal = %+v", p)
	}
	if st.signIn.Issuer != defIDP.IssuerURL || st.signIn.Subject != "s1" || st.signIn.Email != "ada@example.com" {
		t.Fatalf("sign-in = %+v", st.signIn)
	}
	if len(st.gotGroup) != 1 || st.gotGroup[0] != "eng" {
		t.Fatalf("groups = %v", st.gotGroup)
	}
	// The provider list is cached, so a disabled provider stops working within the TTL.
	_, _ = s.Authenticate(t.Context(), "jwt-token")
	if ps.calls != 1 {
		t.Fatalf("provider reads = %d, want 1 (cached)", ps.calls)
	}
	ps.ps = nil // the provider is disabled
	ck.Advance(providerCacheTTL + time.Second)
	_, _ = s.Authenticate(t.Context(), "jwt-token")
	if ps.calls != 2 || len(v.seen) != 0 {
		t.Fatalf("after the TTL: reads = %d, providers offered to the verifier = %v", ps.calls, v.seen)
	}
}

func TestAuthenticateUserFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Service, *fakeVerifier, *fakeProviders, *fakeStore)
		want  domain.ErrorKind
	}{
		{"verifier refuses", func(_ *Service, v *fakeVerifier, _ *fakeProviders, _ *fakeStore) { v.err = domain.Unauthenticated() }, domain.KindUnauthenticated},
		{"keys unavailable", func(_ *Service, v *fakeVerifier, _ *fakeProviders, _ *fakeStore) {
			v.err = domain.Unavailable("identity provider keys", errors.New("down"))
		}, domain.KindUnavailable},
		{"disabled user", func(_ *Service, _ *fakeVerifier, _ *fakeProviders, st *fakeStore) { st.signErr = domain.Unauthenticated() }, domain.KindUnauthenticated},
		{"database down on bindings", func(_ *Service, _ *fakeVerifier, _ *fakeProviders, st *fakeStore) {
			st.bindErr = domain.Unavailable("database", errors.New("x"))
		}, domain.KindUnavailable},
		{"providers unreadable", func(_ *Service, _ *fakeVerifier, ps *fakeProviders, _ *fakeStore) {
			ps.err = domain.Unavailable("database", errors.New("x"))
		}, domain.KindUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, v, ps, st, _, _ := newService()
			c.setup(s, v, ps, st)
			p, err := s.Authenticate(t.Context(), "jwt-token")
			if p != nil || domain.KindOf(err) != c.want {
				t.Fatalf("p=%v err=%v, want %s", p, err, c.want)
			}
		})
	}
	s, _, _, _, _, _ := newService()
	if _, err := s.Authenticate(t.Context(), ""); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("empty token: %v", err)
	}
}

// RA-US-2.4: API keys.
func TestAuthenticateAPIKey(t *testing.T) {
	future := time.Unix(1_900_000_000, 0)
	key := func(k string, rec port.APIKeyRecord) (string, port.APIKeyRecord) { return k, rec }
	cases := []struct {
		name    string
		token   string
		rec     *port.APIKeyRecord
		wantErr bool
		check   func(*authz.Principal) bool
	}{
		{"team admin key", "afsk_good", &port.APIKeyRecord{KeyID: "key_1", TeamID: teamA, Role: "team_admin", State: "active", TeamState: "active", ExpiresAt: &future}, false,
			func(p *authz.Principal) bool {
				return p.ID == "key_1" && p.Type == authz.PrincipalAPIKey && len(p.Bindings) == 1 && p.Bindings[0] == authz.Binding{Role: authz.RoleTeamAdmin, TeamID: teamA} && !p.SecurityOperator
			}},
		{"team viewer key", "afsk_v", &port.APIKeyRecord{KeyID: "key_2", TeamID: teamA, Role: "team_viewer", State: "active", TeamState: "active"}, false,
			func(p *authz.Principal) bool { return p.Bindings[0].Role == authz.RoleTeamViewer }},
		{"service key", "afsk_svc", &port.APIKeyRecord{KeyID: "key_3", Role: "security_operator", State: "active"}, false,
			func(p *authz.Principal) bool { return p.SecurityOperator && len(p.Bindings) == 0 && !p.ServiceAdmin }},
		{"expired", "afsk_old", &port.APIKeyRecord{KeyID: "key_4", TeamID: teamA, Role: "team_admin", State: "expired", TeamState: "active"}, true, nil},
		{"revoked (deleted)", "afsk_del", &port.APIKeyRecord{KeyID: "key_5", TeamID: teamA, Role: "team_admin", State: "revoked", TeamState: "active"}, true, nil},
		{"team suspended", "afsk_susp", &port.APIKeyRecord{KeyID: "key_6", TeamID: teamA, Role: "team_admin", State: "active", TeamState: "suspended"}, true, nil},
		{"role does not fit scope", "afsk_odd", &port.APIKeyRecord{KeyID: "key_7", TeamID: teamA, Role: "security_operator", State: "active", TeamState: "active"}, true, nil},
		{"unknown key", "afsk_nope", nil, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, v, _, _, cr, _ := newService()
			if c.rec != nil {
				k, r := key(c.token, *c.rec)
				cr.Keys[HashAPIKey(k)] = r
			}
			p, err := s.Authenticate(t.Context(), c.token)
			if v.calls != 0 {
				t.Fatal("an API key reached the IdP verifier")
			}
			if c.wantErr {
				if p != nil || domain.KindOf(err) != domain.KindUnauthenticated {
					t.Fatalf("p=%v err=%v, want Unauthenticated", p, err)
				}
				return
			}
			if err != nil || !c.check(p) {
				t.Fatalf("p=%+v err=%v", p, err)
			}
		})
	}
}

type fakeWorkload struct{ p *authz.Principal }

func (f fakeWorkload) Matches(tok string) bool { return tok == "wl" }
func (f fakeWorkload) Authenticate(context.Context, string) (*authz.Principal, error) {
	return f.p, nil
}

func TestWorkloadSeamAndAPIKeyHash(t *testing.T) {
	s, v, _, _, _, _ := newService()
	want := &authz.Principal{ID: "agt_1", Type: authz.PrincipalWorkload}
	s.Workload = fakeWorkload{want}
	if p, err := s.Authenticate(t.Context(), "wl"); err != nil || p != want {
		t.Fatalf("workload: %v %v", p, err)
	}
	if v.calls != 0 {
		t.Fatal("workload token reached the IdP verifier")
	}
	if HashAPIKey("afsk_x") == HashAPIKey("afsk_y") {
		t.Fatal("hash collision")
	}
}

func TestSessionOf(t *testing.T) {
	cases := []struct {
		p    authz.Principal
		want port.ActorType
	}{
		{authz.Principal{ID: "usr_1", Type: authz.PrincipalUser}, port.ActorUser},
		{authz.Principal{ID: "key_1", Type: authz.PrincipalAPIKey}, port.ActorAPIKey},
		{authz.Principal{ID: "agt_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{AgentID: "agt_1"}}, port.ActorAgent},
		{authz.Principal{ID: "pool_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{PoolID: "pool_1"}}, port.ActorPool},
	}
	for _, c := range cases {
		s := SessionOf(c.p, "req_1")
		if s.ActorType != c.want || s.Actor != c.p.ID || s.RequestID != "req_1" {
			t.Errorf("%+v -> %+v", c.p, s)
		}
	}
}
