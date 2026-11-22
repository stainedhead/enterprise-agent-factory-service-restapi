package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reportschedules"
)

// ReportSchedulesRepo implements reportschedules.Repository (the API) and schedules.Store (the schedule
// runner of afs-billing) on afs.report_schedules. Every method runs in the transaction it is given, so
// row-level security applies with that transaction's session: the caller's for the API, the billing
// role's for the runner.
type ReportSchedulesRepo struct{}

// NewReportSchedulesRepo returns a ReportSchedulesRepo.
func NewReportSchedulesRepo() *ReportSchedulesRepo { return &ReportSchedulesRepo{} }

var (
	_ reportschedules.Repository = (*ReportSchedulesRepo)(nil)
	_ schedules.Store            = (*ReportSchedulesRepo)(nil)
)

const scheduleSelect = `SELECT s.id, s.team_id, s.name, s.report, s.query, s.cadence, s.channel_id, s.format, s.enabled, s.next_run_at,
  s.last_run_at, coalesce(s.last_status, ''), coalesce(s.last_error, ''), s.version, s.created_at, s.created_by, s.updated_at
FROM afs.report_schedules s`

func scanSchedule(row pgx.Row) (domain.ReportSchedule, error) {
	var (
		s       domain.ReportSchedule
		report  string
		cadence string
		query   []byte
		version int64
	)
	if err := row.Scan(&s.ID, &s.TeamID, &s.Name, &report, &query, &cadence, &s.ChannelID, &s.Format, &s.Enabled, &s.NextRunAt,
		&s.LastRunAt, &s.LastStatus, &s.LastError, &version, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt); err != nil {
		return domain.ReportSchedule{}, err
	}
	s.Report, s.Cadence, s.Version = domain.ReportKind(report), domain.Cadence(cadence), domain.Version(version)
	if err := json.Unmarshal(query, &s.Query); err != nil {
		return domain.ReportSchedule{}, err
	}
	s.NextRunAt, s.CreatedAt, s.UpdatedAt = s.NextRunAt.UTC(), s.CreatedAt.UTC(), s.UpdatedAt.UTC()
	if s.LastRunAt != nil {
		t := s.LastRunAt.UTC()
		s.LastRunAt = &t
	}
	return s, nil
}

// TeamExists implements reportschedules.Repository.
func (*ReportSchedulesRepo) TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var one int
	err = pt.QueryRow(ctx, `SELECT 1 FROM afs.teams WHERE id = $1`, teamID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, MapError(err, OpRead, "team")
}

// List implements reportschedules.Repository.
func (*ReportSchedulesRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.ReportSchedule, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, scheduleSelect+` WHERE s.team_id = $1 AND s.id > $2 ORDER BY s.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "report schedule")
	}
	defer rows.Close()
	out := []domain.ReportSchedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "report schedule")
		}
		out = append(out, s)
	}
	return out, MapError(rows.Err(), OpRead, "report schedule")
}

// Get implements reportschedules.Repository.
func (*ReportSchedulesRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.ReportSchedule, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ReportSchedule{}, err
	}
	q := scheduleSelect + ` WHERE s.team_id = $1 AND s.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF s`
	}
	s, err := scanSchedule(pt.QueryRow(ctx, q, teamID, id))
	return s, MapError(err, OpRead, "report schedule")
}

// ChannelOwnedByTeam implements reportschedules.Repository: a team channel of that team. A service-level
// channel has no team, and row-level security hides another team's channel from the caller.
func (*ReportSchedulesRepo) ChannelOwnedByTeam(ctx context.Context, tx port.Tx, teamID, channelID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var one int
	err = pt.QueryRow(ctx, `SELECT 1 FROM afs.notification_channels WHERE id = $1 AND team_id = $2`, channelID, teamID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, MapError(err, OpRead, "notification channel")
}

// Insert implements reportschedules.Repository.
func (*ReportSchedulesRepo) Insert(ctx context.Context, tx port.Tx, s domain.ReportSchedule) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	q, err := json.Marshal(s.Query)
	if err != nil {
		return domain.Internal(err)
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.report_schedules (id, team_id, name, report, query, cadence, channel_id, format, enabled, next_run_at)
VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, $10)`, s.ID, s.TeamID, s.Name, string(s.Report), q, string(s.Cadence), s.ChannelID, s.Format, s.Enabled, s.NextRunAt)
	return MapError(err, OpWrite, "report schedule")
}

// Update implements reportschedules.Repository.
func (r *ReportSchedulesRepo) Update(ctx context.Context, tx port.Tx, s domain.ReportSchedule, expect domain.Version) (domain.ReportSchedule, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ReportSchedule{}, err
	}
	q, err := json.Marshal(s.Query)
	if err != nil {
		return domain.ReportSchedule{}, domain.Internal(err)
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.report_schedules SET name=$3, query=$4::jsonb, cadence=$5, channel_id=$6, enabled=$7, next_run_at=$8
WHERE team_id=$1 AND id=$2 AND version=$9`, s.TeamID, s.ID, s.Name, q, string(s.Cadence), s.ChannelID, s.Enabled, s.NextRunAt, int64(expect))
	if err != nil {
		return domain.ReportSchedule{}, MapError(err, OpWrite, "report schedule")
	}
	if tag.RowsAffected() == 0 {
		return domain.ReportSchedule{}, r.missOrStale(ctx, tx, s.TeamID, s.ID)
	}
	return r.Get(ctx, tx, s.TeamID, s.ID, false)
}

// Delete implements reportschedules.Repository.
func (r *ReportSchedulesRepo) Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.report_schedules WHERE team_id=$1 AND id=$2 AND version=$3`, teamID, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "report schedule")
	}
	if tag.RowsAffected() == 0 {
		return r.missOrStale(ctx, tx, teamID, id)
	}
	return nil
}

func (r *ReportSchedulesRepo) missOrStale(ctx context.Context, tx port.Tx, teamID, id string) error {
	if _, err := r.Get(ctx, tx, teamID, id, false); err != nil {
		return err
	}
	return domain.PreconditionFailed()
}

// DueIDs implements schedules.Store.
func (*ReportSchedulesRepo) DueIDs(ctx context.Context, tx port.Tx, now time.Time, limit int) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id FROM afs.report_schedules WHERE enabled AND next_run_at <= $1 ORDER BY next_run_at, id LIMIT $2`, now, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "report schedule")
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "report schedule")
		}
		ids = append(ids, id)
	}
	return ids, MapError(rows.Err(), OpRead, "report schedule")
}

// Claim implements schedules.Store: the row is locked until the transaction ends, and a row another
// runner holds is skipped.
func (*ReportSchedulesRepo) Claim(ctx context.Context, tx port.Tx, id string, now time.Time) (domain.ReportSchedule, bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ReportSchedule{}, false, err
	}
	s, err := scanSchedule(pt.QueryRow(ctx, scheduleSelect+` WHERE s.id = $1 AND s.enabled AND s.next_run_at <= $2 FOR UPDATE OF s SKIP LOCKED`, id, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ReportSchedule{}, false, nil
	}
	if err != nil {
		return domain.ReportSchedule{}, false, MapError(err, OpRead, "report schedule")
	}
	return s, true, nil
}

// Record implements schedules.Store. Only the four columns afs_billing may update are written.
func (*ReportSchedulesRepo) Record(ctx context.Context, tx port.Tx, id string, o schedules.Outcome) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	var next *time.Time
	if o.NextRunAt != nil {
		t := o.NextRunAt.UTC()
		next = &t
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.report_schedules SET last_run_at=$2, last_status=$3, last_error=nullif($4, ''), next_run_at=coalesce($5, next_run_at) WHERE id=$1`,
		id, o.RunAt.UTC(), o.Status, schedules.Truncate(o.Error), next)
	if err != nil {
		return MapError(err, OpWrite, "report schedule")
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("report schedule")
	}
	return nil
}
