package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// AgentRepo implements agents.Repository on afs.agents and its satellites. Agents in state deleted
// are invisible: the reconciler finalizes a delete by that state.
type AgentRepo struct{}

// NewAgentRepo returns an AgentRepo.
func NewAgentRepo() *AgentRepo { return &AgentRepo{} }

var _ agents.Repository = (*AgentRepo)(nil)

const agentSelect = `SELECT a.id, a.team_id, a.name, a.description, a.template, a.image, a.model_id, a.resources,
  a.iam_identity, coalesce(ident.external_ref, ''),
  a.storage_size_gib, a.storage_class, a.storage_on_delete, a.snapshot_schedule, a.snapshot_retain_count,
  a.state, a.version, a.generation, a.created_at, a.created_by, a.updated_at,
  rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at
FROM afs.agents a
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + agents.StatusKind + `' AND rs.resource_id = a.id
LEFT JOIN LATERAL (
  SELECT i.external_ref FROM afs.agent_identities i
  WHERE i.team_id = a.team_id AND i.kind = 'aws_iam_role' AND (i.agent_id = a.id OR i.agent_id IS NULL)
  ORDER BY (i.agent_id IS NULL), i.created_at DESC LIMIT 1) ident ON true
WHERE a.team_id = $1 AND a.state <> 'deleted' AND a.deleted_at IS NULL`

func scanAgent(row pgx.Row) (domain.Agent, error) {
	var (
		a                          domain.Agent
		res, conds                 []byte
		name, image, state, iam    string
		ver, gen                   int64
		phase                      *string
		obs                        *int64
		lastRec                    *time.Time
		sizeGiB, retain            int
		class, onDelete, scheduled string
	)
	err := row.Scan(&a.ID, &a.TeamID, &name, &a.Description, &a.Template, &image, &a.ModelID, &res,
		&iam, &a.IAMRoleARN, &sizeGiB, &class, &onDelete, &scheduled, &retain,
		&state, &ver, &gen, &a.CreatedAt, &a.CreatedBy, &a.UpdatedAt, &phase, &obs, &conds, &lastRec)
	if err != nil {
		return domain.Agent{}, err
	}
	a.Name, a.Image, a.State = domain.Name(name), domain.ImageRef(image), domain.AgentState(state)
	a.IAMIdentity = domain.IAMIdentity(iam)
	a.Version, a.Generation = domain.Version(ver), domain.Generation(gen)
	a.Volume = domain.AgentVolume{SizeGiB: sizeGiB, StorageClass: class, OnDelete: onDelete, SnapshotSchedule: scheduled, SnapshotRetain: retain}
	if len(res) > 0 {
		if err := json.Unmarshal(res, &a.Resources); err != nil {
			return domain.Agent{}, err
		}
	}
	a.Bindings = []domain.SecretBinding{}
	a.Status = domain.PendingStatus(a.CreatedAt)
	if phase != nil {
		a.Status.Phase = domain.Phase(*phase)
		if obs != nil {
			a.Status.ObservedGeneration = *obs
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &a.Status.Conditions); err != nil {
				return domain.Agent{}, err
			}
		}
		if a.Status.Conditions == nil {
			a.Status.Conditions = []domain.Condition{}
		}
		if lastRec != nil {
			a.Status.UpdatedAt = lastRec.UTC()
		}
	}
	return a, nil
}

func loadBindings(ctx context.Context, pt pgx.Tx, teamID string, list []domain.Agent) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	idx := map[string]int{}
	for i, a := range list {
		ids[i], idx[a.ID] = a.ID, i
	}
	rows, err := pt.Query(ctx, `SELECT agent_id, secret_id, delivery, target FROM afs.agent_secret_bindings
WHERE team_id=$1 AND agent_id = ANY($2) ORDER BY agent_id, target`, teamID, ids)
	if err != nil {
		return MapError(err, OpRead, "agent")
	}
	defer rows.Close()
	for rows.Next() {
		var agent string
		var b domain.SecretBinding
		if err := rows.Scan(&agent, &b.SecretID, &b.Delivery, &b.Target); err != nil {
			return MapError(err, OpRead, "agent")
		}
		i := idx[agent]
		list[i].Bindings = append(list[i].Bindings, b)
	}
	return MapError(rows.Err(), OpRead, "agent")
}

// List implements agents.Repository.
func (*AgentRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Agent, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, agentSelect+` AND a.id > $2 ORDER BY a.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "agent")
	}
	out := []domain.Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			rows.Close()
			return nil, MapError(err, OpRead, "agent")
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, MapError(err, OpRead, "agent")
	}
	return out, loadBindings(ctx, pt, teamID, out)
}

// Get implements agents.Repository.
func (*AgentRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Agent, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Agent{}, err
	}
	q := agentSelect + ` AND a.id = $2`
	if forUpdate {
		q += ` FOR UPDATE OF a`
	}
	a, err := scanAgent(pt.QueryRow(ctx, q, teamID, id))
	if err != nil {
		return domain.Agent{}, MapError(err, OpRead, "agent")
	}
	list := []domain.Agent{a}
	if err := loadBindings(ctx, pt, teamID, list); err != nil {
		return domain.Agent{}, err
	}
	return list[0], nil
}

func agentResourcesJSON(r domain.AgentResources) ([]byte, error) { return json.Marshal(r) }

func replaceBindings(ctx context.Context, pt pgx.Tx, a domain.Agent) error {
	if _, err := pt.Exec(ctx, `DELETE FROM afs.agent_secret_bindings WHERE team_id=$1 AND agent_id=$2`, a.TeamID, a.ID); err != nil {
		return MapError(err, OpWrite, "agent")
	}
	for _, b := range a.Bindings {
		if _, err := pt.Exec(ctx, `INSERT INTO afs.agent_secret_bindings (team_id, agent_id, secret_id, delivery, target)
VALUES ($1, $2, $3, $4, $5)`, a.TeamID, a.ID, b.SecretID, b.Delivery, b.Target); err != nil {
			return MapError(err, OpWrite, "agent")
		}
	}
	return nil
}

// Insert implements agents.Repository.
func (*AgentRepo) Insert(ctx context.Context, tx port.Tx, a domain.Agent, templateID string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	res, err := agentResourcesJSON(a.Resources)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agents (id, team_id, name, description, template, template_id, image, model_id, resources,
  iam_identity, storage_size_gib, storage_class, storage_on_delete, snapshot_schedule, snapshot_retain_count, state, generation)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		a.ID, a.TeamID, string(a.Name), a.Description, a.Template, nullable(templateID), string(a.Image), a.ModelID, res,
		string(a.IAMIdentity), a.Volume.SizeGiB, a.Volume.StorageClass, a.Volume.OnDelete, a.Volume.SnapshotSchedule,
		a.Volume.SnapshotRetain, string(a.State), int64(a.Generation))
	if err != nil {
		return MapError(err, OpWrite, "agent")
	}
	return replaceBindings(ctx, pt, a)
}

// Update implements agents.Repository. template and image are named in the UPDATE only when the
// template changed, because the database's template guard runs on UPDATE OF those columns.
func (*AgentRepo) Update(ctx context.Context, tx port.Tx, a domain.Agent, templateID string, templateChanged bool, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	res, err := agentResourcesJSON(a.Resources)
	if err != nil {
		return err
	}
	q := `UPDATE afs.agents SET description=$3, model_id=$4, resources=$5, iam_identity=$6,
  storage_size_gib=$7, storage_class=$8, storage_on_delete=$9, snapshot_schedule=$10, snapshot_retain_count=$11,
  state=$12, generation=$13`
	args := []any{a.TeamID, a.ID, a.Description, a.ModelID, res, string(a.IAMIdentity), a.Volume.SizeGiB, a.Volume.StorageClass,
		a.Volume.OnDelete, a.Volume.SnapshotSchedule, a.Volume.SnapshotRetain, string(a.State), int64(a.Generation)}
	if templateChanged {
		q += `, template=$14, template_id=$15, image=$16`
		args = append(args, a.Template, templateID, string(a.Image))
	}
	q += ` WHERE team_id=$1 AND id=$2 AND version=$` + strconv.Itoa(len(args)+1)
	args = append(args, int64(expect))
	tag, err := pt.Exec(ctx, q, args...)
	if err != nil {
		return MapError(err, OpWrite, "agent")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	// Bindings are replaced only when they differ from the stored set.
	var stored []domain.SecretBinding
	rows, err := pt.Query(ctx, `SELECT secret_id, delivery, target FROM afs.agent_secret_bindings WHERE team_id=$1 AND agent_id=$2 ORDER BY target`, a.TeamID, a.ID)
	if err != nil {
		return MapError(err, OpRead, "agent")
	}
	for rows.Next() {
		var b domain.SecretBinding
		if err := rows.Scan(&b.SecretID, &b.Delivery, &b.Target); err != nil {
			rows.Close()
			return MapError(err, OpRead, "agent")
		}
		stored = append(stored, b)
	}
	rows.Close()
	if sameBindings(stored, a.Bindings) {
		return nil
	}
	return replaceBindings(ctx, pt, a)
}

func sameBindings(a, b []domain.SecretBinding) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]domain.SecretBinding{}
	for _, x := range a {
		m[x.Target] = x
	}
	for _, y := range b {
		if m[y.Target] != y {
			return false
		}
	}
	return true
}

// Template implements agents.Repository.
func (*AgentRepo) Template(ctx context.Context, tx port.Tx, name string) (agents.TemplateInfo, bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return agents.TemplateInfo{}, false, err
	}
	var t agents.TemplateInfo
	err = pt.QueryRow(ctx, `SELECT id, name, enabled, allowed_image_digests FROM afs.agent_templates WHERE name=$1`, name).
		Scan(&t.ID, &t.Name, &t.Enabled, &t.AllowedDigests)
	if errors.Is(err, pgx.ErrNoRows) {
		return agents.TemplateInfo{}, false, nil
	}
	return t, err == nil, MapError(err, OpRead, "agent template")
}

// ModelEnabled implements agents.Repository.
func (*AgentRepo) ModelEnabled(ctx context.Context, tx port.Tx, teamID, modelID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.team_models tm JOIN afs.models m ON m.id = tm.model_id
WHERE tm.team_id=$1 AND tm.model_id=$2 AND m.enabled)`, teamID, modelID).Scan(&ok)
	return ok, MapError(err, OpRead, "model")
}

// StorageClassEnabled implements agents.Repository.
func (*AgentRepo) StorageClassEnabled(ctx context.Context, tx port.Tx, name string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.storage_classes WHERE id=$1 AND enabled)`, name).Scan(&ok)
	return ok, MapError(err, OpRead, "storage class")
}

// Secrets implements agents.Repository.
func (*AgentRepo) Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]agents.SecretInfo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, coalesce(agent_id, ''), coalesce(pool_id, '') FROM afs.secrets WHERE team_id=$1 AND id = ANY($2)`, teamID, ids)
	if err != nil {
		return nil, MapError(err, OpRead, "secret")
	}
	defer rows.Close()
	out := map[string]agents.SecretInfo{}
	for rows.Next() {
		var id string
		var s agents.SecretInfo
		if err := rows.Scan(&id, &s.AgentID, &s.PoolID); err != nil {
			return nil, MapError(err, OpRead, "secret")
		}
		out[id] = s
	}
	return out, MapError(rows.Err(), OpRead, "secret")
}

// Snapshots implements agents.Repository: newest first (ids are time ordered ULIDs).
func (*AgentRepo) Snapshots(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.Snapshot, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, agent_id, reason, state, coalesce(size_gib, 0), requested_at, requested_by, completed_at
FROM afs.agent_volume_snapshots WHERE team_id=$1 AND agent_id=$2 AND ($3 = '' OR id < $3) ORDER BY id DESC LIMIT $4`,
		teamID, agentID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "snapshot")
	}
	defer rows.Close()
	out := []domain.Snapshot{}
	for rows.Next() {
		var s domain.Snapshot
		if err := rows.Scan(&s.ID, &s.AgentID, &s.Reason, &s.State, &s.SizeGiB, &s.RequestedAt, &s.RequestedBy, &s.CompletedAt); err != nil {
			return nil, MapError(err, OpRead, "snapshot")
		}
		out = append(out, s)
	}
	return out, MapError(rows.Err(), OpRead, "snapshot")
}

// InsertSnapshot implements agents.Repository. afs_app may insert only id, team, agent and reason;
// the database fills state, requested_at and requested_by.
func (*AgentRepo) InsertSnapshot(ctx context.Context, tx port.Tx, teamID string, s domain.Snapshot) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.agent_volume_snapshots (id, team_id, agent_id, reason) VALUES ($1, $2, $3, $4)`,
		s.ID, teamID, s.AgentID, s.Reason)
	return MapError(err, OpWrite, "snapshot")
}
