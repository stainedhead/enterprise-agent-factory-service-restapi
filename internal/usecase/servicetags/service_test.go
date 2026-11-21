package servicetags_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/servicetags"
)

type repo struct{ rows map[string]domain.ServiceTag }

func (r *repo) List(_ context.Context, _ port.Tx, after string, limit int) ([]domain.ServiceTag, error) {
	var out []domain.ServiceTag
	for _, t := range r.rows {
		if t.Key > after {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b domain.ServiceTag) int {
		if a.Key < b.Key {
			return -1
		}
		return 1
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, key string, _ bool) (domain.ServiceTag, error) {
	t, ok := r.rows[key]
	if !ok {
		return domain.ServiceTag{}, domain.NotFound("service tag")
	}
	return t, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, t domain.ServiceTag) error {
	t.Version, t.UpdatedAt = 1, time.Now()
	r.rows[t.Key] = t
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, t domain.ServiceTag, expect domain.Version) (domain.ServiceTag, error) {
	if r.rows[t.Key].Version != expect {
		return domain.ServiceTag{}, domain.PreconditionFailed()
	}
	t.Version = expect + 1
	r.rows[t.Key] = t
	return t, nil
}

func (r *repo) Delete(_ context.Context, _ port.Tx, key string, expect domain.Version) error {
	if r.rows[key].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.rows, key)
	return nil
}

func rig() (*servicetags.Service, *fakes.Audit, *fakes.Outbox) {
	a, o := &fakes.Audit{}, &fakes.Outbox{}
	return &servicetags.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: o, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}},
		Repo: &repo{rows: map[string]domain.ServiceTag{}}}, a, o
}

func caller(p authz.Principal) acting.Caller { return acting.Caller{Principal: p, RequestID: "req_1"} }

var (
	svcAdmin = caller(authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true})
	teamAdm  = caller(authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}})
	secop    = caller(authz.Principal{ID: "usr_o", Type: authz.PrincipalUser, SecurityOperator: true})
)

func str(s string) *string { return &s }

func TestPutListDelete(t *testing.T) {
	s, audit, outbox := rig()
	ctx := t.Context()
	tag := domain.ServiceTag{Key: "cost-line", Mode: domain.ServiceTagModeFixed, Value: str("fin")}
	got, err := s.Put(ctx, svcAdmin, tag, nil)
	if err != nil || got.Version != 1 {
		t.Fatalf("put = %+v %v", got, err)
	}
	tag.Value = str("ops")
	v := domain.Version(1)
	got, err = s.Put(ctx, svcAdmin, tag, &v)
	if err != nil || *got.Value != "ops" || got.Version != 2 {
		t.Fatalf("update = %+v %v", got, err)
	}
	if _, err := s.Put(ctx, svcAdmin, tag, &v); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale update = %v", err)
	}
	if _, err := s.Put(ctx, svcAdmin, domain.ServiceTag{Key: "new-key", Mode: domain.ServiceTagModeFixed, Value: str("x")}, &v); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("If-Match on a new tag = %v", err)
	}
	if _, err := s.Put(ctx, svcAdmin, domain.ServiceTag{Key: "Bad Key", Mode: domain.ServiceTagModeFixed}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bad tag = %v", err)
	}
	rows, next, err := s.List(ctx, svcAdmin, acting.Page{Limit: 10})
	if err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("list = %v %q %v", rows, next, err)
	}
	if err := s.Delete(ctx, svcAdmin, "cost-line", 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete = %v", err)
	}
	if err := s.Delete(ctx, svcAdmin, "missing", 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("missing delete = %v", err)
	}
	if err := s.Delete(ctx, svcAdmin, "cost-line", 2); err != nil {
		t.Fatal(err)
	}
	if len(audit.Entries) != 3 || len(outbox.Events) != 0 {
		t.Errorf("audit = %d (want create, update, delete), outbox = %d (want none, CD-7)", len(audit.Entries), len(outbox.Events))
	}
}

func TestDeniedForEveryoneButServiceAdmins(t *testing.T) {
	for name, c := range map[string]acting.Caller{"team admin": teamAdm, "security operator": secop} {
		s, audit, _ := rig()
		ctx := t.Context()
		if _, _, err := s.List(ctx, c, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s list = %v", name, err)
		}
		if _, err := s.Put(ctx, c, domain.ServiceTag{Key: "k", Mode: domain.ServiceTagModeFixed, Value: str("v")}, nil); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s put = %v", name, err)
		}
		if err := s.Delete(ctx, c, "k", 1); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s delete = %v", name, err)
		}
		if len(audit.Entries) != 3 {
			t.Errorf("%s: %d denied audit records, want 3", name, len(audit.Entries))
		}
		for _, e := range audit.Entries {
			if e.Record.Outcome != port.OutcomeDenied {
				t.Errorf("%s: outcome %v", name, e.Record.Outcome)
			}
		}
	}
}

func TestListPages(t *testing.T) {
	s, _, _ := rig()
	for _, k := range []string{"aa", "bb", "cc"} {
		if _, err := s.Put(t.Context(), svcAdmin, domain.ServiceTag{Key: k, Mode: domain.ServiceTagModeFixed, Value: str("v")}, nil); err != nil {
			t.Fatal(err)
		}
	}
	rows, next, err := s.List(t.Context(), svcAdmin, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next != "bb" {
		t.Fatalf("page 1 = %v %q %v", rows, next, err)
	}
	rows, next, _ = s.List(t.Context(), svcAdmin, acting.Page{Limit: 2, Cursor: next})
	if len(rows) != 1 || next != "" {
		t.Errorf("page 2 = %v %q", rows, next)
	}
}
