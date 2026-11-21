package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestSettings(t *testing.T) {
	if _, err := settingsFrom(env(map[string]string{"AFS_MIGRATION_LOCK_TIMEOUT": "x"})); err == nil {
		t.Fatal("bad duration accepted")
	}
	if _, err := settingsFrom(env(map[string]string{"AFS_DATABASE_OWNER_URL": "postgres://x"})); err == nil {
		t.Fatal("owner without app URL accepted")
	}
	s, err := settingsFrom(env(map[string]string{"AFS_MIGRATION_LOCK_TIMEOUT": "3s"}))
	if err != nil || s.MigrationTimeout.Seconds() != 3 {
		t.Fatal(s, err)
	}
}

func get(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec.Code
}

func TestWireWithoutDatabase(t *testing.T) {
	c, err := wire(t.Context(), quiet, settings{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if get(t, c.Handler, "/healthz") != 200 || get(t, c.Handler, "/readyz") != 200 || get(t, c.Handler, "/v1/openapi.yaml") != 200 {
		t.Fatal("health, readiness or OpenAPI not served")
	}
	if get(t, c.Handler, "/v1/me") != 401 {
		t.Fatal("no authenticator must refuse")
	}
	if _, err := wire(t.Context(), quiet, settings{DatabaseURL: "postgres://nobody@127.0.0.1:1/x?connect_timeout=1"}); err == nil {
		t.Fatal("unreachable database accepted")
	}
}

func TestWireWithDatabase(t *testing.T) {
	db := pgtest.Get(t)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"]})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if get(t, c.Handler, "/readyz") != 200 {
		t.Fatal("not ready")
	}
	if c.Runner == nil || c.Audit == nil || c.Outbox == nil || c.Idem == nil || c.Creds == nil {
		t.Fatal("components missing")
	}
	if _, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: "::bad::"}); err == nil {
		t.Fatal("bad owner URL accepted")
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
		if s := sessionOf(c.p, "req_1"); s.ActorType != c.want || s.Actor != c.p.ID || s.RequestID != "req_1" {
			t.Errorf("%+v -> %+v", c.p, s)
		}
	}
}

// DP-1: AFS_DB_IAM_AUTH signs an RDS IAM token per connection and needs a region.
func TestSettingsDBIAMAuth(t *testing.T) {
	if _, err := settingsFrom(env(map[string]string{"AFS_DB_IAM_AUTH": "true"})); err == nil {
		t.Fatal("IAM auth without a region accepted")
	}
	if _, err := settingsFrom(env(map[string]string{"AFS_DB_IAM_AUTH": "maybe"})); err == nil {
		t.Fatal("bad boolean accepted")
	}
	s, err := settingsFrom(env(map[string]string{"AFS_DB_IAM_AUTH": "true", "AWS_REGION": "us-east-1"}))
	if err != nil || !s.DBIAMAuth || s.AWSRegion != "us-east-1" {
		t.Fatal(s, err)
	}
	s, err = settingsFrom(env(map[string]string{"AFS_DB_IAM_AUTH": "true", "AWS_REGION": "us-east-1", "AFS_AWS_REGION": "eu-west-1"}))
	if err != nil || s.AWSRegion != "eu-west-1" {
		t.Fatal("AFS_AWS_REGION must win", s, err)
	}
}
