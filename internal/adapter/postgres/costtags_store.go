package postgres

import (
	"context"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// CostTagsStore implements costtags.Store for afs-billing's login: it reads service_tags, team_tags,
// cost_daily and tag_sets, and upserts cost_allocation_tag_status (database contract §3).
type CostTagsStore struct{}

// NewCostTagsStore returns a CostTagsStore.
func NewCostTagsStore() *CostTagsStore { return &CostTagsStore{} }

var _ costtags.Store = (*CostTagsStore)(nil)

// Keys implements costtags.Store: the system keys, the service tags and the teams' tags.
func (*CostTagsStore) Keys(ctx context.Context, tx port.Tx) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT key FROM afs.service_tags UNION SELECT key FROM afs.team_tags`)
	if err != nil {
		return nil, MapError(err, OpRead, "tag keys")
	}
	defer rows.Close()
	keys := append([]string{}, costtags.SystemKeys...)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, MapError(err, OpRead, "tag keys")
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, MapError(err, OpRead, "tag keys")
	}
	sort.Strings(keys)
	return keys, nil
}

// SeenInCUR implements costtags.Store. A key is seen on a day when a CUR cost_daily row of that day was
// billed with it: it is in the row's tag set, or (for the system keys, which the row's columns carry) the
// row has the agent id, the pool id or, for afs-team-id, any CUR row at all, since the team is attributed
// by that tag.
func (*CostTagsStore) SeenInCUR(ctx context.Context, tx port.Tx, keys []string) (map[string]costtags.Seen, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `
WITH cur AS (
  SELECT c.day, c.agent_id, c.pool_id, c.tag_set_id FROM afs.cost_daily c WHERE c.source IN ('cur_bedrock', 'cur_eks', 'cur_other')
), seen AS (
  SELECT k.key, c.day FROM cur c JOIN afs.tag_sets ts ON ts.id = c.tag_set_id, jsonb_object_keys(ts.tags) AS k(key)
  UNION ALL SELECT 'afs-team-id', day FROM cur
  UNION ALL SELECT 'afs-agent-id', day FROM cur WHERE agent_id IS NOT NULL
  UNION ALL SELECT 'afs-pool-id', day FROM cur WHERE pool_id IS NOT NULL
)
SELECT key, min(day)::timestamptz AT TIME ZONE 'UTC', max(day)::timestamptz AT TIME ZONE 'UTC'
FROM seen WHERE key = ANY($1) GROUP BY key`, keys)
	if err != nil {
		return nil, MapError(err, OpRead, "cost_daily")
	}
	defer rows.Close()
	out := map[string]costtags.Seen{}
	for rows.Next() {
		var k string
		var first, last time.Time
		if err := rows.Scan(&k, &first, &last); err != nil {
			return nil, MapError(err, OpRead, "cost_daily")
		}
		out[k] = costtags.Seen{First: first.UTC(), Last: last.UTC()}
	}
	return out, MapError(rows.Err(), OpRead, "cost_daily")
}

// Stored implements costtags.Store.
func (*CostTagsStore) Stored(ctx context.Context, tx port.Tx) (map[string]costtags.Stored, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT key, first_seen_at, last_seen_at, aws_status, checked_at FROM afs.cost_allocation_tag_status`)
	if err != nil {
		return nil, MapError(err, OpRead, "cost tag status")
	}
	defer rows.Close()
	out := map[string]costtags.Stored{}
	for rows.Next() {
		var k string
		var s costtags.Stored
		if err := rows.Scan(&k, &s.First, &s.Last, &s.AWSStatus, &s.CheckedAt); err != nil {
			return nil, MapError(err, OpRead, "cost tag status")
		}
		out[k] = s
	}
	return out, MapError(rows.Err(), OpRead, "cost tag status")
}

// Save implements costtags.Store.
func (*CostTagsStore) Save(ctx context.Context, tx port.Tx, rows []costtags.Row) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := pt.Exec(ctx, `INSERT INTO afs.cost_allocation_tag_status (key, seen_in_cur, first_seen_at, last_seen_at, aws_status, checked_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (key) DO UPDATE SET seen_in_cur = EXCLUDED.seen_in_cur, first_seen_at = EXCLUDED.first_seen_at,
  last_seen_at = EXCLUDED.last_seen_at, aws_status = EXCLUDED.aws_status, checked_at = EXCLUDED.checked_at`,
			r.Key, r.First != nil, r.First, r.Last, r.AWSStatus, r.CheckedAt); err != nil {
			return MapError(err, OpWrite, "cost tag status")
		}
	}
	return nil
}
