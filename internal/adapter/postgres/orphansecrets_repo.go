package postgres

import (
	"context"
	"encoding/json"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// OrphanSecretRows implements housekeeping.SecretRows for the afs_reconciler login (M2-C5).
type OrphanSecretRows struct {
	runner port.TxRunner
	audit  port.AuditWriter
}

// NewOrphanSecretRows returns the rows port of the orphan sweep.
func NewOrphanSecretRows(runner port.TxRunner, audit port.AuditWriter) *OrphanSecretRows {
	return &OrphanSecretRows{runner: runner, audit: audit}
}

var _ housekeeping.SecretRows = (*OrphanSecretRows)(nil)

// PathPrefix implements housekeeping.SecretRows: service setting secrets.path_prefix, default afs.
func (o *OrphanSecretRows) PathPrefix(ctx context.Context) (string, error) {
	prefix := "afs"
	err := o.runner.ReadTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var raw []byte
		err = t.QueryRow(ctx, `SELECT coalesce((SELECT value FROM afs.service_settings WHERE key = 'secrets.path_prefix'), 'null'::jsonb)`).Scan(&raw)
		if err != nil {
			return err
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			prefix = s
		}
		return nil
	})
	return prefix, MapError(err, OpRead, "service settings")
}

// KnownARNs implements housekeeping.SecretRows.
func (o *OrphanSecretRows) KnownARNs(ctx context.Context, arns []string) (map[string]bool, error) {
	known := map[string]bool{}
	err := o.runner.ReadTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT secrets_manager_arn FROM afs.secrets WHERE secrets_manager_arn = ANY($1)`, arns)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var arn string
			if err := rows.Scan(&arn); err != nil {
				return err
			}
			known[arn] = true
		}
		return rows.Err()
	})
	return known, MapError(err, OpRead, "secrets")
}

// RecordSwept implements housekeeping.SecretRows: an audit record of actor system:afs-reconciler.
func (o *OrphanSecretRows) RecordSwept(ctx context.Context, arn, name, reason string) error {
	return o.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		return o.audit.Write(ctx, tx, port.AuditRecord{Action: "SweepOrphanSecret", Outcome: port.OutcomeAllowed,
			ResourceKind: "secrets", ResourceID: arn, After: map[string]any{"name": name, "reason": reason,
				"recoveryDays": housekeeping.OrphanRecoveryDays}})
	})
}
