package postgres

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costtags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// CostTagsRepo implements costtags.Repository for the afs_app login: row-level security shows
// cost_allocation_tag_status to service admins only, so any other caller reads no rows (the use case
// refuses them first).
type CostTagsRepo struct{}

// NewCostTagsRepo returns a CostTagsRepo.
func NewCostTagsRepo() *CostTagsRepo { return &CostTagsRepo{} }

var _ costtags.Repository = (*CostTagsRepo)(nil)

// List implements costtags.Repository.
func (*CostTagsRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]costtags.Status, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT key, seen_in_cur, first_seen_at, last_seen_at, aws_status, checked_at
FROM afs.cost_allocation_tag_status WHERE key > $1 ORDER BY key LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "cost tag status")
	}
	defer rows.Close()
	out := []costtags.Status{}
	for rows.Next() {
		var s costtags.Status
		if err := rows.Scan(&s.Key, &s.SeenInCUR, &s.FirstSeenAt, &s.LastSeenAt, &s.AWSStatus, &s.CheckedAt); err != nil {
			return nil, MapError(err, OpRead, "cost tag status")
		}
		out = append(out, s)
	}
	return out, MapError(rows.Err(), OpRead, "cost tag status")
}
