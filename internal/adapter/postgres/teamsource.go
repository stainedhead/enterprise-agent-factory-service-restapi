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

// Only models callable by one of the team's live agents or pools get an inference profile: the service,
// team, subject and price levels all pass (afs.effective_agent_models, 000022). The profile state tells a
// row that is being (re)created from an active one.
const loadModelsSQL = `SELECT m.id, coalesce(p.inference_profile_arn, ''), coalesce(p.state <> 'active', false)
  FROM afs.models m
  LEFT JOIN afs.team_inference_profiles p ON p.team_id = $1 AND p.model_id = m.id
 WHERE EXISTS (SELECT 1 FROM afs.effective_agent_models e WHERE e.team_id = $1 AND e.model_id = m.id)
 ORDER BY m.id`

// Profile rows of models that are no longer callable: they keep their profile for the grace period.
const loadRetiringSQL = `SELECT p.model_id, coalesce(p.inference_profile_arn, ''), p.retire_after
  FROM afs.team_inference_profiles p
 WHERE p.team_id = $1
   AND NOT EXISTS (SELECT 1 FROM afs.effective_agent_models e WHERE e.team_id = $1 AND e.model_id = p.model_id)
 ORDER BY p.model_id`

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
			if err := rows.Scan(&m.ID, &m.ProfileARN, &m.Inactive); err != nil {
				return MapError(err, OpRead, "team models")
			}
			d.Models = append(d.Models, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		ret, err := t.Query(ctx, loadRetiringSQL, id)
		if err != nil {
			return MapError(err, OpRead, "retiring team models")
		}
		defer ret.Close()
		for ret.Next() {
			var r team.RetiringModel
			var after *time.Time
			if err := ret.Scan(&r.ID, &r.ProfileARN, &after); err != nil {
				return MapError(err, OpRead, "retiring team models")
			}
			if after != nil {
				r.RetireAfter = *after
			}
			d.Retiring = append(d.Retiring, r)
		}
		return ret.Err()
	})
	return d, err
}

// SetInferenceProfileARN implements team.Source: the team's profile of the model is active with this ARN
// (team_inference_profiles; the reconciler owns the table, and a trigger keeps the 1.0.1 column in step).
func (s *TeamSource) SetInferenceProfileARN(ctx context.Context, teamID, modelID, arn string) error {
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `INSERT INTO afs.team_inference_profiles (team_id, model_id, inference_profile_arn, state) VALUES ($1, $2, $3, 'active')
ON CONFLICT (team_id, model_id) DO UPDATE SET inference_profile_arn = EXCLUDED.inference_profile_arn, state = 'active', retire_after = NULL, updated_at = now()`,
			teamID, modelID, arn); err != nil {
			return MapError(err, OpWrite, "team inference profile")
		}
		return nil
	})
}

// RetireProfile implements team.Source.
func (s *TeamSource) RetireProfile(ctx context.Context, teamID, modelID string, after time.Time) error {
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		ct, err := t.Exec(ctx, `UPDATE afs.team_inference_profiles SET state = 'retiring', retire_after = $3, updated_at = now() WHERE team_id = $1 AND model_id = $2`, teamID, modelID, after)
		if err != nil {
			return MapError(err, OpWrite, "team inference profile")
		}
		if ct.RowsAffected() == 0 {
			return domain.NotFound("team inference profile")
		}
		return nil
	})
}

// DropProfile implements team.Source.
func (s *TeamSource) DropProfile(ctx context.Context, teamID, modelID string) error {
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `DELETE FROM afs.team_inference_profiles WHERE team_id = $1 AND model_id = $2`, teamID, modelID); err != nil {
			return MapError(err, OpWrite, "team inference profile")
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
