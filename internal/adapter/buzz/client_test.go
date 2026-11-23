package buzz_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/buzz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collab"
)

type res struct {
	t   buzz.Target
	err error
}

func (r res) Target(context.Context, string) (buzz.Target, error) { return r.t, r.err }

func TestAddAndRemoveMember(t *testing.T) {
	var got []string
	var name string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		if r.Method == http.MethodPut {
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			name = b["name"]
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := buzz.New(res{t: buzz.Target{BaseURL: srv.URL, AdminKey: collab.Key("adm")}})
	if err := c.AddMember(t.Context(), "team_a", "wk", "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveMember(t.Context(), "team_a", "abc123"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "PUT /admin/members/abc123 Bearer adm" || got[1] != "DELETE /admin/members/abc123 Bearer adm" || name != "wk" {
		t.Errorf("calls %v name %q", got, name)
	}
	status = http.StatusNotFound
	if err := c.RemoveMember(t.Context(), "team_a", "gone"); err != nil {
		t.Errorf("a missing member is already removed: %v", err)
	}
	err := c.AddMember(t.Context(), "team_a", "wk", "abc123")
	var e *buzz.Error
	if err == nil || !strings.Contains(err.Error(), "status 404") || strings.Contains(err.Error(), "adm") {
		t.Errorf("err %v %v", err, e)
	}
}

func TestErrorsCarryNoCredential(t *testing.T) {
	c := buzz.New(res{t: buzz.Target{BaseURL: "http://127.0.0.1:1", AdminKey: collab.Key("topsecret")}})
	err := c.AddMember(t.Context(), "team_a", "wk", "abc")
	if err == nil || strings.Contains(err.Error(), "topsecret") || !strings.Contains(err.Error(), "request failed") {
		t.Errorf("err %v", err)
	}
	if err := c.AddMember(t.Context(), "team_a", "wk", ""); err == nil {
		t.Error("empty key accepted")
	}
	c = buzz.New(res{err: context.Canceled})
	if err := c.RemoveMember(t.Context(), "team_a", "k"); err == nil {
		t.Error("resolver error is lost")
	}
}
