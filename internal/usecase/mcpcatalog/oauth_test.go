package mcpcatalog_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

// fakeAS is an OAuthClient fake: one authorization server; it checks the PKCE pair like a real token endpoint.
type fakeAS struct {
	challenge     string
	exchanged     mcpcatalog.OAuthExchange
	discoverErr   error
	exchangeErr   error
	noRefresh     bool
	discoveredFor []string
}

var testAS = mcpcatalog.OAuthServer{Issuer: "https://login.saas.example", AuthorizationEndpoint: "https://login.saas.example/authorize", //nolint:gosec // fixture endpoints, no secret
	TokenEndpoint: "https://login.saas.example/token", Resource: "https://issues.corp.example/mcp", Scopes: []string{"read", "write"}}

func (f *fakeAS) Discover(_ context.Context, u string) (mcpcatalog.OAuthServer, error) {
	f.discoveredFor = append(f.discoveredFor, u)
	return testAS, f.discoverErr
}

func (f *fakeAS) Exchange(_ context.Context, _ mcpcatalog.OAuthServer, x mcpcatalog.OAuthExchange) (mcpcatalog.OAuthTokens, error) {
	f.exchanged = x
	if f.exchangeErr != nil {
		return mcpcatalog.OAuthTokens{}, f.exchangeErr
	}
	sum := sha256.Sum256([]byte(x.Verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
		return mcpcatalog.OAuthTokens{}, errors.New("invalid_grant: PKCE")
	}
	out := mcpcatalog.OAuthTokens{Scopes: []string{"read"}, Subject: "svc-afs@saas.example"}
	if !f.noRefresh {
		out.RefreshToken, _ = domain.NewSecretValue("refreshToken", "rt-1")
	}
	return out, nil
}

func oauthRig(t *testing.T) (*world, *fakeAS, domain.McpServerRecord) {
	t.Helper()
	w := rig()
	as := &fakeAS{}
	w.s.OAuthHTTP = as
	w.s.OAuth = mcpcatalog.OAuthSettings{ClientID: "https://afs.example/oauth/client.json", RedirectURI: "https://api.afs.example/v1/mcp-oauth/callback",
		ConsoleURL: "https://console.afs.example/", StateKey: bytes.Repeat([]byte{9}, 32), Rand: bytes.NewReader(bytes.Repeat([]byte{1, 2, 3, 4}, 64))}
	in := remote("saas")
	in.AuthKind = str("oauth_user")
	res, err := w.s.Create(t.Context(), svcAdmin, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	x := w.r.servers[res.View.Server.ID]
	x.ReviewStatus, x.Published = domain.McpApproved, true
	w.r.servers[x.ID] = x
	return w, as, x
}

func TestOAuthConnect(t *testing.T) {
	w, as, srv := oauthRig(t)
	ctx := t.Context()
	start, err := w.s.StartOAuth(ctx, teamAdmin, team, srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(start.AuthorizationURL)
	q := u.Query()
	if u.Host != "login.saas.example" || q.Get("client_id") != w.s.OAuth.ClientID || q.Get("redirect_uri") != w.s.OAuth.RedirectURI ||
		q.Get("state") != start.State || q.Get("code_challenge_method") != "S256" || q.Get("resource") != testAS.Resource ||
		q.Get("scope") != "read write" || q.Get("response_type") != "code" || !start.ExpiresAt.Equal(now.Add(mcpcatalog.OAuthStateTTL)) {
		t.Fatalf("start = %+v", start)
	}
	if strings.Contains(start.AuthorizationURL, "verifier") {
		t.Error("the verifier left the API")
	}
	as.challenge = q.Get("code_challenge")
	audited := 0
	for _, e := range w.audit.Entries {
		if e.Record.Action == "StartMcpOAuthConnect" {
			audited++
		}
	}
	if audited != 1 || len(w.events()) != 1 { // the create event only
		t.Errorf("start must be audited (%d) without an event: %v", audited, w.events())
	}

	res := w.s.CompleteOAuth(ctx, mcpcatalog.CallbackInput{Code: "c1", State: start.State, Issuer: testAS.Issuer, RequestID: "req_cb"})
	if res.Reason != "" || res.Redirect != "https://console.afs.example/teams/"+team+"/mcp-servers/"+srv.ID+"?mcpOAuth=connected" {
		t.Fatalf("callback = %+v", res)
	}
	if as.exchanged.Code != "c1" || as.exchanged.Resource != testAS.Resource || as.exchanged.ClientID != w.s.OAuth.ClientID {
		t.Errorf("exchange = %+v", as.exchanged)
	}
	cred := w.r.creds[srv.ID+"/"+team]
	if cred.Kind != "oauth_user" || cred.Subject != "svc-afs@saas.example" || cred.Status != domain.McpCredSet || len(cred.Scopes) != 1 {
		t.Fatalf("credential = %+v", cred)
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(w.sec.values[cred.SecretARN]), &stored); err != nil || stored["refreshToken"] != "rt-1" ||
		stored["tokenEndpoint"] != testAS.TokenEndpoint || stored["clientId"] != w.s.OAuth.ClientID {
		t.Errorf("secret = %v %v", stored, err)
	}
	w.hasEvent(t, domain.McpCredentialEvent(true))

	// Single use.
	if again := w.s.CompleteOAuth(ctx, mcpcatalog.CallbackInput{Code: "c1", State: start.State, Issuer: testAS.Issuer}); again.Reason != "state_invalid" {
		t.Errorf("replayed state = %+v", again)
	}
}

func TestOAuthCallbackRefusals(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name   string
		prep   func(as *fakeAS)
		in     func(state string) mcpcatalog.CallbackInput
		reason string
	}{
		{"unknown state", nil, func(string) mcpcatalog.CallbackInput { return mcpcatalog.CallbackInput{Code: "c", State: "nope"} }, "state_invalid"},
		{"empty state", nil, func(string) mcpcatalog.CallbackInput { return mcpcatalog.CallbackInput{Code: "c"} }, "state_invalid"},
		{"denied by the person", nil, func(s string) mcpcatalog.CallbackInput {
			return mcpcatalog.CallbackInput{State: s, Error: "access_denied"}
		}, "authorization_denied"},
		{"issuer mix-up", nil, func(s string) mcpcatalog.CallbackInput {
			return mcpcatalog.CallbackInput{Code: "c", State: s, Issuer: "https://evil.example"}
		}, "issuer_mismatch"},
		{"token endpoint refuses", func(as *fakeAS) { as.exchangeErr = mcpcatalog.ErrOAuthUpstream }, func(s string) mcpcatalog.CallbackInput {
			return mcpcatalog.CallbackInput{Code: "c", State: s}
		}, "exchange_failed"},
		{"no refresh token", func(as *fakeAS) { as.noRefresh = true }, func(s string) mcpcatalog.CallbackInput {
			return mcpcatalog.CallbackInput{Code: "c", State: s}
		}, "no_refresh_token"},
		{"discovery fails at the callback", func(as *fakeAS) { as.discoverErr = mcpcatalog.ErrOAuthUpstream }, func(s string) mcpcatalog.CallbackInput {
			return mcpcatalog.CallbackInput{Code: "c", State: s}
		}, "discovery_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, as, srv := oauthRig(t)
			start, err := w.s.StartOAuth(ctx, teamAdmin, team, srv.ID)
			if err != nil {
				t.Fatal(err)
			}
			as.challenge = "x"
			if tc.prep != nil {
				tc.prep(as)
			}
			if tc.name == "no refresh token" {
				u, _ := url.Parse(start.AuthorizationURL)
				as.challenge = u.Query().Get("code_challenge")
			}
			res := w.s.CompleteOAuth(ctx, tc.in(start.State))
			if res.Reason != tc.reason || !strings.Contains(res.Redirect, "mcpOAuth=failed") || !strings.Contains(res.Redirect, "reason="+tc.reason) {
				t.Errorf("callback = %+v, want %s", res, tc.reason)
			}
			if _, ok := w.r.creds[srv.ID+"/"+team]; ok {
				t.Error("a refused callback stored a credential")
			}
		})
	}
}

func TestOAuthStartRefusals(t *testing.T) {
	ctx := t.Context()
	t.Run("not configured", func(t *testing.T) {
		w, _, srv := oauthRig(t)
		w.s.OAuth.ClientID = ""
		if _, err := w.s.StartOAuth(ctx, teamAdmin, team, srv.ID); reasonOf(err) != mcpcatalog.ReasonOAuthNotConfigured {
			t.Errorf("err = %v", err)
		}
		if res := w.s.CompleteOAuth(ctx, mcpcatalog.CallbackInput{State: "s"}); res.Reason != "not_configured" || !strings.HasSuffix(res.Redirect, "/mcp-servers?mcpOAuth=failed&reason=not_configured") {
			t.Errorf("callback = %+v", res)
		}
	})
	t.Run("not an oauth_user server", func(t *testing.T) {
		w, _, _ := oauthRig(t)
		x := w.approved(t, "plain")
		if _, err := w.s.StartOAuth(ctx, teamAdmin, team, x.ID); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("discovery fails", func(t *testing.T) {
		w, as, srv := oauthRig(t)
		as.discoverErr = mcpcatalog.ErrOAuthUpstream
		if _, err := w.s.StartOAuth(ctx, teamAdmin, team, srv.ID); reasonOf(err) != mcpcatalog.ReasonOAuthDiscoveryFailed {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("the shared account is the service admin's", func(t *testing.T) {
		w, _, srv := oauthRig(t)
		x := w.r.servers[srv.ID]
		x.CredentialMode = domain.McpCredentialShared
		w.r.servers[x.ID] = x
		if _, err := w.s.StartOAuth(ctx, teamAdmin, team, srv.ID); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("err = %v", err)
		}
		if _, err := w.s.StartOAuth(ctx, svcAdmin, team, srv.ID); err != nil {
			t.Errorf("service admin = %v", err)
		}
	})
	t.Run("viewers and other teams", func(t *testing.T) {
		w, _, srv := oauthRig(t)
		if _, err := w.s.StartOAuth(ctx, viewer, team, srv.ID); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("viewer = %v", err)
		}
		if _, err := w.s.StartOAuth(ctx, other, team, srv.ID); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("other team = %v", err)
		}
	})
	t.Run("an expired state", func(t *testing.T) {
		w, as, srv := oauthRig(t)
		start, _ := w.s.StartOAuth(ctx, teamAdmin, team, srv.ID)
		for k, row := range w.r.oauth {
			row.expires = now.Add(-1)
			w.r.oauth[k] = row
		}
		as.challenge = "x"
		if res := w.s.CompleteOAuth(ctx, mcpcatalog.CallbackInput{Code: "c", State: start.State}); res.Reason != "state_invalid" {
			t.Errorf("expired = %+v", res)
		}
	})
}

func reasonOf(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Reason
	}
	return ""
}
