package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/swarmtemplates"
)

// SwarmTemplateRepo implements swarmtemplates.Repository on afs.swarm_templates (000012). Every
// afs_app caller reads; the write policy admits service admins only (the API checks the permission
// first).
type SwarmTemplateRepo struct{}

// NewSwarmTemplateRepo returns a SwarmTemplateRepo.
func NewSwarmTemplateRepo() *SwarmTemplateRepo { return &SwarmTemplateRepo{} }

var _ swarmtemplates.Repository = (*SwarmTemplateRepo)(nil)

const swarmTemplateSelect = `SELECT id, name, description, kind, spec, budget_usd::text, ttl_hours, teardown_rule, enabled,
  version, created_at, created_by, updated_at FROM afs.swarm_templates`

func scanSwarmTemplate(row pgx.Row) (domain.SwarmTemplate, error) {
	var t domain.SwarmTemplate
	var spec []byte
	var budget string
	var v int64
	var name, kind, rule string
	if err := row.Scan(&t.ID, &name, &t.Description, &kind, &spec, &budget, &t.TTLHours, &rule, &t.Enabled, &v,
		&t.CreatedAt, &t.CreatedBy, &t.UpdatedAt); err != nil {
		return t, err
	}
	t.Name, t.Kind, t.TeardownRule, t.Version = domain.Name(name), domain.WorkOrderKind(kind), domain.TeardownRule(rule), domain.Version(v)
	var err error
	if t.Budget, err = domain.ParseMoney("budget_usd", budget); err != nil {
		return t, err
	}
	if err := json.Unmarshal(spec, &t.Spec); err != nil {
		return t, fmt.Errorf("swarm template %s: spec: %w", t.ID, err)
	}
	return t, nil
}

// List implements swarmtemplates.Repository.
func (*SwarmTemplateRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.SwarmTemplate, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, swarmTemplateSelect+` WHERE name > $1 ORDER BY name LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "swarm template")
	}
	defer rows.Close()
	out := []domain.SwarmTemplate{}
	for rows.Next() {
		t, err := scanSwarmTemplate(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "swarm template")
		}
		out = append(out, t)
	}
	return out, MapError(rows.Err(), OpRead, "swarm template")
}

// Get implements swarmtemplates.Repository.
func (*SwarmTemplateRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.SwarmTemplate, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.SwarmTemplate{}, err
	}
	q := swarmTemplateSelect + ` WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	t, err := scanSwarmTemplate(pt.QueryRow(ctx, q, id))
	return t, MapError(err, OpRead, "swarm template")
}

// Insert implements swarmtemplates.Repository.
func (*SwarmTemplateRepo) Insert(ctx context.Context, tx port.Tx, t domain.SwarmTemplate) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	spec, err := json.Marshal(t.Spec)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.swarm_templates (id, name, description, kind, spec, budget_usd, ttl_hours, teardown_rule, enabled)
VALUES ($1, $2, $3, $4, $5, $6::numeric, $7, $8, $9)`,
		t.ID, string(t.Name), t.Description, string(t.Kind), spec, t.Budget.String(), t.TTLHours, string(t.TeardownRule), t.Enabled)
	return MapError(err, OpWrite, "swarm template")
}

// Update implements swarmtemplates.Repository.
func (r *SwarmTemplateRepo) Update(ctx context.Context, tx port.Tx, t domain.SwarmTemplate, expect domain.Version) (domain.SwarmTemplate, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.SwarmTemplate{}, err
	}
	spec, err := json.Marshal(t.Spec)
	if err != nil {
		return domain.SwarmTemplate{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.swarm_templates SET description = $2, spec = $3, budget_usd = $4::numeric, ttl_hours = $5,
  teardown_rule = $6, enabled = $7 WHERE id = $1 AND version = $8`,
		t.ID, t.Description, spec, t.Budget.String(), t.TTLHours, string(t.TeardownRule), t.Enabled, int64(expect))
	if err != nil {
		return domain.SwarmTemplate{}, MapError(err, OpWrite, "swarm template")
	}
	if tag.RowsAffected() == 0 {
		return domain.SwarmTemplate{}, catalogMissOrStale(ctx, pt, "swarm template", `SELECT 1 FROM afs.swarm_templates WHERE id = $1`, t.ID)
	}
	return r.Get(ctx, tx, t.ID, false)
}

// Delete implements swarmtemplates.Repository. Work orders keep a RESTRICT reference to their
// template, so one a closed work order still names is a Conflict that says to disable it.
func (*SwarmTemplateRepo) Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.swarm_templates WHERE id = $1 AND version = $2`, id, int64(expect))
	if err != nil {
		return catalogInUse(MapError(err, OpDelete, "swarm template"), "swarm template "+id+" is named by work orders; disable it instead", "work_orders")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "swarm template", `SELECT 1 FROM afs.swarm_templates WHERE id = $1`, id)
	}
	return nil
}

// OpenWorkOrders implements swarmtemplates.Repository.
func (*SwarmTemplateRepo) OpenWorkOrders(ctx context.Context, tx port.Tx, id string) (int, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return 0, err
	}
	var n int
	err = pt.QueryRow(ctx, `SELECT count(*) FROM afs.work_orders WHERE template_id = $1 AND status NOT IN ('done', 'failed', 'cancelled')`, id).Scan(&n)
	return n, MapError(err, OpRead, "work order")
}

// CollaborationTemplateOK implements swarmtemplates.Repository.
func (*SwarmTemplateRepo) CollaborationTemplateOK(ctx context.Context, tx port.Tx, id string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.agent_templates WHERE id = $1 AND enabled AND 'paperclip' = ANY (capabilities))`, id).Scan(&ok)
	return ok, MapError(err, OpRead, "agent template")
}
