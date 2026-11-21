package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/audit"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/revisions"
)

// AuditRepo implements audit.Repository: a read-only view of audit_log.
type AuditRepo struct{}

// NewAuditRepo returns an AuditRepo.
func NewAuditRepo() *AuditRepo { return &AuditRepo{} }

var (
	_ audit.Repository     = (*AuditRepo)(nil)
	_ revisions.Repository = (*RevisionRepo)(nil)
)

// List implements audit.Repository.
func (*AuditRepo) List(ctx context.Context, tx port.Tx, f audit.Filter, before int64, limit int) ([]audit.Event, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, occurred_at, actor_type, actor_id, action, outcome, coalesce(resource_kind,''),
  coalesce(resource_id,''), coalesce(team_id,''), coalesce(request_id,''), coalesce(host(source_ip),''),
  coalesce(user_agent,''), before, after
FROM afs.audit_log
WHERE ($1::bigint = 0 OR id < $1)
  AND ($2::timestamptz IS NULL OR occurred_at >= $2) AND ($3::timestamptz IS NULL OR occurred_at < $3)
  AND ($4 = '' OR actor_id = $4) AND ($5 = '' OR action = $5) AND ($6 = '' OR outcome = $6)
  AND ($7 = '' OR resource_kind = $7) AND ($8 = '' OR resource_id = $8) AND ($9 = '' OR team_id = $9)
ORDER BY id DESC LIMIT $10`, before, f.From, f.To, f.ActorID, f.Action, f.Outcome, f.ResourceKind, f.ResourceID, f.TeamID, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "audit event")
	}
	defer rows.Close()
	out := []audit.Event{}
	for rows.Next() {
		var e audit.Event
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.ActorType, &e.ActorID, &e.Action, &e.Outcome, &e.ResourceKind,
			&e.ResourceID, &e.TeamID, &e.RequestID, &e.SourceIP, &e.UserAgent, &e.Before, &e.After); err != nil {
			return nil, MapError(err, OpRead, "audit event")
		}
		out = append(out, e)
	}
	return out, MapError(rows.Err(), OpRead, "audit event")
}

// TeamExists implements audit.Repository.
func (*AuditRepo) TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var one int
	err = pt.QueryRow(ctx, `SELECT 1 FROM afs.teams WHERE id=$1`, teamID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, MapError(err, OpRead, "team")
}

// RevisionRepo implements revisions.Repository: a read-only view of config_revisions.
type RevisionRepo struct{}

// NewRevisionRepo returns a RevisionRepo.
func NewRevisionRepo() *RevisionRepo { return &RevisionRepo{} }

// TeamExists implements revisions.Repository.
func (*RevisionRepo) TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	return (&AuditRepo{}).TeamExists(ctx, tx, teamID)
}

// List implements revisions.Repository.
func (*RevisionRepo) List(ctx context.Context, tx port.Tx, teamID string, f revisions.Filter, before int64, limit int) ([]revisions.Revision, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, resource_kind, resource_id, operation, version, changed_by, changed_at
FROM afs.config_revisions WHERE team_id=$1 AND ($2::bigint = 0 OR id < $2) AND ($3 = '' OR resource_kind = $3)
  AND ($4 = '' OR resource_id = $4) ORDER BY id DESC LIMIT $5`, teamID, before, f.ResourceKind, f.ResourceID, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "revision")
	}
	defer rows.Close()
	out := []revisions.Revision{}
	for rows.Next() {
		var r revisions.Revision
		if err := rows.Scan(&r.Revision, &r.ResourceKind, &r.ResourceID, &r.Operation, &r.Version, &r.ChangedBy, &r.ChangedAt); err != nil {
			return nil, MapError(err, OpRead, "revision")
		}
		out = append(out, r)
	}
	return out, MapError(rows.Err(), OpRead, "revision")
}

// Get implements revisions.Repository.
func (*RevisionRepo) Get(ctx context.Context, tx port.Tx, teamID string, rev int64) (revisions.Revision, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return revisions.Revision{}, err
	}
	var r revisions.Revision
	err = pt.QueryRow(ctx, `SELECT id, resource_kind, resource_id, operation, version, changed_by, changed_at, snapshot
FROM afs.config_revisions WHERE team_id=$1 AND id=$2`, teamID, rev).
		Scan(&r.Revision, &r.ResourceKind, &r.ResourceID, &r.Operation, &r.Version, &r.ChangedBy, &r.ChangedAt, &r.Snapshot)
	return r, MapError(err, OpRead, "revision")
}
