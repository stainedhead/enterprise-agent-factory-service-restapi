// Package fakes holds in-memory implementations of the cross-cutting ports for use case tests.
// Writes made through a fake transaction become visible only when the transaction commits, so tests
// can prove that a failed mutation leaves no audit or outbox record behind.
package fakes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Tx is a fake transaction. Work registered with OnCommit runs only if it commits.
type Tx struct {
	session    port.Session
	readOnly   bool
	onCommit   []func()
	onRollback []func()
}

// Session implements port.Tx.
func (t *Tx) Session() port.Session { return t.session }

// ReadOnly reports whether the transaction was opened with ReadTx.
func (t *Tx) ReadOnly() bool { return t.readOnly }

// OnCommit registers fn to run when the transaction commits.
func (t *Tx) OnCommit(fn func()) { t.onCommit = append(t.onCommit, fn) }

// OnRollback registers fn to run when the transaction rolls back.
func (t *Tx) OnRollback(fn func()) { t.onRollback = append(t.onRollback, fn) }

// ErrReadOnly is returned by fake writers used in a ReadTx.
var ErrReadOnly = errors.New("fakes: write in a read-only transaction")

// TxRunner is a fake port.TxRunner.
type TxRunner struct {
	mu sync.Mutex
	// Sessions lists the session of every transaction opened, in order.
	Sessions []port.Session
	// Commits and Rollbacks count finished transactions.
	Commits, Rollbacks int
	// Err, when set, is returned by InTx and ReadTx without running fn.
	Err error
}

// InTx implements port.TxRunner.
func (r *TxRunner) InTx(ctx context.Context, s port.Session, fn func(context.Context, port.Tx) error) error {
	return r.run(ctx, s, false, fn)
}

// ReadTx implements port.TxRunner.
func (r *TxRunner) ReadTx(ctx context.Context, s port.Session, fn func(context.Context, port.Tx) error) error {
	return r.run(ctx, s, true, fn)
}

func (r *TxRunner) run(ctx context.Context, s port.Session, ro bool, fn func(context.Context, port.Tx) error) error {
	r.mu.Lock()
	r.Sessions = append(r.Sessions, s)
	err := r.Err
	r.mu.Unlock()
	if err != nil {
		return err
	}
	tx := &Tx{session: s, readOnly: ro}
	if err := fn(ctx, tx); err != nil {
		r.mu.Lock()
		r.Rollbacks++
		r.mu.Unlock()
		for _, f := range tx.onRollback {
			f()
		}
		return err
	}
	r.mu.Lock()
	r.Commits++
	r.mu.Unlock()
	for _, f := range tx.onCommit {
		f()
	}
	return nil
}

func asTx(tx port.Tx) (*Tx, error) {
	t, ok := tx.(*Tx)
	if !ok {
		return nil, fmt.Errorf("fakes: transaction %T is not a fake", tx)
	}
	if t.readOnly {
		return nil, ErrReadOnly
	}
	return t, nil
}

// AuditEntry is a committed audit record with the session it was written in.
type AuditEntry struct {
	Session port.Session
	Record  port.AuditRecord
	// Own is true for WriteDenied records, written in their own transaction.
	Own bool
}

// Audit is a fake port.AuditWriter.
type Audit struct {
	mu      sync.Mutex
	Entries []AuditEntry
}

// Write implements port.AuditWriter.
func (a *Audit) Write(_ context.Context, tx port.Tx, rec port.AuditRecord) error {
	t, err := asTx(tx)
	if err != nil {
		return err
	}
	t.OnCommit(func() { a.add(AuditEntry{Session: t.session, Record: rec}) })
	return nil
}

// WriteDenied implements port.AuditWriter.
func (a *Audit) WriteDenied(_ context.Context, s port.Session, rec port.AuditRecord) error {
	rec.Outcome = port.OutcomeDenied
	a.add(AuditEntry{Session: s, Record: rec, Own: true})
	return nil
}

func (a *Audit) add(e AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Entries = append(a.Entries, e)
}

// Outbox is a fake port.OutboxWriter.
type Outbox struct {
	mu     sync.Mutex
	Events []domain.Event
}

// Publish implements port.OutboxWriter.
func (o *Outbox) Publish(_ context.Context, tx port.Tx, ev domain.Event) error {
	t, err := asTx(tx)
	if err != nil {
		return err
	}
	if !ev.Type.Known() {
		return fmt.Errorf("fakes: unknown event type %q", ev.Type)
	}
	t.OnCommit(func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.Events = append(o.Events, ev)
	})
	return nil
}

type idemRow struct {
	op        string
	sum       [32]byte
	completed bool
	resp      port.StoredResponse
}

// Idempotency is a fake port.IdempotencyStore with the rules of database contract §7.1.
type Idempotency struct {
	mu   sync.Mutex
	rows map[[2]string]*idemRow
}

// Begin implements port.IdempotencyStore.
func (s *Idempotency) Begin(_ context.Context, tx port.Tx, k port.IdempotencyKey) (*port.StoredResponse, error) {
	t, err := asTx(tx)
	if err != nil {
		return nil, err
	}
	id := [2]string{t.session.Actor, k.Key}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[[2]string]*idemRow{}
	}
	if r, ok := s.rows[id]; ok {
		switch {
		case r.op != k.Operation || r.sum != k.RequestSHA256:
			return nil, domain.Conflict(domain.ReasonIdempotencyMismatch, "Idempotency-Key was used with a different request", nil)
		case !r.completed:
			e := domain.Conflict(domain.ReasonIdempotencyInProgress, "a request with this Idempotency-Key is in progress", nil)
			e.RetryAfterSeconds = 1
			return nil, e
		default:
			resp := r.resp
			return &resp, nil
		}
	}
	r := &idemRow{op: k.Operation, sum: k.RequestSHA256}
	s.rows[id] = r
	// The claim disappears when the transaction rolls back (a 5xx is never stored).
	t.OnRollback(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.rows[id] == r {
			delete(s.rows, id)
		}
	})
	return nil, nil
}

// Complete implements port.IdempotencyStore.
func (s *Idempotency) Complete(_ context.Context, tx port.Tx, k port.IdempotencyKey, resp port.StoredResponse) error {
	t, err := asTx(tx)
	if err != nil {
		return err
	}
	if resp.Status >= 500 {
		return fmt.Errorf("fakes: a %d response is never stored", resp.Status)
	}
	body, err := port.StripSecretKey(resp.Body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[[2]string{t.session.Actor, k.Key}]
	if !ok {
		return errors.New("fakes: Complete without Begin")
	}
	r.completed = true
	r.resp = port.StoredResponse{Status: resp.Status, Body: body}
	return nil
}

// Clock is a fake port.Clock that returns T.
type Clock struct {
	mu sync.Mutex
	T  time.Time
}

// Now implements port.Clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.T = c.T.Add(d)
}

// IDGen is a fake port.IDGen that returns ids with an increasing counter in the ULID body.
type IDGen struct {
	mu sync.Mutex
	n  uint64
}

// NewID implements port.IDGen.
func (g *IDGen) NewID(prefix domain.IDPrefix) (domain.ID, error) {
	g.mu.Lock()
	g.n++
	n := g.n
	g.mu.Unlock()
	var b [16]byte
	for i := 15; i >= 8; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	return domain.NewID(prefix, time.UnixMilli(0), bytes.NewReader(b[6:]))
}

// Settings is a fake port.SettingsReader: Values override the contract defaults.
type Settings struct {
	mu     sync.Mutex
	Values map[string]json.RawMessage
}

// Set stores a value.
func (s *Settings) Set(key string, v json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Values == nil {
		s.Values = map[string]json.RawMessage{}
	}
	s.Values[key] = v
}

// Setting implements port.SettingsReader.
func (s *Settings) Setting(_ context.Context, key string) (json.RawMessage, error) {
	spec, ok := domain.SettingSpecFor(key)
	if !ok {
		return nil, domain.Validation(domain.FieldError{Field: "key", Reason: "unknown setting " + key})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.Values[key]; ok {
		return slices.Clone(v), nil
	}
	return json.RawMessage(spec.Default), nil
}

// Credentials is a fake port.CredentialLookup and port.TeamDirectory.
type Credentials struct {
	Keys      map[[32]byte]port.APIKeyRecord
	Workloads map[[2]string]port.WorkloadRecord
	Teams     []port.TeamDirectoryEntry
}

// AuthenticateAPIKey implements port.CredentialLookup.
func (c *Credentials) AuthenticateAPIKey(_ context.Context, sum [32]byte) (port.APIKeyRecord, bool, error) {
	r, ok := c.Keys[sum]
	return r, ok, nil
}

// ResolveWorkload implements port.CredentialLookup.
func (c *Credentials) ResolveWorkload(_ context.Context, ns, sa string) (port.WorkloadRecord, bool, error) {
	r, ok := c.Workloads[[2]string{ns, sa}]
	return r, ok, nil
}

// ListTeamDirectory implements port.TeamDirectory.
func (c *Credentials) ListTeamDirectory(_ context.Context, s port.Session) ([]port.TeamDirectoryEntry, error) {
	if !s.ServiceAdmin && !s.SecurityOperator {
		return nil, domain.AccessDenied(string(authz.TeamsReadDirectory))
	}
	return slices.Clone(c.Teams), nil
}

var (
	_ port.TxRunner         = (*TxRunner)(nil)
	_ port.AuditWriter      = (*Audit)(nil)
	_ port.OutboxWriter     = (*Outbox)(nil)
	_ port.IdempotencyStore = (*Idempotency)(nil)
	_ port.Clock            = (*Clock)(nil)
	_ port.IDGen            = (*IDGen)(nil)
	_ port.SettingsReader   = (*Settings)(nil)
	_ port.CredentialLookup = (*Credentials)(nil)
	_ port.TeamDirectory    = (*Credentials)(nil)
)
