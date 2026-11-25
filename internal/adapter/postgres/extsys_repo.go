package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/extsys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ExtSystemRepo implements extsys.Repository on afs.external_systems and afs.external_system_optouts.
// Row-level security shows a team the service catalog and its own systems; the database guard
// (external_system_guard) enforces that only teams with may_add_external_systems declare systems.
type ExtSystemRepo struct{}

// NewExtSystemRepo returns an ExtSystemRepo.
func NewExtSystemRepo() *ExtSystemRepo { return &ExtSystemRepo{} }

var _ extsys.Repository = (*ExtSystemRepo)(nil)

// extSystemCols are the columns of every external system read, with the table alias s.
const extSystemCols = `s.id, coalesce(s.team_id, ''), s.slug, s.name, s.category, s.description, coalesce(s.documentation_url, ''),
  s.owner, s.purpose, s.endpoints, s.enabled, s.builtin, s.version, s.created_at, s.updated_at,
  CASE WHEN s.category = 'mcp' THEN coalesce((SELECT g FROM unnest(s.endpoints) e, afs.mcp_governed_host(e) g WHERE g IS NOT NULL LIMIT 1), '') ELSE '' END`

func scanExtSystemInto(row pgx.Row, extra ...any) (domain.ExternalSystem, error) {
	var x domain.ExternalSystem
	var cat string
	var v int64
	dst := append([]any{&x.ID, &x.TeamID, &x.Slug, &x.Name, &cat, &x.Description, &x.DocsURL, &x.Owner, &x.Purpose, &x.Endpoints,
		&x.Enabled, &x.Builtin, &v, &x.CreatedAt, &x.UpdatedAt, &x.GovernedBy}, extra...)
	err := row.Scan(dst...)
	x.Category, x.Version = domain.ExtCategory(cat), domain.Version(v)
	return x, err
}

func scanExtAccess(row pgx.Row) (domain.ExternalSystemAccess, error) {
	var at *string
	x, err := scanExtSystemInto(row, &at)
	a := domain.ExternalSystemAccess{ExternalSystem: x}
	if at != nil {
		a.DisabledAt = domain.ExtSystemDisabledAt(*at)
	}
	return a, err
}

// teamDisabledAt is the level that has a system off for the team as a whole ($1 team id, s the
// system): the service or the team's own switch of the entry, or the team's opt-out.
const teamDisabledAt = `CASE
  WHEN NOT s.enabled THEN CASE WHEN s.team_id IS NULL THEN 'service' ELSE 'team' END
  WHEN EXISTS (SELECT 1 FROM afs.external_system_optouts o
               WHERE o.team_id = $1 AND o.agent_id IS NULL AND o.pool_id IS NULL AND o.system_id = s.id) THEN 'team'
END`

// agentDisabledAt adds the agent's own opt-out ($2 agent id) to teamDisabledAt.
const agentDisabledAt = `CASE
  WHEN NOT s.enabled THEN CASE WHEN s.team_id IS NULL THEN 'service' ELSE 'team' END
  WHEN EXISTS (SELECT 1 FROM afs.external_system_optouts o
               WHERE o.team_id = $1 AND o.agent_id IS NULL AND o.pool_id IS NULL AND o.system_id = s.id) THEN 'team'
  WHEN EXISTS (SELECT 1 FROM afs.external_system_optouts o WHERE o.agent_id = $2 AND o.system_id = s.id) THEN 'agent'
END`

const teamVisible = `(s.team_id IS NULL OR s.team_id = $1)`

// TeamMayDeclare implements extsys.Repository.
func (*ExtSystemRepo) TeamMayDeclare(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var may bool
	err = pt.QueryRow(ctx, `SELECT may_add_external_systems FROM afs.teams WHERE id = $1 AND state <> 'deleted'`, teamID).Scan(&may)
	return may, MapError(err, OpRead, "team")
}

// ListTeam implements extsys.Repository.
func (*ExtSystemRepo) ListTeam(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+extSystemCols+`, `+teamDisabledAt+` FROM afs.external_systems s
WHERE `+teamVisible+` AND s.id > $2 ORDER BY s.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "external system")
	}
	return collectAccess(rows)
}

func collectAccess(rows pgx.Rows) ([]domain.ExternalSystemAccess, error) {
	defer rows.Close()
	out := []domain.ExternalSystemAccess{}
	for rows.Next() {
		a, err := scanExtAccess(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "external system")
		}
		out = append(out, a)
	}
	return out, MapError(rows.Err(), OpRead, "external system")
}

// GetTeam implements extsys.Repository.
func (*ExtSystemRepo) GetTeam(ctx context.Context, tx port.Tx, teamID, systemID string, forUpdate bool) (domain.ExternalSystemAccess, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	q := `SELECT ` + extSystemCols + `, ` + teamDisabledAt + ` FROM afs.external_systems s WHERE ` + teamVisible + ` AND s.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF s`
	}
	a, err := scanExtAccess(pt.QueryRow(ctx, q, teamID, systemID))
	return a, MapError(err, OpRead, "external system")
}

// CatalogSlugExists implements extsys.Repository.
func (*ExtSystemRepo) CatalogSlugExists(ctx context.Context, tx port.Tx, slug string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.external_systems WHERE team_id IS NULL AND slug = $1)`, slug).Scan(&ok)
	return ok, MapError(err, OpRead, "external system")
}

// Insert implements extsys.Repository.
func (*ExtSystemRepo) Insert(ctx context.Context, tx port.Tx, s domain.ExternalSystem) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.external_systems (id, team_id, slug, name, category, description, documentation_url, owner, purpose, endpoints, enabled)
VALUES ($1, nullif($2, ''), $3, $4, $5, $6, nullif($7, ''), $8, $9, $10, $11)`,
		s.ID, s.TeamID, s.Slug, s.Name, string(s.Category), s.Description, s.DocsURL, s.Owner, s.Purpose, s.Endpoints, s.Enabled)
	return MapError(err, OpWrite, "external system")
}

// Update implements extsys.Repository.
func (*ExtSystemRepo) Update(ctx context.Context, tx port.Tx, s domain.ExternalSystem, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.external_systems SET name = $2, description = $3, documentation_url = nullif($4, ''), owner = $5, purpose = $6,
  endpoints = $7, enabled = $8 WHERE id = $1 AND version = $9`,
		s.ID, s.Name, s.Description, s.DocsURL, s.Owner, s.Purpose, s.Endpoints, s.Enabled, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "external system")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "external system", `SELECT 1 FROM afs.external_systems WHERE id = $1`, s.ID)
	}
	return nil
}

// Delete implements extsys.Repository. Switches (opt-outs) of the system go with it.
func (*ExtSystemRepo) Delete(ctx context.Context, tx port.Tx, teamID, systemID string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.external_systems WHERE id = $1 AND team_id = $2 AND version = $3`, systemID, teamID, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "external system")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "external system", `SELECT 1 FROM afs.external_systems WHERE id = $1 AND team_id = $2`, systemID, teamID)
	}
	return nil
}

// SetTeamOff implements extsys.Repository: a team-wide switch is an opt-out row with no agent and no pool.
func (*ExtSystemRepo) SetTeamOff(ctx context.Context, tx port.Tx, teamID, systemID, optOutID string, off bool) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	return setOptOut(ctx, pt, optOutID, teamID, "", systemID, off)
}

// SetAgentOff implements extsys.Repository.
func (*ExtSystemRepo) SetAgentOff(ctx context.Context, tx port.Tx, teamID, agentID, systemID, optOutID string, off bool) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	return setOptOut(ctx, pt, optOutID, teamID, agentID, systemID, off)
}

// setOptOut writes (with id optOutID) or removes the opt-out of (team, agent or whole team, system);
// both directions are idempotent.
func setOptOut(ctx context.Context, pt *Tx, optOutID, teamID, agentID, systemID string, off bool) error {
	if off {
		_, err := pt.Exec(ctx, `INSERT INTO afs.external_system_optouts (id, team_id, agent_id, system_id)
VALUES ($1, $2, nullif($3, ''), $4) ON CONFLICT ON CONSTRAINT external_system_optouts_owner_system_key DO NOTHING`,
			optOutID, teamID, agentID, systemID)
		return MapError(err, OpWrite, "external system")
	}
	_, err := pt.Exec(ctx, `DELETE FROM afs.external_system_optouts WHERE team_id = $1 AND agent_id IS NOT DISTINCT FROM nullif($2, '')
  AND pool_id IS NULL AND system_id = $3`, teamID, agentID, systemID)
	return MapError(err, OpDelete, "external system")
}

// AgentExists implements extsys.Repository.
func (*ExtSystemRepo) AgentExists(ctx context.Context, tx port.Tx, teamID, agentID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	return agentExists(ctx, pt, teamID, agentID)
}

func agentExists(ctx context.Context, pt *Tx, teamID, agentID string) (bool, error) {
	var ok bool
	err := pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.agents WHERE team_id = $1 AND id = $2 AND deleted_at IS NULL)`, teamID, agentID).Scan(&ok)
	return ok, MapError(err, OpRead, "agent")
}

// ListAgent implements extsys.Repository.
func (*ExtSystemRepo) ListAgent(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+extSystemCols+`, `+agentDisabledAt+` FROM afs.external_systems s
WHERE `+teamVisible+` AND s.id > $3 ORDER BY s.id LIMIT $4`, teamID, agentID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "external system")
	}
	return collectAccess(rows)
}

// GetAgent implements extsys.Repository.
func (*ExtSystemRepo) GetAgent(ctx context.Context, tx port.Tx, teamID, agentID, systemID string) (domain.ExternalSystemAccess, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	a, err := scanExtAccess(pt.QueryRow(ctx, `SELECT `+extSystemCols+`, `+agentDisabledAt+` FROM afs.external_systems s
WHERE `+teamVisible+` AND s.id = $3`, teamID, agentID, systemID))
	return a, MapError(err, OpRead, "external system")
}
