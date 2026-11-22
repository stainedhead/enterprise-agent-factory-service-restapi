package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Database contract §7.1. The claim is inserted in the mutation's own transaction: a concurrent
// request with the same key waits on the primary key until the first commits (and then replays) or
// rolls back (and then runs), so a 5xx, which rolls back, never leaves a key behind.
const (
	purgeExpiredKeySQL = `DELETE FROM afs.idempotency_keys
WHERE principal = $1 AND key = $2 AND expires_at <= now()`
	claimKeySQL = `INSERT INTO afs.idempotency_keys (principal, key, team_id, operation, request_sha256, expires_at)
VALUES ($1, $2, nullif($3, ''), $4, $5, now() + make_interval(secs =>
  coalesce((SELECT (value #>> '{}')::int FROM afs.service_settings WHERE key = 'api.idempotency_key_ttl_seconds'), 3600)))
ON CONFLICT (principal, key) DO NOTHING`
	readKeySQL = `SELECT operation, request_sha256, state, response_status, response_body
FROM afs.idempotency_keys WHERE principal = $1 AND key = $2`
	completeKeySQL = `UPDATE afs.idempotency_keys SET state = 'completed', response_status = $3, response_body = $4
WHERE principal = $1 AND key = $2`
)

// IdempotencyStore implements port.IdempotencyStore.
type IdempotencyStore struct{}

// NewIdempotencyStore returns an IdempotencyStore.
func NewIdempotencyStore() *IdempotencyStore { return &IdempotencyStore{} }

// Begin implements port.IdempotencyStore.
func (IdempotencyStore) Begin(ctx context.Context, tx port.Tx, k port.IdempotencyKey) (*port.StoredResponse, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	p := t.session.Actor
	if _, err := t.Exec(ctx, purgeExpiredKeySQL, p, k.Key); err != nil {
		return nil, MapError(err, OpDelete, "idempotency key")
	}
	tag, err := t.Exec(ctx, claimKeySQL, p, k.Key, k.TeamID, k.Operation, k.RequestSHA256[:])
	if err != nil {
		return nil, MapError(err, OpWrite, "idempotency key")
	}
	if tag.RowsAffected() == 1 {
		return nil, nil
	}
	var (
		op, state string
		sum       []byte
		status    *int16
		body      []byte
	)
	err = t.QueryRow(ctx, readKeySQL, p, k.Key).Scan(&op, &sum, &state, &status, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		// The row expired and was purged between the two statements; the client may retry.
		e := domain.Conflict(domain.ReasonIdempotencyInProgress, "retry the request", nil)
		e.RetryAfterSeconds = 1
		return nil, e
	}
	if err != nil {
		return nil, MapError(err, OpRead, "idempotency key")
	}
	switch {
	case op != k.Operation || string(sum) != string(k.RequestSHA256[:]):
		return nil, domain.Conflict(domain.ReasonIdempotencyMismatch, "Idempotency-Key was used with a different request", nil)
	case state != "completed" || status == nil:
		e := domain.Conflict(domain.ReasonIdempotencyInProgress, "a request with this Idempotency-Key is in progress", nil)
		e.RetryAfterSeconds = 1
		return nil, e
	}
	return &port.StoredResponse{Status: int(*status), Body: json.RawMessage(body)}, nil
}

// Complete implements port.IdempotencyStore.
func (IdempotencyStore) Complete(ctx context.Context, tx port.Tx, k port.IdempotencyKey, resp port.StoredResponse) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 500 {
		return fmt.Errorf("postgres: a %d response is not stored for replay", resp.Status)
	}
	body, err := port.StripSecretKey(resp.Body)
	if err != nil {
		return fmt.Errorf("postgres: idempotent response body: %w", err)
	}
	var arg any
	if len(body) > 0 {
		arg = []byte(body)
	}
	tag, err := t.Exec(ctx, completeKeySQL, t.session.Actor, k.Key, resp.Status, arg)
	if err != nil {
		return MapError(err, OpWrite, "idempotency key")
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("postgres: Complete without Begin for key %q", k.Key)
	}
	return nil
}

var _ port.IdempotencyStore = IdempotencyStore{}
