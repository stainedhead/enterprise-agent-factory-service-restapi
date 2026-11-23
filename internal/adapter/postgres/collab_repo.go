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

const collabSelect = `SELECT c.team_id, c.agent_id, a.name, c.tool, c.web_ui_exposure, c.web_api_exposure, t.max_collaboration_exposure,
  c.registration_mode, c.version, c.generation, c.created_at, c.created_by, c.updated_at, t.slug, t.state, a.state,
  coalesce(p.collab_client_id, ''), rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at,
  (SELECT count(*) FROM afs.agent_collaboration ac WHERE ac.team_id = c.team_id AND ac.connect),
  coalesce(ars.phase, 'Pending')
FROM afs.team_collaboration c
JOIN afs.teams t ON t.id = c.team_id
JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id
LEFT JOIN afs.identity_providers p ON p.id = coalesce(t.identity_provider_id, (SELECT d.id FROM afs.identity_providers d WHERE d.is_default))
LEFT JOIN afs.resource_status ars ON ars.resource_kind = 'agent' AND ars.resource_id = c.agent_id
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + CollaborationStatusKind + `' AND rs.resource_id = c.team_id`

func scanCollab(row pgx.Row) (domain.TeamCollaboration, error) {
	var (
		c                    domain.TeamCollaboration
		tool, ui, api, limit string
		mode, tstate, astate string
		version, gen         int64
		phase                *string
		obs                  *int64
		conds                []byte
		lastRec              *time.Time
		connected            int64
		agentPhase           string
	)
	err := row.Scan(&c.TeamID, &c.AgentID, &c.AgentName, &tool, &ui, &api, &limit, &mode, &version, &gen, &c.CreatedAt, &c.CreatedBy,
		&c.UpdatedAt, &c.TeamSlug, &tstate, &astate, &c.ProviderClientID, &phase, &obs, &conds, &lastRec, &connected, &agentPhase)
	if err != nil {
		return domain.TeamCollaboration{}, err
	}
	c.Tool, c.WebUIExposure, c.WebAPIExposure, c.Limit = domain.CollabTool(tool), domain.Exposure(ui), domain.Exposure(api), domain.Exposure(limit)
	c.Connected = int(connected)
	c.AgentPhase = domain.Phase(agentPhase)
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
	for _, cd := range c.Status.Conditions {
		if cd.Type == "ToolReady" && strings.HasPrefix(cd.Message, "version ") {
			c.ToolVersion = strings.TrimPrefix(cd.Message, "version ")
		}
	}
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
func (*CollabRepo) Update(ctx context.Context, tx port.Tx, teamID string, webUI, webAPI domain.Exposure, mode domain.RegistrationMode) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.team_collaboration SET web_ui_exposure = $2, web_api_exposure = $3, registration_mode = $4 WHERE team_id = $1`,
		teamID, string(webUI), string(webAPI), string(mode))
	return MapError(err, OpWrite, "orchestration")
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

// Endpoints implements collab.Repository. Rows with team id greater than after, in team order; the
// service filters by effective level because that is computed.
func (*CollabRepo) Endpoints(ctx context.Context, tx port.Tx, f collab.EndpointFilter, after string, limit int) ([]collab.EndpointRow, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, strings.Replace(collabSelect, "SELECT c.team_id,", "SELECT acc.last_access, c.team_id,", 1)+
		` LEFT JOIN LATERAL (SELECT max(x.bucket_start) AS last_access FROM afs.collaboration_access x
   WHERE x.team_id = c.team_id AND x.outcome IN ('signed_in', 'allowed')) acc ON true
WHERE c.team_id > $1 AND ($2 = '' OR c.team_id = $2) AND ($3 = '' OR coalesce(rs.phase, 'Pending') = $3) AND ($4 = '' OR c.tool = $4)
  AND (c.web_ui_exposure <> 'off' OR c.web_api_exposure <> 'off')
ORDER BY c.team_id LIMIT $5`, after, f.TeamID, string(f.State), string(f.Tool), limit)
	if err != nil {
		return nil, MapError(err, OpRead, "orchestration endpoint")
	}
	defer rows.Close()
	out := []collab.EndpointRow{}
	for rows.Next() {
		var last *time.Time
		c, err := scanCollab(prefixScan{rows, &last})
		if err != nil {
			return nil, MapError(err, OpRead, "orchestration endpoint")
		}
		out = append(out, collab.EndpointRow{Collab: c, Phase: c.Status.Phase, LastAccessAt: last})
	}
	return out, MapError(rows.Err(), OpRead, "orchestration endpoint")
}

// Migration implements collab.Repository: the teams with and without an orchestration agent, and
// the teams the backfill cannot reach.
func (*CollabRepo) Migration(ctx context.Context, tx port.Tx) (int, int, []domain.OrchestrationBlocked, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return 0, 0, nil, err
	}
	var total, with int
	err = pt.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE EXISTS (SELECT 1 FROM afs.team_collaboration c WHERE c.team_id = t.id))
FROM afs.teams t WHERE t.deleted_at IS NULL AND t.state NOT IN ('deleting', 'deleted')`).Scan(&total, &with)
	if err != nil {
		return 0, 0, nil, MapError(err, OpRead, "team")
	}
	blocked := []domain.OrchestrationBlocked{}
	if total == with {
		return total, with, blocked, nil
	}
	rt, err := toolRuntime(ctx, pt, "")
	if err != nil {
		return 0, 0, nil, err
	}
	rows, err := pt.Query(ctx, `SELECT t.id FROM afs.teams t WHERE t.deleted_at IS NULL AND t.state NOT IN ('deleting', 'deleted')
  AND NOT EXISTS (SELECT 1 FROM afs.team_collaboration c WHERE c.team_id = t.id) ORDER BY t.id`)
	if err != nil {
		return 0, 0, nil, MapError(err, OpRead, "team")
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, 0, nil, MapError(err, OpRead, "team")
		}
		if !rt.Ready() {
			blocked = append(blocked, domain.OrchestrationBlocked{TeamID: id, Reason: domain.ReasonOrchestrationRuntimeUnset})
		}
	}
	return total, with, blocked, MapError(rows.Err(), OpRead, "team")
}

// toolRuntime reads the entry of tool in orchestration.tools (the default tool when tool is empty) and
// the template it names.
func toolRuntime(ctx context.Context, pt *Tx, tool domain.CollabTool) (collab.ToolRuntime, error) {
	var raw []byte
	err := pt.QueryRow(ctx, `SELECT value FROM afs.service_settings WHERE key = 'orchestration.tools'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return collab.ToolRuntime{Tool: tool}, nil
	}
	if err != nil {
		return collab.ToolRuntime{}, MapError(err, OpRead, "setting")
	}
	var tools map[string]struct {
		Enabled         bool    `json:"enabled"`
		Default         bool    `json:"default"`
		TemplateID      *string `json:"templateId"`
		ImageRepository *string `json:"imageRepository"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return collab.ToolRuntime{}, domain.Internal(err)
	}
	if tool == "" {
		for k, v := range tools {
			if v.Default {
				tool = domain.CollabTool(k)
			}
		}
	}
	e, ok := tools[string(tool)]
	rt := collab.ToolRuntime{Tool: tool}
	if !ok {
		return rt, nil
	}
	rt.Enabled = e.Enabled
	if e.TemplateID != nil {
		rt.TemplateID = *e.TemplateID
	}
	if e.ImageRepository != nil {
		rt.ImageRepository = *e.ImageRepository
	}
	if rt.TemplateID != "" {
		err := pt.QueryRow(ctx, `SELECT harness, coalesce(current_image_digest, ''), enabled FROM afs.agent_templates WHERE id = $1`,
			rt.TemplateID).Scan(&rt.Harness, &rt.Digest, &rt.TemplateEnabled)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return rt, MapError(err, OpRead, "agent template")
		}
	}
	return rt, nil
}

// ToolRuntime implements collab.Repository.
func (*CollabRepo) ToolRuntime(ctx context.Context, tx port.Tx, tool domain.CollabTool) (collab.ToolRuntime, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return collab.ToolRuntime{}, err
	}
	return toolRuntime(ctx, pt, tool)
}

// SwitchTool implements collab.Repository: the snapshot request, the agent's template and image, and
// the team's tool, in the caller's transaction. The surfaces the new tool lacks go to off first, so the
// row never holds a level its tool cannot serve.
func (*CollabRepo) SwitchTool(ctx context.Context, tx port.Tx, in collab.SwitchToolInput) (string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_volume_snapshots (id, team_id, agent_id, reason, tool, retain_until)
VALUES ($1, $2, $3, 'pre_tool_change', $4, $5)`, in.SnapshotID, in.TeamID, in.AgentID, string(in.FromTool), in.RetainUntil)
	if err != nil {
		return "", MapError(err, OpWrite, "snapshot")
	}
	_, err = pt.Exec(ctx, `UPDATE afs.agents SET template = $3, template_id = $4, image = $5 WHERE team_id = $1 AND id = $2`,
		in.TeamID, in.AgentID, in.Runtime.Harness, in.Runtime.TemplateID, in.Runtime.ImageRepository+"@"+in.Runtime.Digest)
	if err != nil {
		return "", MapError(err, OpWrite, "agent")
	}
	ui, api := "web_ui_exposure", "web_api_exposure"
	set := "tool = $2, registration_mode = $3"
	if !in.ToTool.Supports(domain.SurfaceWebUI) {
		set += ", " + ui + " = 'off'"
	}
	if !in.ToTool.Supports(domain.SurfaceWebAPI) {
		set += ", " + api + " = 'off'"
	}
	_, err = pt.Exec(ctx, `UPDATE afs.team_collaboration SET `+set+` WHERE team_id = $1`, in.TeamID, string(in.ToTool), string(in.Mode))
	if err != nil {
		return "", MapError(err, OpWrite, "orchestration")
	}
	return in.SnapshotID, nil
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
