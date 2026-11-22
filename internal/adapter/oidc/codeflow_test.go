package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

type secrets struct {
	val string
	err error
}

func (s secrets) Secret(context.Context, string) (string, error) { return s.val, s.err }

type flowRig struct {
	p      *idp
	srv    *httptest.Server
	cf     *CodeFlow
	route  collab.Route
	idTok  string
	status int
	form   url.Values
}

func newFlow(t *testing.T, sec secrets) *flowRig {
	t.Helper()
	p, v, _, _ := setup(t)
	f := &flowRig{p: p, status: 200}
	mux := http.NewServeMux()
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"authorization_endpoint": f.srv.URL + "/authorize?tenant=x",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": p.srv.URL + "/keys"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.form = r.PostForm
		if f.status != 200 {
			http.Error(w, "no", f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": f.idTok})
	})
	v.opts.HTTP = f.srv.Client()
	f.cf = NewCodeFlow(v, sec)
	f.route = collab.Route{TeamID: "team_01", IssuerURL: f.srv.URL, ClientID: "cid", GroupsClaim: "groups", ClientSecretARN: "arn:aws:secretsmanager:eu-west-1:000000000000:secret:x"} //nolint:gosec // fake ARN
	return f
}

func (f *flowRig) token(t *testing.T, extra map[string]any) string {
	c := map[string]any{"iss": f.srv.URL, "sub": "u-1", "aud": []string{"cid", "other"}, "exp": t0.Add(time.Hour).Unix(),
		"nonce": "n1", "groups": []string{"eng"}}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return f.p.sign(t, "k1", c)
}

func TestCodeFlowAuthURL(t *testing.T) {
	f := newFlow(t, secrets{val: "s"}) //nolint:gosec // test fixture
	u, err := f.cf.AuthURL(t.Context(), f.route, "https://apex/oauth2/callback", "st", "ch", "n1", "")
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	q := pu.Query()
	for k, want := range map[string]string{"response_type": "code", "client_id": "cid", "state": "st", "nonce": "n1",
		"code_challenge": "ch", "code_challenge_method": "S256", "tenant": "x", "redirect_uri": "https://apex/oauth2/callback"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q", k, q.Get(k))
		}
	}
	// discovery is cached
	if _, err := f.cf.AuthURL(t.Context(), f.route, "r", "s", "c", "n", ""); err != nil {
		t.Fatal(err)
	}
	r := f.route
	r.ClientID = ""
	if _, err := f.cf.AuthURL(t.Context(), r, "r", "s", "c", "n", ""); err == nil {
		t.Error("no client id must fail")
	}
	r = f.route
	r.IssuerURL = "https://127.0.0.1:1"
	if _, err := f.cf.AuthURL(t.Context(), r, "r", "s", "c", "n", ""); err == nil {
		t.Error("unreachable issuer must fail")
	}
}

func TestCodeFlowDiscoveryRejectsPlainHTTP(t *testing.T) {
	p, v, _, _ := setup(t)
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"authorization_endpoint": "http://x/a", "token_endpoint": "https://x/t"}) //nolint:gosec // test fixture
	}))
	defer bad.Close()
	v.opts.HTTP = bad.Client()
	_ = p
	cf := NewCodeFlow(v, secrets{})
	if _, err := cf.AuthURL(t.Context(), collab.Route{IssuerURL: bad.URL, ClientID: "c"}, "r", "s", "c", "n", ""); err == nil {
		t.Fatal("http authorization endpoint must be refused")
	}
}

func TestCodeFlowExchange(t *testing.T) {
	f := newFlow(t, secrets{val: "shh"})
	ctx := t.Context()
	f.idTok = f.token(t, nil)
	cl, err := f.cf.Exchange(ctx, f.route, "https://apex/cb", "code1", "verifier1")
	if err != nil || cl.Subject != "u-1" || cl.Nonce != "n1" || len(cl.Groups) != 1 || cl.Issuer != f.srv.URL {
		t.Fatalf("%+v %v", cl, err)
	}
	for k, want := range map[string]string{"grant_type": "authorization_code", "code": "code1", "code_verifier": "verifier1",
		"client_id": "cid", "redirect_uri": "https://apex/cb", "client_secret": "shh"} {
		if f.form.Get(k) != want {
			t.Errorf("form %s = %q", k, f.form.Get(k))
		}
	}
	f.status = 400
	if _, err := f.cf.Exchange(ctx, f.route, "r", "c", "v"); err == nil || strings.Contains(err.Error(), "shh") {
		t.Errorf("token endpoint error: %v", err)
	}
	f.status = 200
	f.idTok = ""
	if _, err := f.cf.Exchange(ctx, f.route, "r", "c", "v"); err == nil {
		t.Error("missing id_token")
	}
	f.idTok = f.token(t, map[string]any{"aud": "other"})
	if _, err := f.cf.Exchange(ctx, f.route, "r", "c", "v"); err == nil {
		t.Error("wrong audience")
	}
	f.cf.secrets = secrets{err: errors.New("arn:secret detail")}
	if _, err := f.cf.Exchange(ctx, f.route, "r", "c", "v"); err == nil || strings.Contains(err.Error(), "detail") {
		t.Errorf("secret failure must not leak: %v", err)
	}
	r := f.route
	r.IssuerURL = "https://127.0.0.1:1"
	if _, err := f.cf.Exchange(ctx, r, "r", "c", "v"); err == nil {
		t.Error("discovery failure")
	}
	// unreachable token endpoint
	f2 := newFlow(t, secrets{val: "s"})
	f2.cf.v.opts.HTTP = f2.srv.Client()
	f2.srv.Close()
	f2.cf.disc[normalize(f2.route.IssuerURL)] = discovery{AuthEndpoint: "https://x", TokenEndpoint: "https://127.0.0.1:1/t", fetched: f2.cf.v.opts.Now()} //nolint:gosec // test fixture
	if _, err := f2.cf.Exchange(ctx, f2.route, "r", "c", "v"); err == nil {
		t.Error("unreachable token endpoint")
	}
}

func TestCodeFlowVerifyBearer(t *testing.T) {
	f := newFlow(t, secrets{})
	ctx := t.Context()
	if _, err := f.cf.VerifyBearer(ctx, f.route, f.token(t, nil)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		tok     string
		expired bool
	}{
		"expired":      {f.token(t, map[string]any{"exp": t0.Add(-time.Hour).Unix()}), true},
		"wrong issuer": {f.token(t, map[string]any{"iss": "https://evil"}), false},
		"no sub":       {f.token(t, map[string]any{"sub": ""}), false},
		"no exp":       {f.token(t, map[string]any{"exp": nil}), false},
		"not yet":      {f.token(t, map[string]any{"nbf": t0.Add(time.Hour).Unix()}), false},
		"garbage":      {"nope", false},
	} {
		_, err := f.cf.VerifyBearer(ctx, f.route, c.tok)
		var te *edge.TokenError
		if !errors.As(err, &te) || te.Expired != c.expired {
			t.Errorf("%s: %v", name, err)
		}
	}
	r := f.route
	r.ClientID = ""
	if _, err := f.cf.VerifyBearer(ctx, r, f.token(t, nil)); err == nil {
		t.Error("no client")
	}
	f.p.remove("k1")
	f.p.addRSA(t, "k2")
	tok := f.p.sign(t, "k2", map[string]any{"iss": f.srv.URL, "sub": "x", "aud": "cid", "exp": t0.Add(time.Hour).Unix()})
	f.p.remove("k2")
	f.p.addRSA(t, "k3")
	if _, err := f.cf.VerifyBearer(ctx, f.route, tok); err == nil {
		t.Error("signature by an unknown key")
	}
}

func TestCodeFlowAuthURLPrompt(t *testing.T) {
	f := newFlow(t, secrets{})
	u, err := f.cf.AuthURL(t.Context(), f.route, "r", "s", "c", "n", "none")
	if err != nil {
		t.Fatal(err)
	}
	if pu, _ := url.Parse(u); pu.Query().Get("prompt") != "none" {
		t.Errorf("prompt: %s", u)
	}
	u, _ = f.cf.AuthURL(t.Context(), f.route, "r", "s", "c", "n", "")
	if pu, _ := url.Parse(u); pu.Query().Has("prompt") {
		t.Errorf("no prompt expected: %s", u)
	}
}

// Finding 6: bearer audiences follow afs-api (the provider's own client id); the collaboration client id is
// an additional audience, never a requirement.
func TestCodeFlowVerifyBearerAudience(t *testing.T) {
	f := newFlow(t, secrets{})
	ctx := t.Context()
	tok := func(aud any) string { return f.token(t, map[string]any{"aud": aud}) }
	r := f.route
	r.ProviderClientID = "cli-client"
	for name, c := range map[string]struct {
		route collab.Route
		aud   any
		ok    bool
	}{
		"CLI SSO token issued to the provider's client":  {r, "cli-client", true},
		"provider client among several audiences":        {r, []string{"x", "cli-client"}, true},
		"collab client audience still accepted":          {r, "cid", true},
		"neither audience":                               {r, "someone-else", false},
		"REST API only team: no collab client":           {func() collab.Route { x := r; x.ClientID = ""; return x }(), "cli-client", true},
		"REST API only team, token for another client":   {func() collab.Route { x := r; x.ClientID = ""; return x }(), "someone-else", false},
		"empty audience entry never matches an empty id": {func() collab.Route { x := r; x.ClientID = ""; x.ProviderClientID = ""; return x }(), "", false},
		"no client ids at all":                           {func() collab.Route { x := r; x.ClientID = ""; x.ProviderClientID = ""; return x }(), "cli-client", false},
		"provider client id unknown, collab client":      {func() collab.Route { x := r; x.ProviderClientID = ""; return x }(), "cid", true},
	} {
		_, err := f.cf.VerifyBearer(ctx, c.route, tok(c.aud))
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", name, err, c.ok)
		}
	}
	// the ID token of the code flow stays bound to the collaboration client
	f.idTok = tok("cli-client")
	if _, err := f.cf.Exchange(ctx, r, "https://apex/cb", "code", "ver"); err == nil {
		t.Error("an ID token for another client must not complete a web sign-in")
	}
}
