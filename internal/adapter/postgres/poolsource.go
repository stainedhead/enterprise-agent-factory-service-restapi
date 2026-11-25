package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// PoolSource implements pool.Source for the reconciler (afs_reconciler reads every table).
type PoolSource struct{ runner *TxRunner }

// NewPoolSource returns a PoolSource on the reconciler pool's TxRunner.
func NewPoolSource(runner *TxRunner) *PoolSource { return &PoolSource{runner: runner} }

var _ pool.Source = (*PoolSource)(nil)

// The template is retired when the pool references one that is disabled or whose allowed digest list no longer
// holds the pool's image digest (D54); a pool without a template reference is never flagged.
const loadPoolSQL = `
SELECT p.id, p.name, p.description, p.template, p.image, p.persona, p.model_id, coalesce(p.default_route, ''), p.state, p.generation,
       p.workers, p.resources, p.scratch_size_gib, p.shared_volume,
       t.id, t.slug, t.namespace, t.state, t.isolation_tier, t.gateway_deployment,
       EXISTS (SELECT 1 FROM afs.work_orders w WHERE w.team_id = t.id AND w.status IN ('assigned', 'in_progress', 'pr_opened')),
       CASE WHEN tpl.id IS NULL THEN NULL
            WHEN NOT tpl.enabled THEN 'agent template ' || tpl.name || ' is disabled'
            WHEN substring(p.image FROM '@(sha256:[0-9a-f]{64})$') IS NULL
              OR NOT (substring(p.image FROM '@(sha256:[0-9a-f]{64})$') = ANY (tpl.allowed_image_digests))
              THEN 'the image digest is no longer allowed for agent template ' || tpl.name
       END
  FROM afs.agent_pools p
  JOIN afs.teams t ON t.id = p.team_id
  LEFT JOIN afs.agent_templates tpl ON tpl.id = p.template_id
 WHERE p.id = $1`

const loadPoolBindingsSQL = `
SELECT b.secret_id, s.name, s.secrets_manager_arn, coalesce(s.current_version_id, ''), b.delivery, b.target,
       coalesce(s.external_system_id, ''), coalesce(x.disabled_at IS NOT NULL, false)
  FROM afs.pool_secret_bindings b
  JOIN afs.secrets s ON s.id = b.secret_id AND s.team_id = b.team_id
  LEFT JOIN afs.pool_external_access x ON x.pool_id = b.pool_id AND x.system_id = s.external_system_id
 WHERE b.pool_id = $1 ORDER BY b.delivery, b.target`

// A pool's handoffs name the pools it sends to and receives from; a pool being deleted is no longer a peer, so
// no queue is made or granted for it.
const (
	loadPoolSendsSQL = `SELECT p.id, p.name FROM afs.pool_handoffs h JOIN afs.agent_pools p ON p.id = h.to_pool_id
 WHERE h.from_pool_id = $1 AND p.state IN ('enabled', 'paused') ORDER BY p.name`
	loadPoolReceivesSQL = `SELECT p.id, p.name FROM afs.pool_handoffs h JOIN afs.agent_pools p ON p.id = h.from_pool_id
 WHERE h.to_pool_id = $1 AND p.state IN ('enabled', 'paused') ORDER BY p.name`
)

// LoadPool implements pool.Source.
func (s *PoolSource) LoadPool(ctx context.Context, id string) (pool.Desired, error) {
	var d pool.Desired
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var resources []byte
		var reason *string
		err = t.QueryRow(ctx, loadPoolSQL, id).Scan(&d.ID, &d.Name, &d.Description, &d.Template, &d.Image, &d.Persona, &d.ModelID, &d.Routes.DefaultRoute, &d.State, &d.Generation,
			&d.Workers, &resources, &d.ScratchGiB, &d.SharedVolume,
			&d.Team.ID, &d.Team.Slug, &d.Team.Namespace, &d.Team.State, &d.Team.IsolationTier, &d.Team.GatewayDeployment, &d.Team.WorkOrderAssigned, &reason)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("pool")
		}
		if err != nil {
			return MapError(err, OpRead, "pool")
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

func (s *PoolSource) loadChildren(ctx context.Context, t *Tx, d *pool.Desired) error {
	if err := collect(ctx, t, "pool models", `SELECT model_id FROM afs.effective_agent_models WHERE subject_kind = 'pool' AND subject_id = $1 ORDER BY model_id`,
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
	if d.ModelID, err = loadRoutes(ctx, t, "pool", d.ID, d.ModelID, &d.Routes); err != nil {
		return err
	}
	if d.ModelReason, d.PrevModelUnavailable, err = modelState(ctx, t, "pool", d.ID, d.ModelID, d.Routes.Effective(d.Models)); err != nil {
		return err
	}
	if err := collect(ctx, t, "pool bindings", loadPoolBindingsSQL, []any{d.ID}, func(rows pgx.Rows) error {
		var b agent.Binding
		if err := rows.Scan(&b.SecretID, &b.SecretName, &b.SecretARN, &b.VersionID, &b.Delivery, &b.Target, &b.ExternalSystemID, &b.SystemOff); err != nil {
			return err
		}
		d.Bindings = append(d.Bindings, b)
		return nil
	}); err != nil {
		return err
	}
	if err := collect(ctx, t, "pool configuration", `SELECT name, resolved_value FROM afs.pool_config_resolved WHERE pool_id = $1 AND resolved_value IS NOT NULL ORDER BY name`,
		[]any{d.ID}, func(rows pgx.Rows) error {
			var c agent.ConfigValue
			if err := rows.Scan(&c.Name, &c.Value); err != nil {
				return err
			}
			d.Config = append(d.Config, c)
			return nil
		}); err != nil {
		return err
	}
	if err := collect(ctx, t, "pool role links", `SELECT id, account_id, role_arn, profile_name, external_id, max_session_seconds, status, last_verified_at, updated_at,
       afs.account_allowed(team_id, account_id) FROM afs.pool_role_links WHERE pool_id = $1 ORDER BY profile_name`,
		[]any{d.ID}, func(rows pgx.Rows) error {
			var l pool.RoleLink
			var status string
			if err := rows.Scan(&l.ID, &l.AccountID, &l.RoleARN, &l.ProfileName, &l.ExternalID, &l.MaxSessionSeconds, &status, &l.LastVerified, &l.UpdatedAt, &l.Allowed); err != nil {
				return err
			}
			l.Status = domain.RoleLinkStatus(status)
			d.RoleLinks = append(d.RoleLinks, l)
			return nil
		}); err != nil {
		return err
	}
	d.Tags = map[string]string{}
	if err := collect(ctx, t, "pool tags", `SELECT key, value FROM afs.team_effective_tags WHERE team_id = $1 AND value IS NOT NULL`, []any{d.Team.ID}, func(rows pgx.Rows) error {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		d.Tags[k] = v
		return nil
	}); err != nil {
		return err
	}
	if err := collect(ctx, t, "pool external systems", `SELECT system_id, slug, category, endpoints, coalesce(disabled_at, '') FROM afs.pool_external_access WHERE pool_id = $1 ORDER BY slug`,
		[]any{d.ID}, func(rows pgx.Rows) error {
			var sys egress.System
			if err := rows.Scan(&sys.ID, &sys.Slug, &sys.Category, &sys.Endpoints, &sys.DisabledAt); err != nil {
				return err
			}
			d.Systems = append(d.Systems, sys)
			return nil
		}); err != nil {
		return err
	}
	peers := func(sql string, into *[]pool.Peer) func(string) error {
		return func(what string) error {
			return collect(ctx, t, what, sql, []any{d.ID}, func(rows pgx.Rows) error {
				var p pool.Peer
				if err := rows.Scan(&p.ID, &p.Name); err != nil {
					return err
				}
				*into = append(*into, p)
				return nil
			})
		}
	}
	if err := peers(loadPoolSendsSQL, &d.SendsTo)("pool handoffs"); err != nil {
		return err
	}
	return peers(loadPoolReceivesSQL, &d.ReceivesFrom)("pool handoffs")
}

// ListTeamPools implements pool.Source.
func (s *PoolSource) ListTeamPools(ctx context.Context, teamID string) ([]pool.Listed, error) {
	var out []pool.Listed
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return collect(ctx, t, "team pools", `SELECT p.id, p.name, p.state, t.namespace FROM afs.agent_pools p JOIN afs.teams t ON t.id = p.team_id
		   WHERE p.team_id = $1 AND p.state <> 'deleted' ORDER BY p.id`, []any{teamID}, func(rows pgx.Rows) error {
			var l pool.Listed
			if err := rows.Scan(&l.ID, &l.Name, &l.State, &l.Namespace); err != nil {
				return err
			}
			out = append(out, l)
			return nil
		})
	})
	return out, err
}

// collect runs a query and calls scan for each row, mapping database errors with what as the subject.
func collect(ctx context.Context, t *Tx, what, sql string, args []any, scan func(pgx.Rows) error) error {
	rows, err := t.Query(ctx, sql, args...)
	if err != nil {
		return MapError(err, OpRead, what)
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return MapError(err, OpRead, what)
		}
	}
	return MapError(rows.Err(), OpRead, what)
}
