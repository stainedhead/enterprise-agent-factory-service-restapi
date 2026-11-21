package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/me"
)

type fakeMe struct {
	res  me.Result
	err  error
	got  authz.Principal
	rid  string
	call int
}

func (f *fakeMe) Get(_ context.Context, p authz.Principal, rid string) (me.Result, error) {
	f.call++
	f.got, f.rid = p, rid
	return f.res, f.err
}

type fakeLister struct {
	ps  []auth.Provider
	err error
}

func (f fakeLister) SignInProviders(context.Context) ([]auth.Provider, error) { return f.ps, f.err }

// RA-US-3a.1, RA-US-2.5 (handler half; the use case and database halves have their own tests).
func TestMeHandlers(t *testing.T) {
	uc := &fakeMe{res: me.Result{
		Principal: me.PrincipalInfo{ID: "usr_a", Type: authz.PrincipalUser, Email: "a@example.com", Name: "A"},
		Teams:     []me.TeamEntry{{TeamID: teamA, Slug: "alpha", Role: "team_admin", Permissions: []authz.Permission{authz.TeamsRead}}},
	}}
	lister := fakeLister{ps: []auth.Provider{{ID: "idp_1", Kind: "okta", Name: "Okta", IssuerURL: "https://x.example.com", ClientID: "c1", IsDefault: true, GroupsClaim: "groups"}}}
	h, _ := newServer(t, MeRegistrar(uc, lister))

	rec := do(t, h, "GET", "/v1/me", "admin-a", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	teams := body["teams"].([]any)
	first := teams[0].(map[string]any)
	if body["serviceAdmin"] != false || body["securityOperator"] != false || first["teamId"] != teamA || first["slug"] != "alpha" ||
		first["role"] != "team_admin" || first["permissions"].([]any)[0] != "teams:read" {
		t.Fatalf("body = %s", rec.Body)
	}
	if _, has := body["servicePermissions"]; has {
		t.Fatal("servicePermissions must be omitted when empty")
	}
	if uc.got.ID != "usr_a" || uc.rid == "" {
		t.Fatalf("use case got %+v %q", uc.got, uc.rid)
	}
	// No credential: 401 and the use case is not called.
	calls := uc.call
	if rec := do(t, h, "GET", "/v1/me", "", ""); rec.Code != 401 || uc.call != calls {
		t.Fatalf("%d", rec.Code)
	}
	// Every kind of caller may call it (any signed-in), including one bound to nothing.
	for _, tok := range []string{"viewer-a", "svc", "secop", "ghost-a"} {
		if rec := do(t, h, "GET", "/v1/me", tok, ""); rec.Code != 200 {
			t.Fatalf("%s: %d", tok, rec.Code)
		}
	}
	uc.err = errors.New("db")
	if rec := do(t, h, "GET", "/v1/me", "admin-a", ""); rec.Code != 500 || strings.Contains(rec.Body.String(), "db") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	// Public sign-in list: no token needed, both client ids, nothing secret.
	rec = do(t, h, "GET", "/v1/identity-providers", "", "")
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("%v %s", err, rec.Body)
	}
	it := list.Items[0]
	if it["consoleClientId"] != "c1" || it["cliClientId"] != "c1" || it["isDefault"] != true || it["issuerUrl"] != "https://x.example.com" || it["id"] != "idp_1" {
		t.Fatalf("item = %v", it)
	}
	for k := range it {
		if strings.Contains(strings.ToLower(k), "secret") || k == "groupsClaim" {
			t.Fatalf("provider leaks %s", k)
		}
	}
	lister.err = errors.New("db")
	h2, _ := newServer(t, MeRegistrar(uc, lister))
	if rec := do(t, h2, "GET", "/v1/identity-providers", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("%d", rec.Code)
	}
	// Empty list is an empty array, not null.
	h3, _ := newServer(t, MeRegistrar(uc, fakeLister{}))
	if rec := do(t, h3, "GET", "/v1/identity-providers", "", ""); !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("%s", rec.Body)
	}
}
