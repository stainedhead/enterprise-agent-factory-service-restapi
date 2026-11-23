package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/models"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ModelRepo implements models.Repository on afs.models and afs.model_prices. Row-level security
// (000013) lets every afs_app caller read and only service admins write.
type ModelRepo struct{}

// NewModelRepo returns a ModelRepo.
func NewModelRepo() *ModelRepo { return &ModelRepo{} }

var _ models.Repository = (*ModelRepo)(nil)

// catalogMissOrStale tells a missing row (NotFound) from a stale version (PreconditionFailed) after
// a versioned UPDATE or DELETE matched nothing. q selects 1 for the row by key.
func catalogMissOrStale(ctx context.Context, pt *Tx, resource, q string, args ...any) error {
	var one int
	err := pt.QueryRow(ctx, q, args...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound(resource)
	}
	if err != nil {
		return MapError(err, OpRead, resource)
	}
	return domain.PreconditionFailed()
}

// svcAccessSQL is the service's model access: all while the setting is unset (000022 seeds it).
const svcAccessSQL = `coalesce((SELECT s.value #>> '{}' FROM afs.service_settings s WHERE s.key = 'models.service_access'), 'all')`

// modelSelect adds the derived columns: serviceEnabled, priceState, callable and origin. teamsWithAccess
// is a count the admin list asks for (-1 otherwise).
func modelSelect(withTeams bool) string {
	teams := `-1`
	if withTeams {
		teams = `(SELECT count(*) FROM afs.teams t WHERE t.state <> 'deleted' AND (t.model_access = 'all'
  OR EXISTS (SELECT 1 FROM afs.team_models tm WHERE tm.team_id = t.id AND tm.model_id = m.id)))`
	}
	return `SELECT m.id, m.provider, m.display_name, m.enabled, coalesce(m.context_window, 0), coalesce(m.max_output_tokens, 0), m.version, m.updated_at,
  (m.enabled OR ` + svcAccessSQL + ` = 'all') AS service_enabled,
  CASE WHEN EXISTS (SELECT 1 FROM afs.model_prices p WHERE p.model_id = m.id AND p.effective_from <= now()) THEN 'approved'
       WHEN EXISTS (SELECT 1 FROM afs.model_prices p WHERE p.model_id = m.id AND p.effective_from > now())
         OR EXISTS (SELECT 1 FROM afs.model_price_proposals q WHERE q.model_id = m.id AND q.status = 'pending') THEN 'pending'
       ELSE 'none' END AS price_state,
  m.origin, ` + teams + ` FROM afs.models m`
}

func scanModel(row pgx.Row) (domain.Model, error) {
	var m domain.Model
	var v int64
	var ps, origin string
	var teams int64
	err := row.Scan(&m.ID, &m.Provider, &m.DisplayName, &m.Enabled, &m.ContextWindow, &m.MaxOutputTokens, &v, &m.UpdatedAt,
		&m.ServiceEnabled, &ps, &origin, &teams)
	m.Version = domain.Version(v)
	m.PriceState = domain.PriceState(ps)
	m.Origin = domain.ModelOrigin(origin)
	m.TeamsWithAccess = int(teams)
	m.Callable = m.ServiceEnabled && m.PriceState == domain.PriceApproved
	return m, err
}

// List implements models.Repository.
func (*ModelRepo) List(ctx context.Context, tx port.Tx, usableOnly, withTeams bool, after string, limit int) ([]domain.Model, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, modelSelect(withTeams)+` WHERE m.id > $1 AND (m.enabled OR NOT $2 OR `+svcAccessSQL+` = 'all') ORDER BY m.id LIMIT $3`, after, usableOnly, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "model")
	}
	defer rows.Close()
	out := []domain.Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "model")
		}
		out = append(out, m)
	}
	return out, MapError(rows.Err(), OpRead, "model")
}

// ServiceAccess implements models.Repository.
func (*ModelRepo) ServiceAccess(ctx context.Context, tx port.Tx) (domain.ServiceModelAccess, error) {
	var out domain.ServiceModelAccess
	pt, err := Unwrap(tx)
	if err != nil {
		return out, err
	}
	var access string
	var v int64
	err = pt.QueryRow(ctx, `SELECT `+svcAccessSQL+`, coalesce((SELECT s.version FROM afs.service_settings s WHERE s.key = 'models.service_access'), 0)`).Scan(&access, &v)
	if err != nil {
		return out, MapError(err, OpRead, "setting")
	}
	out.Access, out.Version = domain.ModelAccess(access), domain.Version(v)
	err = pt.QueryRow(ctx, `SELECT count(*) FILTER (WHERE enabled),
  count(*) FILTER (WHERE (enabled OR $1) AND EXISTS (SELECT 1 FROM afs.model_prices p WHERE p.model_id = m.id AND p.effective_from <= now()))
  FROM afs.models m`, access == string(domain.ModelAccessAll)).Scan(&out.EnabledModels, &out.CallableModels)
	return out, MapError(err, OpRead, "model")
}

// PutServiceAccess implements models.Repository.
func (*ModelRepo) PutServiceAccess(ctx context.Context, tx port.Tx, access domain.ModelAccess, expect *domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if expect != nil && *expect > 0 {
		tag, err := pt.Exec(ctx, `UPDATE afs.service_settings SET value = to_jsonb($1::text) WHERE key = 'models.service_access' AND version = $2`,
			string(access), int64(*expect))
		if err != nil {
			return MapError(err, OpWrite, "setting")
		}
		if tag.RowsAffected() == 0 {
			return domain.PreconditionFailed()
		}
		return nil
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.service_settings (key, value) VALUES ('models.service_access', to_jsonb($1::text))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, string(access))
	return MapError(err, OpWrite, "setting")
}

// Get implements models.Repository.
func (*ModelRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.Model, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Model{}, err
	}
	q := modelSelect(false) + ` WHERE m.id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	m, err := scanModel(pt.QueryRow(ctx, q, id))
	return m, MapError(err, OpRead, "model")
}

// Insert implements models.Repository.
func (*ModelRepo) Insert(ctx context.Context, tx port.Tx, m domain.Model) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.models (id, provider, display_name, enabled, context_window, max_output_tokens)
VALUES ($1, $2, $3, $4, $5, $6)`, m.ID, m.Provider, m.DisplayName, m.Enabled, m.ContextWindow, m.MaxOutputTokens)
	return MapError(err, OpWrite, "model")
}

// Update implements models.Repository.
func (r *ModelRepo) Update(ctx context.Context, tx port.Tx, m domain.Model, expect domain.Version) (domain.Model, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Model{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.models SET display_name = $2, enabled = $3, context_window = $4, max_output_tokens = $5
WHERE id = $1 AND version = $6`, m.ID, m.DisplayName, m.Enabled, m.ContextWindow, m.MaxOutputTokens, int64(expect))
	if err != nil {
		return domain.Model{}, MapError(err, OpWrite, "model")
	}
	if tag.RowsAffected() == 0 {
		return domain.Model{}, catalogMissOrStale(ctx, pt, "model", `SELECT 1 FROM afs.models WHERE id = $1`, m.ID)
	}
	return r.Get(ctx, tx, m.ID, false)
}

const priceSelect = `SELECT model_id, effective_from, input_usd_per_mtok::text, output_usd_per_mtok::text,
  cache_read_usd_per_mtok::text, cache_write_usd_per_mtok::text, source, created_at, created_by FROM afs.model_prices`

func scanPrice(row pgx.Row) (domain.ModelPrice, error) {
	var p domain.ModelPrice
	var in, out, cr, cw, src string
	if err := row.Scan(&p.ModelID, &p.EffectiveFrom, &in, &out, &cr, &cw, &src, &p.CreatedAt, &p.CreatedBy); err != nil {
		return p, err
	}
	p.Source = domain.PriceSource(src)
	p.EffectiveFrom = p.EffectiveFrom.UTC()
	for _, f := range []struct {
		dst *domain.Money
		s   string
	}{{&p.Input, in}, {&p.Output, out}, {&p.CacheRead, cr}, {&p.CacheWrite, cw}} {
		m, err := domain.ParseMoney("price", f.s)
		if err != nil {
			return p, err
		}
		*f.dst = m
	}
	return p, nil
}

// ListPrices implements models.Repository.
func (*ModelRepo) ListPrices(ctx context.Context, tx port.Tx, modelID string, before *time.Time, limit int) ([]domain.ModelPrice, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var cursor any
	if before != nil {
		cursor = *before
	}
	rows, err := pt.Query(ctx, priceSelect+` WHERE model_id = $1 AND ($2::timestamptz IS NULL OR effective_from < $2) ORDER BY effective_from DESC LIMIT $3`,
		modelID, cursor, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "model price")
	}
	defer rows.Close()
	out := []domain.ModelPrice{}
	for rows.Next() {
		p, err := scanPrice(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "model price")
		}
		out = append(out, p)
	}
	return out, MapError(rows.Err(), OpRead, "model price")
}

// InsertPrice implements models.Repository.
func (*ModelRepo) InsertPrice(ctx context.Context, tx port.Tx, p domain.ModelPrice) (domain.ModelPrice, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ModelPrice{}, err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok,
  cache_read_usd_per_mtok, cache_write_usd_per_mtok, source) VALUES ($1, $2, $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7)`,
		p.ModelID, p.EffectiveFrom, p.Input.String(), p.Output.String(), p.CacheRead.String(), p.CacheWrite.String(), string(p.Source))
	if err != nil {
		return domain.ModelPrice{}, MapError(err, OpWrite, "model price")
	}
	out, err := scanPrice(pt.QueryRow(ctx, priceSelect+` WHERE model_id = $1 AND effective_from = $2`, p.ModelID, p.EffectiveFrom))
	return out, MapError(err, OpRead, "model price")
}
