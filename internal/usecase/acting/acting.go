// Package acting holds what the M1 use case packages (teams, members, apikeys, settings, idp,
// servicetags, audit, revisions) share: the calling principal and its database session, the common
// ports bundle, event and audit helpers, and the idempotent-create wrapper.
package acting

import (
	"context"
	"encoding/json"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Caller is who is calling and from where.
type Caller struct {
	Principal authz.Principal
	RequestID string
	SourceIP  string
	UserAgent string
}

// Session turns the caller into the database session of database contract §3 (auth.SessionOf).
func (c Caller) Session() port.Session { return auth.SessionOf(c.Principal, c.RequestID) }

// Record builds an audit record for an allowed mutation by the caller.
func (c Caller) Record(action, kind, id, teamID string, before, after any) port.AuditRecord {
	return port.AuditRecord{Action: action, Outcome: port.OutcomeAllowed, ResourceKind: kind, ResourceID: id,
		TeamID: teamID, SourceIP: c.SourceIP, UserAgent: c.UserAgent, Before: before, After: after}
}

// Deps are the cross-cutting ports every area uses.
type Deps struct {
	Tx     port.TxRunner
	Audit  port.AuditWriter
	Outbox port.OutboxWriter
	Idem   port.IdempotencyStore
	Clock  port.Clock
	IDs    port.IDGen
}

// Emit publishes an API contract §7 event in tx.
func (d Deps) Emit(ctx context.Context, tx port.Tx, c Caller, typ domain.EventType, teamID, kind, id string, data map[string]any) error {
	eid, err := d.IDs.NewID("evt")
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, eid.String(), d.Clock.Now(), c.Session().Actor, teamID, kind, id, data)
	if err != nil {
		return err
	}
	return d.Outbox.Publish(ctx, tx, ev)
}

// Denied records a refused request in its own transaction and returns the AccessDenied error.
func (d Deps) Denied(ctx context.Context, c Caller, action, teamID string, perm authz.Permission) error {
	_ = d.Audit.WriteDenied(ctx, c.Session(), port.AuditRecord{Action: action, TeamID: teamID, SourceIP: c.SourceIP,
		UserAgent: c.UserAgent, After: map[string]any{"requiredPermission": string(perm)}})
	return domain.AccessDenied(string(perm))
}

// Idem makes a create idempotent: the claim and the completion happen in the mutation's own
// transaction (M0-F6). Render turns the result into the status and JSON body that is stored and
// replayed; the store strips secretKey.
type Idem struct {
	Key    port.IdempotencyKey
	Render func(result any) (status int, body json.RawMessage, err error)
}

// Begin claims the key; a non-nil response is a replay.
func (i *Idem) Begin(ctx context.Context, store port.IdempotencyStore, tx port.Tx) (*port.StoredResponse, error) {
	if i == nil {
		return nil, nil
	}
	return store.Begin(ctx, tx, i.Key)
}

// Complete stores the rendered response.
func (i *Idem) Complete(ctx context.Context, store port.IdempotencyStore, tx port.Tx, result any) error {
	if i == nil {
		return nil
	}
	st, body, err := i.Render(result)
	if err != nil {
		return err
	}
	return store.Complete(ctx, tx, i.Key, port.StoredResponse{Status: st, Body: body})
}

// Page is a page request: Limit rows after the keyset Cursor.
type Page struct {
	Limit  int
	Cursor string
}

// Normalized returns p with a default limit.
func (p Page) Normalized() Page {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	return p
}

// Slice trims rows fetched with Limit+1 to a page and returns the cursor of the next one ("" on the
// last page). key returns a row's cursor.
func Slice[T any](rows []T, limit int, key func(T) string) ([]T, string) {
	if len(rows) <= limit {
		return rows, ""
	}
	return rows[:limit], key(rows[limit-1])
}

// Now is the clock time in UTC.
func (d Deps) Now() time.Time { return d.Clock.Now().UTC() }
