package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/notify"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ChannelRepo implements notify.Repository on afs.notification_channels. teamID "" addresses the
// service-level channels (team_id NULL). Every method runs in the transaction it is given, so
// row-level security applies with the caller's session.
type ChannelRepo struct{}

// NewChannelRepo returns a ChannelRepo.
func NewChannelRepo() *ChannelRepo { return &ChannelRepo{} }

var _ notify.Repository = (*ChannelRepo)(nil)

const channelSelect = `SELECT c.id, coalesce(c.team_id, ''), c.name, c.kind, c.config, coalesce(c.secret_id, ''), c.event_types, c.enabled, c.version,
  c.created_at, c.created_by, c.updated_at, rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at
FROM afs.notification_channels c
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + domain.ChannelStatusKind + `' AND rs.resource_id = c.id`

const channelScope = `c.team_id IS NOT DISTINCT FROM nullif($1, '')`

func scanChannel(row pgx.Row) (domain.NotificationChannel, error) {
	var (
		c       domain.NotificationChannel
		kind    string
		cfg     []byte
		version int64
		phase   *string
		obs     *int64
		conds   []byte
		lastRec *time.Time
	)
	if err := row.Scan(&c.ID, &c.TeamID, &c.Name, &kind, &cfg, &c.SecretID, &c.EventTypes, &c.Enabled, &version,
		&c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &phase, &obs, &conds, &lastRec); err != nil {
		return domain.NotificationChannel{}, err
	}
	c.Kind = domain.ChannelKind(kind)
	c.Version, c.Generation = domain.Version(version), domain.Generation(version)
	c.Config = map[string]any{}
	if err := json.Unmarshal(cfg, &c.Config); err != nil {
		return domain.NotificationChannel{}, err
	}
	if c.EventTypes == nil {
		c.EventTypes = []string{}
	}
	c.Status = domain.PendingStatus(c.CreatedAt)
	if phase != nil {
		c.Status.Phase = domain.Phase(*phase)
		if obs != nil {
			c.Status.ObservedGeneration = *obs
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &c.Status.Conditions); err != nil {
				return domain.NotificationChannel{}, err
			}
		}
		if c.Status.Conditions == nil {
			c.Status.Conditions = []domain.Condition{}
		}
		if lastRec != nil {
			c.Status.UpdatedAt = lastRec.UTC()
		}
	}
	return c, nil
}

// List implements notify.Repository.
func (*ChannelRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.NotificationChannel, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, channelSelect+` WHERE `+channelScope+` AND c.id > $2 ORDER BY c.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "notification channel")
	}
	defer rows.Close()
	out := []domain.NotificationChannel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "notification channel")
		}
		out = append(out, c)
	}
	return out, MapError(rows.Err(), OpRead, "notification channel")
}

// Get implements notify.Repository.
func (*ChannelRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.NotificationChannel, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	q := channelSelect + ` WHERE ` + channelScope + ` AND c.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF c`
	}
	c, err := scanChannel(pt.QueryRow(ctx, q, teamID, id))
	return c, MapError(err, OpRead, "notification channel")
}

// SecretExists implements notify.Repository. Only a team-shared secret qualifies: an agent-scoped
// secret may be bound to its one agent only (database contract).
func (*ChannelRepo) SecretExists(ctx context.Context, tx port.Tx, teamID, secretID string) (bool, error) {
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

// Insert implements notify.Repository.
func (*ChannelRepo) Insert(ctx context.Context, tx port.Tx, c domain.NotificationChannel) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return domain.Internal(err)
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.notification_channels (id, team_id, name, kind, config, secret_id, event_types, enabled)
VALUES ($1, nullif($2, ''), $3, $4, $5::jsonb, $6, $7, $8)`, c.ID, c.TeamID, c.Name, string(c.Kind), cfg, nullable(c.SecretID), c.EventTypes, c.Enabled)
	return MapError(err, OpWrite, "notification channel")
}

// Update implements notify.Repository.
func (r *ChannelRepo) Update(ctx context.Context, tx port.Tx, c domain.NotificationChannel, expect domain.Version) (domain.NotificationChannel, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.NotificationChannel{}, err
	}
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return domain.NotificationChannel{}, domain.Internal(err)
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.notification_channels c SET name=$3, config=$4::jsonb, secret_id=$5, event_types=$6, enabled=$7
WHERE `+channelScope+` AND c.id=$2 AND c.version=$8`, c.TeamID, c.ID, c.Name, cfg, nullable(c.SecretID), c.EventTypes, c.Enabled, int64(expect))
	if err != nil {
		return domain.NotificationChannel{}, MapError(err, OpWrite, "notification channel")
	}
	if tag.RowsAffected() == 0 {
		return domain.NotificationChannel{}, r.missOrStale(ctx, tx, c.TeamID, c.ID)
	}
	return r.Get(ctx, tx, c.TeamID, c.ID, false)
}

// Delete implements notify.Repository.
func (r *ChannelRepo) Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.notification_channels c WHERE `+channelScope+` AND c.id=$2 AND c.version=$3`, teamID, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "notification channel")
	}
	if tag.RowsAffected() == 0 {
		return r.missOrStale(ctx, tx, teamID, id)
	}
	// afs_app cannot touch resource_status: the reconciler removes the channel's rule and its status row.
	return nil
}

// NamedByWorkOrderSetting implements notify.Repository.
func (*ChannelRepo) NamedByWorkOrderSetting(ctx context.Context, tx port.Tx, id string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var one int
	err = pt.QueryRow(ctx, `SELECT 1 FROM afs.service_settings WHERE key = $1 AND value = to_jsonb($2::text)`,
		domain.SettingWorkOrdersNotificationChannelID, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, MapError(err, OpRead, "service setting")
}

func (r *ChannelRepo) missOrStale(ctx context.Context, tx port.Tx, teamID, id string) error {
	if _, err := r.Get(ctx, tx, teamID, id, false); err != nil {
		return err
	}
	return domain.PreconditionFailed()
}
