package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// CollaborationStatusKind is the resource_status kind of a team's collaboration resource; the
// resource id is the team id (RC-24).
const CollaborationStatusKind = "team_collaboration"

// UnmanagedCondition is the condition type that carries the names of Paperclip agents AFS does not
// manage (RC-26). The names are the sanitized, comma separated condition message.
const UnmanagedCondition = "UnmanagedPaperclipAgents"

// CollabRepo implements collab.Repository on afs.team_collaboration and afs.agent_collaboration.
type CollabRepo struct{}

// NewCollabRepo returns a CollabRepo.
func NewCollabRepo() *CollabRepo { return &CollabRepo{} }

var _ collab.Repository = (*CollabRepo)(nil)

const collabSelect = `SELECT c.team_id, c.agent_id, c.web_ui_enabled, c.rest_api_enabled, c.registration_mode, c.version,
  c.generation, c.created_at, c.created_by, c.updated_at, t.slug, t.state, t.may_expose_collaboration, a.state,
  coalesce(p.collab_client_id, ''), rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id
JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id
LEFT JOIN afs.identity_providers p ON p.id = coalesce(t.identity_provider_id, (SELECT d.id FROM afs.identity_providers d WHERE d.is_default))
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + CollaborationStatusKind + `' AND rs.resource_id = c.team_id`

func scanCollab(row pgx.Row) (domain.TeamCollaboration, error) {
	var (
		c            domain.TeamCollaboration
		mode, tstate string
		astate       string
		version, gen int64
		phase        *string
		obs          *int64
		conds        []byte
		lastRec      *time.Time
	)
	err := row.Scan(&c.TeamID, &c.AgentID, &c.WebUIEnabled, &c.RESTAPIEnabled, &mode, &version, &gen, &c.CreatedAt, &c.CreatedBy,
		&c.UpdatedAt, &c.TeamSlug, &tstate, &c.MayExpose, &astate, &c.ProviderClientID, &phase, &obs, &conds, &lastRec)
	if err != nil {
		return domain.TeamCollaboration{}, err
	}
	c.Mode, c.Version, c.Generation = domain.RegistrationMode(mode), domain.Version(version), domain.Generation(gen)
	c.TeamState, c.AgentState = domain.TeamState(tstate), domain.AgentState(astate)
	c.Status = domain.PendingStatus(c.CreatedAt)
	if phase != nil {
		c.Status.Phase = domain.Phase(*phase)
		if obs != nil {
			c.Status.ObservedGeneration = *obs
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &c.Status.Conditions); err != nil {
				return domain.TeamCollaboration{}, err
			}
		}
		if lastRec != nil {
			c.Status.UpdatedAt = lastRec.UTC()
		}
	}
	if c.Status.Conditions == nil {
		c.Status.Conditions = []domain.Condition{}
	}
	c.Unmanaged = unmanagedNames(c.Status.Conditions)
	return c, nil
}

// unmanagedNames reads the names from the UnmanagedPaperclipAgents condition (RD-12).
func unmanagedNames(conds []domain.Condition) []string {
	out := []string{}
	for _, c := range conds {
		if c.Type != UnmanagedCondition || c.Status != domain.ConditionTrue || strings.TrimSpace(c.Message) == "" {
			continue
		}
		for _, n := range strings.Split(c.Message, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

// Get implements collab.Repository.
func (*CollabRepo) Get(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (domain.TeamCollaboration, bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.TeamCollaboration{}, false, err
	}
	q := collabSelect + ` WHERE c.team_id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF c`
	}
	c, err := scanCollab(pt.QueryRow(ctx, q, teamID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.TeamCollaboration{}, false, nil
	}
	return c, err == nil, MapError(err, OpRead, "collaboration")
}

// Update implements collab.Repository.
func (*CollabRepo) Update(ctx context.Context, tx port.Tx, teamID string, web, rest bool, mode domain.RegistrationMode) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.team_collaboration SET web_ui_enabled = $2, rest_api_enabled = $3, registration_mode = $4 WHERE team_id = $1`,
		teamID, web, rest, string(mode))
	return MapError(err, OpWrite, "collaboration")
}

// ListLinks implements collab.Repository.
func (*CollabRepo) ListLinks(ctx context.Context, tx port.Tx, teamID string, f collab.LinkFilter, after string, limit int) ([]domain.CollaborationLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	connect := f.Connect
	rows, err := pt.Query(ctx, `SELECT a.id, a.name, coalesce(ac.connect, false), coalesce(ac.paperclip_agent_id, ''),
  coalesce(ac.link_state, 'disabled'), ac.last_synced_at, coalesce(ac.last_error, ''), coalesce(ac.version, 1)
FROM afs.agents a
LEFT JOIN afs.agent_collaboration ac ON ac.team_id = a.team_id AND ac.agent_id = a.id
WHERE a.team_id = $1 AND a.role = 'worker' AND a.deleted_at IS NULL AND a.state NOT IN ('deleting', 'deleted')
  AND a.id > $2 AND ($3 = '' OR coalesce(ac.link_state, 'disabled') = $3) AND ($4::boolean IS NULL OR coalesce(ac.connect, false) = $4)
ORDER BY a.id LIMIT $5`, teamID, after, string(f.State), connect, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "collaboration link")
	}
	defer rows.Close()
	out := []domain.CollaborationLink{}
	for rows.Next() {
		var (
			l     domain.CollaborationLink
			state string
			v     int64
		)
		if err := rows.Scan(&l.AgentID, &l.AgentName, &l.Connect, &l.PaperclipAgentID, &state, &l.LastSyncedAt, &l.LastError, &v); err != nil {
			return nil, MapError(err, OpRead, "collaboration link")
		}
		l.LinkState, l.Version = domain.LinkState(state), domain.Version(v)
		out = append(out, l)
	}
	return out, MapError(rows.Err(), OpRead, "collaboration link")
}

// Agent implements collab.Repository.
func (*CollabRepo) Agent(ctx context.Context, tx port.Tx, teamID, agentID string) (collab.AgentRef, bool, error) {
	if strings.HasPrefix(agentID, "pool_") {
		return collab.AgentRef{ID: agentID, Pool: true}, true, nil
	}
	pt, err := Unwrap(tx)
	if err != nil {
		return collab.AgentRef{}, false, err
	}
	var a collab.AgentRef
	var role, state string
	err = pt.QueryRow(ctx, `SELECT id, name, role, state FROM afs.agents WHERE team_id = $1 AND id = $2 AND deleted_at IS NULL
  AND state NOT IN ('deleting', 'deleted')`, teamID, agentID).Scan(&a.ID, &a.Name, &role, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.AgentRef{}, false, nil
	}
	a.Role, a.State = domain.AgentRole(role), domain.AgentState(state)
	return a, err == nil, MapError(err, OpRead, "agent")
}

// Link implements collab.Repository.
func (*CollabRepo) Link(ctx context.Context, tx port.Tx, teamID, agentID string, forUpdate bool) (domain.CollaborationLink, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.CollaborationLink{}, err
	}
	l := domain.CollaborationLink{AgentID: agentID, LinkState: domain.LinkDisabled, Version: 1}
	if err := pt.QueryRow(ctx, `SELECT name FROM afs.agents WHERE team_id = $1 AND id = $2`, teamID, agentID).Scan(&l.AgentName); err != nil {
		return l, MapError(err, OpRead, "agent")
	}
	q := `SELECT connect, coalesce(paperclip_agent_id, ''), link_state, last_synced_at, coalesce(last_error, ''), version
FROM afs.agent_collaboration WHERE team_id = $1 AND agent_id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var state string
	var v int64
	err = pt.QueryRow(ctx, q, teamID, agentID).Scan(&l.Connect, &l.PaperclipAgentID, &state, &l.LastSyncedAt, &l.LastError, &v)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, nil
	}
	l.LinkState, l.Version = domain.LinkState(state), domain.Version(v)
	return l, MapError(err, OpRead, "collaboration link")
}

// SetLink implements collab.Repository. A nil paperclipAgentID keeps the stored value.
func (*CollabRepo) SetLink(ctx context.Context, tx port.Tx, teamID, agentID string, connect bool, paperclipAgentID *string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_collaboration (team_id, agent_id, connect, paperclip_agent_id) VALUES ($1, $2, $3, $4)
ON CONFLICT (team_id, agent_id) DO UPDATE SET connect = EXCLUDED.connect,
  paperclip_agent_id = coalesce(EXCLUDED.paperclip_agent_id, afs.agent_collaboration.paperclip_agent_id)`,
		teamID, agentID, connect, paperclipAgentID)
	return MapError(err, OpWrite, "collaboration link")
}

// BumpAgent implements collab.Repository: the generation rises, so the reconciler rolls the pod.
func (*CollabRepo) BumpAgent(ctx context.Context, tx port.Tx, teamID, agentID string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.agents SET generation = generation + 1 WHERE team_id = $1 AND id = $2`, teamID, agentID)
	return MapError(err, OpWrite, "agent")
}

// BootSecret implements collab.Repository.
func (*CollabRepo) BootSecret(ctx context.Context, tx port.Tx, teamID, agentID, name string) (collab.SecretRef, bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return collab.SecretRef{}, false, err
	}
	var s collab.SecretRef
	err = pt.QueryRow(ctx, `SELECT id, secrets_manager_arn FROM afs.secrets WHERE team_id = $1 AND agent_id = $2 AND name = $3 AND managed_by = 'afs'`,
		teamID, agentID, name).Scan(&s.ID, &s.ARN)
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.SecretRef{}, false, nil
	}
	return s, err == nil, MapError(err, OpRead, "secret")
}

// SetSecretVersion implements collab.Repository.
func (*CollabRepo) SetSecretVersion(ctx context.Context, tx port.Tx, teamID, secretID, versionID string, at time.Time) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.secrets SET current_version_id = $3, last_changed_at = $4 WHERE team_id = $1 AND id = $2`, teamID, secretID, versionID, at)
	return MapError(err, OpWrite, "secret")
}

// Endpoints implements collab.Repository.
func (*CollabRepo) Endpoints(ctx context.Context, tx port.Tx, f collab.EndpointFilter, after string, limit int) ([]collab.EndpointRow, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, strings.Replace(collabSelect, "SELECT c.team_id,", "SELECT acc.last_access, c.team_id,", 1)+
		` LEFT JOIN LATERAL (SELECT max(x.bucket_start) AS last_access FROM afs.collaboration_access x
   WHERE x.team_id = c.team_id AND x.outcome IN ('signed_in', 'allowed')) acc ON true
WHERE c.team_id > $1 AND ($2 = '' OR c.team_id = $2) AND ($3 = '' OR coalesce(rs.phase, 'Pending') = $3)
ORDER BY c.team_id LIMIT $4`, after, f.TeamID, string(f.State), limit)
	if err != nil {
		return nil, MapError(err, OpRead, "collaboration endpoint")
	}
	defer rows.Close()
	out := []collab.EndpointRow{}
	for rows.Next() {
		var last *time.Time
		c, err := scanCollab(prefixScan{rows, &last})
		if err != nil {
			return nil, MapError(err, OpRead, "collaboration endpoint")
		}
		out = append(out, collab.EndpointRow{Collab: c, Phase: c.Status.Phase, LastAccessAt: last})
	}
	return out, MapError(rows.Err(), OpRead, "collaboration endpoint")
}

// prefixScan scans one leading column into dst before the columns scanCollab reads.
type prefixScan struct {
	pgx.Rows
	dst **time.Time
}

// Scan implements pgx.Row.
func (p prefixScan) Scan(dest ...any) error {
	return p.Rows.Scan(append([]any{p.dst}, dest...)...)
}
