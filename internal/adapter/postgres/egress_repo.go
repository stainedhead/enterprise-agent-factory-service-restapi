package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// EgressSource implements egress.Source on the afs_reconciler pool (M2-H1).
type EgressSource struct{ runner *TxRunner }

// NewEgressSource returns an EgressSource.
func NewEgressSource(runner *TxRunner) *EgressSource { return &EgressSource{runner: runner} }

var _ egress.Source = (*EgressSource)(nil)

const (
	loadEgressTeamSQL   = `SELECT id, namespace, state FROM afs.teams WHERE id = $1`
	loadEgressAgentsSQL = `SELECT id, state FROM afs.agents WHERE team_id = $1 ORDER BY id`
	// agent_external_access has one row per agent and system the agent's team can see; disabled_at
	// names the level that switched the system off.
	loadEgressAccessSQL = `SELECT agent_id, system_id, slug, category, endpoints, coalesce(disabled_at, '')
FROM afs.agent_external_access WHERE team_id = $1 ORDER BY agent_id, slug`
)

// LoadEgress implements egress.Source.
func (s *EgressSource) LoadEgress(ctx context.Context, teamID string) (egress.Desired, error) {
	var d egress.Desired
	err := s.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, loadEgressTeamSQL, teamID).Scan(&d.TeamID, &d.Namespace, &d.State)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("team")
		}
		if err != nil {
			return MapError(err, OpRead, "team")
		}
		rows, err := t.Query(ctx, loadEgressAgentsSQL, teamID)
		if err != nil {
			return MapError(err, OpRead, "agents")
		}
		idx := map[string]int{}
		for rows.Next() {
			var a egress.Agent
			if err := rows.Scan(&a.ID, &a.State); err != nil {
				rows.Close()
				return MapError(err, OpRead, "agents")
			}
			idx[a.ID] = len(d.Agents)
			d.Agents = append(d.Agents, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return MapError(err, OpRead, "agents")
		}
		rows, err = t.Query(ctx, loadEgressAccessSQL, teamID)
		if err != nil {
			return MapError(err, OpRead, "external systems")
		}
		defer rows.Close()
		for rows.Next() {
			var (
				agent string
				sys   egress.System
			)
			if err := rows.Scan(&agent, &sys.ID, &sys.Slug, &sys.Category, &sys.Endpoints, &sys.DisabledAt); err != nil {
				return MapError(err, OpRead, "external systems")
			}
			if i, ok := idx[agent]; ok {
				d.Agents[i].Systems = append(d.Agents[i].Systems, sys)
			}
		}
		return MapError(rows.Err(), OpRead, "external systems")
	})
	return d, err
}

// AgentConditions implements egress.AgentConditions: it merges one condition into the agent's
// resource_status row and leaves the phase and every other column alone.
type AgentConditions struct {
	runner *TxRunner
	now    func() time.Time
}

// NewAgentConditions returns an AgentConditions.
func NewAgentConditions(runner *TxRunner, now func() time.Time) *AgentConditions {
	if now == nil {
		now = time.Now
	}
	return &AgentConditions{runner: runner, now: now}
}

var _ egress.AgentConditions = (*AgentConditions)(nil)

// SetCondition implements egress.AgentConditions. An agent without a status row is skipped.
func (a *AgentConditions) SetCondition(ctx context.Context, agentID string, c domain.Condition) error {
	return a.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var raw []byte
		err = t.QueryRow(ctx, `SELECT conditions FROM afs.resource_status WHERE resource_kind = 'agent' AND resource_id = $1 FOR UPDATE`, agentID).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return MapError(err, OpRead, "resource status")
		}
		var conds []domain.Condition
		if err := json.Unmarshal(raw, &conds); err != nil {
			return err
		}
		i := slices.IndexFunc(conds, func(x domain.Condition) bool { return x.Type == c.Type })
		if i >= 0 && conds[i].Status == c.Status && conds[i].Reason == c.Reason && conds[i].Message == c.Message {
			return nil
		}
		if i >= 0 && conds[i].Status == c.Status {
			c.LastTransitionTime = conds[i].LastTransitionTime
		} else {
			c.LastTransitionTime = a.now().UTC()
		}
		if i >= 0 {
			conds[i] = c
		} else {
			conds = append(conds, c)
		}
		out, err := json.Marshal(conds)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `UPDATE afs.resource_status SET conditions = $2 WHERE resource_kind = 'agent' AND resource_id = $1`, agentID, out)
		return MapError(err, OpWrite, "resource status")
	})
}

// TeamLister adds team-level kinds (one resource per team that is not deleting or deleted) to another
// engine.Lister. Their resources are rows of the team, not resources of their own, so the stale-generation
// poll does not apply: a change notifies the engine directly.
type TeamLister struct {
	engine.Lister
	pool  Querier2
	kinds map[engine.Kind]bool
}

// NewTeamLister wraps base; pool is the afs_reconciler pool.
func NewTeamLister(base engine.Lister, pool Querier2, kinds ...engine.Kind) *TeamLister {
	m := map[engine.Kind]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return &TeamLister{Lister: base, pool: pool, kinds: m}
}

var _ engine.Lister = (*TeamLister)(nil)

// List implements engine.Lister.
func (l *TeamLister) List(ctx context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	var rest, mine []engine.Kind
	for _, k := range kinds {
		if l.kinds[k] {
			mine = append(mine, k)
		} else {
			rest = append(rest, k)
		}
	}
	var out []engine.Ref
	if len(rest) > 0 {
		var err error
		if out, err = l.Lister.List(ctx, rest); err != nil {
			return nil, err
		}
	}
	if len(mine) == 0 {
		return out, nil
	}
	rows, err := l.pool.Query(ctx, `SELECT id FROM afs.teams WHERE state IN ('active', 'suspended') ORDER BY id`)
	if err != nil {
		return nil, MapError(err, OpRead, "teams")
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "teams")
		}
		for _, k := range mine {
			out = append(out, engine.NewRef(k, id, id))
		}
	}
	return out, MapError(rows.Err(), OpRead, "teams")
}

// Stale implements engine.Lister: the team-level kinds have no generation to compare.
func (l *TeamLister) Stale(ctx context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	var rest []engine.Kind
	for _, k := range kinds {
		if !l.kinds[k] {
			rest = append(rest, k)
		}
	}
	if len(rest) == 0 {
		return nil, nil
	}
	return l.Lister.Stale(ctx, rest)
}
