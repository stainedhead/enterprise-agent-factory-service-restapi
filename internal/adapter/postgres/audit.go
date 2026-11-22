package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// insertAuditSQL never uses RETURNING: afs_app may insert audit rows outside its team scope
// (policy app_own_audit, 000016) but not read them back.
const insertAuditSQL = `INSERT INTO afs.audit_log
  (actor_type, actor_id, action, outcome, resource_kind, resource_id, team_id, request_id,
   source_ip, user_agent, before, after, pool_id, worker_pod)
VALUES ($1, $2, $3, $4, nullif($5, ''), nullif($6, ''), nullif($7, ''), nullif($8, ''),
   $9, nullif($10, ''), $11, $12, nullif($13, ''), nullif($14, ''))`

// AuditWriter implements port.AuditWriter.
type AuditWriter struct {
	runner *TxRunner
}

// NewAuditWriter returns an AuditWriter; runner opens the own transaction of WriteDenied.
func NewAuditWriter(runner *TxRunner) *AuditWriter { return &AuditWriter{runner: runner} }

// Write implements port.AuditWriter.
func (w *AuditWriter) Write(ctx context.Context, tx port.Tx, rec port.AuditRecord) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	args, err := auditArgs(t.session, rec)
	if err != nil {
		return err
	}
	if _, err := t.Exec(ctx, insertAuditSQL, args...); err != nil {
		return MapError(err, OpWrite, "audit record")
	}
	return nil
}

// WriteDenied implements port.AuditWriter.
func (w *AuditWriter) WriteDenied(ctx context.Context, s port.Session, rec port.AuditRecord) error {
	rec.Outcome = port.OutcomeDenied
	return w.runner.InTx(ctx, s, func(ctx context.Context, tx port.Tx) error {
		return w.Write(ctx, tx, rec)
	})
}

func auditArgs(s port.Session, rec port.AuditRecord) ([]any, error) {
	actorType := s.ActorType
	if actorType == "" {
		return nil, fmt.Errorf("postgres: audit record without actor type")
	}
	if rec.Outcome == "" || rec.Action == "" {
		return nil, fmt.Errorf("postgres: audit record needs action and outcome")
	}
	var ip any
	if rec.SourceIP != "" {
		a, err := netip.ParseAddr(rec.SourceIP)
		if err != nil {
			return nil, fmt.Errorf("postgres: audit source ip: %w", err)
		}
		ip = a
	}
	before, err := redactedJSON(rec.ResourceKind, rec.Before)
	if err != nil {
		return nil, err
	}
	after, err := redactedJSON(rec.ResourceKind, rec.After)
	if err != nil {
		return nil, err
	}
	// A pool's rows carry the pool and the worker pod of its session; an explicit record wins.
	if rec.PoolID == "" {
		rec.PoolID, rec.WorkerPod = s.PoolID, s.WorkerPod
	}
	return []any{string(actorType), s.Actor, rec.Action, string(rec.Outcome), rec.ResourceKind,
		rec.ResourceID, rec.TeamID, s.RequestID, ip, rec.UserAgent, before, after, rec.PoolID, rec.WorkerPod}, nil
}

// secretKeys are member names whose values are never written to audit payloads.
var secretKeys = map[string]bool{
	"secretkey": true, "secretvalue": true, "secret_string": true, "secretstring": true,
	"password": true, "plaintext": true, "token": true, "accesstoken": true, "refreshtoken": true,
	"idtoken": true, "clientsecret": true, "apikey": true, "authorization": true, "privatekey": true,
}

// redactedJSON encodes v for an audit payload with secret-looking members removed. For secrets
// (resource kind "secrets") the "value" member is removed too.
func redactedJSON(kind string, v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("postgres: audit payload: %w", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return json.Marshal(redact(doc, kind == "secrets"))
}

func redact(v any, dropValue bool) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			lk := strings.ToLower(k)
			if secretKeys[lk] || (dropValue && lk == "value") {
				delete(t, k)
				continue
			}
			t[k] = redact(x, dropValue)
		}
	case []any:
		for i, x := range t {
			t[i] = redact(x, dropValue)
		}
	}
	return v
}

var _ port.AuditWriter = (*AuditWriter)(nil)
