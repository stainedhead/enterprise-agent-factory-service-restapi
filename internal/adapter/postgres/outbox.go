package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// insertOutboxSQL never uses RETURNING (see insertAuditSQL; policy app_operator_work_order_events).
const insertOutboxSQL = `INSERT INTO afs.outbox_events (event_type, aggregate_kind, aggregate_id, team_id, payload)
VALUES ($1, $2, $3, nullif($4, ''), $5)`

// OutboxWriter implements port.OutboxWriter.
type OutboxWriter struct{}

// NewOutboxWriter returns an OutboxWriter.
func NewOutboxWriter() *OutboxWriter { return &OutboxWriter{} }

// Publish implements port.OutboxWriter. The payload is the API contract §7 detail; an empty actor
// is filled from the session.
func (OutboxWriter) Publish(ctx context.Context, tx port.Tx, ev domain.Event) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if !ev.Type.Known() {
		return fmt.Errorf("postgres: unknown event type %q", ev.Type)
	}
	if ev.AggregateKind == "" || ev.AggregateID == "" || ev.Detail.ID == "" || ev.Detail.OccurredAt.IsZero() {
		return fmt.Errorf("postgres: event %s needs aggregate, id and occurredAt", ev.Type)
	}
	if ev.Detail.Actor == "" {
		ev.Detail.Actor = t.session.Actor
	}
	if ev.Detail.Data == nil {
		ev.Detail.Data = map[string]any{}
	}
	payload, err := json.Marshal(ev.Detail)
	if err != nil {
		return fmt.Errorf("postgres: event payload: %w", err)
	}
	if _, err := t.Exec(ctx, insertOutboxSQL, string(ev.Type), ev.AggregateKind, ev.AggregateID, ev.Detail.TeamID, payload); err != nil {
		return MapError(err, OpWrite, "outbox event")
	}
	return nil
}

var _ port.OutboxWriter = OutboxWriter{}
