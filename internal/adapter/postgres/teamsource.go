package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// TeamSource implements team.Source for the reconciler.
type TeamSource struct{ runner *TxRunner }

// NewTeamSource returns a TeamSource on the afs_reconciler pool's TxRunner.
func NewTeamSource(runner *TxRunner) *TeamSource { return &TeamSource{runner: runner} }

var _ team.Source = (*TeamSource)(nil)

const loadTeamSQL = `SELECT id, slug, namespace, display_name, kind, state, generation, quotas, deleted_at
  FROM afs.teams WHERE id = $1`

// Only models enabled for the team and in the catalog get an inference profile.
const loadModelsSQL = `SELECT tm.model_id, coalesce(tm.inference_profile_arn, '')
  FROM afs.team_models tm JOIN afs.models m ON m.id = tm.model_id
 WHERE tm.team_id = $1 AND m.enabled ORDER BY tm.model_id`

// The team's tags beyond the system ones. Required keys with no value are listed with a NULL value and
// are left out (they are reported as missing, not applied).
const loadTeamTagsSQL = `SELECT key, value FROM afs.team_effective_tags
 WHERE team_id = $1 AND source <> 'system' AND value IS NOT NULL ORDER BY key`

// LoadTeam implements team.Source.
func (s *TeamSource) LoadTeam(ctx context.Context, id string) (team.Desired, error) {
	var d team.Desired
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var quotas []byte
		var deleted *time.Time
		err = t.QueryRow(ctx, loadTeamSQL, id).Scan(&d.ID, &d.Slug, &d.Namespace, &d.DisplayName, &d.Kind, &d.State, &d.Generation, &quotas, &deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("team")
		}
		if err != nil {
			return MapError(err, OpRead, "team")
		}
		d.DeletedAt = deleted
		if d.Quotas, err = ParseQuotas(quotas); err != nil {
			return fmt.Errorf("postgres: quotas of %s: %w", id, err)
		}
		if d.Tags, err = loadTags(ctx, t, loadTeamTagsSQL, id); err != nil {
			return err
		}
		rows, err := t.Query(ctx, loadModelsSQL, id)
		if err != nil {
			return MapError(err, OpRead, "team models")
		}
		defer rows.Close()
		d.Models = nil
		for rows.Next() {
			var m team.Model
			if err := rows.Scan(&m.ID, &m.ProfileARN); err != nil {
				return MapError(err, OpRead, "team models")
			}
			d.Models = append(d.Models, m)
		}
		return rows.Err()
	})
	return d, err
}

// SetInferenceProfileARN implements team.Source. The reconciler may update only this column of
// team_models (column grant, database contract §4).
func (s *TeamSource) SetInferenceProfileARN(ctx context.Context, teamID, modelID, arn string) error {
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		ct, err := t.Exec(ctx, `UPDATE afs.team_models SET inference_profile_arn = $3 WHERE team_id = $1 AND model_id = $2`, teamID, modelID, arn)
		if err != nil {
			return MapError(err, OpWrite, "team model")
		}
		if ct.RowsAffected() == 0 {
			return domain.NotFound("team model")
		}
		return nil
	})
}

// ParseQuotas reads teams.quotas ({"cpu":"32","memory":"128Gi","pods":100,"storage":"1Ti"}); values may
// be JSON strings or numbers.
func ParseQuotas(raw []byte) (team.Quotas, error) {
	var q team.Quotas
	if len(raw) == 0 {
		return q, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return q, err
	}
	str := func(key string) (string, error) {
		v, ok := m[key]
		if !ok || string(v) == "null" {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s, nil
		}
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return "", fmt.Errorf("quota %s must be a string or a number", key)
		}
		return n.String(), nil
	}
	var err error
	if q.CPU, err = str("cpu"); err != nil {
		return q, err
	}
	if q.Memory, err = str("memory"); err != nil {
		return q, err
	}
	if q.Storage, err = str("storage"); err != nil {
		return q, err
	}
	pods, err := str("pods")
	if err != nil {
		return q, err
	}
	if pods != "" {
		if q.Pods, err = strconv.ParseInt(pods, 10, 64); err != nil {
			return q, fmt.Errorf("quota pods: %w", err)
		}
	}
	return q, nil
}

// loadTags reads (key, value) rows into a map.
func loadTags(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sql string, args ...any) (map[string]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, MapError(err, OpRead, "tags")
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, MapError(err, OpRead, "tags")
		}
		out[k] = v
	}
	return out, rows.Err()
}
