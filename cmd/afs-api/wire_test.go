package main

import (
	"context"
	"errors"
	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
	// M1-L2: requests are counted by operation, and the readiness checks cover database and schema.
	_ = get(t, c.Handler, "/v1/me")
	rec := httptest.NewRecorder()
	c.Metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `afs_api_requests_total{code="401",method="GET",route="GetMe"} 1`) {
		t.Errorf("RED metrics missing:\n%s", rec.Body)
	}
	for _, ck := range c.Checks {
		if err := ck.Fn(t.Context()); err != nil {
			t.Errorf("check %s: %v", ck.Name, err)
		}
	}
	if len(c.Checks) != 2 {
		t.Errorf("checks = %d, want database and schema", len(c.Checks))
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

// D-2 (FR-R07 AC2): the page-token key is a setting; more than one replica needs a shared key of at
// least 32 bytes.
func TestSettingsPageTokenKey(t *testing.T) {
	key32 := strings.Repeat("k", 32)
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		wantKey string
	}{
		{"one replica, no key", nil, false, ""},
		{"one replica, key", map[string]string{"AFS_PAGE_TOKEN_KEY": key32}, false, key32},
		{"one replica, short key is not used", map[string]string{"AFS_PAGE_TOKEN_KEY": "short"}, false, ""},
		{"three replicas, no key", map[string]string{"AFS_API_REPLICAS": "3"}, true, ""},
		{"three replicas, 31 bytes", map[string]string{"AFS_API_REPLICAS": "3", "AFS_PAGE_TOKEN_KEY": key32[:31]}, true, ""},
		{"three replicas, 32 bytes", map[string]string{"AFS_API_REPLICAS": "3", "AFS_PAGE_TOKEN_KEY": key32, "AFS_IDEMPOTENCY_HMAC_KEY": key32}, false, key32},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := settingsFrom(env(c.env))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && string(s.PageTokenKey) != c.wantKey {
				t.Fatalf("key = %q, want %q", s.PageTokenKey, c.wantKey)
			}
			if err != nil && strings.Contains(err.Error(), key32[:31]) {
				t.Fatalf("error leaks the key: %v", err)
			}
		})
	}
}

// D-5 (FR-R14, RD-2): the idempotency HMAC key is a setting; more than one replica needs a shared key of
// at least 32 bytes, so a replay is recognised on any replica.
func TestSettingsIdempotencyHMACKey(t *testing.T) {
	key32 := strings.Repeat("h", 32)
	pk := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
		wantKey string
	}{
		{"one replica, no key", nil, false, ""},
		{"one replica, key", map[string]string{"AFS_IDEMPOTENCY_HMAC_KEY": key32}, false, key32},
		{"one replica, short key is not used", map[string]string{"AFS_IDEMPOTENCY_HMAC_KEY": "short"}, false, ""},
		{"two replicas, no key", map[string]string{"AFS_API_REPLICAS": "2", "AFS_PAGE_TOKEN_KEY": pk}, true, ""},
		{"two replicas, 31 bytes", map[string]string{"AFS_API_REPLICAS": "2", "AFS_PAGE_TOKEN_KEY": pk, "AFS_IDEMPOTENCY_HMAC_KEY": key32[:31]}, true, ""},
		{"two replicas, 32 bytes", map[string]string{"AFS_API_REPLICAS": "2", "AFS_PAGE_TOKEN_KEY": pk, "AFS_IDEMPOTENCY_HMAC_KEY": key32}, false, key32},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := settingsFrom(env(c.env))
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && string(s.IdempotencyHMACKey) != c.wantKey {
				t.Fatalf("key = %q, want %q", s.IdempotencyHMACKey, c.wantKey)
			}
		})
	}
}

// D-6 (FR-R09 AC1): the read and idle timeouts are settings with defaults of 60 s and 120 s.
func TestSettingsHTTPTimeouts(t *testing.T) {
	s, err := settingsFrom(env(nil))
	if err != nil || s.HTTPReadTimeout != 60*time.Second || s.HTTPIdleTimeout != 120*time.Second {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	s, err = settingsFrom(env(map[string]string{"AFS_HTTP_READ_TIMEOUT": "5s", "AFS_HTTP_IDLE_TIMEOUT": "7s"}))
	if err != nil || s.HTTPReadTimeout != 5*time.Second || s.HTTPIdleTimeout != 7*time.Second {
		t.Fatalf("set: %+v %v", s, err)
	}
	for _, bad := range []map[string]string{
		{"AFS_HTTP_READ_TIMEOUT": "soon"}, {"AFS_HTTP_READ_TIMEOUT": "0s"}, {"AFS_HTTP_READ_TIMEOUT": "-1s"},
		{"AFS_HTTP_IDLE_TIMEOUT": "x"}, {"AFS_HTTP_IDLE_TIMEOUT": "0"},
	} {
		if _, err := settingsFrom(env(bad)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// D-7 (FR-R10): under AFS_DB_IAM_AUTH the startup migration signs an RDS IAM token for the owner login,
// like the pool does for the app login.
func TestMigrateUsesIAMPasswordWhenIAMAuthIsOn(t *testing.T) {
	creds := awssdk.CredentialsProviderFunc(func(context.Context) (awssdk.Credentials, error) {
		return awssdk.Credentials{AccessKeyID: "test-key-id", SecretAccessKey: "test-secret", Source: "test"}, nil
	})
	errStop := errors.New("stop after migrate")
	run := func(iam bool, ownerURL string) (called bool, pw string, pwErr error) {
		mig := func(ctx context.Context, url string, _ time.Duration, password func(context.Context) (string, error)) (int, error) {
			called = true
			if url != ownerURL {
				t.Errorf("migrated %q, want the owner URL", url)
			}
			if password != nil {
				pw, pwErr = password(ctx)
			}
			return 0, errStop
		}
		_, err := wire(t.Context(), quiet, settings{
			DatabaseURL: "postgres://afs_app@db.example.internal:5432/afs", OwnerURL: ownerURL,
			DBIAMAuth: iam, AWSRegion: "us-east-1", AWSCredentials: creds, Migrate: mig,
		})
		if !errors.Is(err, errStop) {
			t.Fatalf("wire err = %v", err)
		}
		return called, pw, pwErr
	}

	owner := "postgres://afs_owner@db.example.internal:5432/afs"
	called, pw, err := run(true, owner)
	if !called || err != nil {
		t.Fatalf("migrate called=%v err=%v", called, err)
	}
	if !strings.Contains(pw, "X-Amz-Signature") || !strings.Contains(pw, "DBUser=afs_owner") {
		t.Fatalf("migration password is not an IAM token for the owner login: %q", pw)
	}
	// Without IAM auth the owner URL's own credentials are used: no password function.
	called, pw, _ = run(false, owner)
	if !called || pw != "" {
		t.Fatalf("IAM off: called=%v password=%q", called, pw)
	}
}
