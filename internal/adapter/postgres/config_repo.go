package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/config"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ConfigRepo implements config.Repository on afs.namespace_config_values and afs.agent_config_values
// (000005). Row-level security scopes both to the caller's teams; database triggers refuse a link to
// a missing namespace value, a name that clashes with a secret binding, and a delete or rename of a
// namespace value agents link to.
type ConfigRepo struct{}

// NewConfigRepo returns a ConfigRepo.
func NewConfigRepo() *ConfigRepo { return &ConfigRepo{} }

var _ config.Repository = (*ConfigRepo)(nil)

const nsSelect = `SELECT n.id, n.team_id, n.name, n.config_value, n.version, n.created_at, n.updated_at,
  coalesce((SELECT array_agg(a.agent_id ORDER BY a.agent_id) FROM afs.agent_config_values a
            WHERE a.source = 'namespace' AND a.team_id = n.team_id AND a.name = n.name), '{}')
FROM afs.namespace_config_values n`

func scanNS(row pgx.Row) (domain.NamespaceConfigValue, error) {
	var v domain.NamespaceConfigValue
	var name string
	var ver int64
	err := row.Scan(&v.ID, &v.TeamID, &name, &v.Value, &ver, &v.CreatedAt, &v.UpdatedAt, &v.UsedBy)
	v.Name, v.Version = domain.ConfigName(name), domain.Version(ver)
	return v, err
}

// ListNamespace implements config.Repository.
func (*ConfigRepo) ListNamespace(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.NamespaceConfigValue, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, nsSelect+` WHERE n.team_id = $1 AND n.name > $2 ORDER BY n.name LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "configuration value")
	}
	defer rows.Close()
	out := []domain.NamespaceConfigValue{}
	for rows.Next() {
		v, err := scanNS(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "configuration value")
		}
		out = append(out, v)
	}
	return out, MapError(rows.Err(), OpRead, "configuration value")
}

// GetNamespace implements config.Repository.
func (*ConfigRepo) GetNamespace(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName, forUpdate bool) (domain.NamespaceConfigValue, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.NamespaceConfigValue{}, err
	}
	q := nsSelect + ` WHERE n.team_id = $1 AND n.name = $2`
	if forUpdate {
		q += ` FOR UPDATE OF n`
	}
	v, err := scanNS(pt.QueryRow(ctx, q, teamID, string(name)))
	return v, MapError(err, OpRead, "configuration value")
}

// InsertNamespace implements config.Repository.
func (*ConfigRepo) InsertNamespace(ctx context.Context, tx port.Tx, v domain.NamespaceConfigValue) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.namespace_config_values (id, team_id, name, config_value) VALUES ($1, $2, $3, $4)`,
		v.ID, v.TeamID, string(v.Name), v.Value)
	return MapError(err, OpWrite, "configuration value")
}

// UpdateNamespace implements config.Repository.
func (*ConfigRepo) UpdateNamespace(ctx context.Context, tx port.Tx, v domain.NamespaceConfigValue, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.namespace_config_values SET config_value = $3 WHERE team_id = $1 AND name = $2 AND version = $4`,
		v.TeamID, string(v.Name), v.Value, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "configuration value")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "configuration value", `SELECT 1 FROM afs.namespace_config_values WHERE team_id = $1 AND name = $2`, v.TeamID, string(v.Name))
	}
	return nil
}

// DeleteNamespace implements config.Repository.
func (*ConfigRepo) DeleteNamespace(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.namespace_config_values WHERE team_id = $1 AND name = $2 AND version = $3`, teamID, string(name), int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "configuration value")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "configuration value", `SELECT 1 FROM afs.namespace_config_values WHERE team_id = $1 AND name = $2`, teamID, string(name))
	}
	return nil
}

// AgentExists implements config.Repository.
func (*ConfigRepo) AgentExists(ctx context.Context, tx port.Tx, teamID, agentID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	return agentExists(ctx, pt, teamID, agentID)
}

const agentValueSelect = `SELECT a.id, a.team_id, a.agent_id, a.name, a.source, a.config_value,
  CASE a.source WHEN 'literal' THEN a.config_value ELSE n.config_value END, a.version, a.updated_at
FROM afs.agent_config_values a
LEFT JOIN afs.namespace_config_values n ON a.source = 'namespace' AND n.team_id = a.team_id AND n.name = a.name`

func scanAgentValue(row pgx.Row) (domain.AgentConfigValue, error) {
	var v domain.AgentConfigValue
	var name, src string
	var ver int64
	err := row.Scan(&v.ID, &v.TeamID, &v.AgentID, &name, &src, &v.Value, &v.ResolvedValue, &ver, &v.UpdatedAt)
	v.Name, v.Source, v.Version = domain.ConfigName(name), domain.ConfigSource(src), domain.Version(ver)
	return v, err
}

// ListAgent implements config.Repository.
func (*ConfigRepo) ListAgent(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.AgentConfigValue, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, agentValueSelect+` WHERE a.team_id = $1 AND a.agent_id = $2 AND a.name > $3 ORDER BY a.name LIMIT $4`, teamID, agentID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "configuration value")
	}
	defer rows.Close()
	out := []domain.AgentConfigValue{}
	for rows.Next() {
		v, err := scanAgentValue(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "configuration value")
		}
		out = append(out, v)
	}
	return out, MapError(rows.Err(), OpRead, "configuration value")
}

// GetAgent implements config.Repository.
func (*ConfigRepo) GetAgent(ctx context.Context, tx port.Tx, teamID, agentID string, name domain.ConfigName, forUpdate bool) (domain.AgentConfigValue, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.AgentConfigValue{}, err
	}
	q := agentValueSelect + ` WHERE a.team_id = $1 AND a.agent_id = $2 AND a.name = $3`
	if forUpdate {
		q += ` FOR UPDATE OF a`
	}
	v, err := scanAgentValue(pt.QueryRow(ctx, q, teamID, agentID, string(name)))
	return v, MapError(err, OpRead, "configuration value")
}

// InsertAgent implements config.Repository.
func (*ConfigRepo) InsertAgent(ctx context.Context, tx port.Tx, v domain.AgentConfigValue) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_config_values (id, team_id, agent_id, name, source, config_value) VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.TeamID, v.AgentID, string(v.Name), string(v.Source), v.Value)
	return MapError(err, OpWrite, "configuration value")
}

// UpdateAgent implements config.Repository.
func (*ConfigRepo) UpdateAgent(ctx context.Context, tx port.Tx, v domain.AgentConfigValue, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.agent_config_values SET source = $4, config_value = $5
WHERE team_id = $1 AND agent_id = $2 AND name = $3 AND version = $6`, v.TeamID, v.AgentID, string(v.Name), string(v.Source), v.Value, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "configuration value")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "configuration value", `SELECT 1 FROM afs.agent_config_values WHERE team_id = $1 AND agent_id = $2 AND name = $3`,
			v.TeamID, v.AgentID, string(v.Name))
	}
	return nil
}

// DeleteAgent implements config.Repository.
func (*ConfigRepo) DeleteAgent(ctx context.Context, tx port.Tx, teamID, agentID string, name domain.ConfigName, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.agent_config_values WHERE team_id = $1 AND agent_id = $2 AND name = $3 AND version = $4`,
		teamID, agentID, string(name), int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "configuration value")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "configuration value", `SELECT 1 FROM afs.agent_config_values WHERE team_id = $1 AND agent_id = $2 AND name = $3`,
			teamID, agentID, string(name))
	}
	return nil
}
