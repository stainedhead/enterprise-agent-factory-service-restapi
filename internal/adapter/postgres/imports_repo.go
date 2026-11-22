package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ImportRepo implements imports.Repository. It writes desired state through the ordinary tables as the
// calling service admin, so row-level security, constraints and guard triggers apply exactly as they
// do for the API's own writes. Only the columns of the bundle allowlists (bundle_tables.go) are written,
// resolved ids and defaults aside: a bundle cannot set a version, a creator, a status or a secret value.
type ImportRepo struct{ ids port.IDGen }

// NewImportRepo returns an ImportRepo.
func NewImportRepo(ids port.IDGen) *ImportRepo { return &ImportRepo{ids: ids} }

var _ imports.Repository = (*ImportRepo)(nil)

// tbl describes how a bundle row is written to a table.
type tbl struct {
	name   string
	prefix domain.IDPrefix // id prefix; empty when the table has no id column
	cols   []string        // columns written from the bundle on insert
	upd    []string        // columns written from the bundle on update; nil means cols
	bump   bool            // raise generation when an update changed something
}

var (
	tblTeams = tbl{name: "teams", prefix: domain.PrefixTeam, cols: bundleTeamCols, bump: true}
	// slug is the identity of a team and can never change.
	tblTeamsUpd   = bundleTeamCols[1:]
	tblBudget     = tbl{name: "budgets", prefix: domain.PrefixBudget, cols: bundleBudgetCols}
	tblNSConfig   = tbl{name: "namespace_config_values", prefix: domain.PrefixNamespaceConfig, cols: bundleNsConfigCols, upd: []string{"config_value", "description"}}
	tblTeamSystem = tbl{name: "external_systems", prefix: domain.PrefixExtSystem, cols: bundleExtSystemCols, upd: bundleExtSystemCols[1:]}
	tblCatalog    = tbl{name: "external_systems", prefix: domain.PrefixExtSystem, cols: append(append([]string{}, bundleExtSystemCols...), "builtin")}
	tblOptout     = tbl{name: "external_system_optouts", prefix: domain.PrefixExtSystemOptout, cols: []string{"reason"}}
	tblSecret     = tbl{name: "secrets", prefix: domain.PrefixSecret, cols: []string{"name", "description", "secrets_manager_arn", "rotation_days"},
		upd: []string{"description", "rotation_days"}}
	tblAgent       = tbl{name: "agents", prefix: domain.PrefixAgent, cols: bundleAgentCols, upd: bundleAgentCols[1:], bump: true}
	tblAgentConfig = tbl{name: "agent_config_values", prefix: domain.PrefixAgentConfig, cols: bundleAgentConfigCols, upd: bundleAgentConfigCols[1:]}
	tblIdentity    = tbl{name: "agent_identities", prefix: domain.PrefixAgentIdentity, cols: []string{"kind", "external_ref"}, upd: []string{"external_ref"}}
	tblRoleLink    = tbl{name: "agent_role_links", prefix: domain.PrefixRoleLink, cols: bundleRoleLinkCols,
		upd: []string{"profile_name", "max_session_seconds"}}
)

type importer struct {
	t       *Tx
	ids     port.IDGen
	teamID  string
	changes []imports.Change
	notes   []string
}

// upsert finds the row with the SQL condition find (a query selecting its id) and inserts it when absent,
// or updates the tbl.upd columns when they differ. extra holds the resolved columns that are written on
// insert only (team_id, agent_id, ...). It returns the row's id and what happened: created, updated or "".
func (im *importer) upsert(ctx context.Context, tb tbl, find string, findArgs []any, row domain.Row, extra map[string]any, label string) (string, string, error) {
	var id string
	err := im.t.QueryRow(ctx, find, findArgs...).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", MapError(err, OpRead, tb.name)
	}
	doc := map[string]any{}
	for _, c := range tb.cols {
		if v, ok := row[c]; ok {
			doc[c] = v
		}
	}
	for k, v := range extra {
		doc[k] = v
	}
	if errors.Is(err, pgx.ErrNoRows) {
		nid, err := im.ids.NewID(tb.prefix)
		if err != nil {
			return "", "", err
		}
		doc["id"] = nid.String()
		cols := append([]string{"id"}, tb.cols...)
		for k := range extra {
			cols = append(cols, k)
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return "", "", fmt.Errorf("import %s: marshal row: %w", tb.name, err)
		}
		list := strings.Join(cols, ", ")
		if _, err := im.t.Exec(ctx, fmt.Sprintf(`INSERT INTO afs.%s (%s) SELECT %s FROM jsonb_populate_record(NULL::afs.%s, $1) r`,
			tb.name, list, "r."+strings.Join(cols, ", r."), tb.name), raw); err != nil {
			return "", "", MapError(err, OpWrite, tb.name)
		}
		im.changes = append(im.changes, imports.Change{Kind: tb.name, ID: nid.String(), Name: label, Action: "created"})
		return nid.String(), "created", nil
	}
	upd := tb.upd
	if upd == nil {
		upd = tb.cols
	}
	if len(upd) == 0 {
		return id, "", nil
	}
	sets := make([]string, len(upd))
	for i, c := range upd {
		sets[i] = c + " = r." + c
	}
	if tb.bump {
		sets = append(sets, "generation = x.generation + 1")
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", "", fmt.Errorf("import %s: marshal row: %w", tb.name, err)
	}
	tag, err := im.t.Exec(ctx, fmt.Sprintf(`UPDATE afs.%s x SET %s FROM jsonb_populate_record(NULL::afs.%s, $1) r
WHERE x.id = $2 AND ROW(x.%s) IS DISTINCT FROM ROW(r.%s)`, tb.name, strings.Join(sets, ", "), tb.name,
		strings.Join(upd, ", x."), strings.Join(upd, ", r.")), raw, id)
	if err != nil {
		return "", "", MapError(err, OpWrite, tb.name)
	}
	if tag.RowsAffected() == 0 {
		return id, "", nil
	}
	im.changes = append(im.changes, imports.Change{Kind: tb.name, ID: id, Name: label, Action: "updated"})
	return id, "updated", nil
}

func str(r domain.Row, k string) string { s, _ := r[k].(string); return s }

// nullIfEmpty returns nil for an empty string so it binds as NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ApplyCatalog implements imports.Repository: service level systems are created when missing and never
// overwritten (an operator may have edited them).
func (r *ImportRepo) ApplyCatalog(ctx context.Context, tx port.Tx, systems []domain.Row) ([]imports.Change, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	im := &importer{t: t, ids: r.ids}
	seen := tblCatalog
	seen.upd = []string{}
	for _, s := range systems {
		slug := str(s, "slug")
		if _, _, err := im.upsert(ctx, seen, `SELECT id FROM afs.external_systems WHERE team_id IS NULL AND slug = $1`, []any{slug}, s, nil, slug); err != nil {
			return nil, err
		}
	}
	return im.changes, nil
}

// RecordImport implements imports.Repository.
func (r *ImportRepo) RecordImport(ctx context.Context, tx port.Tx, rec imports.Record) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	status := "succeeded"
	if rec.Failed {
		status = "failed"
	}
	summary, _ := json.Marshal(rec.Summary)
	_, err = t.Exec(ctx, `INSERT INTO afs.export_bundles (id, direction, schema_version, sha256, dry_run, status, summary, finished_at)
VALUES ($1, 'import', $2, $3, $4, $5, $6, now())`, rec.ID, rec.SchemaVersion, rec.SHA256, rec.DryRun, status, summary)
	return MapError(err, OpWrite, "import")
}

// ApplyTeam implements imports.Repository.
func (r *ImportRepo) ApplyTeam(ctx context.Context, tx port.Tx, tb domain.TeamBundle) (imports.TeamOutcome, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return imports.TeamOutcome{}, err
	}
	im := &importer{t: t, ids: r.ids}
	out := imports.TeamOutcome{}
	if err := im.team(ctx, tb, &out); err != nil {
		return imports.TeamOutcome{}, err
	}
	out.TeamID, out.Changes, out.Notes = im.teamID, im.changes, im.notes
	return out, nil
}

func (im *importer) team(ctx context.Context, tb domain.TeamBundle, out *imports.TeamOutcome) error {
	slug := tb.Slug()
	var state string
	err := im.t.QueryRow(ctx, `SELECT id, state FROM afs.teams WHERE slug = $1`, slug).Scan(&im.teamID, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		out.Created = true
	case err != nil:
		return MapError(err, OpRead, "team")
	case state == "deleting" || state == "deleted":
		return domain.Conflict("invalid_state", "the team "+slug+" is being deleted or was deleted", nil)
	}
	tt := tblTeams
	tt.upd = tblTeamsUpd
	teamID, _, err := im.upsert(ctx, tt, `SELECT id FROM afs.teams WHERE slug = $1`, []any{slug}, tb.Team, nil, slug)
	if err != nil {
		return err
	}
	im.teamID = teamID
	// A new team's own row is its single "created" change; resources below are reported as they are.
	for _, step := range []func(context.Context, domain.TeamBundle) error{im.models, im.tags, im.budget, im.members, im.nsConfig, im.teamSystems, im.teamOptouts} {
		if err := step(ctx, tb); err != nil {
			return err
		}
	}
	secretIDs, err := im.secrets(ctx, tb, "")
	if err != nil {
		return err
	}
	for _, a := range tb.Agents {
		if err := im.agent(ctx, a, tb, secretIDs); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) models(ctx context.Context, tb domain.TeamBundle) error {
	for _, m := range tb.Models {
		tag, err := im.t.Exec(ctx, `INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, im.teamID, m)
		if err != nil {
			return MapError(err, OpWrite, "team model")
		}
		if tag.RowsAffected() > 0 {
			im.changes = append(im.changes, imports.Change{Kind: "team_models", ID: im.teamID + "/" + m, Name: m, Action: "created"})
		}
	}
	return nil
}

func (im *importer) tags(ctx context.Context, tb domain.TeamBundle) error {
	for _, g := range tb.Tags {
		k, v := str(g, "key"), str(g, "value")
		var cur string
		err := im.t.QueryRow(ctx, `SELECT value FROM afs.team_tags WHERE team_id = $1 AND key = $2`, im.teamID, k).Scan(&cur)
		action := ""
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = im.t.Exec(ctx, `INSERT INTO afs.team_tags (team_id, key, value) VALUES ($1, $2, $3)`, im.teamID, k, v)
			action = "created"
		case err == nil && cur != v:
			_, err = im.t.Exec(ctx, `UPDATE afs.team_tags SET value = $3 WHERE team_id = $1 AND key = $2`, im.teamID, k, v)
			action = "updated"
		}
		if err != nil {
			return MapError(err, OpWrite, "team tag")
		}
		if action != "" {
			im.changes = append(im.changes, imports.Change{Kind: "team_tags", ID: im.teamID + "/" + k, Name: k, Action: action})
		}
	}
	return nil
}

func (im *importer) budget(ctx context.Context, tb domain.TeamBundle) error {
	if len(tb.Budget) == 0 {
		return nil
	}
	_, _, err := im.upsert(ctx, tblBudget, `SELECT id FROM afs.budgets WHERE team_id = $1`, []any{im.teamID}, tb.Budget,
		map[string]any{"team_id": im.teamID}, "budget")
	return err
}

func (im *importer) members(ctx context.Context, tb domain.TeamBundle) error {
	for _, m := range tb.Members {
		role, subj := str(m, "role"), str(m, "subject_type")
		if role == "service_admin" {
			continue
		}
		var userID, group any
		label := str(m, "group_name")
		if subj == "user" {
			email := str(m, "user_email")
			label = email
			var id string
			err := im.t.QueryRow(ctx, `SELECT id FROM afs.users WHERE lower(email) = lower($1) ORDER BY id LIMIT 1`, email).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				im.notes = append(im.notes, fmt.Sprintf("members/%s: skipped, the user has not signed in to this service", email))
				continue
			}
			if err != nil {
				return MapError(err, OpRead, "user")
			}
			userID = id
		} else {
			group = nullIfEmpty(label)
		}
		var one int
		err := im.t.QueryRow(ctx, `SELECT 1 FROM afs.role_bindings WHERE team_id = $1 AND role = $2 AND subject_type = $3
  AND user_id IS NOT DISTINCT FROM $4 AND group_name IS NOT DISTINCT FROM $5`, im.teamID, role, subj, userID, group).Scan(&one)
		if err == nil {
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return MapError(err, OpRead, "role binding")
		}
		id, err := im.ids.NewID(domain.PrefixBinding)
		if err != nil {
			return err
		}
		if _, err := im.t.Exec(ctx, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, user_id, group_name) VALUES ($1, $2, $3, $4, $5, $6)`,
			id.String(), role, im.teamID, subj, userID, group); err != nil {
			return MapError(err, OpWrite, "role binding")
		}
		im.changes = append(im.changes, imports.Change{Kind: "role_bindings", ID: id.String(), Name: role + ":" + label, Action: "created"})
	}
	return nil
}

func (im *importer) nsConfig(ctx context.Context, tb domain.TeamBundle) error {
	for _, c := range tb.NamespaceConfig {
		n := str(c, "name")
		if _, _, err := im.upsert(ctx, tblNSConfig, `SELECT id FROM afs.namespace_config_values WHERE team_id = $1 AND name = $2`,
			[]any{im.teamID, n}, c, map[string]any{"team_id": im.teamID}, n); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) teamSystems(ctx context.Context, tb domain.TeamBundle) error {
	for _, s := range tb.ExternalSystems {
		slug := str(s, "slug")
		if _, _, err := im.upsert(ctx, tblTeamSystem, `SELECT id FROM afs.external_systems WHERE team_id = $1 AND slug = $2`,
			[]any{im.teamID, slug}, s, map[string]any{"team_id": im.teamID}, slug); err != nil {
			return err
		}
	}
	return nil
}

// systemID resolves an external system by slug: the team's own declaration first, then the catalog.
func (im *importer) systemID(ctx context.Context, slug string) (string, error) {
	var id string
	err := im.t.QueryRow(ctx, `SELECT id FROM afs.external_systems WHERE slug = $1 AND (team_id = $2 OR team_id IS NULL)
ORDER BY team_id NULLS LAST LIMIT 1`, slug, im.teamID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.Validation(domain.FieldError{Field: "externalSystemOptouts", Reason: "unknown external system " + slug})
	}
	return id, MapError(err, OpRead, "external system")
}

func (im *importer) optouts(ctx context.Context, rows []domain.Row, agentID string) error {
	for _, o := range rows {
		slug := str(o, "system_slug")
		sys, err := im.systemID(ctx, slug)
		if err != nil {
			return err
		}
		extra := map[string]any{"team_id": im.teamID, "system_id": sys}
		if agentID != "" {
			extra["agent_id"] = agentID
		}
		if _, _, err := im.upsert(ctx, tblOptout, `SELECT id FROM afs.external_system_optouts WHERE team_id = $1 AND agent_id IS NOT DISTINCT FROM $2
  AND system_id = $3 AND pool_id IS NULL`, []any{im.teamID, nullIfEmpty(agentID), sys}, o, extra, "opt-out "+slug); err != nil {
			return err
		}
	}
	return nil
}

func (im *importer) teamOptouts(ctx context.Context, tb domain.TeamBundle) error {
	return im.optouts(ctx, tb.Optouts, "")
}

// secrets imports the secret references of the bundle (team shared ones when agentName is empty) and
// returns path -> id. A created secret has no value: it must be put again (RA-US-14.2).
func (im *importer) secrets(ctx context.Context, tb domain.TeamBundle, agentName string) (map[string]string, error) {
	ids := map[string]string{}
	agentIDs := map[string]string{}
	for _, s := range tb.Secrets {
		owner := str(s, "agent_name")
		if owner != agentName {
			continue // team shared secrets come first, agent scoped ones with their agent
		}
		var agentID any
		if owner != "" {
			if _, ok := agentIDs[owner]; !ok {
				var id string
				if err := im.t.QueryRow(ctx, `SELECT id FROM afs.agents WHERE team_id = $1 AND name = $2 AND deleted_at IS NULL`, im.teamID, owner).Scan(&id); err != nil {
					return nil, MapError(err, OpRead, "agent")
				}
				agentIDs[owner] = id
			}
			agentID = agentIDs[owner]
		}
		id, action, err := im.upsert(ctx, tblSecret, `SELECT id FROM afs.secrets WHERE team_id = $1 AND agent_id IS NOT DISTINCT FROM $2
  AND name = $3 AND pool_id IS NULL`, []any{im.teamID, agentID, str(s, "name")}, s,
			map[string]any{"team_id": im.teamID, "agent_id": agentID, "origin": "managed"}, secretLabel(s))
		if err != nil {
			return nil, err
		}
		ids[str(s, "path")] = id
		if action == "created" {
			im.notes = append(im.notes, fmt.Sprintf("secrets/%s: created without a value; put the value again", secretLabel(s)))
		}
	}
	return ids, nil
}

func secretLabel(s domain.Row) string {
	if a := str(s, "agent_name"); a != "" {
		return a + "/" + str(s, "name")
	}
	return str(s, "name")
}

func (im *importer) agent(ctx context.Context, a domain.AgentBundle, tb domain.TeamBundle, shared map[string]string) error {
	name := str(a.Agent, "name")
	agentID, _, err := im.upsert(ctx, tblAgent, `SELECT id FROM afs.agents WHERE team_id = $1 AND name = $2 AND deleted_at IS NULL`,
		[]any{im.teamID, name}, a.Agent, map[string]any{"team_id": im.teamID}, name)
	if err != nil {
		return err
	}
	own, err := im.secrets(ctx, domain.TeamBundle{Secrets: tb.Secrets}, name)
	if err != nil {
		return err
	}
	byPath := map[string]string{}
	for k, v := range shared {
		byPath[k] = v
	}
	for k, v := range own {
		byPath[k] = v
	}
	for _, c := range a.ConfigValues {
		n := str(c, "name")
		if _, _, err := im.upsert(ctx, tblAgentConfig, `SELECT id FROM afs.agent_config_values WHERE agent_id = $1 AND name = $2`, []any{agentID, n}, c,
			map[string]any{"team_id": im.teamID, "agent_id": agentID}, name+"/"+n); err != nil {
			return err
		}
	}
	for _, b := range a.SecretBindings {
		if err := im.binding(ctx, agentID, name, b, byPath); err != nil {
			return err
		}
	}
	for _, i := range a.Identities {
		extra := map[string]any{"team_id": im.teamID, "agent_id": agentID, "status": "pending"}
		if p := str(i, "secret_path"); p != "" {
			id, ok := byPath[p]
			if !ok {
				return domain.Validation(domain.FieldError{Field: "identities", Reason: "secret " + p + " is not in the bundle"})
			}
			extra["secret_id"] = id
		}
		if _, _, err := im.upsert(ctx, tblIdentity, `SELECT id FROM afs.agent_identities WHERE team_id = $1 AND agent_id = $2 AND kind = $3`,
			[]any{im.teamID, agentID, str(i, "kind")}, i, extra, name+"/"+str(i, "kind")); err != nil {
			return err
		}
	}
	for _, l := range a.RoleLinks {
		// A role link always starts pending_trust: trust in the target account is verified afresh (RA-US-14.3).
		if _, _, err := im.upsert(ctx, tblRoleLink, `SELECT id FROM afs.agent_role_links WHERE agent_id = $1 AND role_arn = $2`,
			[]any{agentID, str(l, "role_arn")}, l, map[string]any{"team_id": im.teamID, "agent_id": agentID, "status": "pending_trust"},
			name+"/"+str(l, "role_arn")); err != nil {
			return err
		}
	}
	if err := im.optouts(ctx, a.Optouts, agentID); err != nil {
		return err
	}
	if ref := str(a.LatestSnapshot, "ebs_snapshot_id"); ref != "" {
		im.notes = append(im.notes, fmt.Sprintf("agents/%s: volume snapshot %s is not restored; share or copy it to the target account first", name, ref))
	}
	return nil
}

func (im *importer) binding(ctx context.Context, agentID, agentName string, b domain.Row, byPath map[string]string) error {
	path, target := str(b, "secret_path"), str(b, "target")
	secretID, ok := byPath[path]
	if !ok {
		return domain.Validation(domain.FieldError{Field: "secretBindings", Reason: "secret " + path + " is not in the bundle"})
	}
	var curSecret, curDelivery string
	err := im.t.QueryRow(ctx, `SELECT secret_id, delivery FROM afs.agent_secret_bindings WHERE agent_id = $1 AND target = $2`, agentID, target).Scan(&curSecret, &curDelivery)
	action := ""
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = im.t.Exec(ctx, `INSERT INTO afs.agent_secret_bindings (team_id, agent_id, secret_id, delivery, target) VALUES ($1, $2, $3, $4, $5)`,
			im.teamID, agentID, secretID, str(b, "delivery"), target)
		action = "created"
	case err == nil && (curSecret != secretID || curDelivery != str(b, "delivery")):
		_, err = im.t.Exec(ctx, `UPDATE afs.agent_secret_bindings SET secret_id = $3, delivery = $4 WHERE agent_id = $1 AND target = $2`,
			agentID, target, secretID, str(b, "delivery"))
		action = "updated"
	}
	if err != nil {
		return MapError(err, OpWrite, "secret binding")
	}
	if action != "" {
		im.changes = append(im.changes, imports.Change{Kind: "agent_secret_bindings", ID: agentID + "/" + target, Name: agentName + "/" + target, Action: action})
	}
	return nil
}
