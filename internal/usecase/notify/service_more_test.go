package notify_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func idemFor(op, key string) *acting.Idem {
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: op, TeamID: team, RequestSHA256: [32]byte{1}},
		Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{"ok":true}`), nil }}
}

func TestCreateAndUpdateRejections(t *testing.T) {
	r := newRig()
	ta := caller(authz.RoleTeamAdmin, team)
	res, err := r.svc.CreateTeam(t.Context(), ta, team, email("e"), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := res.Channel.ID
	empty := ""
	foreign := "sec_01JD2Q3R4S5T6V7W8X9Y0Z0009" //nolint:gosec // fixture id
	cases := []struct {
		name string
		do   func() error
		want domain.ErrorKind
	}{
		{"invalid create", func() error {
			_, err := r.svc.CreateTeam(t.Context(), ta, team, domain.NewChannelParams{Name: "", Kind: domain.ChannelEmail}, nil)
			return err
		}, domain.KindValidation},
		{"invalid patch", func() error {
			_, err := r.svc.UpdateTeam(t.Context(), ta, team, id, domain.ChannelPatch{Name: &empty}, 1)
			return err
		}, domain.KindValidation},
		{"foreign secret on update", func() error {
			_, err := r.svc.UpdateTeam(t.Context(), ta, team, id, domain.ChannelPatch{SecretID: &foreign}, 1)
			return err
		}, domain.KindValidation},
		{"unknown update", func() error {
			_, err := r.svc.UpdateTeam(t.Context(), ta, team, "nch_01JD2Q3R4S5T6V7W8X9Y0Z0099", domain.ChannelPatch{}, 1)
			return err
		}, domain.KindNotFound},
		{"other team admin cannot update", func() error {
			_, err := r.svc.UpdateTeam(t.Context(), caller(authz.RoleTeamAdmin, other), team, id, domain.ChannelPatch{}, 1)
			return err
		}, domain.KindNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := domain.KindOf(tc.do()); got != tc.want {
				t.Fatalf("kind = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIdempotentReplays(t *testing.T) {
	r := newRig()
	ta := caller(authz.RoleTeamAdmin, team)
	first, err := r.svc.CreateTeam(t.Context(), ta, team, email("e"), idemFor("CreateNotificationChannel", "k1"))
	if err != nil || first.Replay != nil {
		t.Fatalf("%+v %v", first, err)
	}
	again, err := r.svc.CreateTeam(t.Context(), ta, team, email("e"), idemFor("CreateNotificationChannel", "k1"))
	if err != nil || again.Replay == nil {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if len(r.repo.Rows) != 1 {
		t.Fatalf("rows = %d", len(r.repo.Rows))
	}
	out, err := r.svc.TestChannel(t.Context(), ta, team, first.Channel.ID, idemFor("TestNotificationChannel", "k2"))
	if err != nil || out.Replay != nil || !out.Result.Pending {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = r.svc.TestChannel(t.Context(), ta, team, first.Channel.ID, idemFor("TestNotificationChannel", "k2"))
	if err != nil || out.Replay == nil {
		t.Fatalf("test replay: %+v %v", out, err)
	}
}

func TestAwaitOutcomes(t *testing.T) {
	r := newRig()
	ta := caller(authz.RoleTeamAdmin, team)
	res, _ := r.svc.CreateTeam(t.Context(), ta, team, email("e"), nil)
	id := res.Channel.ID

	// Delivered: reported on the first poll.
	r.svc.Sleep = func(context.Context, time.Duration) {
		tid := r.outbox.Events[len(r.outbox.Events)-1].Detail.Data["testId"].(string)
		ok := true
		r.repo.SetStatus(id, []domain.Condition{domain.DeliveryDetail{LastTestID: tid, TestDelivered: &ok}.Condition(time.Now())})
	}
	out, err := r.svc.TestChannel(t.Context(), ta, team, id, nil)
	if err != nil || out.Result.Pending || !out.Result.Delivered {
		t.Fatalf("%+v %v", out, err)
	}

	// The caller's context ends while waiting: pending.
	ctx, cancel := context.WithCancel(t.Context())
	r.svc.Sleep = func(context.Context, time.Duration) { cancel() }
	r.repo.SetStatus(id, nil)
	out, err = r.svc.TestChannel(ctx, ta, team, id, nil)
	if err != nil || !out.Result.Pending {
		t.Fatalf("cancelled: %+v %v", out, err)
	}

	// The channel disappears while waiting: pending, not an error (the test was recorded).
	r.svc.Sleep = func(context.Context, time.Duration) { delete(r.repo.Rows, id) }
	out, err = r.svc.TestChannel(t.Context(), ta, team, id, nil)
	if err != nil || !out.Result.Pending {
		t.Fatalf("vanished: %+v %v", out, err)
	}
}

func TestAwaitDefaultsAndRealSleep(t *testing.T) {
	r := newRig()
	r.svc.TestWait, r.svc.Poll, r.svc.Sleep = 5*time.Millisecond, time.Millisecond, nil
	ta := caller(authz.RoleTeamAdmin, team)
	res, _ := r.svc.CreateTeam(t.Context(), ta, team, email("e"), nil)
	out, err := r.svc.TestChannel(t.Context(), ta, team, res.Channel.ID, nil)
	if err != nil || !out.Result.Pending {
		t.Fatalf("%+v %v", out, err)
	}
}
