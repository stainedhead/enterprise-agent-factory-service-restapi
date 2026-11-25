package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// McpAccessRepo stores the MCP access of agents and pools (database contract §7.5): the mcp_access and mcp_mode
// columns, afs.agent_mcp_servers and afs.pool_mcp_servers, and the agent- and pool-level rules of afs.mcp_policies.
// The schema's guard refuses a server that is not switched on for the team.
type McpAccessRepo struct{}

// NewMcpAccessRepo returns an McpAccessRepo.
func NewMcpAccessRepo() *McpAccessRepo { return &McpAccessRepo{} }

var _ mcpaccess.Store = (*McpAccessRepo)(nil)

// subjectLinks names the server link table and its subject column.
func subjectLinks(kind string) (table, col string) {
	if kind == mcpaccess.KindPool {
		return "pool_mcp_servers", "pool_id"
	}
	return "agent_mcp_servers", "agent_id"
}

// TeamServers implements mcpaccess.Store.
func (*McpAccessRepo) TeamServers(ctx context.Context, tx port.Tx, teamID string) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	err = collect(ctx, pt, "team MCP servers", `SELECT server_id FROM afs.team_mcp_servers WHERE team_id = $1 AND enabled ORDER BY server_id`,
		[]any{teamID}, func(rows pgx.Rows) error {
			var id string
			err := rows.Scan(&id)
			out = append(out, id)
			return err
		})
	return out, err
}

// DefaultMode implements mcpaccess.Store.
func (*McpAccessRepo) DefaultMode(ctx context.Context, tx port.Tx) (domain.McpAgentMode, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var m string
	err = pt.QueryRow(ctx, `SELECT coalesce(afs.mcp_setting('mcp.default_agent_mode') #>> '{}', 'search')`).Scan(&m)
	return domain.McpAgentMode(m), MapError(err, OpRead, "service settings")
}

// Stored implements mcpaccess.Store.
func (*McpAccessRepo) Stored(ctx context.Context, tx port.Tx, teamID, kind string, ids []string) (map[string]mcpaccess.Stored, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	table, _, err := subjectTable(kind)
	if err != nil {
		return nil, err
	}
	links, col := subjectLinks(kind)
	out := map[string]mcpaccess.Stored{}
	err = collect(ctx, pt, kind, `SELECT x.id, x.mcp_access, coalesce(x.mcp_mode, ''),
  coalesce((SELECT array_agg(l.server_id ORDER BY l.server_id) FROM afs.`+links+` l WHERE l.team_id = x.team_id AND l.`+col+` = x.id), '{}'),
  coalesce((SELECT array_agg(DISTINCT e.server_id ORDER BY e.server_id) FROM afs.effective_agent_mcp e
             WHERE e.subject_kind = $3 AND e.team_id = x.team_id AND e.subject_id = x.id), '{}'),
  coalesce((SELECT array_agg(DISTINCT e.slug ORDER BY e.slug) FROM afs.effective_agent_mcp e
             WHERE e.subject_kind = $3 AND e.team_id = x.team_id AND e.subject_id = x.id), '{}')
FROM afs.`+table+` x WHERE x.team_id = $1 AND x.id = ANY($2) AND x.deleted_at IS NULL`, []any{teamID, ids, kind}, func(rows pgx.Rows) error {
		var (
			id, mode string
			s        mcpaccess.Stored
		)
		if err := rows.Scan(&id, &s.Access.Access, &mode, &s.Access.ServerIDs, &s.Effective, &s.EffectiveSlugs); err != nil {
			return err
		}
		s.Access.Mode = domain.McpAgentMode(mode)
		out[id] = s
		return nil
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	err = collect(ctx, pt, "MCP policy", `SELECT `+col+`, server_id, coalesce(name, ''), coalesce(class, '') FROM afs.mcp_policies
WHERE level = $1 AND team_id = $2 AND `+col+` = ANY($3) ORDER BY created_at, id`, []any{kind, teamID, ids}, func(rows pgx.Rows) error {
		var (
			id, class string
			b         = domain.McpAgentBlock{Effect: "block"}
		)
		if err := rows.Scan(&id, &b.ServerID, &b.Name, &class); err != nil {
			return err
		}
		b.Class = mcp.Class(class)
		if s, ok := out[id]; ok {
			s.Access.Blocks = append(s.Access.Blocks, b)
			out[id] = s
		}
		return nil
	})
	return out, err
}

// Put implements mcpaccess.Store. The columns are written only when they change, so a body that repeats the
// stored access does not raise the row's version a second time.
func (*McpAccessRepo) Put(ctx context.Context, tx port.Tx, teamID, kind, id string, m domain.AgentMcp, blockIDs []string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if len(blockIDs) != len(m.Blocks) {
		return domain.Internal(errors.New("postgres: one id per MCP block"))
	}
	table, _, err := subjectTable(kind)
	if err != nil {
		return err
	}
	links, col := subjectLinks(kind)
	if _, err := pt.Exec(ctx, `UPDATE afs.`+table+` SET mcp_access = $3, mcp_mode = nullif($4, '')
WHERE team_id = $1 AND id = $2 AND (mcp_access, mcp_mode) IS DISTINCT FROM ($3, nullif($4, ''))`, teamID, id, m.Access, string(m.Mode)); err != nil {
		return MapError(err, OpWrite, kind)
	}
	ids := m.ServerIDs
	if ids == nil {
		ids = []string{}
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.`+links+` WHERE team_id = $1 AND `+col+` = $2 AND server_id <> ALL($3)`, teamID, id, ids); err != nil {
		return MapError(err, OpDelete, "MCP server access")
	}
	if _, err := pt.Exec(ctx, `INSERT INTO afs.`+links+` (team_id, `+col+`, server_id) SELECT $1, $2, s FROM unnest($3::text[]) s
ON CONFLICT DO NOTHING`, teamID, id, ids); err != nil {
		return MapError(err, OpWrite, "MCP server access")
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.mcp_policies WHERE level = $1 AND team_id = $2 AND `+col+` = $3`, kind, teamID, id); err != nil {
		return MapError(err, OpDelete, "MCP policy")
	}
	for i, b := range m.Blocks {
		if _, err := pt.Exec(ctx, `INSERT INTO afs.mcp_policies (id, level, team_id, `+col+`, server_id, name, class, effect, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'block', clock_timestamp())`, blockIDs[i], kind, teamID, id, b.ServerID, nullIfEmpty(b.Name),
			nullIfEmpty(string(b.Class))); err != nil {
			return MapError(err, OpWrite, "MCP policy")
		}
	}
	return nil
}
