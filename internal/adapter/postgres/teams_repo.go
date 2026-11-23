package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

// TeamRepo implements teams.Repository on the afs schema. Every method runs in the transaction it
// is given, so row-level security applies with the caller's session.
type TeamRepo struct{}

// NewTeamRepo returns a TeamRepo.
func NewTeamRepo() *TeamRepo { return &TeamRepo{} }

var _ teams.Repository = (*TeamRepo)(nil)

const teamStatusKind = teams.StatusKind

const teamSelect = `SELECT t.id, t.slug, t.display_name, t.description, t.kind, t.state, t.namespace, t.cost_center,
  t.owner_email, t.max_budget_usd::text, t.ceiling_override_usd::text, t.ceiling_override_expires_at, t.quotas,
  t.isolation_tier, t.viewer_kubectl, t.expires_at, coalesce(t.teardown_rule, ''), t.torn_down_at,
  coalesce(t.teardown_reason, ''), coalesce(t.identity_provider_id, ''), t.allowed_aws_accounts,
  t.may_manage_aws_accounts, t.may_add_external_systems, t.max_collaboration_exposure, t.model_access,
  coalesce((SELECT a.id FROM afs.agents a WHERE a.team_id = t.id AND a.role = 'orchestration' AND a.deleted_at IS NULL), ''), t.version, t.generation, t.created_at, t.created_by,
  t.updated_at, rs.phase, rs.observed_generation, rs.conditions, rs.last_reconciled_at
FROM afs.teams t
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + teamStatusKind + `' AND rs.resource_id = t.id`

func scanTeam(row pgx.Row) (domain.Team, error) {
	var (
		t                       domain.Team
		slug, kind, state, tier string
		rule, reason            string
		maxB                    string
		ceil                    *string
		quotas, conds           []byte
		phase                   *string
		obs                     *int64
		lastRec                 *time.Time
		version, generation     int64
		limit, access           string
	)
	err := row.Scan(&t.ID, &slug, &t.DisplayName, &t.Description, &kind, &state, &t.Namespace, &t.CostCenter,
		&t.OwnerEmail, &maxB, &ceil, &t.CeilingOverrideExpiresAt, &quotas,
		&tier, &t.ViewerKubectl, &t.ExpiresAt, &rule, &t.TornDownAt,
		&reason, &t.IdentityProviderID, &t.AllowedAwsAccounts,
		&t.MayManageAwsAccounts, &t.MayAddExternalSystems, &limit, &access, &t.OrchestrationAgentID, &version, &generation, &t.CreatedAt, &t.CreatedBy,
		&t.UpdatedAt, &phase, &obs, &conds, &lastRec)
	if err != nil {
		return domain.Team{}, err
	}
	t.MaxCollaborationExposure, t.ModelAccess = domain.Exposure(limit), domain.ModelAccess(access)
	t.MayExposeCollaboration = t.MaxCollaborationExposure != domain.ExposureOff
	t.Slug, t.Kind, t.State = domain.Name(slug), domain.TeamKind(kind), domain.TeamState(state)
	t.IsolationTier, t.TeardownRule, t.TeardownReason = domain.IsolationTier(tier), domain.TeardownRule(rule), domain.TeardownReason(reason)
	t.Version, t.Generation = domain.Version(version), domain.Generation(generation)
	if t.MaxBudget, err = domain.ParseMoney("maxBudgetUsd", maxB); err != nil {
		return domain.Team{}, err
	}
	if ceil != nil {
		m, err := domain.ParseMoney("ceilingOverrideUsd", *ceil)
		if err != nil {
			return domain.Team{}, err
		}
		t.CeilingOverride = &m
	}
	if len(quotas) > 0 {
		if err := json.Unmarshal(quotas, &t.Quotas); err != nil {
			return domain.Team{}, err
		}
	}
	if t.AllowedAwsAccounts == nil {
		t.AllowedAwsAccounts = []string{}
	}
	t.Status = domain.PendingStatus(t.CreatedAt)
	if phase != nil {
		t.Status.Phase = domain.Phase(*phase)
		if obs != nil {
			t.Status.ObservedGeneration = *obs
		}
		if len(conds) > 0 {
			if err := json.Unmarshal(conds, &t.Status.Conditions); err != nil {
				return domain.Team{}, err
			}
		}
		if t.Status.Conditions == nil {
			t.Status.Conditions = []domain.Condition{}
		}
		if lastRec != nil {
			t.Status.UpdatedAt = lastRec.UTC()
		}
	}
	return t, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func moneyPtr(m *domain.Money) *string {
	if m == nil {
		return nil
	}
	s := m.String()
	return &s
}

// Insert implements teams.Repository.
func (*TeamRepo) Insert(ctx context.Context, tx port.Tx, t domain.Team) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	q, _ := json.Marshal(t.Quotas)
	accounts := t.AllowedAwsAccounts
	if accounts == nil {
		accounts = []string{}
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.teams (id, slug, display_name, description, kind, state, cost_center, owner_email,
  max_budget_usd, quotas, isolation_tier, viewer_kubectl, expires_at, teardown_rule, identity_provider_id,
  allowed_aws_accounts, may_manage_aws_accounts, may_add_external_systems, generation, max_collaboration_exposure, model_access)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10::jsonb,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		t.ID, string(t.Slug), t.DisplayName, t.Description, string(t.Kind), string(t.State), t.CostCenter, t.OwnerEmail,
		t.MaxBudget.String(), q, string(t.IsolationTier), t.ViewerKubectl, t.ExpiresAt, nullable(string(t.TeardownRule)),
		nullable(t.IdentityProviderID), accounts, t.MayManageAwsAccounts, t.MayAddExternalSystems, int64(t.Generation), string(t.MaxCollaborationExposure), string(accessOrAll(t.ModelAccess)))
	if err != nil {
		return MapError(err, OpWrite, "team")
	}
	return nil
}

// Get implements teams.Repository.
func (*TeamRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.Team, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Team{}, err
	}
	q := teamSelect + ` WHERE t.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF t`
	}
	t, err := scanTeam(pt.QueryRow(ctx, q, id))
	if err != nil {
		return domain.Team{}, MapError(err, OpRead, "team")
	}
	return t, nil
}

// List implements teams.Repository.
func (*TeamRepo) List(ctx context.Context, tx port.Tx, f teams.ListFilter, after string, limit int) ([]domain.Team, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, teamSelect+` WHERE t.id > $1
  AND ($2 = '' OR t.state = $2) AND ($3 = '' OR t.kind = $3) AND (NOT $4 OR t.state <> 'deleted')
ORDER BY t.id LIMIT $5`, after, string(f.State), string(f.Kind), f.HideDeleted, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "team")
	}
	defer rows.Close()
	out := []domain.Team{}
	for rows.Next() {
		t, err := scanTeam(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "team")
		}
		out = append(out, t)
	}
	return out, MapError(rows.Err(), OpRead, "team")
}

// Update implements teams.Repository.
func (*TeamRepo) Update(ctx context.Context, tx port.Tx, t domain.Team, expect domain.Version) (domain.Team, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Team{}, err
	}
	q, _ := json.Marshal(t.Quotas)
	accounts := t.AllowedAwsAccounts
	if accounts == nil {
		accounts = []string{}
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.teams SET display_name=$3, description=$4, state=$5, cost_center=$6, owner_email=$7,
  max_budget_usd=$8::numeric, ceiling_override_usd=$9::numeric, ceiling_override_expires_at=$10, quotas=$11::jsonb,
  isolation_tier=$12, viewer_kubectl=$13, expires_at=$14, identity_provider_id=$15, allowed_aws_accounts=$16,
  may_manage_aws_accounts=$17, may_add_external_systems=$18, generation=$19, max_collaboration_exposure=$20, model_access=$21,
  deleted_at = CASE WHEN $5 = 'deleted' THEN coalesce(deleted_at, now()) ELSE NULL END
WHERE id=$1 AND version=$2`,
		t.ID, int64(expect), t.DisplayName, t.Description, string(t.State), t.CostCenter, t.OwnerEmail,
		t.MaxBudget.String(), moneyPtr(t.CeilingOverride), t.CeilingOverrideExpiresAt, q,
		string(t.IsolationTier), t.ViewerKubectl, t.ExpiresAt, nullable(t.IdentityProviderID), accounts,
		t.MayManageAwsAccounts, t.MayAddExternalSystems, int64(t.Generation), string(t.MaxCollaborationExposure), string(accessOrAll(t.ModelAccess)))
	if err != nil {
		return domain.Team{}, MapError(err, OpWrite, "team")
	}
	if tag.RowsAffected() == 0 {
		var one int
		err := pt.QueryRow(ctx, `SELECT 1 FROM afs.teams WHERE id=$1`, t.ID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Team{}, domain.NotFound("team")
		}
		if err != nil {
			return domain.Team{}, MapError(err, OpRead, "team")
		}
		return domain.Team{}, domain.PreconditionFailed()
	}
	return (&TeamRepo{}).Get(ctx, tx, t.ID, false)
}

// BudgetAmount implements teams.Repository.
func (*TeamRepo) BudgetAmount(ctx context.Context, tx port.Tx, teamID string) (*domain.Money, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var s string
	err = pt.QueryRow(ctx, `SELECT amount_usd::text FROM afs.budgets WHERE team_id=$1`, teamID).Scan(&s)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, MapError(err, OpRead, "budget")
	}
	m, err := domain.ParseMoney("amountUsd", s)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// InsertBudget implements teams.Repository.
func (*TeamRepo) InsertBudget(ctx context.Context, tx port.Tx, teamID string, b teams.BudgetInput) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	th := make([]int16, len(b.Thresholds))
	for i, v := range b.Thresholds {
		th[i] = int16(v) //nolint:gosec // thresholds are 1 to 100, checked by the database
	}
	if len(th) == 0 {
		th = []int16{50, 80, 100}
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.budgets (id, team_id, period, amount_usd, thresholds)
VALUES ($1, $2, 'lifetime', $3::numeric, $4)`, b.ID, teamID, b.Amount.String(), th)
	return MapError(err, OpWrite, "budget")
}

// ResolveUser implements teams.Repository.
func (*TeamRepo) ResolveUser(ctx context.Context, tx port.Tx, userID, email string) (string, bool, error) {
	return resolveUser(ctx, tx, userID, email)
}

func resolveUser(ctx context.Context, tx port.Tx, userID, email string) (string, bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", false, err
	}
	var id string
	err = pt.QueryRow(ctx, `SELECT id FROM afs.users WHERE ($1 <> '' AND id = $1) OR ($2 <> '' AND lower(email) = lower($2)) LIMIT 1`,
		userID, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, MapError(err, OpRead, "user")
}

// InsertAdminBinding implements teams.Repository.
func (*TeamRepo) InsertAdminBinding(ctx context.Context, tx port.Tx, bindingID, teamID string, m domain.NewMemberParams) error {
	return insertBinding(ctx, tx, bindingID, teamID, m)
}

func insertBinding(ctx context.Context, tx port.Tx, bindingID, teamID string, m domain.NewMemberParams) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, user_id, group_name, identity_provider_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, bindingID, string(m.Role), teamID, string(m.SubjectType),
		nullable(m.UserID), nullable(m.GroupName), nullable(m.IdentityProviderID))
	return MapError(err, OpWrite, "member")
}

// revisionVersion is the version of a set stored as many rows: one more than the number of
// revisions recorded for the team's rows of table.
func revisionVersion(ctx context.Context, pt *Tx, table, teamID string) (domain.Version, error) {
	var n int64
	err := pt.QueryRow(ctx, `SELECT count(*) FROM afs.config_revisions WHERE resource_kind=$1 AND team_id=$2`, table, teamID).Scan(&n)
	return domain.Version(n + 1), MapError(err, OpRead, table)
}

func toModelInfo(m domain.Model) teams.ModelInfo {
	return teams.ModelInfo{ID: m.ID, Provider: m.Provider, DisplayName: m.DisplayName, Enabled: m.Enabled, ContextWindow: m.ContextWindow,
		MaxOutputTokens: m.MaxOutputTokens, Version: int64(m.Version), UpdatedAt: m.UpdatedAt, Callable: m.Callable, PriceState: string(m.PriceState)}
}

// Models implements teams.Repository: the access, the stored selection and the models the team may use
// now (service and team), each with callable and priceState. The version is the team's revision count.
func (*TeamRepo) Models(ctx context.Context, tx port.Tx, teamID string) (teams.TeamModels, error) {
	var out teams.TeamModels
	pt, err := Unwrap(tx)
	if err != nil {
		return out, err
	}
	var access string
	if err := pt.QueryRow(ctx, `SELECT model_access FROM afs.teams WHERE id = $1`, teamID).Scan(&access); err != nil {
		return out, MapError(err, OpRead, "team")
	}
	out.Access = domain.ModelAccess(access)
	out.ModelIDs = []string{}
	rows, err := pt.Query(ctx, `SELECT model_id FROM afs.team_models WHERE team_id = $1 ORDER BY model_id`, teamID)
	if err != nil {
		return out, MapError(err, OpRead, "model")
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, MapError(err, OpRead, "model")
		}
		out.ModelIDs = append(out.ModelIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, MapError(err, OpRead, "model")
	}
	items, err := pt.Query(ctx, modelSelect(false)+` WHERE (m.enabled OR `+svcAccessSQL+` = 'all')
  AND ($2 = 'all' OR EXISTS (SELECT 1 FROM afs.team_models tm WHERE tm.team_id = $1 AND tm.model_id = m.id)) ORDER BY m.id`, teamID, access)
	if err != nil {
		return out, MapError(err, OpRead, "model")
	}
	out.Items = []teams.ModelInfo{}
	for items.Next() {
		m, err := scanModel(items)
		if err != nil {
			items.Close()
			return out, MapError(err, OpRead, "model")
		}
		out.Items = append(out.Items, toModelInfo(m))
	}
	items.Close()
	if err := items.Err(); err != nil {
		return out, MapError(err, OpRead, "model")
	}
	profiles, err := pt.Query(ctx, `SELECT model_id, state FROM afs.team_inference_profiles WHERE team_id = $1`, teamID)
	if err != nil {
		return out, MapError(err, OpRead, "model")
	}
	state := map[string]string{}
	for profiles.Next() {
		var id, st string
		if err := profiles.Scan(&id, &st); err != nil {
			profiles.Close()
			return out, MapError(err, OpRead, "model")
		}
		state[id] = st
	}
	profiles.Close()
	if err := profiles.Err(); err != nil {
		return out, MapError(err, OpRead, "model")
	}
	for i := range out.Items {
		out.Items[i].InferenceProfileState = state[out.Items[i].ID]
	}
	out.Version, err = revisionVersion(ctx, pt, "team_models", teamID)
	return out, err
}

// ServiceModelIDs implements teams.Repository.
func (*TeamRepo) ServiceModelIDs(ctx context.Context, tx port.Tx) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	return serviceModelIDs(ctx, pt)
}

func serviceModelIDs(ctx context.Context, pt *Tx) ([]string, error) {
	rows, err := pt.Query(ctx, `SELECT m.id FROM afs.models m WHERE m.enabled OR `+svcAccessSQL+` = 'all' ORDER BY m.id`)
	if err != nil {
		return nil, MapError(err, OpRead, "model")
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "model")
		}
		out = append(out, id)
	}
	return out, MapError(rows.Err(), OpRead, "model")
}

// SetModelAccess implements teams.Repository.
func (*TeamRepo) SetModelAccess(ctx context.Context, tx port.Tx, teamID string, access domain.ModelAccess) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `UPDATE afs.teams SET model_access = $2 WHERE id = $1`, teamID, string(access))
	return MapError(err, OpWrite, "team")
}

// ModelSets implements teams.Repository.
func (*TeamRepo) ModelSets(ctx context.Context, tx port.Tx, teamID string) (domain.ModelSets, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ModelSets{}, err
	}
	return loadModelSets(ctx, pt, teamID)
}

// loadModelSets reads the sets the models of an agent or pool are checked against: the service's
// models, the team's within them, the ones with a price in force, and models.default_model_id.
func loadModelSets(ctx context.Context, pt *Tx, teamID string) (domain.ModelSets, error) {
	var out domain.ModelSets
	var err error
	if out.Service, err = serviceModelIDs(ctx, pt); err != nil {
		return out, err
	}
	err = pt.QueryRow(ctx, `SELECT coalesce(array_agg(m.id ORDER BY m.id), '{}') FROM afs.models m
WHERE (m.enabled OR `+svcAccessSQL+` = 'all')
  AND (afs.model_allowed_for_team($1, m.id))`, teamID).Scan(&out.Team)
	if err != nil {
		return out, MapError(err, OpRead, "model")
	}
	err = pt.QueryRow(ctx, `SELECT coalesce(array_agg(m.id ORDER BY m.id), '{}') FROM afs.models m
WHERE EXISTS (SELECT 1 FROM afs.model_prices p WHERE p.model_id = m.id AND p.effective_from <= now())`).Scan(&out.Callable)
	if err != nil {
		return out, MapError(err, OpRead, "model")
	}
	err = pt.QueryRow(ctx, `SELECT coalesce((SELECT s.value #>> '{}' FROM afs.service_settings s
  WHERE s.key = 'models.default_model_id' AND jsonb_typeof(s.value) = 'string'), '')`).Scan(&out.SettingDefault)
	return out, MapError(err, OpRead, "model")
}

// InsertOrchestration implements teams.Repository: afs.create_orchestration_agent (service admins), which
// also writes the agent's models, the audit record and both outbox events.
func (*TeamRepo) InsertOrchestration(ctx context.Context, tx port.Tx, in teams.OrchestrationAgent) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	var res []byte
	if in.Resources != nil {
		if res, err = json.Marshal(in.Resources); err != nil {
			return domain.Internal(err)
		}
	}
	ids := in.Models.ModelIDs
	if ids == nil {
		ids = []string{}
	}
	var vol *int
	if in.VolumeSizeGiB > 0 {
		vol = &in.VolumeSizeGiB
	}
	var access *string
	if in.Models.Access != "" {
		a := string(in.Models.Access)
		access = &a
	}
	_, err = pt.Exec(ctx, `SELECT afs.create_orchestration_agent($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12, $13)`,
		in.TeamID, in.AgentID, nullable(in.Name), nullable(string(in.Tool)), nullable(string(in.WebUIExposure)), nullable(string(in.WebAPIExposure)),
		nullable(string(in.RegistrationMode)), access, ids, nullable(in.Models.DefaultModelID), res, vol, nullable(in.VolumeOnDelete))
	return MapError(err, OpWrite, "orchestration")
}

// WithoutOrchestration implements teams.Repository.
func (*TeamRepo) WithoutOrchestration(ctx context.Context, tx port.Tx, limit int) ([]string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT t.id FROM afs.teams t WHERE t.state NOT IN ('deleting', 'deleted')
  AND NOT EXISTS (SELECT 1 FROM afs.agents a WHERE a.team_id = t.id AND a.role = 'orchestration' AND a.deleted_at IS NULL)
ORDER BY t.id LIMIT $1`, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "team")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "team")
		}
		out = append(out, id)
	}
	return out, MapError(rows.Err(), OpRead, "team")
}

// HasOrchestration implements teams.Repository.
func (*TeamRepo) HasOrchestration(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.agents WHERE team_id = $1 AND role = 'orchestration' AND deleted_at IS NULL)`, teamID).Scan(&ok)
	return ok, MapError(err, OpRead, "team")
}

// ModelsByID implements teams.Repository.
func (*TeamRepo) ModelsByID(ctx context.Context, tx port.Tx, ids []string) (map[string]teams.ModelInfo, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT id, provider, display_name, enabled, coalesce(context_window, 0), coalesce(max_output_tokens, 0), version, updated_at
FROM afs.models WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, MapError(err, OpRead, "model")
	}
	defer rows.Close()
	out := map[string]teams.ModelInfo{}
	for rows.Next() {
		var m teams.ModelInfo
		var v int64
		if err := rows.Scan(&m.ID, &m.Provider, &m.DisplayName, &m.Enabled, &m.ContextWindow, &m.MaxOutputTokens, &v, &m.UpdatedAt); err != nil {
			return nil, MapError(err, OpRead, "model")
		}
		m.Version = v
		out[m.ID] = m
	}
	return out, MapError(rows.Err(), OpRead, "model")
}

// ReplaceModels implements teams.Repository.
func (*TeamRepo) ReplaceModels(ctx context.Context, tx port.Tx, teamID string, ids []string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.team_models WHERE team_id=$1 AND NOT (model_id = ANY($2))`, teamID, ids); err != nil {
		return MapError(err, OpDelete, "model")
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.team_models (team_id, model_id) SELECT $1, unnest($2::text[]) ON CONFLICT DO NOTHING`, teamID, ids)
	return MapError(err, OpWrite, "model")
}

// ServiceTags implements teams.Repository.
func (*TeamRepo) ServiceTags(ctx context.Context, tx port.Tx) ([]domain.ServiceTag, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	return listServiceTags(ctx, pt)
}

// TeamTags implements teams.Repository.
func (*TeamRepo) TeamTags(ctx context.Context, tx port.Tx, teamID string) ([]domain.TagValue, domain.Version, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, 0, err
	}
	rows, err := pt.Query(ctx, `SELECT key, value FROM afs.team_tags WHERE team_id=$1 ORDER BY key`, teamID)
	if err != nil {
		return nil, 0, MapError(err, OpRead, "tag")
	}
	defer rows.Close()
	out := []domain.TagValue{}
	for rows.Next() {
		var v domain.TagValue
		if err := rows.Scan(&v.Key, &v.Value); err != nil {
			return nil, 0, MapError(err, OpRead, "tag")
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, MapError(err, OpRead, "tag")
	}
	rows.Close()
	v, err := revisionVersion(ctx, pt, "team_tags", teamID)
	return out, v, err
}

// ReplaceTeamTags implements teams.Repository.
func (*TeamRepo) ReplaceTeamTags(ctx context.Context, tx port.Tx, teamID string, values []domain.TagValue) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	keys, vals := make([]string, len(values)), make([]string, len(values))
	for i, v := range values {
		keys[i], vals[i] = v.Key, v.Value
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.team_tags WHERE team_id=$1 AND NOT (key = ANY($2))`, teamID, keys); err != nil {
		return MapError(err, OpDelete, "tag")
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.team_tags (team_id, key, value) SELECT $1, k, v FROM unnest($2::text[], $3::text[]) AS u(k, v)
ON CONFLICT (team_id, key) DO UPDATE SET value = EXCLUDED.value WHERE afs.team_tags.value IS DISTINCT FROM EXCLUDED.value`, teamID, keys, vals)
	return MapError(err, OpWrite, "tag")
}
