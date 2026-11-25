package settings_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/settings"
)

type repo struct {
	rows           map[string]settings.Stored
	allErr, putErr error
}

func (r *repo) All(context.Context, port.Tx) (map[string]settings.Stored, error) {
	return r.rows, r.allErr
}

func (r *repo) Put(_ context.Context, _ port.Tx, key string, v json.RawMessage, expect *domain.Version) (settings.Stored, error) {
	if r.putErr != nil {
		return settings.Stored{}, r.putErr
	}
	cur, ok := r.rows[key]
	if expect != nil && (!ok || cur.Version != *expect) {
		return settings.Stored{}, domain.PreconditionFailed()
	}
	r.rows[key] = settings.Stored{Value: v, Version: cur.Version + 1, UpdatedAt: time.Now()}
	return r.rows[key], nil
}

func rig() (*settings.Service, *fakes.Audit) {
	a := &fakes.Audit{}
	return &settings.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}},
		Repo: &repo{rows: map[string]settings.Stored{}}}, a
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
)

func TestGetReturnsEverySettingWithDefaults(t *testing.T) {
	s, _ := rig()
	got, err := s.Get(t.Context(), svcAdmin)
	if err != nil || len(got) != len(domain.AllSettingSpecs()) {
		t.Fatalf("get = %d settings, %v", len(got), err)
	}
	for _, g := range got {
		if g.UpdatedAt != nil || g.Version != 0 || string(g.Value) != g.Spec.Default {
			t.Errorf("%s not at its default: %+v", g.Spec.Key, g)
		}
	}
}

func TestPut(t *testing.T) {
	s, audit := rig()
	ctx := t.Context()
	key := domain.SettingTeamsEphemeralMaxDays
	got, err := s.Put(ctx, svcAdmin, key, json.RawMessage(`30`), nil)
	if err != nil || string(got.Value) != "30" || got.Version != 1 || got.UpdatedAt == nil {
		t.Fatalf("put = %+v %v", got, err)
	}
	v := got.Version
	if _, err := s.Put(ctx, svcAdmin, key, json.RawMessage(`31`), &v); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, svcAdmin, key, json.RawMessage(`32`), &v); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	for name, tc := range map[string]struct {
		key string
		val string
	}{
		"unknown key":  {"no.such_key", `1`},
		"out of range": {key, `100000`},
		"wrong type":   {key, `"x"`},
	} {
		if _, err := s.Put(ctx, svcAdmin, tc.key, json.RawMessage(tc.val), nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s = %v", name, err)
		}
	}
	if len(audit.Entries) != 2 {
		t.Errorf("audit = %d, want 2 (rejected values write nothing)", len(audit.Entries))
	}
}

func TestDeniedForTeamAdmins(t *testing.T) {
	s, audit := rig()
	if _, err := s.Get(t.Context(), teamAdm); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("get = %v", err)
	}
	if _, err := s.Put(t.Context(), teamAdm, domain.SettingTeamsEphemeralMaxDays, json.RawMessage(`30`), nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("put = %v", err)
	}
	if len(audit.Entries) != 2 {
		t.Errorf("denied audit = %d", len(audit.Entries))
	}
}

func TestPutCollabSettingRanges(t *testing.T) {
	s, _ := rig()
	ok := []struct{ key, val string }{
		{"orchestration.public_base_domain", `"collab.example.com"`}, {"orchestration.public_base_domain", `null`}, {"orchestration.public_endpoints", `false`},
		{"collab.allowed_source_cidrs", `["10.0.0.0/8","2001:db8::/32"]`}, {"collab.session_ttl_minutes", `15`},
		{"collab.session_ttl_minutes", `1440`}, {"collab.content_scan", `"warn"`}, {"collab.max_body_mb", `100`},
		{"collab.denied_spike_threshold", `100000`},
	}
	for _, tc := range ok {
		if _, err := s.Put(t.Context(), svcAdmin, tc.key, json.RawMessage(tc.val), nil); err != nil {
			t.Errorf("%s=%s refused: %v", tc.key, tc.val, err)
		}
	}
	bad := []struct{ key, val string }{
		{"orchestration.public_base_domain", `"nodots"`}, {"orchestration.public_endpoints", `"yes"`}, {"collab.allowed_source_cidrs", `["999.1.1.1/99"]`},
		{"collab.session_ttl_minutes", `14`}, {"collab.session_ttl_minutes", `1441`}, {"collab.content_scan", `"strict"`},
		{"collab.max_body_mb", `0`}, {"collab.max_body_mb", `101`}, {"collab.denied_spike_threshold", `0`},
		{"collab.denied_spike_threshold", `100001`},
	}
	for _, tc := range bad {
		if _, err := s.Put(t.Context(), svcAdmin, tc.key, json.RawMessage(tc.val), nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s=%s: want ValidationError, got %v", tc.key, tc.val, err)
		}
	}
}

func TestRenamedKeysAreReadOnlyAliases(t *testing.T) {
	s, audit := rig()
	_, err := s.Put(t.Context(), svcAdmin, domain.SettingCollabBaseDomain, json.RawMessage(`"x.example.com"`), nil)
	var de *domain.Error
	if !errors.As(err, &de) || de.Reason != "setting_renamed" || de.Details["newKey"] != domain.SettingOrchestrationPublicBaseDomain {
		t.Fatalf("renamed = %v", err)
	}
	if len(audit.Entries) != 0 {
		t.Errorf("a refused write is audited: %d", len(audit.Entries))
	}
}

func TestRepositoryFailures(t *testing.T) {
	boom := errors.New("boom")
	s, _ := rig()
	s.Repo.(*repo).allErr = boom
	if _, err := s.Get(t.Context(), svcAdmin); !errors.Is(err, boom) {
		t.Errorf("get = %v", err)
	}
	if _, err := s.Put(t.Context(), svcAdmin, domain.SettingTeamsEphemeralMaxDays, json.RawMessage(`30`), nil); !errors.Is(err, boom) {
		t.Errorf("put all = %v", err)
	}
	s.Repo.(*repo).allErr = nil
	s.Repo.(*repo).putErr = boom
	if _, err := s.Put(t.Context(), svcAdmin, domain.SettingTeamsEphemeralMaxDays, json.RawMessage(`30`), nil); !errors.Is(err, boom) {
		t.Errorf("put = %v", err)
	}
}

func TestPutMcpSettingEmitsChange(t *testing.T) {
	s, audit := rig()
	out := s.Outbox.(*fakes.Outbox)
	key := domain.SettingMcpTeamServerApproval
	if _, err := s.Put(t.Context(), svcAdmin, key, json.RawMessage(`"security_all"`), nil); err != nil {
		t.Fatal(err)
	}
	if len(out.Events) != 1 || out.Events[0].Type != domain.EventMcpSettingsChanged {
		t.Fatalf("events = %+v", out.Events)
	}
	if d := out.Events[0].Detail.Data; d["key"] != key || string(d["to"].(json.RawMessage)) != `"security_all"` {
		t.Errorf("event data = %v", d)
	}
	if _, err := s.Put(t.Context(), svcAdmin, domain.SettingTeamsEphemeralMaxDays, json.RawMessage(`30`), nil); err != nil || len(out.Events) != 1 {
		t.Errorf("non-MCP setting emitted: %v %d", err, len(out.Events))
	}
	if _, err := s.Put(t.Context(), teamAdm, key, json.RawMessage(`"security_all"`), nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("team admin = %v", err)
	}
	if len(audit.Entries) != 3 {
		t.Errorf("audit = %d", len(audit.Entries))
	}
}
