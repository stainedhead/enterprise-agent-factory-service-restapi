package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

// TagSyncSource implements tagsync.Source on the afs_reconciler pool (B1, M6-X2).
type TagSyncSource struct{ runner *TxRunner }

// NewTagSyncSource returns a TagSyncSource.
func NewTagSyncSource(runner *TxRunner) *TagSyncSource { return &TagSyncSource{runner: runner} }

var _ tagsync.Source = (*TagSyncSource)(nil)

const (
	loadTagSyncTeamSQL = `SELECT id, slug, state FROM afs.teams WHERE id = $1`
	// The owner names (afs-agent-name, afs-pool-name) are tags too, so the secret rows carry them.
	loadTagSyncSecretsSQL = `SELECT s.secrets_manager_arn, coalesce(s.agent_id, ''), coalesce(a.name, ''), coalesce(s.pool_id, ''), coalesce(p.name, '')
FROM afs.secrets s
LEFT JOIN afs.agents a ON a.team_id = s.team_id AND a.id = s.agent_id
LEFT JOIN afs.agent_pools p ON p.team_id = s.team_id AND p.id = s.pool_id
WHERE s.team_id = $1 ORDER BY s.secrets_manager_arn`
	loadTagSyncPoolsSQL = `SELECT id, name FROM afs.agent_pools WHERE team_id = $1 AND state IN ('enabled', 'paused') ORDER BY id`
)

// LoadTagSync implements tagsync.Source.
func (s *TagSyncSource) LoadTagSync(ctx context.Context, teamID string) (tagsync.Desired, error) {
	var d tagsync.Desired
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, loadTagSyncTeamSQL, teamID).Scan(&d.TeamID, &d.Slug, &d.State)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("team")
		}
		if err != nil {
			return MapError(err, OpRead, "team")
		}
		if d.Tags, err = loadTags(ctx, t, loadTeamTagsSQL, teamID); err != nil {
			return err
		}
		rows, err := t.Query(ctx, loadTagSyncSecretsSQL, teamID)
		if err != nil {
			return MapError(err, OpRead, "secrets")
		}
		for rows.Next() {
			var sec tagsync.Secret
			if err := rows.Scan(&sec.ARN, &sec.AgentID, &sec.AgentName, &sec.PoolID, &sec.PoolName); err != nil {
				rows.Close()
				return MapError(err, OpRead, "secrets")
			}
			d.Secrets = append(d.Secrets, sec)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return MapError(err, OpRead, "secrets")
		}
		rows, err = t.Query(ctx, loadTagSyncPoolsSQL, teamID)
		if err != nil {
			return MapError(err, OpRead, "pools")
		}
		defer rows.Close()
		for rows.Next() {
			var p tagsync.Pool
			if err := rows.Scan(&p.ID, &p.Name); err != nil {
				return MapError(err, OpRead, "pools")
			}
			d.Pools = append(d.Pools, p)
		}
		return MapError(rows.Err(), OpRead, "pools")
	})
	return d, err
}
