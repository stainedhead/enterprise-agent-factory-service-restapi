package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agenttemplates"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// AgentTemplateRepo implements agenttemplates.Repository on afs.agent_templates (000014). Every
// afs_app caller reads; only service admins write (policy app_write).
type AgentTemplateRepo struct{}

// NewAgentTemplateRepo returns an AgentTemplateRepo.
func NewAgentTemplateRepo() *AgentTemplateRepo { return &AgentTemplateRepo{} }

var _ agenttemplates.Repository = (*AgentTemplateRepo)(nil)

const templateSelect = `SELECT id, name, description, harness, default_resources, allowed_image_digests, current_image_digest, enabled,
  version, created_at, updated_at, capabilities FROM afs.agent_templates`

func scanTemplate(row pgx.Row) (domain.AgentTemplate, error) {
	var t domain.AgentTemplate
	var res []byte
	var v int64
	if err := row.Scan(&t.ID, &t.Name, &t.Description, &t.Harness, &res, &t.AllowedDigests, &t.CurrentDigest, &t.Enabled, &v, &t.CreatedAt, &t.UpdatedAt, &t.Capabilities); err != nil {
		return t, err
	}
	t.Version = domain.Version(v)
	if err := json.Unmarshal(res, &t.DefaultResources); err != nil {
		return t, fmt.Errorf("agent template %s: default_resources: %w", t.Name, err)
	}
	return t, nil
}

// List implements agenttemplates.Repository.
func (*AgentTemplateRepo) List(ctx context.Context, tx port.Tx, enabledOnly bool, capability, after string, limit int) ([]domain.AgentTemplate, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, templateSelect+` WHERE name > $1 AND (enabled OR NOT $2) AND ($4 = '' OR $4 = ANY(capabilities)) ORDER BY name LIMIT $3`, after, enabledOnly, limit, capability)
	if err != nil {
		return nil, MapError(err, OpRead, "agent template")
	}
	defer rows.Close()
	out := []domain.AgentTemplate{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "agent template")
		}
		out = append(out, t)
	}
	return out, MapError(rows.Err(), OpRead, "agent template")
}

// Get implements agenttemplates.Repository.
func (*AgentTemplateRepo) Get(ctx context.Context, tx port.Tx, name string, forUpdate bool) (domain.AgentTemplate, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	q := templateSelect + ` WHERE name = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	t, err := scanTemplate(pt.QueryRow(ctx, q, name))
	return t, MapError(err, OpRead, "agent template")
}

func resourcesJSON(r domain.Resources) ([]byte, error) { return json.Marshal(r) }

// Insert implements agenttemplates.Repository.
func (*AgentTemplateRepo) Insert(ctx context.Context, tx port.Tx, t domain.AgentTemplate) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	res, err := resourcesJSON(t.DefaultResources)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_templates (id, name, description, harness, default_resources, allowed_image_digests, current_image_digest, enabled, capabilities)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, t.ID, t.Name, t.Description, t.Harness, res, t.AllowedDigests, t.CurrentDigest, t.Enabled, nonNilStrings(t.Capabilities))
	return MapError(err, OpWrite, "agent template")
}

// Update implements agenttemplates.Repository.
func (r *AgentTemplateRepo) Update(ctx context.Context, tx port.Tx, t domain.AgentTemplate, expect domain.Version) (domain.AgentTemplate, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	res, err := resourcesJSON(t.DefaultResources)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.agent_templates SET description = $2, harness = $3, default_resources = $4, allowed_image_digests = $5,
  current_image_digest = $6, enabled = $7, capabilities = $9 WHERE id = $1 AND version = $8`,
		t.ID, t.Description, t.Harness, res, t.AllowedDigests, t.CurrentDigest, t.Enabled, int64(expect), nonNilStrings(t.Capabilities))
	if err != nil {
		return domain.AgentTemplate{}, MapError(err, OpWrite, "agent template")
	}
	if tag.RowsAffected() == 0 {
		return domain.AgentTemplate{}, catalogMissOrStale(ctx, pt, "agent template", `SELECT 1 FROM afs.agent_templates WHERE id = $1`, t.ID)
	}
	return r.Get(ctx, tx, t.Name, false)
}

// Delete implements agenttemplates.Repository. An agent or pool that uses the template makes the
// foreign key refuse the delete; that is a Conflict that says so.
func (*AgentTemplateRepo) Delete(ctx context.Context, tx port.Tx, name string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.agent_templates WHERE name = $1 AND version = $2`, name, int64(expect))
	if err != nil {
		return catalogInUse(MapError(err, OpDelete, "agent template"), "agent template "+name+" is used by agents or pools; disable it instead", "agents")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "agent template", `SELECT 1 FROM afs.agent_templates WHERE name = $1`, name)
	}
	return nil
}

// nonNilStrings returns s, or an empty slice for nil (a NULL would break NOT NULL columns).
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
