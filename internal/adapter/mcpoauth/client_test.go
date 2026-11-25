package mcpoauth_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpoauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

// authServer is a fake MCP server and authorization server on one TLS host.
type authServer struct {
	srv                       *httptest.Server
	prmPath, asmPath, oidc    bool
	issuer                    string // overrides the issuer in the metadata
	noS256, otherResource     bool
	tokenStatus               int
	gotForm                   map[string]string
	refresh, idToken, wrongCT string
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	a := &authServer{prmPath: true, asmPath: true, tokenStatus: http.StatusOK, refresh: "rt-9"}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *authServer) base() string { return a.srv.URL }

func (a *authServer) serve(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/.well-known/oauth-protected-resource/mcp" && a.prmPath,
		r.URL.Path == "/.well-known/oauth-protected-resource" && !a.prmPath && r.URL.Query().Get("x") == "":
		res := a.base() + "/mcp"
		if a.otherResource {
			res = "https://elsewhere.example/mcp"
		}
		write(map[string]any{"resource": res, "authorization_servers": []string{a.base() + "/as"}, "scopes_supported": []string{"tickets:read"}})
	case r.URL.Path == "/.well-known/oauth-authorization-server/as" && a.asmPath,
		r.URL.Path == "/as/.well-known/openid-configuration" && a.oidc:
		iss := a.base() + "/as"
		if a.issuer != "" {
			iss = a.issuer
		}
		methods := []string{"S256"}
		if a.noS256 {
			methods = []string{"plain"}
		}
		write(map[string]any{"issuer": iss, "authorization_endpoint": a.base() + "/as/authorize", "token_endpoint": a.base() + "/as/token",
			"code_challenge_methods_supported": methods})
	case r.URL.Path == "/as/token" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		a.gotForm = map[string]string{}
		for k := range r.PostForm {
			a.gotForm[k] = r.PostForm.Get(k)
		}
		if a.tokenStatus != http.StatusOK {
			w.WriteHeader(a.tokenStatus)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"secret-ish detail"}`))
			return
		}
		if a.wrongCT != "" {
			_, _ = w.Write([]byte(a.wrongCT))
			return
		}
		write(map[string]any{"access_token": "at", "refresh_token": a.refresh, "scope": "tickets:read tickets:write", "id_token": a.idToken})
	default:
		http.NotFound(w, r)
	}
}

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestDiscoverAndExchange(t *testing.T) {
	a := newAuthServer(t)
	a.idToken = jwt(map[string]any{"sub": "123", "email": "svc-afs@saas.example"})
	c := &mcpoauth.Client{HTTP: a.srv.Client()}
	as, err := c.Discover(t.Context(), a.base()+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if as.Issuer != a.base()+"/as" || as.TokenEndpoint != a.base()+"/as/token" || as.Resource != a.base()+"/mcp" || len(as.Scopes) != 1 {
		t.Fatalf("discovered = %+v", as)
	}
	tok, err := c.Exchange(t.Context(), as, mcpcatalog.OAuthExchange{Code: "c", Verifier: "v", RedirectURI: "https://api.example/cb",
		ClientID: "https://afs.example/client.json", Resource: as.Resource})
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken.Expose() != "rt-9" || tok.Subject != "svc-afs@saas.example" || len(tok.Scopes) != 2 {
		t.Errorf("tokens = %+v", tok)
	}
	want := map[string]string{"grant_type": "authorization_code", "code": "c", "code_verifier": "v", "redirect_uri": "https://api.example/cb",
		"client_id": "https://afs.example/client.json", "resource": as.Resource}
	for k, v := range want {
		if a.gotForm[k] != v {
			t.Errorf("form %s = %q, want %q", k, a.gotForm[k], v)
		}
	}
}

func TestDiscoverFallbacks(t *testing.T) {
	a := newAuthServer(t)
	a.prmPath, a.asmPath, a.oidc = false, false, true
	c := &mcpoauth.Client{HTTP: a.srv.Client()}
	if as, err := c.Discover(t.Context(), a.base()+"/mcp"); err != nil || as.Issuer != a.base()+"/as" {
		t.Fatalf("root metadata and OpenID discovery = %+v %v", as, err)
	}
}

func TestDiscoverRefusals(t *testing.T) {
	cases := map[string]func(a *authServer) string{
		"not https":                 func(a *authServer) string { return "http://" + strings.TrimPrefix(a.base(), "https://") + "/mcp" },
		"no metadata":               func(a *authServer) string { return a.base() + "/elsewhere/mcp" },
		"another resource":          func(a *authServer) string { a.otherResource = true; return a.base() + "/mcp" },
		"issuer differs (RFC 8414)": func(a *authServer) string { a.issuer = "https://evil.example"; return a.base() + "/mcp" },
		"no PKCE S256":              func(a *authServer) string { a.noS256 = true; return a.base() + "/mcp" },
		"no authorization server":   func(a *authServer) string { a.asmPath = false; return a.base() + "/mcp" },
	}
	for name, prep := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAuthServer(t)
			u := prep(a)
			_, err := (&mcpoauth.Client{HTTP: a.srv.Client()}).Discover(t.Context(), u)
			if !errors.Is(err, mcpcatalog.ErrOAuthUpstream) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestExchangeRefusals(t *testing.T) {
	a := newAuthServer(t)
	c := &mcpoauth.Client{HTTP: a.srv.Client()}
	as, err := c.Discover(t.Context(), a.base()+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	a.tokenStatus = http.StatusBadRequest
	_, err = c.Exchange(t.Context(), as, mcpcatalog.OAuthExchange{Code: "c"})
	if !errors.Is(err, mcpcatalog.ErrOAuthUpstream) || strings.Contains(err.Error(), "secret-ish") {
		t.Errorf("400 = %v", err)
	}
	a.tokenStatus, a.wrongCT = http.StatusOK, "<html>"
	if _, err := c.Exchange(t.Context(), as, mcpcatalog.OAuthExchange{Code: "c"}); !errors.Is(err, mcpcatalog.ErrOAuthUpstream) {
		t.Errorf("not JSON = %v", err)
	}
	a.wrongCT, a.refresh = "", ""
	if tok, err := c.Exchange(t.Context(), as, mcpcatalog.OAuthExchange{Code: "c"}); err != nil || !tok.RefreshToken.IsZero() {
		t.Errorf("no refresh token = %+v %v", tok, err)
	}
	if _, err := c.Exchange(t.Context(), mcpcatalog.OAuthServer{TokenEndpoint: "http://x"}, mcpcatalog.OAuthExchange{}); !errors.Is(err, mcpcatalog.ErrOAuthUpstream) {
		t.Errorf("http token endpoint = %v", err)
	}
	if _, err := (&mcpoauth.Client{}).Discover(t.Context(), "https://127.0.0.1:1/mcp"); err == nil {
		t.Error("default client reached a closed port")
	}
}

func TestSubjectFromIDToken(t *testing.T) {
	a := newAuthServer(t)
	c := &mcpoauth.Client{HTTP: a.srv.Client()}
	as, _ := c.Discover(t.Context(), a.base()+"/mcp")
	for tokenStr, want := range map[string]string{
		jwt(map[string]any{"preferred_username": "svc"}): "svc",
		jwt(map[string]any{"sub": "s-1"}):                "s-1",
		"not-a-jwt":                                      "",
		"a.!!!.c":                                        "",
		"a." + base64.RawURLEncoding.EncodeToString([]byte("[]")) + ".c": "",
		jwt(map[string]any{"email": strings.Repeat("x", 300)}):           strings.Repeat("x", 200),
	} {
		a.idToken = tokenStr
		tok, err := c.Exchange(t.Context(), as, mcpcatalog.OAuthExchange{Code: "c"})
		if err != nil || tok.Subject != want {
			t.Errorf("subject of %q = %q %v, want %q", tokenStr, tok.Subject, err, want)
		}
	}
}
