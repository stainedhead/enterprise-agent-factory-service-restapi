package secops_test

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
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secops"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secops/secopstest"
)

const team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func rig() (*secops.Service, *secopstest.Repo, *fakes.Audit, *fakes.Outbox) {
	repo, audit, outbox := secopstest.New(), &fakes.Audit{}, &fakes.Outbox{}
	return &secops.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}}, Repo: repo}, repo, audit, outbox
}

var (
	serviceAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_sa", ServiceAdmin: true}}
	teamAdmin    = acting.Caller{Principal: authz.Principal{ID: "usr_ta", Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}}
	operator     = acting.Caller{Principal: authz.Principal{ID: "usr_so", SecurityOperator: true}}
	workload     = acting.Caller{Principal: authz.Principal{ID: "pool_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: team, PoolID: "pool_1"}}}
)

func group(name string) domain.NewMemberParams {
	return domain.NewMemberParams{SubjectType: domain.SubjectTypeGroup, GroupName: name, IdentityProviderID: "idp_01JD2Q3R4S5T6V7W8X9Y0Z0001"}
}

func TestAddListRemove(t *testing.T) {
	svc, repo, audit, outbox := rig()
	res, err := svc.Add(t.Context(), serviceAdmin, group("security-response"), nil)
	if err != nil || res.Operator.Role != secops.RoleSecurityOperator || res.Operator.TeamID != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(audit.Entries) != 1 || audit.Entries[0].Record.Action != "AddSecurityOperator" || audit.Entries[0].Record.TeamID != "" || len(outbox.Events) != 0 {
		t.Fatalf("audit=%+v outbox=%d", audit.Entries, len(outbox.Events))
	}
	if _, err := svc.Add(t.Context(), serviceAdmin, group("security-response"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate: %v", err)
	}
	rows, next, err := svc.List(t.Context(), serviceAdmin, acting.Page{})
	if err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("%v %q %v", rows, next, err)
	}
	if err := svc.Remove(t.Context(), serviceAdmin, res.Operator.ID, res.Operator.Version+1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale version: %v", err)
	}
	if err := svc.Remove(t.Context(), serviceAdmin, res.Operator.ID, res.Operator.Version); err != nil || len(repo.Rows) != 0 {
		t.Fatalf("%v %v", err, repo.Rows)
	}
	if err := svc.Remove(t.Context(), serviceAdmin, res.Operator.ID, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("gone: %v", err)
	}
	if last := audit.Entries[len(audit.Entries)-1]; last.Record.Action != "RemoveSecurityOperator" {
		t.Fatalf("%+v", last)
	}
}

func TestOnlyServiceAdminsManageOperators(t *testing.T) {
	svc, repo, audit, _ := rig()
	own, _ := svc.Add(t.Context(), serviceAdmin, group("sec"), nil)
	for name, c := range map[string]acting.Caller{"team admin": teamAdmin, "security operator": operator, "workload": workload} {
		t.Run(name, func(t *testing.T) {
			before := len(repo.Rows)
			if _, err := svc.Add(t.Context(), c, group("other"), nil); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("add: %v", err)
			}
			if _, _, err := svc.List(t.Context(), c, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("list: %v", err)
			}
			if err := svc.Remove(t.Context(), c, own.Operator.ID, own.Operator.Version); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("remove: %v", err)
			}
			if len(repo.Rows) != before {
				t.Error("a refused call changed the bindings")
			}
		})
	}
	denied := 0
	for _, e := range audit.Entries {
		if e.Record.Outcome == port.OutcomeDenied {
			denied++
		}
	}
	if denied != 9 {
		t.Errorf("denied audit records = %d, want 9", denied)
	}
}

func TestAddValidatesTheSubject(t *testing.T) {
	svc, repo, _, _ := rig()
	repo.Users["pat@example.com"] = "usr_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	for _, tc := range []struct {
		name string
		in   domain.NewMemberParams
		kind domain.ErrorKind
	}{
		{"missing subject type", domain.NewMemberParams{}, domain.KindValidation},
		{"group without name", domain.NewMemberParams{SubjectType: domain.SubjectTypeGroup}, domain.KindValidation},
		{"user and email", domain.NewMemberParams{SubjectType: domain.SubjectTypeUser}, domain.KindValidation},
		{"user with provider", domain.NewMemberParams{SubjectType: domain.SubjectTypeUser, Email: "pat@example.com", IdentityProviderID: "idp_x"}, domain.KindValidation},
		{"unknown user", domain.NewMemberParams{SubjectType: domain.SubjectTypeUser, Email: "nobody@example.com"}, domain.KindValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.Add(t.Context(), serviceAdmin, tc.in, nil); domain.KindOf(err) != tc.kind {
				t.Fatalf("got %v", err)
			}
		})
	}
	res, err := svc.Add(t.Context(), serviceAdmin, domain.NewMemberParams{SubjectType: domain.SubjectTypeUser, Email: "pat@example.com"}, nil)
	if err != nil || res.Operator.UserID != "usr_01JD2Q3R4S5T6V7W8X9Y0Z0009" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAddIsIdempotent(t *testing.T) {
	svc, repo, _, _ := rig()
	render := func(v any) (int, json.RawMessage, error) {
		b, err := json.Marshal(map[string]any{"id": v.(secops.Operator).ID})
		return 201, b, err
	}
	key := port.IdempotencyKey{Key: "k1", Operation: "AddSecurityOperator", RequestSHA256: [32]byte{1}}
	first, err := svc.Add(t.Context(), serviceAdmin, group("sec"), &acting.Idem{Key: key, Render: render})
	if err != nil || first.Replay != nil {
		t.Fatalf("%+v %v", first, err)
	}
	again, err := svc.Add(t.Context(), serviceAdmin, group("sec"), &acting.Idem{Key: key, Render: render})
	if err != nil || again.Replay == nil || len(repo.Rows) != 1 {
		t.Fatalf("%+v %v rows=%d", again, err, len(repo.Rows))
	}
}

type failingRepo struct {
	*secopstest.Repo
	list, resolve, insert, get, del error
}

func (r *failingRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]secops.Operator, error) {
	if r.list != nil {
		return nil, r.list
	}
	return r.Repo.List(ctx, tx, after, limit)
}

func (r *failingRepo) ResolveUser(ctx context.Context, tx port.Tx, id, email string) (string, bool, error) {
	if r.resolve != nil {
		return "", false, r.resolve
	}
	return r.Repo.ResolveUser(ctx, tx, id, email)
}

func (r *failingRepo) Insert(ctx context.Context, tx port.Tx, m secops.Operator) error {
	if r.insert != nil {
		return r.insert
	}
	return r.Repo.Insert(ctx, tx, m)
}

func (r *failingRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (secops.Operator, error) {
	if r.get != nil {
		return secops.Operator{}, r.get
	}
	return r.Repo.Get(ctx, tx, id, forUpdate)
}

func (r *failingRepo) Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	if r.del != nil {
		return r.del
	}
	return r.Repo.Delete(ctx, tx, id, expect)
}

// A failure of the repository, the transaction or the id source reaches the caller unchanged and writes nothing.
func TestRepositoryFailures(t *testing.T) {
	boom := errors.New("boom")
	svc, repo, audit, _ := rig()
	repo.Users["pat@example.com"] = "usr_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	user := domain.NewMemberParams{SubjectType: domain.SubjectTypeUser, Email: "pat@example.com"}
	existing, err := svc.Add(t.Context(), serviceAdmin, group("keep"), nil)
	if err != nil {
		t.Fatal(err)
	}
	before := len(audit.Entries)
	for name, f := range map[string]func(*failingRepo) error{
		"list": func(r *failingRepo) error {
			r.list = boom
			_, _, err := svc.List(t.Context(), serviceAdmin, acting.Page{})
			return err
		},
		"resolve user": func(r *failingRepo) error {
			r.resolve = boom
			_, err := svc.Add(t.Context(), serviceAdmin, user, nil)
			return err
		},
		"insert": func(r *failingRepo) error {
			r.insert = boom
			_, err := svc.Add(t.Context(), serviceAdmin, group("new"), nil)
			return err
		},
		"read back": func(r *failingRepo) error {
			r.get = boom
			_, err := svc.Add(t.Context(), serviceAdmin, group("new2"), nil)
			return err
		},
		"remove get": func(r *failingRepo) error {
			r.get = boom
			return svc.Remove(t.Context(), serviceAdmin, existing.Operator.ID, existing.Operator.Version)
		},
		"remove delete": func(r *failingRepo) error {
			r.del = boom
			return svc.Remove(t.Context(), serviceAdmin, existing.Operator.ID, existing.Operator.Version)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fr := &failingRepo{Repo: repo}
			svc.Repo = fr
			if err := f(fr); !errors.Is(err, boom) {
				t.Errorf("err = %v, want the repository's", err)
			}
		})
	}
	svc.Repo = repo
	if len(audit.Entries) != before {
		t.Errorf("a failed call wrote %d audit records", len(audit.Entries)-before)
	}
	// A failing transaction runner and id source.
	tx := &fakes.TxRunner{Err: boom}
	svc.Tx = tx
	if _, _, err := svc.List(t.Context(), serviceAdmin, acting.Page{}); !errors.Is(err, boom) {
		t.Errorf("list tx: %v", err)
	}
	if _, err := svc.Add(t.Context(), serviceAdmin, group("tx"), nil); !errors.Is(err, boom) {
		t.Errorf("add tx: %v", err)
	}
}

// List pages by id.
func TestListPages(t *testing.T) {
	svc, _, _, _ := rig()
	for _, g := range []string{"a-group", "b-group", "c-group"} {
		if _, err := svc.Add(t.Context(), serviceAdmin, group(g), nil); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := svc.List(t.Context(), serviceAdmin, acting.Page{Limit: 2})
	if err != nil || len(first) != 2 || next == "" {
		t.Fatalf("%d rows, next %q, %v", len(first), next, err)
	}
	rest, next, err := svc.List(t.Context(), serviceAdmin, acting.Page{Limit: 2, Cursor: next})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("%d rows, next %q, %v", len(rest), next, err)
	}
}
