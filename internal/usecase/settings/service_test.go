package settings_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/settings"
)

type repo struct{ rows map[string]settings.Stored }

func (r *repo) All(context.Context, port.Tx) (map[string]settings.Stored, error) { return r.rows, nil }

func (r *repo) Put(_ context.Context, _ port.Tx, key string, v json.RawMessage, expect *domain.Version) (settings.Stored, error) {
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
	if err != nil || len(got) != len(domain.SettingSpecs) {
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
