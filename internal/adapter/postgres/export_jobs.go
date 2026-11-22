package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/export"
)

// ExportJobs implements export.Store for the reconciler's login: it claims pending export requests,
// reads the desired state into a bundle and records the outcome. The reconciler may update only the
// outcome columns of export_bundles (database contract, 000014).
type ExportJobs struct {
	runner *TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
	now    func() time.Time
}

// NewExportJobs returns an ExportJobs.
func NewExportJobs(runner *TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen) *ExportJobs {
	return &ExportJobs{runner: runner, audit: audit, outbox: outbox, ids: ids, now: time.Now}
}

var _ export.Store = (*ExportJobs)(nil)

// Claim implements export.Store.
func (j *ExportJobs) Claim(ctx context.Context) (*export.Request, error) {
	var out *export.Request
	err := j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		out = nil
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var id string
		var summary []byte
		err = t.QueryRow(ctx, `UPDATE afs.export_bundles SET status = 'running'
WHERE id = (SELECT id FROM afs.export_bundles WHERE direction = 'export' AND status = 'pending'
            ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING id, summary`).Scan(&id, &summary)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var s struct {
			TeamIDs []string `json:"teamIds"`
		}
		_ = json.Unmarshal(summary, &s)
		out = &export.Request{ID: id, TeamIDs: s.TeamIDs}
		return nil
	})
	return out, MapError(err, OpWrite, "export")
}

// Build implements export.Store.
func (j *ExportJobs) Build(ctx context.Context, teamIDs []string, now time.Time) (domain.Bundle, error) {
	b := domain.Bundle{SchemaVersion: domain.BundleSchemaVersion, ExportedAt: now.UTC().Format(time.RFC3339),
		ExternalSystems: []domain.Row{}, Teams: []domain.TeamBundle{}}
	err := j.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		src := bundleSource{t: t}
		if b.ExternalSystems, err = src.rows(ctx, `SELECT `+obj("s", bundleExtSystemCols, "'builtin', s.builtin")+`
FROM afs.external_systems s WHERE s.team_id IS NULL ORDER BY s.slug`); err != nil {
			return err
		}
		ids, err := src.teamIDs(ctx, teamIDs)
		if err != nil {
			return err
		}
		for _, id := range ids {
			tb, err := src.team(ctx, id)
			if err != nil {
				return err
			}
			b.Teams = append(b.Teams, tb)
		}
		return nil
	})
	return b, MapError(err, OpRead, "export bundle")
}

// Succeed implements export.Store.
func (j *ExportJobs) Succeed(ctx context.Context, id string, r export.Result) error {
	return j.finish(ctx, id, domain.EventExportCompleted, "ExportCompleted", map[string]any{"exportId": id, "sha256": r.SHA256,
		"sizeBytes": r.SizeBytes, "teams": r.Teams}, func(t *Tx) error {
		_, err := t.Exec(ctx, `UPDATE afs.export_bundles SET status = 'succeeded', sha256 = $2, size_bytes = $3, s3_uri = $4,
  download_url = $5, download_expires_at = $6, finished_at = now(),
  summary = summary || jsonb_build_object('teams', $7::int)
WHERE id = $1 AND direction = 'export'`, id, r.SHA256, r.SizeBytes, r.S3URI, r.DownloadURL, r.ExpiresAt, r.Teams)
		return err
	})
}

// Fail implements export.Store.
func (j *ExportJobs) Fail(ctx context.Context, id, summary string) error {
	return j.finish(ctx, id, domain.EventExportFailed, "ExportFailed", map[string]any{"exportId": id, "error": summary}, func(t *Tx) error {
		_, err := t.Exec(ctx, `UPDATE afs.export_bundles SET status = 'failed', finished_at = now(),
  summary = summary || jsonb_build_object('error', $2::text)
WHERE id = $1 AND direction = 'export'`, id, summary)
		return err
	})
}

func (j *ExportJobs) finish(ctx context.Context, id string, typ domain.EventType, action string, data map[string]any, update func(*Tx) error) error {
	err := j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		if err := update(t); err != nil {
			return err
		}
		if err := j.audit.Write(ctx, tx, port.AuditRecord{Action: action, Outcome: port.OutcomeAllowed, ResourceKind: "export_bundles",
			ResourceID: id, After: data}); err != nil {
			return err
		}
		eid, err := j.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		ev, err := domain.NewEvent(typ, eid.String(), j.now(), tx.Session().Actor, "", "export", id, data)
		if err != nil {
			return err
		}
		return j.outbox.Publish(ctx, tx, ev)
	})
	return MapError(err, OpWrite, "export")
}

// bundleSource reads desired state as JSON rows.
type bundleSource struct{ t *Tx }

func (s bundleSource) rows(ctx context.Context, sql string, args ...any) ([]domain.Row, error) {
	rs, err := s.t.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []domain.Row{}
	for rs.Next() {
		var raw []byte
		if err := rs.Scan(&raw); err != nil {
			return nil, err
		}
		r, err := decodeRow(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

func (s bundleSource) strings(ctx context.Context, sql string, args ...any) ([]string, error) {
	rs, err := s.t.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []string{}
	for rs.Next() {
		var v string
		if err := rs.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rs.Err()
}

func (s bundleSource) one(ctx context.Context, sql string, args ...any) (domain.Row, error) {
	rs, err := s.rows(ctx, sql, args...)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return rs[0], nil
}

// teamIDs returns the exportable teams among ids (all when ids is empty): persistent teams that are
// active or suspended.
func (s bundleSource) teamIDs(ctx context.Context, ids []string) ([]string, error) {
	return s.strings(ctx, `SELECT id FROM afs.teams WHERE kind = 'persistent' AND state IN ('active', 'suspended')
  AND (cardinality($1::text[]) = 0 OR id = ANY ($1)) ORDER BY slug`, ids)
}

func (s bundleSource) team(ctx context.Context, id string) (domain.TeamBundle, error) {
	var tb domain.TeamBundle
	var err error
	if tb.Team, err = s.one(ctx, `SELECT `+obj("t", bundleTeamCols)+` FROM afs.teams t WHERE t.id = $1`, id); err != nil {
		return tb, err
	}
	if tb.Models, err = s.strings(ctx, `SELECT model_id FROM afs.team_models WHERE team_id = $1 ORDER BY model_id`, id); err != nil {
		return tb, err
	}
	if tb.Tags, err = s.rows(ctx, `SELECT `+obj("g", bundleTagCols)+` FROM afs.team_tags g WHERE g.team_id = $1 ORDER BY g.key`, id); err != nil {
		return tb, err
	}
	if tb.Budget, err = s.one(ctx, `SELECT `+obj("b", bundleBudgetCols)+` FROM afs.budgets b WHERE b.team_id = $1`, id); err != nil {
		return tb, err
	}
	if tb.Members, err = s.rows(ctx, `SELECT jsonb_build_object('role', b.role, 'subject_type', b.subject_type,
  'group_name', b.group_name, 'user_email', u.email)
FROM afs.role_bindings b LEFT JOIN afs.users u ON u.id = b.user_id WHERE b.team_id = $1
ORDER BY b.role, coalesce(b.group_name, u.email)`, id); err != nil {
		return tb, err
	}
	if tb.NamespaceConfig, err = s.rows(ctx, `SELECT `+obj("c", bundleNsConfigCols)+` FROM afs.namespace_config_values c WHERE c.team_id = $1 ORDER BY c.name`, id); err != nil {
		return tb, err
	}
	if tb.ExternalSystems, err = s.rows(ctx, `SELECT `+obj("s", bundleExtSystemCols)+` FROM afs.external_systems s WHERE s.team_id = $1 ORDER BY s.slug`, id); err != nil {
		return tb, err
	}
	if tb.Optouts, err = s.rows(ctx, `SELECT jsonb_build_object('system_slug', s.slug, 'reason', o.reason)
FROM afs.external_system_optouts o JOIN afs.external_systems s ON s.id = o.system_id
WHERE o.team_id = $1 AND o.agent_id IS NULL AND o.pool_id IS NULL ORDER BY s.slug`, id); err != nil {
		return tb, err
	}
	// Secrets: references and metadata only. The ARN is a reference; no value is ever read.
	if tb.Secrets, err = s.rows(ctx, `SELECT jsonb_build_object('name', s.name, 'description', s.description,
  'secrets_manager_arn', s.secrets_manager_arn, 'rotation_days', s.rotation_days, 'agent_name', a.name, 'path', s.path)
FROM afs.secrets s LEFT JOIN afs.agents a ON a.id = s.agent_id
WHERE s.team_id = $1 AND s.pool_id IS NULL AND s.managed_by = 'user' AND (s.agent_id IS NULL OR a.deleted_at IS NULL) ORDER BY s.path`, id); err != nil {
		return tb, err
	}
	// Collaboration settings and the collaboration agent's name (COL-26): choices only.
	if tb.Collaboration, err = s.one(ctx, `SELECT jsonb_build_object('agent_name', a.name, 'web_ui_enabled', c.web_ui_enabled,
  'rest_api_enabled', c.rest_api_enabled, 'registration_mode', c.registration_mode)
FROM afs.team_collaboration c JOIN afs.agents a ON a.team_id = c.team_id AND a.id = c.agent_id WHERE c.team_id = $1`, id); err != nil {
		return tb, err
	}
	tb.Agents = []domain.AgentBundle{}
	rs, err := s.t.Query(ctx, `SELECT a.id, `+obj("a", bundleAgentCols, "'role', a.role")+` FROM afs.agents a
WHERE a.team_id = $1 AND a.deleted_at IS NULL AND a.state IN ('enabled', 'paused') ORDER BY a.name`, id)
	if err != nil {
		return tb, err
	}
	type agentRow struct {
		id  string
		raw []byte
	}
	var found []agentRow
	for rs.Next() {
		var r agentRow
		if err := rs.Scan(&r.id, &r.raw); err != nil {
			rs.Close()
			return tb, err
		}
		found = append(found, r)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return tb, err
	}
	for _, f := range found {
		ab, err := s.agent(ctx, f.id, f.raw)
		if err != nil {
			return tb, err
		}
		tb.Agents = append(tb.Agents, ab)
	}
	return tb, nil
}

func (s bundleSource) agent(ctx context.Context, id string, raw []byte) (domain.AgentBundle, error) {
	var ab domain.AgentBundle
	var err error
	if ab.Agent, err = decodeRow(raw); err != nil {
		return ab, err
	}
	if ab.ConfigValues, err = s.rows(ctx, `SELECT `+obj("c", bundleAgentConfigCols)+` FROM afs.agent_config_values c WHERE c.agent_id = $1 ORDER BY c.name`, id); err != nil {
		return ab, err
	}
	// Bindings to secrets the service manages (the Paperclip agent key) are not exported: they are made again
	// when the link is registered.
	if ab.SecretBindings, err = s.rows(ctx, `SELECT jsonb_build_object('secret_path', s.path, 'delivery', b.delivery, 'target', b.target)
FROM afs.agent_secret_bindings b JOIN afs.secrets s ON s.id = b.secret_id AND s.team_id = b.team_id
WHERE b.agent_id = $1 AND s.managed_by = 'user' ORDER BY b.target`, id); err != nil {
		return ab, err
	}
	if ab.Collaboration, err = s.one(ctx, `SELECT jsonb_build_object('connect', c.connect) FROM afs.agent_collaboration c WHERE c.agent_id = $1`, id); err != nil {
		return ab, err
	}
	if ab.Identities, err = s.rows(ctx, `SELECT jsonb_build_object('kind', i.kind, 'external_ref', i.external_ref, 'secret_path', s.path)
FROM afs.agent_identities i LEFT JOIN afs.secrets s ON s.id = i.secret_id AND s.team_id = i.team_id
WHERE i.agent_id = $1 ORDER BY i.kind`, id); err != nil {
		return ab, err
	}
	if ab.RoleLinks, err = s.rows(ctx, `SELECT `+obj("l", bundleRoleLinkCols)+` FROM afs.agent_role_links l WHERE l.agent_id = $1 ORDER BY l.role_arn`, id); err != nil {
		return ab, err
	}
	if ab.Optouts, err = s.rows(ctx, `SELECT jsonb_build_object('system_slug', s.slug, 'reason', o.reason)
FROM afs.external_system_optouts o JOIN afs.external_systems s ON s.id = o.system_id WHERE o.agent_id = $1 ORDER BY s.slug`, id); err != nil {
		return ab, err
	}
	if ab.LatestSnapshot, err = s.one(ctx, `SELECT jsonb_build_object('id', v.id, 'ebs_snapshot_id', v.ebs_snapshot_id,
  'size_gib', v.size_gib, 'completed_at', v.completed_at)
FROM afs.agent_volume_snapshots v WHERE v.agent_id = $1 AND v.state = 'completed' AND v.ebs_snapshot_id IS NOT NULL
ORDER BY v.completed_at DESC, v.id DESC LIMIT 1`, id); err != nil {
		return ab, err
	}
	return ab, nil
}
