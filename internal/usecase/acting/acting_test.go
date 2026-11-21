package acting_test

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
)

func TestSessionActorTypes(t *testing.T) {
	for name, tc := range map[string]struct {
		p    authz.Principal
		want port.ActorType
	}{
		"user":    {authz.Principal{ID: "usr_1", Type: authz.PrincipalUser, ServiceAdmin: true}, port.ActorUser},
		"api key": {authz.Principal{ID: "key_1", Type: authz.PrincipalAPIKey}, port.ActorAPIKey},
		"agent":   {authz.Principal{ID: "agt_1", Workload: &authz.Workload{TeamID: "team_1"}}, port.ActorAgent},
		"pool":    {authz.Principal{ID: "pool_1", Workload: &authz.Workload{TeamID: "team_1", PoolID: "pool_1"}}, port.ActorPool},
	} {
		s := acting.Caller{Principal: tc.p, RequestID: "req_1"}.Session()
		if s.ActorType != tc.want || s.Actor != tc.p.ID || s.RequestID != "req_1" || s.ServiceAdmin != tc.p.ServiceAdmin {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

func TestRecordCarriesTheCaller(t *testing.T) {
	c := acting.Caller{SourceIP: "10.0.0.1", UserAgent: "ua"}
	r := c.Record("CreateTeam", "teams", "team_1", "team_1", nil, map[string]any{"a": 1})
	if r.Action != "CreateTeam" || r.Outcome != port.OutcomeAllowed || r.SourceIP != "10.0.0.1" || r.UserAgent != "ua" || r.TeamID != "team_1" {
		t.Errorf("record = %+v", r)
	}
}

func TestPageAndSlice(t *testing.T) {
	if (acting.Page{}).Normalized().Limit != 50 || (acting.Page{Limit: 7}).Normalized().Limit != 7 {
		t.Error("Normalized")
	}
	rows := []string{"a", "b", "c"}
	page, next := acting.Slice(rows, 2, func(s string) string { return s })
	if len(page) != 2 || next != "b" {
		t.Errorf("slice = %v %q", page, next)
	}
	page, next = acting.Slice(rows, 3, func(s string) string { return s })
	if len(page) != 3 || next != "" {
		t.Errorf("full page = %v %q", page, next)
	}
}

func TestNilIdemIsANoOp(t *testing.T) {
	var i *acting.Idem
	r, err := i.Begin(t.Context(), &fakes.Idempotency{}, nil)
	if r != nil || err != nil {
		t.Errorf("begin = %v %v", r, err)
	}
	if err := i.Complete(t.Context(), &fakes.Idempotency{}, nil, 1); err != nil {
		t.Error(err)
	}
}

func TestDeniedWritesTheDeniedAuditAndReturnsAccessDenied(t *testing.T) {
	a := &fakes.Audit{}
	d := acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}
	err := d.Denied(t.Context(), acting.Caller{RequestID: "req_1"}, "PutSetting", "", authz.AdminSettings)
	if domain.KindOf(err) != domain.KindAccessDenied || len(a.Entries) != 1 || !a.Entries[0].Own {
		t.Fatalf("err = %v, entries = %+v", err, a.Entries)
	}
}

func TestEmitPublishesAnEvent(t *testing.T) {
	o := &fakes.Outbox{}
	tx := &fakes.TxRunner{}
	d := acting.Deps{Tx: tx, Audit: &fakes.Audit{}, Outbox: o, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}
	err := tx.InTx(t.Context(), port.Session{Actor: "usr_1", ActorType: port.ActorUser}, func(ctx context.Context, tx port.Tx) error {
		return d.Emit(ctx, tx, acting.Caller{RequestID: "req_1"}, domain.EventTeamCreated, "team_1", "teams", "team_1", map[string]any{"slug": "x"})
	})
	if err != nil || len(o.Events) != 1 || o.Events[0].Type != domain.EventTeamCreated {
		t.Fatalf("err = %v, events = %+v", err, o.Events)
	}
}

type badIDs struct{}

func (badIDs) NewID(domain.IDPrefix) (domain.ID, error) {
	var id domain.ID
	return id, errors.New("no entropy")
}

func TestEmitFailures(t *testing.T) {
	tx := &fakes.TxRunner{}
	d := acting.Deps{Tx: tx, Audit: &fakes.Audit{}, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}
	run := func(d acting.Deps, typ domain.EventType) error {
		return tx.InTx(t.Context(), port.Session{Actor: "usr_1", ActorType: port.ActorUser}, func(ctx context.Context, tx port.Tx) error {
			return d.Emit(ctx, tx, acting.Caller{}, typ, "team_1", "teams", "team_1", nil)
		})
	}
	if err := run(d, "no.such_event"); err == nil {
		t.Error("an unknown event type was published")
	}
	d.IDs = badIDs{}
	if err := run(d, domain.EventTeamCreated); err == nil {
		t.Error("an id failure was swallowed")
	}
}

func TestIdemBeginCompleteAndNow(t *testing.T) {
	tx := &fakes.TxRunner{}
	store := &fakes.Idempotency{}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d := acting.Deps{Clock: &fakes.Clock{T: at}}
	if !d.Now().Equal(at) {
		t.Errorf("now = %v", d.Now())
	}
	idem := &acting.Idem{Key: port.IdempotencyKey{Key: "k", Operation: "Op"}, Render: func(v any) (int, json.RawMessage, error) {
		return 201, json.RawMessage(`{"v":1}`), nil
	}}
	err := tx.InTx(t.Context(), port.Session{Actor: "usr_1", ActorType: port.ActorUser}, func(ctx context.Context, tx port.Tx) error {
		if r, err := idem.Begin(ctx, store, tx); r != nil || err != nil {
			t.Errorf("first begin = %v %v", r, err)
		}
		return idem.Complete(ctx, store, tx, "x")
	})
	if err != nil {
		t.Fatal(err)
	}
	err = tx.InTx(t.Context(), port.Session{Actor: "usr_1", ActorType: port.ActorUser}, func(ctx context.Context, tx port.Tx) error {
		r, err := idem.Begin(ctx, store, tx)
		if err != nil || r == nil || r.Status != 201 {
			t.Errorf("replay = %v %v", r, err)
		}
		idem.Render = func(any) (int, json.RawMessage, error) { return 0, nil, errors.New("render") }
		if err := idem.Complete(ctx, store, tx, "x"); err == nil {
			t.Error("render error swallowed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
