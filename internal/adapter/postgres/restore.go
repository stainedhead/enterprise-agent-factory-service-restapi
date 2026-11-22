package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

// RestoreRepo implements restore.Repository on the desired-state tables and afs.config_revisions, as the
// calling team admin or service admin (row-level security applies).
type RestoreRepo struct{}

// NewRestoreRepo returns a RestoreRepo.
func NewRestoreRepo() *RestoreRepo { return &RestoreRepo{} }

var _ restore.Repository = (*RestoreRepo)(nil)

// LockTeam implements restore.Repository: SELECT ... FOR UPDATE, so a concurrent mutation of the team
// waits for the restore to commit and the restore waits for one in flight.
func (*RestoreRepo) LockTeam(ctx context.Context, tx port.Tx, teamID string) (restore.TeamLock, bool, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return restore.TeamLock{}, false, err
	}
	var l restore.TeamLock
	var state string
	var ver int64
	err = t.QueryRow(ctx, `SELECT state, version, generation FROM afs.teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&state, &ver, &l.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return l, false, nil
	}
	l.State, l.Version = domain.TeamState(state), domain.Version(ver)
	return l, err == nil, MapError(err, OpRead, "team")
}

// HasRevision implements restore.Repository.
func (*RestoreRepo) HasRevision(ctx context.Context, tx port.Tx, teamID string, rev int64) (bool, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var one int
	err = t.QueryRow(ctx, `SELECT 1 FROM afs.config_revisions WHERE team_id = $1 AND id = $2`, teamID, rev).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, MapError(err, OpRead, "revision")
}

// Target implements restore.Repository: the newest revision of each resource at or before rev. A resource
// whose newest revision is a delete did not exist then.
func (*RestoreRepo) Target(ctx context.Context, tx port.Tx, teamID string, rev int64) (domain.RestoreState, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT DISTINCT ON (resource_kind, resource_id) resource_kind, resource_id, operation, snapshot
FROM afs.config_revisions
WHERE team_id = $1 AND id <= $2 AND resource_kind = ANY ($3)
  AND (resource_kind <> 'external_system_optouts' OR snapshot ->> 'pool_id' IS NULL)
ORDER BY resource_kind, resource_id, id DESC`, teamID, rev, domain.RestoreKinds())
	if err != nil {
		return nil, MapError(err, OpRead, "revision")
	}
	defer rows.Close()
	out := domain.RestoreState{}
	for rows.Next() {
		var kind, id, op string
		var raw []byte
		if err := rows.Scan(&kind, &id, &op, &raw); err != nil {
			return nil, MapError(err, OpRead, "revision")
		}
		if op == "DELETE" {
			continue
		}
		r, err := decodeRow(raw)
		if err != nil {
			return nil, err
		}
		if out[kind] == nil {
			out[kind] = map[string]domain.Row{}
		}
		out[kind][id] = r
	}
	return out, MapError(rows.Err(), OpRead, "revision")
}

// currentQueries select (resource id, row as JSON) of each restorable kind for team $1. A deleting or
// deleted agent is not current: a restore leaves it alone.
var currentQueries = map[string]string{
	"teams":                   `SELECT id, to_jsonb(x) FROM afs.teams x WHERE id = $1`,
	"agents":                  `SELECT id, to_jsonb(x) FROM afs.agents x WHERE team_id = $1 AND deleted_at IS NULL AND state IN ('enabled', 'paused')`,
	"namespace_config_values": `SELECT id, to_jsonb(x) FROM afs.namespace_config_values x WHERE team_id = $1`,
	"agent_config_values":     `SELECT id, to_jsonb(x) FROM afs.agent_config_values x WHERE team_id = $1`,
	"external_system_optouts": `SELECT id, to_jsonb(x) FROM afs.external_system_optouts x WHERE team_id = $1 AND pool_id IS NULL`,
	"team_tags":               `SELECT team_id || '/' || key, to_jsonb(x) FROM afs.team_tags x WHERE team_id = $1`,
	"budgets":                 `SELECT id, to_jsonb(x) FROM afs.budgets x WHERE team_id = $1`,
}

// Current implements restore.Repository.
func (*RestoreRepo) Current(ctx context.Context, tx port.Tx, teamID string) (domain.RestoreState, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	out := domain.RestoreState{}
	for _, kind := range domain.RestoreKinds() {
		rows, err := t.Query(ctx, currentQueries[kind], teamID)
		if err != nil {
			return nil, MapError(err, OpRead, kind)
		}
		m := map[string]domain.Row{}
		for rows.Next() {
			var id string
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return nil, MapError(err, OpRead, kind)
			}
			r, err := decodeRow(raw)
			if err != nil {
				rows.Close()
				return nil, err
			}
			m[id] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, MapError(err, OpRead, kind)
		}
		out[kind] = m
	}
	return out, nil
}

// skippable reports whether err is a refusal by a constraint, guard trigger or privilege of one row: the
// step is then rolled back alone and reported as skipped.
func skippable(err error) (string, bool) {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return "", false
	}
	switch {
	case strings.HasPrefix(pe.Code, "23"), strings.HasPrefix(pe.Code, "22"), pe.Code == "P0001", pe.Code == "55000", pe.Code == "42501":
		return "could not be restored: " + pe.Message, true
	}
	return "", false
}

// restorePK is the WHERE clause (alias x, $1 = team id, keyParam = the key) that selects the row of a resource id.
func restorePK(kind, keyParam string) string {
	switch kind {
	case "teams":
		return "x.id = $1"
	case "team_tags":
		return "x.team_id = $1 AND x.key = " + keyParam
	}
	return "x.team_id = $1 AND x.id = " + keyParam
}

func restoreKey(kind, teamID, id string) string {
	if kind == "team_tags" {
		return strings.TrimPrefix(id, teamID+"/")
	}
	return id
}

// Apply implements restore.Repository. Each step runs in a savepoint.
func (*RestoreRepo) Apply(ctx context.Context, tx port.Tx, teamID string, step domain.RestoreStep) (string, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	kind := step.Change.ResourceKind
	var spec *domain.RestoreSpec
	for i := range domain.RestoreSpecs {
		if domain.RestoreSpecs[i].Kind == kind {
			spec = &domain.RestoreSpecs[i]
		}
	}
	if spec == nil {
		return "", fmt.Errorf("postgres: restore of %s is not supported", kind)
	}
	key := restoreKey(kind, teamID, step.Change.ResourceID)
	var raw []byte
	if step.Row != nil {
		if raw, err = json.Marshal(step.Row); err != nil {
			return "", err
		}
	}
	sp, err := t.Begin(ctx)
	if err != nil {
		return "", MapError(err, OpWrite, kind)
	}
	var execErr error
	switch step.Change.Action {
	case domain.RestoreUpdate:
		sets := make([]string, 0, len(spec.Fields)+1)
		for _, f := range spec.Fields {
			sets = append(sets, f+" = r."+f)
		}
		if kind == "agents" {
			sets = append(sets, "generation = x.generation + 1")
		}
		q := fmt.Sprintf(`UPDATE afs.%s x SET %s FROM jsonb_populate_record(NULL::afs.%s, $2) r WHERE %s`,
			kind, strings.Join(sets, ", "), kind, restorePK(kind, "$3"))
		if kind == "teams" {
			_, execErr = sp.Exec(ctx, q, teamID, raw)
		} else {
			_, execErr = sp.Exec(ctx, q, teamID, raw, key)
		}
	case domain.RestoreCreate:
		_, execErr = sp.Exec(ctx, fmt.Sprintf(`INSERT INTO afs.%s SELECT * FROM jsonb_populate_record(NULL::afs.%s, $1)`, kind, kind), raw)
	case domain.RestoreDelete:
		_, execErr = sp.Exec(ctx, fmt.Sprintf(`DELETE FROM afs.%s x WHERE %s`, kind, restorePK(kind, "$2")), teamID, key)
	default:
		execErr = fmt.Errorf("postgres: restore action %q", step.Change.Action)
	}
	if execErr != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return "", MapError(rbErr, OpWrite, kind)
		}
		if why, ok := skippable(execErr); ok {
			return why, nil
		}
		return "", MapError(execErr, OpWrite, kind)
	}
	return "", MapError(sp.Commit(ctx), OpWrite, kind)
}

// BumpGeneration implements restore.Repository.
func (*RestoreRepo) BumpGeneration(ctx context.Context, tx port.Tx, teamID string) (int64, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return 0, err
	}
	var gen int64
	err = t.QueryRow(ctx, `UPDATE afs.teams SET generation = generation + 1 WHERE id = $1 RETURNING generation`, teamID).Scan(&gen)
	return gen, MapError(err, OpWrite, "team")
}

