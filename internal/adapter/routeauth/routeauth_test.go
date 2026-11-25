package routeauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/routeauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

const team = "team_01JD0000000000000000000A00"

func TestFileCredentialsAndRotation(t *testing.T) {
	dir := t.TempDir()
	a := &routeauth.Auth{Dir: dir}
	ep := gateway.RouteEndpoint{ID: "mep_1", Slug: "openai", AuthKind: domain.AuthBearer}
	if _, err := a.Credential(t.Context(), ep, team); !errors.Is(err, gateway.ErrCredentialMissing) {
		t.Fatalf("missing file: %v", err)
	}
	p := filepath.Join(dir, "openai")
	if err := os.WriteFile(p, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Credential(t.Context(), ep, team); !errors.Is(err, gateway.ErrCredentialMissing) {
		t.Fatalf("empty file: %v", err)
	}
	if err := os.WriteFile(p, []byte("sk-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := a.Credential(t.Context(), ep, team)
	if err != nil || c.Header != "Authorization" || c.Value != "Bearer sk-one" {
		t.Fatalf("cred %+v err %v", c, err)
	}
	// Rotation: the reconciler rewrites the mounted file; the next call reads the new value.
	if err := os.WriteFile(p, []byte("sk-rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(p, future, future)
	if c, _ := a.Credential(t.Context(), ep, team); c.Value != "Bearer sk-rotated" {
		t.Fatalf("after rotation %q", c.Value)
	}
	ep.AuthKind, ep.AuthHeader = domain.AuthHeader, "api-key"
	if c, _ := a.Credential(t.Context(), ep, team); c.Header != "api-key" || c.Value != "sk-rotated" {
		t.Fatalf("header cred %+v", c)
	}
	if c, err := a.Credential(t.Context(), gateway.RouteEndpoint{AuthKind: domain.AuthNone}, team); err != nil || c.Header != "" {
		t.Fatalf("none: %+v %v", c, err)
	}
	if _, err := a.Credential(t.Context(), gateway.RouteEndpoint{Slug: "../etc", AuthKind: domain.AuthBearer}, team); !errors.Is(err, gateway.ErrCredentialMissing) {
		t.Fatalf("traversal: %v", err)
	}
	if _, err := a.Credential(t.Context(), gateway.RouteEndpoint{AuthKind: "weird"}, team); !errors.Is(err, gateway.ErrCredentialMissing) {
		t.Fatalf("unknown kind: %v", err)
	}
}

type fedStore struct {
	f       routeauth.Federation
	err     error
	reports []string
}

func (s *fedStore) Federation(context.Context, string, string) (routeauth.Federation, error) {
	return s.f, s.err
}

func (s *fedStore) ReportFederation(_ context.Context, _, _, status, _ string) error {
	s.reports = append(s.reports, status)
	return nil
}

type fakeExchanger struct {
	calls int
	err   error
}

func (x *fakeExchanger) Exchange(context.Context, routeauth.Federation) (string, time.Time, error) {
	x.calls++
	if x.err != nil {
		return "", time.Time{}, x.err
	}
	return "tok-1", time.Now().Add(time.Hour), nil
}

func TestFederatedCredential(t *testing.T) {
	store := &fedStore{f: routeauth.Federation{Kind: "azure", TenantID: "t", ClientID: "c", Status: "pending"}}
	ex := &fakeExchanger{}
	a := &routeauth.Auth{Federations: store, Azure: ex}
	ep := gateway.RouteEndpoint{ID: "mep_4", Slug: "azure-openai", AuthKind: domain.AuthAzureFederated}
	for range 2 {
		c, err := a.Credential(t.Context(), ep, team)
		if err != nil || c.Value != "Bearer tok-1" {
			t.Fatalf("cred %+v err %v", c, err)
		}
	}
	if ex.calls != 1 || len(store.reports) != 1 || store.reports[0] != "ready" {
		t.Fatalf("calls %d reports %v", ex.calls, store.reports)
	}
	// Google has no exchanger configured: not ready.
	if _, err := a.Credential(t.Context(), gateway.RouteEndpoint{ID: "mep_5", AuthKind: domain.AuthGoogleFederated}, team); !errors.Is(err, gateway.ErrFederationNotReady) {
		t.Fatalf("google: %v", err)
	}
	failing := &fakeExchanger{err: errors.New("AADSTS700016")}
	a2 := &routeauth.Auth{Federations: store, Azure: failing}
	if _, err := a2.Credential(t.Context(), ep, team); !errors.Is(err, gateway.ErrFederationNotReady) {
		t.Fatalf("failing: %v", err)
	}
	if store.reports[len(store.reports)-1] != "failing" {
		t.Fatalf("reports %v", store.reports)
	}
	none := &routeauth.Auth{Federations: &fedStore{err: gateway.ErrNotFound}, Azure: ex}
	if _, err := none.Credential(t.Context(), gateway.RouteEndpoint{ID: "x", AuthKind: domain.AuthAzureFederated}, team); !errors.Is(err, gateway.ErrFederationNotReady) {
		t.Fatalf("no record: %v", err)
	}
	if _, err := (&routeauth.Auth{}).Credential(t.Context(), ep, team); !errors.Is(err, gateway.ErrFederationNotReady) {
		t.Fatalf("no store: %v", err)
	}
}

func tokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("projected.jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAzureExchanger(t *testing.T) {
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form = r.URL.Path + "?" + string(b)
		_, _ = io.WriteString(w, `{"access_token":"entra-token","expires_in":600}`)
	}))
	defer srv.Close()
	x := &routeauth.AzureExchanger{TokenFile: tokenFile(t), Authority: srv.URL, HTTP: srv.Client()}
	tok, exp, err := x.Exchange(t.Context(), routeauth.Federation{TenantID: "tid", ClientID: "cid"})
	if err != nil || tok != "entra-token" || time.Until(exp) > 11*time.Minute {
		t.Fatalf("tok %q exp %v err %v", tok, exp, err)
	}
	if !strings.HasPrefix(form, "/tid/oauth2/v2.0/token?") || !strings.Contains(form, "client_assertion=projected.jwt") || !strings.Contains(form, "client_id=cid") {
		t.Fatalf("form %s", form)
	}
	if _, _, err := x.Exchange(t.Context(), routeauth.Federation{}); err == nil {
		t.Fatal("missing ids accepted")
	}
	bad := &routeauth.AzureExchanger{TokenFile: filepath.Join(t.TempDir(), "absent"), Authority: srv.URL}
	if _, _, err := bad.Exchange(t.Context(), routeauth.Federation{TenantID: "t", ClientID: "c"}); err == nil {
		t.Fatal("missing token file accepted")
	}
}

func TestGoogleExchanger(t *testing.T) {
	var got map[string]string
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		if status == 200 {
			_, _ = io.WriteString(w, `{"access_token":"ya29.token","expires_in":3599}`)
			return
		}
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"echoes projected.jwt"}`)
	}))
	defer srv.Close()
	x := &routeauth.GoogleExchanger{TokenFile: tokenFile(t), STSURL: srv.URL, HTTP: srv.Client()}
	f := routeauth.Federation{ProjectNumber: "123456", PoolID: "afs-pool", ProviderID: "afs-eks"}
	tok, _, err := x.Exchange(t.Context(), f)
	if err != nil || tok != "ya29.token" || got["audience"] != "//iam.googleapis.com/projects/123456/locations/global/workloadIdentityPools/afs-pool/providers/afs-eks" ||
		got["subjectToken"] != "projected.jwt" {
		t.Fatalf("tok %q err %v body %v", tok, err, got)
	}
	status = 400
	_, _, err = x.Exchange(t.Context(), f)
	if err == nil || strings.Contains(err.Error(), "projected.jwt") {
		t.Fatalf("error leaks or is nil: %v", err)
	}
	if _, _, err := x.Exchange(t.Context(), routeauth.Federation{}); err == nil {
		t.Fatal("missing fields accepted")
	}
}
