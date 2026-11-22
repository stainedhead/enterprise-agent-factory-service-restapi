package secrets_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

var errBoom = errors.New("boom")

type failAudit struct{ fakes.Audit }

func (*failAudit) Write(context.Context, port.Tx, port.AuditRecord) error { return errBoom }

type failOutbox struct{ fakes.Outbox }

func (*failOutbox) Publish(context.Context, port.Tx, domain.Event) error { return errBoom }

// A failing audit write, event or row update aborts each mutation with that error.
func TestFailuresAbort(t *testing.T) {
	faults := map[string]func(*rig){
		"audit":   func(r *rig) { r.svc.Audit = &failAudit{} },
		"outbox":  func(r *rig) { r.svc.Outbox = &failOutbox{} },
		"update":  func(r *rig) { r.repo.UpdateErr = errBoom },
		"version": func(r *rig) { r.repo.VersionErr = errBoom },
		"insert":  func(r *rig) { r.repo.InsertErr = errBoom },
	}
	d := "changed"
	v, _ := domain.NewSecretValue("value", "another-fixture-value")
	ops := map[string]func(*rig, domain.Secret) error{
		"create": func(r *rig, _ domain.Secret) error {
			_, err := r.svc.Create(t.Context(), admin, team, params("fresh"), nil)
			return err
		},
		"update": func(r *rig, s domain.Secret) error {
			_, err := r.svc.Update(t.Context(), admin, team, s.ID, s.Version, domain.SecretPatch{Description: &d})
			return err
		},
		"delete": func(r *rig, s domain.Secret) error { return r.svc.Delete(t.Context(), admin, team, s.ID, s.Version) },
		"put":    func(r *rig, s domain.Secret) error { return r.svc.PutValue(t.Context(), admin, team, s.ID, nil, v) },
		"rotate": func(r *rig, s domain.Secret) error {
			_, err := r.svc.Rotate(t.Context(), admin, team, s.ID, v, nil)
			return err
		},
	}
	for on, op := range ops {
		for fn, fault := range faults {
			t.Run(on+"/"+fn, func(t *testing.T) {
				r := newRig()
				seed, err := r.svc.Create(t.Context(), admin, team, params("seeded"), nil)
				if err != nil {
					t.Fatal(err)
				}
				fault(r)
				err = op(r, seed.Secret)
				if err == nil {
					t.Skip("this fault does not touch this operation")
				}
				if !errors.Is(err, errBoom) {
					t.Fatalf("err = %v", err)
				}
				r.noLeak(t, err)
			})
		}
	}
}

// A replayed rotate returns the stored response and calls Secrets Manager no more.
func TestRotateReplay(t *testing.T) {
	r := newRig()
	seed, err := r.svc.Create(t.Context(), admin, team, params("seeded"), nil)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := domain.NewSecretValue("value", value)
	mk := func() *acting.Idem { return idemFor("RotateSecret", "rot-1") }
	if _, err := r.svc.Rotate(t.Context(), admin, team, seed.Secret.ID, v, mk()); err != nil {
		t.Fatal(err)
	}
	n := len(r.w.Calls)
	res, err := r.svc.Rotate(t.Context(), admin, team, seed.Secret.ID, v, mk())
	if err != nil || res.Replay == nil || len(r.w.Calls) != n {
		t.Fatalf("replay: %+v %v", res, err)
	}
}

func TestListSecretsPaged(t *testing.T) {
	r := newRig()
	for _, n := range []string{"a1", "b1", "c1"} {
		if _, err := r.svc.Create(t.Context(), admin, team, params(n), nil); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := r.svc.List(t.Context(), admin, team, acting.Page{Limit: 2})
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page 1: %d %q %v", len(page), next, err)
	}
	rest, next, err := r.svc.List(t.Context(), admin, team, acting.Page{Limit: 2, Cursor: next})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("page 2: %d %q %v", len(rest), next, err)
	}
}

func idemFor(op, key string) *acting.Idem {
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: op, TeamID: team},
		Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{"ok":true}`), nil }}
}
