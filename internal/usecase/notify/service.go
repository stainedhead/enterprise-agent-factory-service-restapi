// Package notify implements the notification channel operations (API contract §4.10, RC-15): the
// team's channels (ListNotificationChannels … DeleteNotificationChannel, TestNotificationChannel) and
// the service-level channels (admin:settings). A channel is desired state read by the reconciler,
// which turns it into an EventBridge rule; it holds non-secret settings and a reference to the secret
// that carries the credential, never a credential. Contract §7 has no channel event, so a change writes
// the row (the database notifies the reconciler) and an audit record (CD-7); a test writes the
// notification.test_requested event (CD-3).
package notify

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "notification_channels"

// Default test wait: OQ-S1 says up to 10 s for the reconciler's result.
const (
	DefaultTestWait = 10 * time.Second
	DefaultPoll     = 250 * time.Millisecond
)

// Repository is the notification_channels store. teamID "" addresses the service-level channels.
// Every method runs in the transaction it is given, so row-level security applies with the caller's
// session.
type Repository interface {
	// List returns the scope's channels with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.NotificationChannel, error)
	// Get returns one channel of the scope (NotFound otherwise); forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.NotificationChannel, error)
	// SecretExists reports whether the team has a team-shared (not agent-scoped) secret with this id.
	SecretExists(ctx context.Context, tx port.Tx, teamID, secretID string) (bool, error)
	// Insert stores a channel; a duplicate name is a Conflict.
	Insert(ctx context.Context, tx port.Tx, c domain.NotificationChannel) error
	// Update stores the changed channel; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, c domain.NotificationChannel, expect domain.Version) (domain.NotificationChannel, error)
	// Delete removes the channel; PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error
	// NamedByWorkOrderSetting reports whether service setting work_orders.notification_channel_id names id.
	NamedByWorkOrderSetting(ctx context.Context, tx port.Tx, id string) (bool, error)
}

// Service is the notification channel use case set.
type Service struct {
	acting.Deps
	Repo Repository
	// TestWait and Poll bound how long TestChannel waits for the reconciler (defaults above).
	TestWait time.Duration
	Poll     time.Duration
	// Sleep waits d or until ctx ends; time-based by default (tests replace it).
	Sleep func(ctx context.Context, d time.Duration)
}

// Result is a channel with the replayed response of an idempotent create.
type Result struct {
	Channel domain.NotificationChannel
	Replay  *port.StoredResponse
}

// TestOutcome is the result of TestChannel and, for an idempotent replay, the stored response.
type TestOutcome struct {
	Result domain.TestResult
	Replay *port.StoredResponse
}

// scope is who a call is for: a team, or the service when TeamID is empty.
type scope struct {
	TeamID string
}

func (s scope) perm(write bool) authz.Permission {
	switch {
	case s.TeamID == "":
		return authz.AdminSettings
	case write:
		return authz.NotificationsWrite
	}
	return authz.NotificationsRead
}

func (s scope) resource() authz.Resource {
	if s.TeamID == "" {
		return authz.Resource{Scope: authz.ScopeService}
	}
	return authz.Resource{Scope: authz.ScopeTeam, TeamID: s.TeamID}
}

// can authorizes the call; a denied write is audited. Reads are authorized the same way but need no audit.
func (s *Service) can(ctx context.Context, c acting.Caller, action string, sc scope, write bool) error {
	p := sc.perm(write)
	switch d := authz.Authorize(c.Principal, p, sc.resource()); d.Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	if !write {
		return domain.AccessDenied(string(p))
	}
	return s.Denied(ctx, c, action, sc.TeamID, p)
}

func auditOf(c domain.NotificationChannel) map[string]any {
	return map[string]any{"name": c.Name, "kind": c.Kind, "config": c.Config, "secretId": c.SecretID, "eventTypes": c.EventTypes, "enabled": c.Enabled}
}

func (s *Service) list(ctx context.Context, c acting.Caller, action string, sc scope, p acting.Page) ([]domain.NotificationChannel, string, error) {
	if err := s.can(ctx, c, action, sc, false); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.NotificationChannel
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, sc.TeamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(d domain.NotificationChannel) string { return d.ID })
	return page, next, nil
}

func (s *Service) get(ctx context.Context, c acting.Caller, action string, sc scope, id string) (domain.NotificationChannel, error) {
	var out domain.NotificationChannel
	if err := s.can(ctx, c, action, sc, false); err != nil {
		return out, err
	}
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.Get(ctx, tx, sc.TeamID, id, false)
		return err
	})
	return out, err
}

func (s *Service) checkSecret(ctx context.Context, tx port.Tx, sc scope, secretID string) error {
	if secretID == "" || sc.TeamID == "" {
		return nil
	}
	ok, err := s.Repo.SecretExists(ctx, tx, sc.TeamID, secretID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.Validation(domain.FieldError{Field: "secretId", Reason: "no such team secret (agent-scoped secrets cannot be used)"})
	}
	return nil
}

func (s *Service) create(ctx context.Context, c acting.Caller, action string, sc scope, in domain.NewChannelParams, idem *acting.Idem) (Result, error) {
	if err := s.can(ctx, c, action, sc, true); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := in.Validate(sc.TeamID == ""); err != nil {
			return err
		}
		if err := s.checkSecret(ctx, tx, sc, in.SecretID); err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixChannel)
		if err != nil {
			return err
		}
		ch, err := in.New(id.String(), sc.TeamID, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, ch); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kind, ch.ID, sc.TeamID, nil, auditOf(ch))); err != nil {
			return err
		}
		if ch, err = s.Repo.Get(ctx, tx, sc.TeamID, ch.ID, false); err != nil {
			return err
		}
		res.Channel = ch
		return idem.Complete(ctx, s.Idem, tx, ch)
	})
	return res, err
}

func (s *Service) update(ctx context.Context, c acting.Caller, action string, sc scope, id string, p domain.ChannelPatch, expect domain.Version) (domain.NotificationChannel, error) {
	if err := s.can(ctx, c, action, sc, true); err != nil {
		return domain.NotificationChannel{}, err
	}
	var out domain.NotificationChannel
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, sc.TeamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(p, s.Now())
		if err != nil {
			return err
		}
		if next.SecretID != cur.SecretID {
			if err := s.checkSecret(ctx, tx, sc, next.SecretID); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record(action, kind, id, sc.TeamID, auditOf(cur), auditOf(out)))
	})
	return out, err
}

func (s *Service) del(ctx context.Context, c acting.Caller, action string, sc scope, id string, expect domain.Version) (domain.NotificationChannel, error) {
	if err := s.can(ctx, c, action, sc, true); err != nil {
		return domain.NotificationChannel{}, err
	}
	var out domain.NotificationChannel
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, sc.TeamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if sc.TeamID == "" {
			named, err := s.Repo.NamedByWorkOrderSetting(ctx, tx, id)
			if err != nil {
				return err
			}
			if named {
				return domain.Conflict("channel_in_use", "the channel is named by setting "+domain.SettingWorkOrdersNotificationChannelID+"; change the setting first",
					map[string]any{"setting": domain.SettingWorkOrdersNotificationChannelID})
			}
		}
		if err := s.Repo.Delete(ctx, tx, sc.TeamID, id, expect); err != nil {
			return err
		}
		out = cur
		out.Status.Phase = domain.PhaseDeleting
		return s.Audit.Write(ctx, tx, c.Record(action, kind, id, sc.TeamID, auditOf(cur), nil))
	})
	return out, err
}

// ListTeam implements ListNotificationChannels.
func (s *Service) ListTeam(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.NotificationChannel, string, error) {
	return s.list(ctx, c, "ListNotificationChannels", scope{teamID}, p)
}

// GetTeam implements GetNotificationChannel.
func (s *Service) GetTeam(ctx context.Context, c acting.Caller, teamID, id string) (domain.NotificationChannel, error) {
	return s.get(ctx, c, "GetNotificationChannel", scope{teamID}, id)
}

// CreateTeam implements CreateNotificationChannel.
func (s *Service) CreateTeam(ctx context.Context, c acting.Caller, teamID string, in domain.NewChannelParams, idem *acting.Idem) (Result, error) {
	return s.create(ctx, c, "CreateNotificationChannel", scope{teamID}, in, idem)
}

// UpdateTeam implements UpdateNotificationChannel (If-Match required).
func (s *Service) UpdateTeam(ctx context.Context, c acting.Caller, teamID, id string, p domain.ChannelPatch, expect domain.Version) (domain.NotificationChannel, error) {
	return s.update(ctx, c, "UpdateNotificationChannel", scope{teamID}, id, p, expect)
}

// DeleteTeam implements DeleteNotificationChannel (If-Match required); the returned channel is in
// phase Deleting (CD-1).
func (s *Service) DeleteTeam(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) (domain.NotificationChannel, error) {
	return s.del(ctx, c, "DeleteNotificationChannel", scope{teamID}, id, expect)
}

// ListService implements ListServiceNotificationChannels.
func (s *Service) ListService(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.NotificationChannel, string, error) {
	return s.list(ctx, c, "ListServiceNotificationChannels", scope{}, p)
}

// CreateService implements CreateServiceNotificationChannel.
func (s *Service) CreateService(ctx context.Context, c acting.Caller, in domain.NewChannelParams, idem *acting.Idem) (Result, error) {
	return s.create(ctx, c, "CreateServiceNotificationChannel", scope{}, in, idem)
}

// UpdateService implements UpdateServiceNotificationChannel (If-Match required).
func (s *Service) UpdateService(ctx context.Context, c acting.Caller, id string, p domain.ChannelPatch, expect domain.Version) (domain.NotificationChannel, error) {
	return s.update(ctx, c, "UpdateServiceNotificationChannel", scope{}, id, p, expect)
}

// DeleteService implements DeleteServiceNotificationChannel (If-Match required). A channel named by
// work_orders.notification_channel_id is refused with 409 (RA-US-15.6).
func (s *Service) DeleteService(ctx context.Context, c acting.Caller, id string, expect domain.Version) (domain.NotificationChannel, error) {
	return s.del(ctx, c, "DeleteServiceNotificationChannel", scope{}, id, expect)
}

// TestChannel implements TestNotificationChannel (OQ-S1, CD-3). It writes notification.test_requested
// and an audit record in the transaction, then waits up to TestWait for the reconciler to record the
// result on the channel's status. Not reported in time: Pending with the test id. The idempotent
// response stored for a replay is always the pending one; the test's outcome is on the channel.
func (s *Service) TestChannel(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (TestOutcome, error) {
	sc := scope{teamID}
	if err := s.can(ctx, c, "TestNotificationChannel", sc, true); err != nil {
		return TestOutcome{}, err
	}
	var out TestOutcome
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		out = TestOutcome{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			out.Replay = replay
			return err
		}
		ch, err := s.Repo.Get(ctx, tx, teamID, id, false)
		if err != nil {
			return err
		}
		tid, err := s.IDs.NewID(domain.PrefixChannel)
		if err != nil {
			return err
		}
		testID := "ntest_" + tid.String()[len("nch_"):len("nch_")+12]
		out.Result = domain.TestResult{TestID: testID, Pending: true}
		if err := s.emitTest(ctx, tx, c, ch, testID); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("TestNotificationChannel", kind, id, teamID, nil, map[string]any{"testId": testID})); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, out.Result)
	})
	if err != nil || out.Replay != nil {
		return out, err
	}
	return s.await(ctx, c, teamID, id, out.Result)
}

func (s *Service) emitTest(ctx context.Context, tx port.Tx, c acting.Caller, ch domain.NotificationChannel, testID string) error {
	return s.Emit(ctx, tx, c, domain.EventNotificationTestRequested, ch.TeamID, kind, ch.ID, map[string]any{"channelId": ch.ID, "testId": testID})
}

func (s *Service) await(ctx context.Context, c acting.Caller, teamID, id string, pending domain.TestResult) (TestOutcome, error) {
	wait, poll, sleep := s.TestWait, s.Poll, s.Sleep
	if wait <= 0 {
		wait = DefaultTestWait
	}
	if poll <= 0 {
		poll = DefaultPoll
	}
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
			case <-t.C:
			}
		}
	}
	for waited := time.Duration(0); ; waited += poll {
		var ch domain.NotificationChannel
		err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
			var err error
			ch, err = s.Repo.Get(ctx, tx, teamID, id, false)
			return err
		})
		if err != nil {
			return TestOutcome{Result: pending}, nil //nolint:nilerr // the test was recorded; a read failure only means "not reported yet"
		}
		d := domain.DeliveryDetailOf(ch.Status.Conditions)
		if d.LastTestID == pending.TestID && d.TestDelivered != nil {
			return TestOutcome{Result: domain.TestResult{TestID: pending.TestID, Delivered: *d.TestDelivered, Error: d.TestError}}, nil
		}
		if waited >= wait || ctx.Err() != nil {
			return TestOutcome{Result: pending}, nil
		}
		sleep(ctx, poll)
	}
}
