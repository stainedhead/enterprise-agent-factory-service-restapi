package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// AgentSource implements agent.Source for the reconciler (afs_reconciler reads every table).
type AgentSource struct{ runner *TxRunner }

// NewAgentSource returns an AgentSource on the reconciler pool's TxRunner.
func NewAgentSource(runner *TxRunner) *AgentSource { return &AgentSource{runner: runner} }

var _ agent.Source = (*AgentSource)(nil)

// The template is retired when the agent references one that is disabled or whose allowed digest list no longer
// holds the agent's image digest (D54); an agent without a template reference is never flagged. The restart
// nonce is the id of the newest agent.restarted outbox event of the agent: every RestartAgent writes one, so
// the value changes on each restart and never otherwise.
const loadAgentSQL = `
SELECT a.id, a.name, a.description, a.template, a.image, a.persona, a.model_id, coalesce(a.default_route, ''), a.state, a.generation,
       a.resources, a.iam_identity, a.role,
       a.storage_size_gib, a.storage_class, a.storage_on_delete, a.snapshot_schedule, a.snapshot_retain_count,
       t.id, t.slug, t.namespace, t.state, t.isolation_tier, t.gateway_deployment,
       EXISTS (SELECT 1 FROM afs.work_orders w WHERE w.team_id = t.id AND w.status IN ('assigned', 'in_progress', 'pr_opened')),
       CASE WHEN tpl.id IS NULL THEN NULL
            WHEN NOT tpl.enabled THEN 'agent template ' || tpl.name || ' is disabled'
            WHEN substring(a.image FROM '@(sha256:[0-9a-f]{64})$') IS NULL
              OR NOT (substring(a.image FROM '@(sha256:[0-9a-f]{64})$') = ANY (tpl.allowed_image_digests))
              THEN 'the image digest is no longer allowed for agent template ' || tpl.name
       END,
       coalesce((SELECT max(o.id)::text FROM afs.outbox_events o
                  WHERE o.event_type = 'agent.restarted' AND o.aggregate_id = a.id), '')
  FROM afs.agents a
  JOIN afs.teams t ON t.id = a.team_id
  LEFT JOIN afs.agent_templates tpl ON tpl.id = a.template_id
 WHERE a.id = $1`

const loadBindingsSQL = `
SELECT b.secret_id, s.name, s.secrets_manager_arn, coalesce(s.current_version_id, ''), b.delivery, b.target,
       coalesce(s.external_system_id, ''), coalesce(x.disabled_at IS NOT NULL, false)
  FROM afs.agent_secret_bindings b
  JOIN afs.secrets s ON s.id = b.secret_id AND s.team_id = b.team_id
  LEFT JOIN afs.agent_external_access x ON x.agent_id = b.agent_id AND x.system_id = s.external_system_id
 WHERE b.agent_id = $1 ORDER BY b.delivery, b.target`

// LoadAgent implements agent.Source.
func (s *AgentSource) LoadAgent(ctx context.Context, id string) (agent.Desired, error) {
	var d agent.Desired
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var resources []byte
		var reason *string
		err = t.QueryRow(ctx, loadAgentSQL, id).Scan(&d.ID, &d.Name, &d.Description, &d.Template, &d.Image, &d.Persona, &d.ModelID, &d.Routes.DefaultRoute,
			&d.State, &d.Generation, &resources, &d.IAMIdentity, &d.Role,
			&d.Volume.SizeGiB, &d.Volume.StorageClass, &d.Volume.OnDelete, &d.Volume.SnapshotSchedule, &d.Volume.SnapshotRetain,
			&d.Team.ID, &d.Team.Slug, &d.Team.Namespace, &d.Team.State, &d.Team.IsolationTier, &d.Team.GatewayDeployment, &d.Team.WorkOrderAssigned,
			&reason, &d.RestartNonce)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("agent")
		}
		if err != nil {
			return MapError(err, OpRead, "agent")
		}
		if d.Resources, err = ParseAgentResources(resources); err != nil {
			return fmt.Errorf("postgres: resources of %s: %w", id, err)
		}
		if reason != nil {
			d.TemplateRetired, d.TemplateReason = true, *reason
		}
		return s.loadChildren(ctx, t, &d)
	})
	return d, err
}

func (s *AgentSource) loadChildren(ctx context.Context, t *Tx, d *agent.Desired) error {
	if !d.IsCollaboration() {
		var id string
		err := t.QueryRow(ctx, `SELECT coalesce(paperclip_agent_id, '') FROM afs.agent_collaboration
WHERE team_id = $1 AND agent_id = $2 AND connect AND link_state <> 'disabled'`, d.Team.ID, d.ID).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return MapError(err, OpRead, "collaboration link")
		default:
			d.Link = &agent.CollabLink{PaperclipAgentID: id}
			if err := t.QueryRow(ctx, `SELECT tool FROM afs.team_collaboration WHERE team_id = $1`, d.Team.ID).Scan(&d.Link.Tool); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return MapError(err, OpRead, "orchestration tool")
			}
		}
	}
	if err := collect(ctx, t, "agent models", `SELECT model_id FROM afs.effective_agent_models WHERE subject_kind = 'agent' AND subject_id = $1 ORDER BY model_id`,
		[]any{d.ID}, func(rows pgx.Rows) error {
			var m string
			if err := rows.Scan(&m); err != nil {
				return err
			}
			d.Models = append(d.Models, m)
			return nil
		}); err != nil {
		return err
	}
	var err error
	if d.ModelID, err = loadRoutes(ctx, t, "agent", d.ID, d.ModelID, &d.Routes); err != nil {
		return err
	}
	if err := loadMcp(ctx, t, "agent", d.ID, &d.Mcp); err != nil {
		return err
	}
	if d.ModelReason, d.PrevModelUnavailable, err = modelState(ctx, t, "agent", d.ID, d.ModelID, d.Routes.Effective(d.Models)); err != nil {
		return err
	}
	if d.IsCollaboration() {
		var mode, tool string
		var base *string
		err := t.QueryRow(ctx, `SELECT coalesce((SELECT registration_mode FROM afs.team_collaboration WHERE team_id = $1 AND agent_id = $2), 'manual'),
  coalesce((SELECT tool FROM afs.team_collaboration WHERE team_id = $1 AND agent_id = $2), 'paperclip'),
  (SELECT value #>> '{}' FROM afs.service_settings WHERE key = 'orchestration.public_base_domain' AND jsonb_typeof(value) = 'string')`, d.Team.ID, d.ID).Scan(&mode, &tool, &base)
		if err != nil {
			return MapError(err, OpRead, "collaboration")
		}
		d.Collab = &agent.Collab{Mode: mode, Tool: tool}
		if base != nil && *base != "" {
			d.Collab.Host = d.Team.Slug + "." + *base
		}
	}
	rows, err := t.Query(ctx, loadBindingsSQL, d.ID)
	if err != nil {
		return MapError(err, OpRead, "agent bindings")
	}
	for rows.Next() {
		var b agent.Binding
		if err := rows.Scan(&b.SecretID, &b.SecretName, &b.SecretARN, &b.VersionID, &b.Delivery, &b.Target, &b.ExternalSystemID, &b.SystemOff); err != nil {
			rows.Close()
			return MapError(err, OpRead, "agent bindings")
		}
		d.Bindings = append(d.Bindings, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MapError(err, OpRead, "agent bindings")
	}

	rows, err = t.Query(ctx, `SELECT name, resolved_value FROM afs.agent_config_resolved WHERE agent_id = $1 AND resolved_value IS NOT NULL ORDER BY name`, d.ID)
	if err != nil {
		return MapError(err, OpRead, "agent configuration")
	}
	for rows.Next() {
		var c agent.ConfigValue
		if err := rows.Scan(&c.Name, &c.Value); err != nil {
			rows.Close()
			return MapError(err, OpRead, "agent configuration")
		}
		d.Config = append(d.Config, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MapError(err, OpRead, "agent configuration")
	}

	rows, err = t.Query(ctx, `SELECT profile_name, role_arn, external_id, max_session_seconds, status FROM afs.agent_role_links WHERE agent_id = $1 ORDER BY profile_name`, d.ID)
	if err != nil {
		return MapError(err, OpRead, "agent role links")
	}
	for rows.Next() {
		var l agent.RoleLink
		if err := rows.Scan(&l.ProfileName, &l.RoleARN, &l.ExternalID, &l.MaxSessionSeconds, &l.Status); err != nil {
			rows.Close()
			return MapError(err, OpRead, "agent role links")
		}
		d.RoleLinks = append(d.RoleLinks, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MapError(err, OpRead, "agent role links")
	}

	rows, err = t.Query(ctx, `SELECT key, value FROM afs.agent_effective_tags WHERE agent_id = $1`, d.ID)
	if err != nil {
		return MapError(err, OpRead, "agent tags")
	}
	defer rows.Close()
	d.Tags = map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return MapError(err, OpRead, "agent tags")
		}
		d.Tags[k] = v
	}
	return rows.Err()
}

// ListTeamAgents implements agent.Source.
func (s *AgentSource) ListTeamAgents(ctx context.Context, teamID string) ([]agent.Listed, error) {
	var out []agent.Listed
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT a.id, a.name, a.state, t.namespace FROM afs.agents a JOIN afs.teams t ON t.id = a.team_id
		   WHERE a.team_id = $1 AND a.state <> 'deleted' ORDER BY a.id`, teamID)
		if err != nil {
			return MapError(err, OpRead, "team agents")
		}
		defer rows.Close()
		for rows.Next() {
			var l agent.Listed
			if err := rows.Scan(&l.ID, &l.Name, &l.State, &l.Namespace); err != nil {
				return MapError(err, OpRead, "team agents")
			}
			out = append(out, l)
		}
		return rows.Err()
	})
	return out, err
}

// ParseAgentResources reads agents.resources ({"cpu":"500m","memory":"1Gi"}); numbers are accepted for
// cpu.
func ParseAgentResources(raw []byte) (agent.Resources, error) {
	var r agent.Resources
	if len(raw) == 0 {
		return r, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return r, err
	}
	str := func(key string) (string, error) {
		v, ok := m[key]
		if !ok || string(v) == "null" {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s, nil
		}
		var n json.Number
		if err := json.Unmarshal(v, &n); err != nil {
			return "", fmt.Errorf("resources %s must be a string or a number", key)
		}
		return n.String(), nil
	}
	var err error
	if r.CPU, err = str("cpu"); err != nil {
		return r, err
	}
	r.Memory, err = str("memory")
	return r, err
}
