package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/logdest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// LogDestinationStatusKind is the resource_status kind of a log destination (written by the
// reconciler's log delivery controller, M2-I3).
const LogDestinationStatusKind = "log_destination"

// LogDestRepo implements logdest.Repository on afs.log_destinations. Every method runs in the
// transaction it is given, so row-level security applies with the caller's session.
type LogDestRepo struct{}

// NewLogDestRepo returns a LogDestRepo.
func NewLogDestRepo() *LogDestRepo { return &LogDestRepo{} }

var _ logdest.Repository = (*LogDestRepo)(nil)

const logDestSelect = `SELECT d.id, d.team_id, d.name, d.kind, d.config, coalesce(d.secret_id, ''), d.enabled, d.version,
  d.created_at, d.created_by, d.updated_at, rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at
FROM afs.log_destinations d
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + LogDestinationStatusKind + `' AND rs.resource_id = d.id`

func scanLogDestination(row pgx.Row) (domain.LogDestination, error) {
	var (
		d       domain.LogDestination
		kind    string
		cfg     []byte
		version int64
		phase   *string
		obs     *int64
		conds   []byte
		lastRec *time.Time
	)
	if err := row.Scan(&d.ID, &d.TeamID, &d.Name, &kind, &cfg, &d.SecretID, &d.Enabled, &version,
		&d.CreatedAt, &d.CreatedBy, &d.UpdatedAt, &phase, &obs, &conds, &lastRec); err != nil {
		return domain.LogDestination{}, err
	}
	d.Kind = domain.LogDestinationKind(kind)
	d.Version, d.Generation = domain.Version(version), domain.Generation(version)
	d.Config = map[string]string{}
	if err := json.Unmarshal(cfg, &d.Config); err != nil {
		return domain.LogDestination{}, err
	}
	d.Status = domain.PendingStatus(d.CreatedAt)
	if phase != nil {
		d.Status.Phase = domain.Phase(*phase)
		if obs != nil {
			d.Status.ObservedGeneration = *obs
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &d.Status.Conditions); err != nil {
				return domain.LogDestination{}, err
			}
		}
		if d.Status.Conditions == nil {
			d.Status.Conditions = []domain.Condition{}
		}
		if lastRec != nil {
			d.Status.UpdatedAt = lastRec.UTC()
		}
	}
	return d, nil
}

// List implements logdest.Repository.
func (*LogDestRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.LogDestination, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, logDestSelect+` WHERE d.team_id = $1 AND d.id > $2 ORDER BY d.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "log destination")
	}
	defer rows.Close()
	out := []domain.LogDestination{}
	for rows.Next() {
		d, err := scanLogDestination(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "log destination")
		}
		out = append(out, d)
	}
	return out, MapError(rows.Err(), OpRead, "log destination")
}

// Get implements logdest.Repository.
func (*LogDestRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.LogDestination, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.LogDestination{}, err
	}
	q := logDestSelect + ` WHERE d.team_id = $1 AND d.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF d`
	}
	d, err := scanLogDestination(pt.QueryRow(ctx, q, teamID, id))
	return d, MapError(err, OpRead, "log destination")
}

// SecretExists implements logdest.Repository. Only a team-shared secret qualifies: an agent-scoped
// secret may be bound to its one agent only (database contract), and the team's aggregator is no agent.
func (*LogDestRepo) SecretExists(ctx context.Context, tx port.Tx, teamID, secretID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var one int
	err = pt.QueryRow(ctx, `SELECT 1 FROM afs.secrets WHERE team_id = $1 AND id = $2 AND agent_id IS NULL`, teamID, secretID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, MapError(err, OpRead, "secret")
}

// Insert implements logdest.Repository.
func (*LogDestRepo) Insert(ctx context.Context, tx port.Tx, d domain.LogDestination) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	cfg, err := json.Marshal(d.Config)
	if err != nil {
		return domain.Internal(err)
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.log_destinations (id, team_id, name, kind, config, secret_id, enabled)
VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)`, d.ID, d.TeamID, d.Name, string(d.Kind), cfg, nullable(d.SecretID), d.Enabled)
	return MapError(err, OpWrite, "log destination")
}

// Update implements logdest.Repository.
func (r *LogDestRepo) Update(ctx context.Context, tx port.Tx, d domain.LogDestination, expect domain.Version) (domain.LogDestination, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.LogDestination{}, err
	}
	cfg, err := json.Marshal(d.Config)
	if err != nil {
		return domain.LogDestination{}, domain.Internal(err)
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.log_destinations SET name=$3, config=$4::jsonb, secret_id=$5, enabled=$6
WHERE team_id=$1 AND id=$2 AND version=$7`, d.TeamID, d.ID, d.Name, cfg, nullable(d.SecretID), d.Enabled, int64(expect))
	if err != nil {
		return domain.LogDestination{}, MapError(err, OpWrite, "log destination")
	}
	if tag.RowsAffected() == 0 {
		return domain.LogDestination{}, r.missOrStale(ctx, tx, d.TeamID, d.ID)
	}
	return r.Get(ctx, tx, d.TeamID, d.ID, false)
}

// Delete implements logdest.Repository.
func (r *LogDestRepo) Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.log_destinations WHERE team_id=$1 AND id=$2 AND version=$3`, teamID, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "log destination")
	}
	if tag.RowsAffected() == 0 {
		return r.missOrStale(ctx, tx, teamID, id)
	}
	// afs_app cannot touch resource_status: the reconciler removes the destination's status row when it
	// removes the output from the team's aggregator.
	return nil
}

// missOrStale tells a vanished row from a version that moved on.
func (r *LogDestRepo) missOrStale(ctx context.Context, tx port.Tx, teamID, id string) error {
	if _, err := r.Get(ctx, tx, teamID, id, false); err != nil {
		return err
	}
	return domain.PreconditionFailed()
}
