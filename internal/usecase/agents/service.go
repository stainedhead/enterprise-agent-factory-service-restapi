// Package agents implements the agent operations of API contract §4.4 (M2-B). The API writes the
// desired state of an agent (one pod, no replica count) and the reconciler acts on it: a delete
// only marks the agent, pause, resume and restart change its state or generation, and an agent
// whose template is disabled later is not touched here (RA-US-4.5).
package agents

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindAgent = "agents"
	// StatusKind is the resource_kind the reconciler writes agent status under (engine.KindAgent) and
	// the kind GetAgentStatus reports.
	StatusKind = "agent"
)

// TemplateInfo is a catalog agent template as the checks need it.
type TemplateInfo struct {
	ID             string
	Name           string
	Enabled        bool
	AllowedDigests []string
}

// SecretInfo says who owns a secret: an agent, a pool, or neither (team shared).
type SecretInfo struct {
	AgentID string
	PoolID  string
}

// Repository is the agent store, plus the catalog reads CreateAgent checks against.
type Repository interface {
	// List returns the team's agents that are not deleted, with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Agent, error)
	// Get returns one agent of the team that is not deleted; NotFound otherwise. forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Agent, error)
	// Insert stores a new agent, its bindings and the catalog template reference.
	Insert(ctx context.Context, tx port.Tx, a domain.Agent, templateID string) error
	// Update stores the agent's spec, state, generation and bindings. Template and image are written
	// only when templateChanged, so the database's template guard does not run for any other change
	// (RA-US-4.5). PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, a domain.Agent, templateID string, templateChanged bool, expect domain.Version) error
	// Template returns the catalog template of that name.
	Template(ctx context.Context, tx port.Tx, name string) (TemplateInfo, bool, error)
	// ModelEnabled reports whether the model is enabled and offered to the team.
	ModelEnabled(ctx context.Context, tx port.Tx, teamID, modelID string) (bool, error)
	// StorageClassEnabled reports whether the storage class exists and is offered.
	StorageClassEnabled(ctx context.Context, tx port.Tx, name string) (bool, error)
	// Secrets returns the owners of the team's secrets with these ids; unknown ids are absent.
	Secrets(ctx context.Context, tx port.Tx, teamID string, ids []string) (map[string]SecretInfo, error)
	// Snapshots lists the agent's snapshots, newest id order, with id less than after (or all when
	// after is empty).
	Snapshots(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.Snapshot, error)
	// InsertSnapshot stores a manual snapshot request.
	InsertSnapshot(ctx context.Context, tx port.Tx, teamID string, s domain.Snapshot) error
}

// guard is the validator CreateAgent and UpdateAgent share with RestoreRevision (FR-R01).
func (s *Service) guard() Guard { return Guard{Src: s.Repo} }

// Service is the agent use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is an agent with the replayed response of an idempotent call.
type Result struct {
	Agent  domain.Agent
	Replay *port.StoredResponse
}

// SnapshotResult is a snapshot with the replayed response of an idempotent create.
type SnapshotResult struct {
	Snapshot domain.Snapshot
	Replay   *port.StoredResponse
}

func auditOf(a domain.Agent) map[string]any {
	b := make([]map[string]string, 0, len(a.Bindings))
	for _, x := range a.Bindings {
		b = append(b, map[string]string{"secretId": x.SecretID, "delivery": x.Delivery, "target": x.Target})
	}
	return map[string]any{"name": string(a.Name), "description": a.Description, "template": a.Template, "image": string(a.Image),
		"modelId": a.ModelID, "resources": a.Resources, "iamIdentity": a.IAMIdentity, "state": a.State, "generation": a.Generation,
		"volume": map[string]any{"sizeGiB": a.Volume.SizeGiB, "storageClass": a.Volume.StorageClass, "onDelete": a.Volume.OnDelete,
			"snapshotSchedule": a.Volume.SnapshotSchedule, "snapshotRetain": a.Volume.SnapshotRetain},
		"secretBindings": b}
}

// emit publishes an agent event; unlike Deps.Emit it sets agentId in the detail.
func (s *Service) emit(ctx context.Context, tx port.Tx, c acting.Caller, typ domain.EventType, a domain.Agent, data map[string]any) error {
	eid, err := s.IDs.NewID("evt")
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, eid.String(), s.Clock.Now(), c.Session().Actor, a.TeamID, kindAgent, a.ID, data)
	if err != nil {
		return err
	}
	ev.Detail.AgentID = a.ID
	return s.Outbox.Publish(ctx, tx, ev)
}

// List implements ListAgents.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.Agent, string, error) {
	p = p.Normalized()
	var rows []domain.Agent
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(a domain.Agent) string { return a.ID })
	for i := range page {
		page[i] = page[i].WithDeletingStatus()
	}
	return page, next, nil
}

// Get implements GetAgent and GetAgentStatus.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID, id string) (domain.Agent, error) {
	var a domain.Agent
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		a, err = s.Repo.Get(ctx, tx, teamID, id, false)
		return err
	})
	return a.WithDeletingStatus(), err
}

// Create implements CreateAgent: the model must be enabled for the team, the storage class offered,
// the template known, enabled and listing the image digest, and no binding may name another
// agent's secret. One transaction writes the row, the audit record and agent.created.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewAgentParams, idem *acting.Idem) (Result, error) {
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixAgent)
		if err != nil {
			return err
		}
		a, err := domain.NewAgent(id.String(), teamID, in, s.Now())
		if err != nil {
			return err
		}
		if err := s.guard().CheckModel(ctx, tx, teamID, a.ModelID); err != nil {
			return err
		}
		if err := s.guard().CheckStorageClass(ctx, tx, a.Volume.StorageClass); err != nil {
			return err
		}
		tplID, err := s.guard().CheckTemplate(ctx, tx, a.Template, a.Image)
		if err != nil {
			return err
		}
		if err := s.guard().CheckBindings(ctx, tx, teamID, a.ID, a.Bindings); err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, a, tplID); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateAgent", kindAgent, a.ID, teamID, nil, auditOf(a))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventAgentCreated, a, map[string]any{"name": string(a.Name), "template": a.Template}); err != nil {
			return err
		}
		if res.Agent, err = s.Repo.Get(ctx, tx, teamID, a.ID, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Agent)
	})
	return res, err
}

func changedFields(p domain.AgentPatch) []string {
	var f []string
	add := func(set bool, n string) {
		if set {
			f = append(f, n)
		}
	}
	add(p.Description != nil, "description")
	add(p.Template != nil, "template")
	add(p.Image != nil, "image")
	add(p.ModelID != nil, "modelId")
	add(p.Resources != nil, "resources")
	add(p.IAMIdentity != nil, "iamIdentity")
	add(p.Volume != nil, "volume")
	add(p.Bindings != nil, "secretBindings")
	return f
}

// Update implements UpdateAgent (If-Match required). Template, image, model and bindings are
// checked only when the request changes them, so an agent whose template was disabled later can
// still be edited in other fields (RA-US-4.5).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version, p domain.AgentPatch) (domain.Agent, error) {
	var out domain.Agent
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(p, s.Now())
		if err != nil {
			return err
		}
		tplChanged := next.Template != cur.Template || next.Image != cur.Image
		tplID := ""
		if tplChanged {
			if tplID, err = s.guard().CheckTemplate(ctx, tx, next.Template, next.Image); err != nil {
				return err
			}
		}
		if next.ModelID != cur.ModelID {
			if err := s.guard().CheckModel(ctx, tx, teamID, next.ModelID); err != nil {
				return err
			}
		}
		if p.Bindings != nil {
			if err := s.guard().CheckBindings(ctx, tx, teamID, id, next.Bindings); err != nil {
				return err
			}
		}
		if err := s.Repo.Update(ctx, tx, next, tplID, tplChanged, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdateAgent", kindAgent, id, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventAgentUpdated, next, map[string]any{"fields": changedFields(p)}); err != nil {
			return err
		}
		out, err = s.Repo.Get(ctx, tx, teamID, id, false)
		return err
	})
	return out, err
}

// Delete implements DeleteAgent: the agent is marked deleting (status Deleting) and the reconciler
// runs the delete steps and finalizes it. Deleting a deleting agent changes nothing.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) (domain.Agent, error) {
	var out domain.Agent
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, changed, err := cur.MarkDeleting(s.Now())
		if err != nil {
			return err
		}
		if !changed {
			out = next
			return nil
		}
		if err := s.Repo.Update(ctx, tx, next, "", false, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("DeleteAgent", kindAgent, id, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventAgentDeleted, next, map[string]any{"name": string(next.Name)}); err != nil {
			return err
		}
		if out, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		out = out.WithDeletingStatus()
		return nil
	})
	return out, err
}

type verb struct {
	action string
	event  domain.EventType
	apply  func(domain.Agent) (domain.Agent, bool, error)
}

func (s *Service) transition(ctx context.Context, c acting.Caller, teamID, id string, v verb, idem *acting.Idem) (Result, error) {
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		next, changed, err := v.apply(cur)
		if err != nil {
			return err
		}
		if changed {
			if err := s.Repo.Update(ctx, tx, next, "", false, cur.Version); err != nil {
				return err
			}
			if err := s.Audit.Write(ctx, tx, c.Record(v.action, kindAgent, id, teamID,
				map[string]any{"state": cur.State, "generation": cur.Generation}, map[string]any{"state": next.State, "generation": next.Generation})); err != nil {
				return err
			}
			if err := s.emit(ctx, tx, c, v.event, next, map[string]any{"name": string(next.Name)}); err != nil {
				return err
			}
		}
		if res.Agent, err = s.Repo.Get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Agent)
	})
	return res, err
}

// Pause implements PauseAgent: the pod scales to zero, the volume stays.
func (s *Service) Pause(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"PauseAgent", domain.EventAgentPaused,
		func(a domain.Agent) (domain.Agent, bool, error) { return a.Pause(s.Now()) }}, idem)
}

// Resume implements ResumeAgent: the same volume comes back.
func (s *Service) Resume(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"ResumeAgent", domain.EventAgentResumed,
		func(a domain.Agent) (domain.Agent, bool, error) { return a.Resume(s.Now()) }}, idem)
}

// Restart implements RestartAgent: the generation rises and the reconciler starts a fresh pod.
func (s *Service) Restart(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"RestartAgent", domain.EventAgentRestarted,
		func(a domain.Agent) (domain.Agent, bool, error) {
			n, err := a.Restart(s.Now())
			return n, err == nil, err
		}}, idem)
}

// ListSnapshots implements ListAgentSnapshots, newest first.
func (s *Service) ListSnapshots(ctx context.Context, c acting.Caller, teamID, agentID string, p acting.Page) ([]domain.Snapshot, string, error) {
	p = p.Normalized()
	var rows []domain.Snapshot
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Get(ctx, tx, teamID, agentID, false); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.Snapshots(ctx, tx, teamID, agentID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.Snapshot) string { return x.ID })
	return page, next, nil
}

// CreateSnapshot implements CreateAgentSnapshot: it records a manual request the reconciler
// carries out. Only an enabled or paused agent can be snapshotted.
func (s *Service) CreateSnapshot(ctx context.Context, c acting.Caller, teamID, agentID, description string, idem *acting.Idem) (SnapshotResult, error) {
	if len([]rune(description)) > 200 {
		return SnapshotResult{}, domain.Validation(domain.FieldError{Field: "description", Reason: "must be at most 200 characters"})
	}
	var res SnapshotResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = SnapshotResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		a, err := s.Repo.Get(ctx, tx, teamID, agentID, true)
		if err != nil {
			return err
		}
		if a.State != domain.AgentEnabled && a.State != domain.AgentPaused {
			return domain.Conflict("invalid_state", "an agent in state "+string(a.State)+" cannot be snapshotted", nil)
		}
		id, err := s.IDs.NewID(domain.PrefixSnapshot)
		if err != nil {
			return err
		}
		snap := domain.Snapshot{ID: id.String(), AgentID: agentID, Reason: "manual", State: "requested", RequestedAt: s.Now(), RequestedBy: c.Session().Actor}
		if err := s.Repo.InsertSnapshot(ctx, tx, teamID, snap); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateAgentSnapshot", kindAgent, agentID, teamID, nil,
			map[string]any{"snapshotId": snap.ID, "reason": snap.Reason, "description": description})); err != nil {
			return err
		}
		res.Snapshot = snap
		return idem.Complete(ctx, s.Idem, tx, res.Snapshot)
	})
	return res, err
}
