// Package mcpaccess binds the mcp member of CreateAgent, UpdateAgent, CreatePool and UpdatePool (API contract
// v1.6.0 §4.14): which of the team's switched-on MCP servers an agent or pool reaches, its mode and its own block
// rules. The agents and pools use cases call it inside their transaction, so the access, the row, the audit record
// and the event commit together.
package mcpaccess

import (
	"context"
	"errors"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Subject kinds.
const (
	KindAgent = "agent"
	KindPool  = "pool"
)

// Store is the MCP access store of agents and pools.
type Store interface {
	// TeamServers returns the ids of the MCP servers switched on for the team.
	TeamServers(ctx context.Context, tx port.Tx, teamID string) ([]string, error)
	// DefaultMode returns mcp.default_agent_mode.
	DefaultMode(ctx context.Context, tx port.Tx) (domain.McpAgentMode, error)
	// Stored returns each subject's stored access (an empty mode follows the default) and the ids of the servers
	// it reaches (afs.effective_agent_mcp). A subject without a row is absent.
	Stored(ctx context.Context, tx port.Tx, teamID, kind string, ids []string) (map[string]Stored, error)
	// Put replaces the subject's access: the access and mode columns, its server rows and its own block rules
	// (blockIDs holds one new policy id per block).
	Put(ctx context.Context, tx port.Tx, teamID, kind, id string, m domain.AgentMcp, blockIDs []string) error
}

// Stored is a subject's access as stored, with the servers it reaches.
type Stored struct {
	Access    domain.AgentMcp
	Effective []string
	// EffectiveSlugs are the slugs of the Effective servers.
	EffectiveSlugs []string
}

// Write merges the patch into the stored access (the defaults for a new subject), validates it against the team's
// switched-on servers and stores it. It returns the audit forms before and after.
func Write(ctx context.Context, tx port.Tx, st Store, ids port.IDGen, teamID, kind, id string, p domain.McpAccessPatch) (before, after map[string]any, err error) {
	cur := domain.AgentMcp{Access: "all"}
	got, err := st.Stored(ctx, tx, teamID, kind, []string{id})
	if err != nil {
		return nil, nil, err
	}
	if s, ok := got[id]; ok {
		cur = s.Access
	}
	next := p.Merge(cur)
	on, err := st.TeamServers(ctx, tx, teamID)
	if err != nil {
		return nil, nil, err
	}
	if err := next.Validate(on); err != nil {
		return nil, nil, err
	}
	for _, b := range next.Blocks {
		if !slices.Contains(on, b.ServerID) {
			return nil, nil, domain.Validation(domain.FieldError{Field: "mcp.blocks", Reason: domain.ReasonMcpServerNotEnabledForTeam})
		}
	}
	blockIDs := make([]string, 0, len(next.Blocks))
	for range next.Blocks {
		bid, err := ids.NewID(domain.PrefixMcpPolicy)
		if err != nil {
			return nil, nil, err
		}
		blockIDs = append(blockIDs, bid.String())
	}
	if err := st.Put(ctx, tx, teamID, kind, id, next, blockIDs); err != nil {
		return nil, nil, err
	}
	return cur.Audit(), next.Audit(), nil
}

// Fill returns the mcp member of each subject; the mode that applies is the stored one or the default.
func Fill(ctx context.Context, tx port.Tx, st Store, teamID, kind string, ids []string) (map[string]*domain.McpAccessState, error) {
	if len(ids) == 0 {
		return map[string]*domain.McpAccessState{}, nil
	}
	got, err := st.Stored(ctx, tx, teamID, kind, ids)
	if err != nil {
		return nil, err
	}
	def, err := st.DefaultMode(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*domain.McpAccessState, len(got))
	for id, s := range got {
		mode := s.Access.Mode
		if mode == "" {
			mode = def
		}
		ids, blocks, eff := s.Access.ServerIDs, s.Access.Blocks, s.Effective
		if ids == nil {
			ids = []string{}
		}
		if blocks == nil {
			blocks = []domain.McpAgentBlock{}
		}
		if eff == nil {
			eff = []string{}
		}
		out[id] = &domain.McpAccessState{Access: s.Access.Access, ServerIDs: ids, Mode: mode, Blocks: blocks, Effective: eff,
			Env: domain.McpEnv(s.EffectiveSlugs)}
	}
	return out, nil
}

// ErrNoStore is returned for an mcp member when no MCP access store is wired.
var ErrNoStore = domain.Internal(errors.New("mcpaccess: no MCP access store"))
