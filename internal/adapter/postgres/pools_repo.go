package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// PoolRepo implements pools.Repository on afs.agent_pools and its satellites (000012): stages,
// handoffs, secret bindings, configuration values, external system opt-outs and role links. Pools in
// state deleted are invisible: the reconciler finalizes a delete by that state. Row-level security
// scopes every table to the caller's teams.
type PoolRepo struct{}

// NewPoolRepo returns a PoolRepo.
func NewPoolRepo() *PoolRepo { return &PoolRepo{} }

var _ pools.Repository = (*PoolRepo)(nil)

const poolSelect = `SELECT p.id, p.team_id, p.name, p.description, coalesce(p.stage_id, ''), p.template, p.image, p.persona,
  p.model_id, p.workers, p.resources, p.scratch_size_gib, p.shared_volume, p.state, p.version, p.generation,
  p.created_at, p.created_by, p.updated_at,
  rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at
FROM afs.agent_pools p
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + pools.StatusKind + `' AND rs.resource_id = p.id
WHERE p.team_id = $1 AND p.state <> 'deleted' AND p.deleted_at IS NULL`

func scanPool(row pgx.Row) (domain.Pool, error) {
	var (
		p          domain.Pool
		name       string
		image      string
		state      string
		res, conds []byte
		ver, gen   int64
		phase      *string
		obs        *int64
		lastRec    *time.Time
	)
	err := row.Scan(&p.ID, &p.TeamID, &name, &p.Description, &p.StageID, &p.Template, &image, &p.Persona,
		&p.ModelID, &p.Workers, &res, &p.ScratchSizeGiB, &p.SharedVolume, &state, &ver, &gen,
		&p.CreatedAt, &p.CreatedBy, &p.UpdatedAt, &phase, &obs, &conds, &lastRec)
	if err != nil {
		return domain.Pool{}, err
	}
	p.Name, p.Image, p.State = domain.Name(name), domain.ImageRef(image), domain.PoolState(state)
	p.Version, p.Generation = domain.Version(ver), domain.Generation(gen)
	if len(res) > 0 {
		if err := json.Unmarshal(res, &p.Resources); err != nil {
			return domain.Pool{}, err
		}
	}
	p.Handoffs = []string{}
	p.Status = domain.PendingStatus(p.CreatedAt)
	if phase != nil {
		p.Status.Phase = domain.Phase(*phase)
		if obs != nil {
			p.Status.ObservedGeneration = *obs
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &p.Status.Conditions); err != nil {
				return domain.Pool{}, err
			}
		}
		if p.Status.Conditions == nil {
			p.Status.Conditions = []domain.Condition{}
		}
		if lastRec != nil {
			p.Status.UpdatedAt = lastRec.UTC()
		}
	}
	return p, nil
}

func loadHandoffs(ctx context.Context, pt *Tx, teamID string, list []domain.Pool) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	idx := map[string]int{}
	for i, p := range list {
		ids[i], idx[p.ID] = p.ID, i
	}
	rows, err := pt.Query(ctx, `SELECT from_pool_id, to_pool_id FROM afs.pool_handoffs
WHERE team_id=$1 AND from_pool_id = ANY($2) ORDER BY from_pool_id, to_pool_id`, teamID, ids)
	if err != nil {
		return MapError(err, OpRead, "pool")
	}
	defer rows.Close()
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return MapError(err, OpRead, "pool")
		}
		i := idx[from]
		list[i].Handoffs = append(list[i].Handoffs, to)
	}
	return MapError(rows.Err(), OpRead, "pool")
}

// TeamKind implements pools.PoolRepository.
func (*PoolRepo) TeamKind(ctx context.Context, tx port.Tx, teamID string) (domain.TeamKind, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var kind string
	err = pt.QueryRow(ctx, `SELECT kind FROM afs.teams WHERE id = $1 AND state <> 'deleted'`, teamID).Scan(&kind)
	return domain.TeamKind(kind), MapError(err, OpRead, "team")
}

// List implements pools.PoolRepository.
func (*PoolRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Pool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, poolSelect+` AND p.id > $2 ORDER BY p.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "pool")
	}
	out := []domain.Pool{}
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			rows.Close()
			return nil, MapError(err, OpRead, "pool")
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, MapError(err, OpRead, "pool")
	}
	return out, loadHandoffs(ctx, pt, teamID, out)
}

// Get implements pools.PoolRepository.
func (*PoolRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Pool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Pool{}, err
	}
	q := poolSelect + ` AND p.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF p`
	}
	p, err := scanPool(pt.QueryRow(ctx, q, teamID, id))
	if err != nil {
		return domain.Pool{}, MapError(err, OpRead, "pool")
	}
	list := []domain.Pool{p}
	if err := loadHandoffs(ctx, pt, teamID, list); err != nil {
		return domain.Pool{}, err
	}
	return list[0], nil
}

// Insert implements pools.PoolRepository.
func (*PoolRepo) Insert(ctx context.Context, tx port.Tx, p domain.Pool, templateID string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	res, err := json.Marshal(p.Resources)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_pools (id, team_id, name, description, stage_id, template, template_id, image, persona,
  model_id, workers, resources, scratch_size_gib, shared_volume, state, generation)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		p.ID, p.TeamID, string(p.Name), p.Description, nullable(p.StageID), p.Template, nullable(templateID), string(p.Image), p.Persona,
		p.ModelID, p.Workers, res, p.ScratchSizeGiB, p.SharedVolume, string(p.State), int64(p.Generation))
	return MapError(err, OpWrite, "pool")
}

// Update implements pools.PoolRepository. template and image are named in the UPDATE only when the
// template changed, because the database's template guard runs on UPDATE OF those columns.
func (*PoolRepo) Update(ctx context.Context, tx port.Tx, p domain.Pool, templateID string, templateChanged bool, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	res, err := json.Marshal(p.Resources)
	if err != nil {
		return err
	}
	q := `UPDATE afs.agent_pools SET description=$3, stage_id=$4, persona=$5, model_id=$6, workers=$7, resources=$8,
  scratch_size_gib=$9, shared_volume=$10, state=$11, generation=$12`
	args := []any{p.TeamID, p.ID, p.Description, nullable(p.StageID), p.Persona, p.ModelID, p.Workers, res,
		p.ScratchSizeGiB, p.SharedVolume, string(p.State), int64(p.Generation)}
	if templateChanged {
		q += `, template=$13, template_id=$14, image=$15`
		args = append(args, p.Template, templateID, string(p.Image))
	}
	q += ` WHERE team_id=$1 AND id=$2 AND deleted_at IS NULL AND version=$` + strconv.Itoa(len(args)+1)
	args = append(args, int64(expect))
	tag, err := pt.Exec(ctx, q, args...)
	if err != nil {
		return MapError(err, OpWrite, "pool")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// Touch implements pools.PoolRepository: a generation bump, which the version trigger and the change
// feed pick up, for satellite changes that have no revision trigger of their own.
func (*PoolRepo) Touch(ctx context.Context, tx port.Tx, teamID string, poolIDs []string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.agent_pools SET generation = generation + 1 WHERE team_id = $1 AND id = ANY($2) AND deleted_at IS NULL`, teamID, poolIDs)
	return MapError(err, OpWrite, "pool")
}

// Template implements pools.PoolRepository.
func (*PoolRepo) Template(ctx context.Context, tx port.Tx, name string) (pools.TemplateInfo, bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return pools.TemplateInfo{}, false, err
	}
	var t pools.TemplateInfo
	err = pt.QueryRow(ctx, `SELECT id, name, enabled, allowed_image_digests FROM afs.agent_templates WHERE name=$1`, name).
		Scan(&t.ID, &t.Name, &t.Enabled, &t.AllowedDigests)
	if errors.Is(err, pgx.ErrNoRows) {
		return pools.TemplateInfo{}, false, nil
	}
	return t, err == nil, MapError(err, OpRead, "agent template")
}

// ModelEnabled implements pools.PoolRepository.
func (*PoolRepo) ModelEnabled(ctx context.Context, tx port.Tx, teamID, modelID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.team_models tm JOIN afs.models m ON m.id = tm.model_id
WHERE tm.team_id=$1 AND tm.model_id=$2 AND m.enabled)`, teamID, modelID).Scan(&ok)
	return ok, MapError(err, OpRead, "model")
}

// AgentNameTaken implements pools.PoolRepository.
func (*PoolRepo) AgentNameTaken(ctx context.Context, tx port.Tx, teamID, name string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.agents WHERE team_id=$1 AND name=$2 AND deleted_at IS NULL)`, teamID, name).Scan(&ok)
	return ok, MapError(err, OpRead, "agent")
}

// Workers implements pools.PoolRepository.
func (*PoolRepo) Workers(ctx context.Context, tx port.Tx, teamID, poolID string) (domain.PoolWorkers, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.PoolWorkers{}, err
	}
	var w domain.PoolWorkers
	err = pt.QueryRow(ctx, `SELECT desired_workers, reporting_workers, working_workers, idle_workers, impaired_workers
FROM afs.pool_workers_current WHERE team_id=$1 AND pool_id=$2`, teamID, poolID).
		Scan(&w.Desired, &w.Reporting, &w.Working, &w.Idle, &w.Impaired)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PoolWorkers{}, nil
	}
	return w, MapError(err, OpRead, "pool")
}

// --- stages ---

const stageCols = `id, team_id, name, position, description, version, created_at, created_by, updated_at`

func scanStage(row pgx.Row) (domain.Stage, error) {
	var s domain.Stage
	var name string
	var ver int64
	err := row.Scan(&s.ID, &s.TeamID, &name, &s.Position, &s.Description, &ver, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt)
	s.Name, s.Version = domain.Name(name), domain.Version(ver)
	return s, err
}

// ListStages implements pools.StageRepository.
func (*PoolRepo) ListStages(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Stage, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+stageCols+` FROM afs.swarm_stages WHERE team_id = $1 AND id > $2 ORDER BY id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "stage")
	}
	defer rows.Close()
	out := []domain.Stage{}
	for rows.Next() {
		s, err := scanStage(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "stage")
		}
		out = append(out, s)
	}
	return out, MapError(rows.Err(), OpRead, "stage")
}

// GetStage implements pools.StageRepository.
func (*PoolRepo) GetStage(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Stage, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Stage{}, err
	}
	q := `SELECT ` + stageCols + ` FROM afs.swarm_stages WHERE team_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	s, err := scanStage(pt.QueryRow(ctx, q, teamID, id))
	return s, MapError(err, OpRead, "stage")
}

// InsertStage implements pools.StageRepository.
func (*PoolRepo) InsertStage(ctx context.Context, tx port.Tx, s domain.Stage) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.swarm_stages (id, team_id, name, position, description) VALUES ($1, $2, $3, $4, $5)`,
		s.ID, s.TeamID, string(s.Name), s.Position, s.Description)
	return MapError(err, OpWrite, "stage")
}

// UpdateStage implements pools.StageRepository.
func (*PoolRepo) UpdateStage(ctx context.Context, tx port.Tx, s domain.Stage, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.swarm_stages SET position = $3, description = $4 WHERE team_id = $1 AND id = $2 AND version = $5`,
		s.TeamID, s.ID, s.Position, s.Description, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "stage")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "stage", `SELECT 1 FROM afs.swarm_stages WHERE team_id = $1 AND id = $2`, s.TeamID, s.ID)
	}
	return nil
}

// DeleteStage implements pools.StageRepository: the foreign key from agent_pools restricts it.
func (*PoolRepo) DeleteStage(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.swarm_stages WHERE team_id = $1 AND id = $2 AND version = $3`, teamID, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "stage")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "stage", `SELECT 1 FROM afs.swarm_stages WHERE team_id = $1 AND id = $2`, teamID, id)
	}
	return nil
}

// --- handoffs ---

// Handoffs implements pools.SatelliteRepository.
func (*PoolRepo) Handoffs(ctx context.Context, tx port.Tx, teamID, poolID string) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT to_pool_id FROM afs.pool_handoffs WHERE team_id = $1 AND from_pool_id = $2 ORDER BY to_pool_id`, teamID, poolID)
	if err != nil {
		return nil, MapError(err, OpRead, "handoff")
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			return nil, MapError(err, OpRead, "handoff")
		}
		out = append(out, to)
	}
	return out, MapError(rows.Err(), OpRead, "handoff")
}

// PoolsExist implements pools.SatelliteRepository.
func (*PoolRepo) PoolsExist(ctx context.Context, tx port.Tx, teamID string, ids []string) (int, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return 0, err
	}
	var n int
	err = pt.QueryRow(ctx, `SELECT count(*) FROM afs.agent_pools WHERE team_id = $1 AND id = ANY($2) AND deleted_at IS NULL`, teamID, ids).Scan(&n)
	return n, MapError(err, OpRead, "pool")
}

// ReplaceHandoffs implements pools.SatelliteRepository.
func (*PoolRepo) ReplaceHandoffs(ctx context.Context, tx port.Tx, teamID, poolID string, to []string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.pool_handoffs WHERE team_id = $1 AND from_pool_id = $2`, teamID, poolID); err != nil {
		return MapError(err, OpWrite, "handoff")
	}
	for _, id := range to {
		if _, err := pt.Exec(ctx, `INSERT INTO afs.pool_handoffs (team_id, from_pool_id, to_pool_id) VALUES ($1, $2, $3)`, teamID, poolID, id); err != nil {
			return MapError(err, OpWrite, "handoff")
		}
	}
	return nil
}

// --- secret bindings ---

// Bindings implements pools.SatelliteRepository.
func (*PoolRepo) Bindings(ctx context.Context, tx port.Tx, teamID, poolID string) ([]domain.SecretBinding, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT secret_id, delivery, target FROM afs.pool_secret_bindings WHERE team_id = $1 AND pool_id = $2 ORDER BY target`, teamID, poolID)
	if err != nil {
		return nil, MapError(err, OpRead, "secret binding")
	}
	defer rows.Close()
	out := []domain.SecretBinding{}
	for rows.Next() {
		var b domain.SecretBinding
		if err := rows.Scan(&b.SecretID, &b.Delivery, &b.Target); err != nil {
			return nil, MapError(err, OpRead, "secret binding")
		}
		out = append(out, b)
	}
	return out, MapError(rows.Err(), OpRead, "secret binding")
}

// Secrets implements pools.SatelliteRepository.
func (*PoolRepo) Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]pools.SecretInfo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, coalesce(agent_id, ''), coalesce(pool_id, '') FROM afs.secrets WHERE team_id=$1 AND id = ANY($2)`, teamID, ids)
	if err != nil {
		return nil, MapError(err, OpRead, "secret")
	}
	defer rows.Close()
	out := map[string]pools.SecretInfo{}
	for rows.Next() {
		var id string
		var s pools.SecretInfo
		if err := rows.Scan(&id, &s.AgentID, &s.PoolID); err != nil {
			return nil, MapError(err, OpRead, "secret")
		}
		out[id] = s
	}
	return out, MapError(rows.Err(), OpRead, "secret")
}

// ReplaceBindings implements pools.SatelliteRepository.
func (*PoolRepo) ReplaceBindings(ctx context.Context, tx port.Tx, teamID, poolID string, bs []domain.SecretBinding) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.pool_secret_bindings WHERE team_id = $1 AND pool_id = $2`, teamID, poolID); err != nil {
		return MapError(err, OpWrite, "secret binding")
	}
	for _, b := range bs {
		if _, err := pt.Exec(ctx, `INSERT INTO afs.pool_secret_bindings (team_id, pool_id, secret_id, delivery, target) VALUES ($1, $2, $3, $4, $5)`,
			teamID, poolID, b.SecretID, b.Delivery, b.Target); err != nil {
			return MapError(err, OpWrite, "secret binding")
		}
	}
	return nil
}

// --- configuration values ---

const poolValueSelect = `SELECT v.id, v.team_id, v.pool_id, v.name, v.source, v.config_value,
  CASE v.source WHEN 'literal' THEN v.config_value ELSE n.config_value END, v.version, v.updated_at
FROM afs.pool_config_values v
LEFT JOIN afs.namespace_config_values n ON v.source = 'namespace' AND n.team_id = v.team_id AND n.name = v.name`

func scanPoolValue(row pgx.Row) (domain.PoolConfigValue, error) {
	var v domain.PoolConfigValue
	var name, src string
	var ver int64
	err := row.Scan(&v.ID, &v.TeamID, &v.PoolID, &name, &src, &v.Value, &v.ResolvedValue, &ver, &v.UpdatedAt)
	v.Name, v.Source, v.Version = domain.ConfigName(name), domain.ConfigSource(src), domain.Version(ver)
	return v, err
}

// ListConfig implements pools.SatelliteRepository.
func (*PoolRepo) ListConfig(ctx context.Context, tx port.Tx, teamID, poolID, after string, limit int) ([]domain.PoolConfigValue, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, poolValueSelect+` WHERE v.team_id = $1 AND v.pool_id = $2 AND v.name > $3 ORDER BY v.name LIMIT $4`, teamID, poolID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "configuration value")
	}
	defer rows.Close()
	out := []domain.PoolConfigValue{}
	for rows.Next() {
		v, err := scanPoolValue(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "configuration value")
		}
		out = append(out, v)
	}
	return out, MapError(rows.Err(), OpRead, "configuration value")
}

// GetConfig implements pools.SatelliteRepository.
func (*PoolRepo) GetConfig(ctx context.Context, tx port.Tx, teamID, poolID string, name domain.ConfigName, forUpdate bool) (domain.PoolConfigValue, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.PoolConfigValue{}, err
	}
	q := poolValueSelect + ` WHERE v.team_id = $1 AND v.pool_id = $2 AND v.name = $3`
	if forUpdate {
		q += ` FOR UPDATE OF v`
	}
	v, err := scanPoolValue(pt.QueryRow(ctx, q, teamID, poolID, string(name)))
	return v, MapError(err, OpRead, "configuration value")
}

// NamespaceValueExists implements pools.SatelliteRepository.
func (*PoolRepo) NamespaceValueExists(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.namespace_config_values WHERE team_id = $1 AND name = $2)`, teamID, string(name)).Scan(&ok)
	return ok, MapError(err, OpRead, "configuration value")
}

// InsertConfig implements pools.SatelliteRepository.
func (*PoolRepo) InsertConfig(ctx context.Context, tx port.Tx, v domain.PoolConfigValue) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.pool_config_values (id, team_id, pool_id, name, source, config_value) VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.TeamID, v.PoolID, string(v.Name), string(v.Source), v.Value)
	return MapError(err, OpWrite, "configuration value")
}

// UpdateConfig implements pools.SatelliteRepository.
func (*PoolRepo) UpdateConfig(ctx context.Context, tx port.Tx, v domain.PoolConfigValue, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.pool_config_values SET source = $4, config_value = $5
WHERE team_id = $1 AND pool_id = $2 AND name = $3 AND version = $6`, v.TeamID, v.PoolID, string(v.Name), string(v.Source), v.Value, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "configuration value")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "configuration value", `SELECT 1 FROM afs.pool_config_values WHERE team_id = $1 AND pool_id = $2 AND name = $3`,
			v.TeamID, v.PoolID, string(v.Name))
	}
	return nil
}

// DeleteConfig implements pools.SatelliteRepository.
func (*PoolRepo) DeleteConfig(ctx context.Context, tx port.Tx, teamID, poolID string, name domain.ConfigName, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.pool_config_values WHERE team_id = $1 AND pool_id = $2 AND name = $3 AND version = $4`,
		teamID, poolID, string(name), int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "configuration value")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "configuration value", `SELECT 1 FROM afs.pool_config_values WHERE team_id = $1 AND pool_id = $2 AND name = $3`,
			teamID, poolID, string(name))
	}
	return nil
}

// --- external systems ---

// poolDisabledAt is the level that has a system off for the pool ($1 team id, $2 pool id, s the
// system). A pool's own switch is reported as "agent": the contract's disabledAt enumeration has no
// pool level (recorded in implementation-notes.md).
const poolDisabledAt = `CASE
  WHEN NOT s.enabled THEN CASE WHEN s.team_id IS NULL THEN 'service' ELSE 'team' END
  WHEN EXISTS (SELECT 1 FROM afs.external_system_optouts o
               WHERE o.team_id = $1 AND o.agent_id IS NULL AND o.pool_id IS NULL AND o.system_id = s.id) THEN 'team'
  WHEN EXISTS (SELECT 1 FROM afs.external_system_optouts o WHERE o.pool_id = $2 AND o.system_id = s.id) THEN 'agent'
END`

// ListSystems implements pools.SatelliteRepository.
func (*PoolRepo) ListSystems(ctx context.Context, tx port.Tx, teamID, poolID, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+extSystemCols+`, `+poolDisabledAt+` FROM afs.external_systems s
WHERE `+teamVisible+` AND s.id > $3 ORDER BY s.id LIMIT $4`, teamID, poolID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "external system")
	}
	return collectAccess(rows)
}

// GetSystem implements pools.SatelliteRepository.
func (*PoolRepo) GetSystem(ctx context.Context, tx port.Tx, teamID, poolID, systemID string) (domain.ExternalSystemAccess, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	a, err := scanExtAccess(pt.QueryRow(ctx, `SELECT `+extSystemCols+`, `+poolDisabledAt+` FROM afs.external_systems s
WHERE `+teamVisible+` AND s.id = $3`, teamID, poolID, systemID))
	return a, MapError(err, OpRead, "external system")
}

// SetSystemOff implements pools.SatelliteRepository; both directions are idempotent.
func (*PoolRepo) SetSystemOff(ctx context.Context, tx port.Tx, teamID, poolID, systemID, optOutID string, off bool) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if off {
		_, err := pt.Exec(ctx, `INSERT INTO afs.external_system_optouts (id, team_id, pool_id, system_id)
VALUES ($1, $2, $3, $4) ON CONFLICT ON CONSTRAINT external_system_optouts_owner_system_key DO NOTHING`,
			optOutID, teamID, poolID, systemID)
		return MapError(err, OpWrite, "external system")
	}
	_, err = pt.Exec(ctx, `DELETE FROM afs.external_system_optouts WHERE team_id = $1 AND pool_id = $2 AND system_id = $3`, teamID, poolID, systemID)
	return MapError(err, OpDelete, "external system")
}

// --- role links ---

// AccountAllowed implements pools.SatelliteRepository.
func (*PoolRepo) AccountAllowed(ctx context.Context, tx port.Tx, teamID, accountID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT afs.account_allowed($1, $2)`, teamID, accountID).Scan(&ok)
	return ok, MapError(err, OpRead, "account")
}

// LinkRoleName implements pools.SatelliteRepository.
func (*PoolRepo) LinkRoleName(ctx context.Context, tx port.Tx, teamID, poolID string) (string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var name string
	err = pt.QueryRow(ctx, `SELECT role_name FROM afs.pool_role_links WHERE team_id = $1 AND pool_id = $2 LIMIT 1`, teamID, poolID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return name, MapError(err, OpRead, "role link")
}

const poolLinkCols = `id, team_id, pool_id, account_id, role_name, role_arn, profile_name, external_id, max_session_seconds, status,
  last_verified_at, last_error, version, created_at, created_by, updated_at`

func scanPoolLink(row pgx.Row) (domain.PoolRoleLink, error) {
	var l domain.PoolRoleLink
	var status string
	var verified *time.Time
	var lastErr *string
	var v int64
	err := row.Scan(&l.ID, &l.TeamID, &l.PoolID, &l.AccountID, &l.RoleName, &l.RoleARN, &l.ProfileName, &l.ExternalID, &l.MaxSessionSeconds,
		&status, &verified, &lastErr, &v, &l.CreatedAt, &l.CreatedBy, &l.UpdatedAt)
	l.Status, l.LastVerifiedAt, l.Version = domain.RoleLinkStatus(status), verified, domain.Version(v)
	if lastErr != nil {
		l.LastError = *lastErr
	}
	return l, err
}

// ListLinks implements pools.SatelliteRepository.
func (*PoolRepo) ListLinks(ctx context.Context, tx port.Tx, teamID, poolID, after string, limit int) ([]domain.PoolRoleLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+poolLinkCols+` FROM afs.pool_role_links WHERE team_id = $1 AND pool_id = $2 AND id > $3 ORDER BY id LIMIT $4`,
		teamID, poolID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "role link")
	}
	defer rows.Close()
	out := []domain.PoolRoleLink{}
	for rows.Next() {
		l, err := scanPoolLink(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "role link")
		}
		out = append(out, l)
	}
	return out, MapError(rows.Err(), OpRead, "role link")
}

// GetLink implements pools.SatelliteRepository.
func (*PoolRepo) GetLink(ctx context.Context, tx port.Tx, teamID, poolID, linkID string, forUpdate bool) (domain.PoolRoleLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.PoolRoleLink{}, err
	}
	q := `SELECT ` + poolLinkCols + ` FROM afs.pool_role_links WHERE id = $1 AND team_id = $2 AND pool_id = $3`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	l, err := scanPoolLink(pt.QueryRow(ctx, q, linkID, teamID, poolID))
	return l, MapError(err, OpRead, "role link")
}

// InsertLink implements pools.SatelliteRepository; role_arn is generated by the database.
func (*PoolRepo) InsertLink(ctx context.Context, tx port.Tx, l domain.PoolRoleLink) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.pool_role_links (id, team_id, pool_id, account_id, role_name, profile_name, external_id, max_session_seconds)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, l.ID, l.TeamID, l.PoolID, l.AccountID, l.RoleName, l.ProfileName, l.ExternalID, l.MaxSessionSeconds)
	return MapError(err, OpWrite, "role link")
}

// DeleteLink implements pools.SatelliteRepository.
func (*PoolRepo) DeleteLink(ctx context.Context, tx port.Tx, teamID, poolID, linkID string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.pool_role_links WHERE id = $1 AND team_id = $2 AND pool_id = $3 AND version = $4`, linkID, teamID, poolID, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "role link")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// RequestVerify implements pools.SatelliteRepository: back to pending_trust, which notifies the reconciler.
func (*PoolRepo) RequestVerify(ctx context.Context, tx port.Tx, teamID, poolID, linkID string) (domain.PoolRoleLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.PoolRoleLink{}, err
	}
	if _, err := pt.Exec(ctx, `UPDATE afs.pool_role_links SET status = 'pending_trust', last_error = NULL WHERE id = $1 AND team_id = $2 AND pool_id = $3`,
		linkID, teamID, poolID); err != nil {
		return domain.PoolRoleLink{}, MapError(err, OpWrite, "role link")
	}
	l, err := scanPoolLink(pt.QueryRow(ctx, `SELECT `+poolLinkCols+` FROM afs.pool_role_links WHERE id = $1`, linkID))
	return l, MapError(err, OpRead, "role link")
}
