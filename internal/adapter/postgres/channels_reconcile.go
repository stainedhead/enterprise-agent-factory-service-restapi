package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

// The reconciler's side of notification channels (M3-G3, M3-G4): the channel source, the lister that
// adds one resource per channel, the status recorder, the delivery failure event and the test
// request reader. All run as afs_reconciler.

// ChannelSource implements notify.Source: the channel, its team's state, the ARN of the referenced
// secret (never a value) and the stored status.
type ChannelSource struct{ runner *TxRunner }

// NewChannelSource returns a ChannelSource on the afs_reconciler pool's TxRunner.
func NewChannelSource(runner *TxRunner) *ChannelSource { return &ChannelSource{runner: runner} }

var _ notify.Source = (*ChannelSource)(nil)

const loadChannelSQL = `SELECT c.id, coalesce(c.team_id, ''), c.name, c.kind, c.config, coalesce(s.secrets_manager_arn, ''), c.event_types,
  c.enabled, c.version, coalesce(t.state, 'active'), rs.conditions, coalesce(rs.observed_generation, 0)
FROM afs.notification_channels c
LEFT JOIN afs.teams t ON t.id = c.team_id
LEFT JOIN afs.secrets s ON s.team_id = c.team_id AND s.id = c.secret_id
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + domain.ChannelStatusKind + `' AND rs.resource_id = c.id
WHERE c.id = $1`

// LoadChannel implements notify.Source.
func (s *ChannelSource) LoadChannel(ctx context.Context, id string) (notify.Channel, error) {
	var ch notify.Channel
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var (
			kind  string
			cfg   []byte
			conds []byte
		)
		err = t.QueryRow(ctx, loadChannelSQL, id).Scan(&ch.ID, &ch.TeamID, &ch.Name, &kind, &cfg, &ch.SecretARN, &ch.EventTypes,
			&ch.Enabled, &ch.Version, &ch.TeamState, &conds, &ch.ObservedGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("notification channel")
		}
		if err != nil {
			return MapError(err, OpRead, "notification channel")
		}
		ch.Kind = domain.ChannelKind(kind)
		if err := json.Unmarshal(cfg, &ch.Config); err != nil {
			return domain.Internal(err)
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &ch.Prior); err != nil {
				return domain.Internal(err)
			}
		}
		return nil
	})
	return ch, err
}

// ChannelLister adds the per-channel resources to another engine.Lister: every notification channel,
// service-level ones included. A channel is stale when its status has not observed its version.
type ChannelLister struct {
	engine.Lister
	pool Querier2
}

// NewChannelLister wraps base; pool is the afs_reconciler pool.
func NewChannelLister(base engine.Lister, pool Querier2) *ChannelLister {
	return &ChannelLister{Lister: base, pool: pool}
}

var _ engine.Lister = (*ChannelLister)(nil)

func (l *ChannelLister) split(kinds []engine.Kind) (rest []engine.Kind, want bool) {
	for _, k := range kinds {
		if k == notify.Kind {
			want = true
		} else {
			rest = append(rest, k)
		}
	}
	return rest, want
}

func (l *ChannelLister) channels(ctx context.Context, stale bool) ([]engine.Ref, error) {
	q := `SELECT c.id, coalesce(c.team_id, '') FROM afs.notification_channels c`
	if stale {
		q += ` LEFT JOIN afs.resource_status s ON s.resource_kind = '` + domain.ChannelStatusKind + `' AND s.resource_id = c.id
 WHERE s.resource_id IS NULL OR s.observed_generation <> c.version`
	}
	rows, err := l.pool.Query(ctx, q+` ORDER BY 1`)
	if err != nil {
		return nil, MapError(err, OpRead, "notification channels")
	}
	defer rows.Close()
	var out []engine.Ref
	for rows.Next() {
		var id, team string
		if err := rows.Scan(&id, &team); err != nil {
			return nil, MapError(err, OpRead, "notification channels")
		}
		out = append(out, engine.NewRef(notify.Kind, id, team))
	}
	return out, MapError(rows.Err(), OpRead, "notification channels")
}

// List implements engine.Lister.
func (l *ChannelLister) List(ctx context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	rest, want := l.split(kinds)
	var out []engine.Ref
	if len(rest) > 0 {
		var err error
		if out, err = l.Lister.List(ctx, rest); err != nil {
			return nil, err
		}
	}
	if !want {
		return out, nil
	}
	chs, err := l.channels(ctx, false)
	return append(out, chs...), err
}

// Stale implements engine.Lister.
func (l *ChannelLister) Stale(ctx context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	rest, want := l.split(kinds)
	var out []engine.Ref
	if len(rest) > 0 {
		var err error
		if out, err = l.Lister.Stale(ctx, rest); err != nil {
			return nil, err
		}
	}
	if !want {
		return out, nil
	}
	chs, err := l.channels(ctx, true)
	return append(out, chs...), err
}

// ChannelStatus implements notify.StatusCleaner and notify.Recorder on resource_status.
type ChannelStatus struct {
	runner *TxRunner
	status engine.StatusWriter
	now    func() time.Time
}

// NewChannelStatus returns a ChannelStatus; status is the engine's StatusWriter.
func NewChannelStatus(runner *TxRunner, status engine.StatusWriter, now func() time.Time) *ChannelStatus {
	return &ChannelStatus{runner: runner, status: status, now: now}
}

var (
	_ notify.StatusCleaner = (*ChannelStatus)(nil)
	_ notify.Recorder      = (*ChannelStatus)(nil)
)

// DeleteStatus implements notify.StatusCleaner.
func (s *ChannelStatus) DeleteStatus(ctx context.Context, channelID string) error {
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `DELETE FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, domain.ChannelStatusKind, channelID)
		return MapError(err, OpDelete, "channel status")
	})
}

// Record implements notify.Recorder: the outcome is merged into the stored conditions (a failure also
// marks the channel Degraded, a success Ready).
func (s *ChannelStatus) Record(ctx context.Context, ch notify.Channel, d notify.Delivery) error {
	var prior []domain.Condition
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var conds []byte
		err = t.QueryRow(ctx, `SELECT conditions FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, domain.ChannelStatusKind, ch.ID).Scan(&conds)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return MapError(err, OpRead, "channel status")
		}
		return json.Unmarshal(conds, &prior)
	})
	if err != nil {
		return err
	}
	phase := domain.PhaseReady
	if !d.OK {
		phase = domain.PhaseDegraded
	}
	return s.status.Apply(ctx, engine.Report{Ref: engine.NewRef(notify.Kind, ch.ID, ch.TeamID), Phase: phase,
		Conditions: notify.ApplyDelivery(prior, d), ReconciledAt: s.now()})
}

// ChannelEvents implements notify.Events: notification.delivery_failed on the outbox (RC-US-13.3).
type ChannelEvents struct {
	runner *TxRunner
	outbox port.OutboxWriter
	ids    port.IDGen
	now    func() time.Time
}

// NewChannelEvents returns a ChannelEvents.
func NewChannelEvents(runner *TxRunner, outbox port.OutboxWriter, ids port.IDGen, now func() time.Time) *ChannelEvents {
	return &ChannelEvents{runner: runner, outbox: outbox, ids: ids, now: now}
}

var _ notify.Events = (*ChannelEvents)(nil)

// DeliveryFailed implements notify.Events. The error text has been stripped of URLs by the caller.
func (e *ChannelEvents) DeliveryFailed(ctx context.Context, ch notify.Channel, errText, eventType string) error {
	return e.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		id, err := e.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		ev, err := domain.NewEvent(domain.EventNotificationDeliveryFailed, id.String(), e.now(), tx.Session().Actor, ch.TeamID, domain.ChannelStatusKind, ch.ID,
			map[string]any{"channelId": ch.ID, "channelName": ch.Name, "kind": string(ch.Kind), "error": errText, "eventType": eventType})
		if err != nil {
			return err
		}
		return e.outbox.Publish(ctx, tx, ev)
	})
}

// ChannelTests implements notify.TestRequests by reading notification.test_requested rows of the outbox.
type ChannelTests struct{ runner *TxRunner }

// NewChannelTests returns a ChannelTests.
func NewChannelTests(runner *TxRunner) *ChannelTests { return &ChannelTests{runner: runner} }

var _ notify.TestRequests = (*ChannelTests)(nil)

// PendingTests implements notify.TestRequests.
func (c *ChannelTests) PendingTests(ctx context.Context, since time.Time) ([]notify.TestRequest, error) {
	var out []notify.TestRequest
	err := c.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT payload #>> '{data,channelId}', payload #>> '{data,testId}', created_at FROM afs.outbox_events
WHERE event_type = $1 AND created_at >= $2 ORDER BY id`, string(domain.EventNotificationTestRequested), since)
		if err != nil {
			return MapError(err, OpRead, "outbox")
		}
		defer rows.Close()
		for rows.Next() {
			var r notify.TestRequest
			if err := rows.Scan(&r.ChannelID, &r.TestID, &r.At); err != nil {
				return MapError(err, OpRead, "outbox")
			}
			if r.ChannelID != "" && r.TestID != "" {
				out = append(out, r)
			}
		}
		return rows.Err()
	})
	return out, err
}
