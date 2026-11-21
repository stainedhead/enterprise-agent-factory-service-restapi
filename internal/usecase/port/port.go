// Package port defines the cross-cutting ports every use case shares: transactions and their session
// settings, audit and outbox writers, idempotency, time, ids, service settings and the pre-scope
// credential lookups. Adapters in internal/adapter implement them; fakes live in port/fakes.
package port

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// ActorType is the kind of principal recorded in audit rows (database audit_log.actor_type).
type ActorType string

// Actor types.
const (
	ActorUser   ActorType = "user"
	ActorAPIKey ActorType = "api_key"
	ActorSystem ActorType = "system"
	ActorAgent  ActorType = "agent"
	ActorPool   ActorType = "pool"
)

// Session holds the settings every afs-api transaction sets with SET LOCAL (database contract §3).
// A zero Session sees no team rows.
type Session struct {
	// Actor is the principal id (usr_…, key_…, agt_…, pool_…) or system:<service>.
	Actor string
	// ActorType is recorded in audit rows.
	ActorType ActorType
	// ServiceAdmin sets afs.service_admin.
	ServiceAdmin bool
	// SecurityOperator sets afs.security_operator.
	SecurityOperator bool
	// TeamIDs sets afs.team_ids: the teams the caller is bound to.
	TeamIDs []string
	// RequestID is copied into audit rows.
	RequestID string
}

// SystemSession is the session of a background job of service (actor system:<service>).
func SystemSession(service string) Session {
	return Session{Actor: "system:" + service, ActorType: ActorSystem}
}

// Tx is an open transaction. It is opaque to use cases: they pass it to repositories and writers,
// which belong to the same adapter as the TxRunner that opened it.
type Tx interface {
	// Session returns the settings the transaction was opened with.
	Session() Session
}

// TxRunner opens transactions. fn may run more than once: a serialization failure or deadlock is
// retried up to three times, so fn must not have effects outside the transaction.
type TxRunner interface {
	// InTx runs fn in a READ COMMITTED read-write transaction with the session settings; it commits
	// when fn returns nil and rolls back otherwise.
	InTx(ctx context.Context, s Session, fn func(ctx context.Context, tx Tx) error) error
	// ReadTx is InTx in a READ ONLY transaction.
	ReadTx(ctx context.Context, s Session, fn func(ctx context.Context, tx Tx) error) error
}

// AuditOutcome is the outcome recorded in an audit row.
type AuditOutcome string

// Audit outcomes.
const (
	OutcomeAllowed AuditOutcome = "allowed"
	OutcomeDenied  AuditOutcome = "denied"
	OutcomeFailed  AuditOutcome = "failed"
)

// AuditRecord is one audit_log row. The actor, actor type and request id come from the session.
// Before and After never hold a secret value; writers drop any key that looks like one.
type AuditRecord struct {
	Action       string
	Outcome      AuditOutcome
	ResourceKind string
	ResourceID   string
	TeamID       string
	SourceIP     string
	UserAgent    string
	PoolID       string
	WorkerPod    string
	Before       any
	After        any
}

// AuditWriter writes audit rows.
type AuditWriter interface {
	// Write inserts rec in tx, so it commits or rolls back with the mutation it records.
	Write(ctx context.Context, tx Tx, rec AuditRecord) error
	// WriteDenied records a refused request in its own transaction, because the request's own
	// transaction, if any, rolls back.
	WriteDenied(ctx context.Context, s Session, rec AuditRecord) error
}

// OutboxWriter writes outbox events (API contract §7) in the mutation's transaction.
type OutboxWriter interface {
	Publish(ctx context.Context, tx Tx, ev domain.Event) error
}

// IdempotencyKey identifies one idempotent create (database contract §7.1). The principal is the
// session actor.
type IdempotencyKey struct {
	Key           string
	Operation     string
	TeamID        string
	RequestSHA256 [32]byte
}

// StoredResponse is a completed idempotent response.
type StoredResponse struct {
	Status int
	Body   json.RawMessage
}

// IdempotencyStore implements database contract §7.1.
type IdempotencyStore interface {
	// Begin claims the key in tx. It returns a stored response to replay, nil to go ahead, or a
	// Conflict (idempotency_mismatch, or idempotency_in_progress with RetryAfterSeconds).
	Begin(ctx context.Context, tx Tx, k IdempotencyKey) (*StoredResponse, error)
	// Complete stores the response in tx. secretKey is removed from the body before it is stored.
	// A 5xx is never stored: the caller rolls tx back instead.
	Complete(ctx context.Context, tx Tx, k IdempotencyKey, resp StoredResponse) error
}

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// IDGen generates prefixed ULIDs.
type IDGen interface {
	NewID(prefix domain.IDPrefix) (domain.ID, error)
}

// SettingsReader reads service settings (root PRD §6.13). Values are JSON as stored.
type SettingsReader interface {
	// Setting returns the value of key, or its default when unset. An unknown key is a
	// ValidationError.
	Setting(ctx context.Context, key string) (json.RawMessage, error)
}

// APIKeyRecord is what afs.authenticate_api_key returns for a key hash. It never holds the hash.
type APIKeyRecord struct {
	KeyID     string
	TeamID    string // empty for a service-level key
	Principal string
	Role      string
	Roles     []string
	ExpiresAt *time.Time
	RevokedAt *time.Time
	State     string // active, expired or revoked
	TeamState string // empty for a service-level key
}

// Usable reports whether the key may authenticate: database contract §4.2 answers 401 unless the
// key is active and its team, if any, is active.
func (r APIKeyRecord) Usable() bool {
	return r.State == "active" && (r.TeamID == "" || r.TeamState == "active")
}

// WorkloadRecord is what afs.resolve_workload returns: the team and exactly one of agent or pool.
type WorkloadRecord struct {
	TeamID  string
	AgentID string
	PoolID  string
}

// TeamDirectoryEntry is one row of afs.team_directory.
type TeamDirectoryEntry struct {
	ID          string
	Slug        string
	DisplayName string
}

// CredentialLookup finds a principal before its team scope is known (X-DB-1, X-DB-2).
type CredentialLookup interface {
	// AuthenticateAPIKey looks up a key by the SHA-256 of its secret; ok is false for an unknown hash.
	AuthenticateAPIKey(ctx context.Context, keySHA256 [32]byte) (rec APIKeyRecord, ok bool, err error)
	// ResolveWorkload maps a workload token's namespace and ServiceAccount to its team and live agent
	// or pool; ok is false when nothing matches.
	ResolveWorkload(ctx context.Context, namespace, serviceAccount string) (rec WorkloadRecord, ok bool, err error)
}

// TeamDirectory lists every team for service admins and security operators (X-DB-3). Other
// callers get AccessDenied.
type TeamDirectory interface {
	ListTeamDirectory(ctx context.Context, s Session) ([]TeamDirectoryEntry, error)
}

// StripSecretKey removes every "secretKey" member from a JSON document.
func StripSecretKey(body json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return json.Marshal(strip(v))
}

func strip(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "secretKey")
		for k, x := range t {
			t[k] = strip(x)
		}
	case []any:
		for i, x := range t {
			t[i] = strip(x)
		}
	}
	return v
}
