package pools

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Service is the pool use case set.
type Service struct {
	acting.Deps
	Repo Repository
	// Entropy feeds the external ids of role links.
	Entropy io.Reader
}

// Result is a pool with the replayed response of an idempotent call.
type Result struct {
	Pool   domain.Pool
	Replay *port.StoredResponse
}

func auditOf(p domain.Pool) map[string]any {
	return map[string]any{"name": string(p.Name), "description": p.Description, "stageId": p.StageID, "template": p.Template,
		"image": string(p.Image), "modelId": p.ModelID, "models": p.Models.Audit(), "workers": p.Workers, "resources": p.Resources,
		"scratchSizeGiB": p.ScratchSizeGiB, "sharedVolume": p.SharedVolume, "state": p.State, "generation": p.Generation}
}

// emit publishes a pool event; unlike Deps.Emit it sets poolId in the detail.
func (s *Service) emit(ctx context.Context, tx port.Tx, c acting.Caller, typ domain.EventType, p domain.Pool, data map[string]any) error {
	eid, err := s.IDs.NewID("evt")
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, eid.String(), s.Clock.Now(), c.Session().Actor, p.TeamID, kindPool, p.ID, data)
	if err != nil {
		return err
	}
	ev.Detail.PoolID = p.ID
	return s.Outbox.Publish(ctx, tx, ev)
}

// requireEphemeral refuses a persistent team with a 409 before anything is written (RA-US-10.3). A team
// the caller cannot see is NotFound.
func (s *Service) requireEphemeral(ctx context.Context, tx port.Tx, teamID string) error {
	kind, err := s.Repo.TeamKind(ctx, tx, teamID)
	if err != nil {
		return err
	}
	if kind != domain.TeamKindEphemeral {
		return domain.Conflict("team_not_ephemeral", "pools run only in ephemeral teams (swarms); this team is "+string(kind),
			map[string]any{"kind": string(kind)})
	}
	return nil
}

// withHandoffs fills the handoffs of the pools.
func (s *Service) withHandoffs(ctx context.Context, tx port.Tx, list []domain.Pool) error {
	for i := range list {
		h, err := s.Repo.Handoffs(ctx, tx, list[i].TeamID, list[i].ID)
		if err != nil {
			return err
		}
		list[i].Handoffs = h
	}
	return nil
}

func (s *Service) get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Pool, error) {
	p, err := s.Repo.Get(ctx, tx, teamID, id, forUpdate)
	if err != nil {
		return domain.Pool{}, err
	}
	list := []domain.Pool{p}
	if err := s.withHandoffs(ctx, tx, list); err != nil {
		return domain.Pool{}, err
	}
	if err := s.Repo.EffectiveModels(ctx, tx, teamID, list); err != nil {
		return domain.Pool{}, err
	}
	return list[0].WithDeletingStatus(), nil
}

// List implements ListPools.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.Pool, string, error) {
	p = p.Normalized()
	var rows []domain.Pool
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		if rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1); err != nil {
			return err
		}
		if err := s.withHandoffs(ctx, tx, rows); err != nil {
			return err
		}
		return s.Repo.EffectiveModels(ctx, tx, teamID, rows)
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.Pool) string { return x.ID })
	for i := range page {
		page[i] = page[i].WithDeletingStatus()
	}
	return page, next, nil
}

// Get implements GetPool.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID, id string) (domain.Pool, error) {
	var out domain.Pool
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.get(ctx, tx, teamID, id, false)
		return err
	})
	return out, err
}

// Status is the answer of GetPoolStatus.
type Status struct {
	Pool    domain.Pool
	Workers domain.PoolWorkers
}

// GetStatus implements GetPoolStatus: the reconcile status and the workers by state. A paused pool
// desires no workers.
func (s *Service) GetStatus(ctx context.Context, c acting.Caller, teamID, id string) (Status, error) {
	var out Status
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		if out.Pool, err = s.get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		if out.Workers, err = s.Repo.Workers(ctx, tx, teamID, id); err != nil {
			return err
		}
		out.Workers.Desired = out.Pool.Workers
		if out.Pool.State != domain.PoolStateEnabled {
			out.Workers.Desired = 0
		}
		return nil
	})
	return out, err
}

func (s *Service) checkTemplate(ctx context.Context, tx port.Tx, name string, image domain.ImageRef) (string, error) {
	t, ok, err := s.Repo.Template(ctx, tx, name)
	if err != nil {
		return "", err
	}
	switch {
	case !ok:
		return "", domain.Validation(domain.FieldError{Field: "template", Reason: "no agent template named " + name})
	case !t.Enabled:
		return "", domain.Validation(domain.FieldError{Field: "template", Reason: "agent template " + name + " is disabled"})
	}
	digest := string(image)[strings.LastIndex(string(image), "@")+1:]
	if !slices.Contains(t.AllowedDigests, digest) {
		return "", domain.Validation(domain.FieldError{Field: "image", Reason: "digest " + digest + " is not allowed by agent template " + name})
	}
	return t.ID, nil
}

func (s *Service) checkStage(ctx context.Context, tx port.Tx, teamID, stageID string) error {
	if stageID == "" {
		return nil
	}
	if _, err := s.Repo.GetStage(ctx, tx, teamID, stageID, false); err != nil {
		if domain.KindOf(err) == domain.KindNotFound {
			return domain.Validation(domain.FieldError{Field: "stageId", Reason: "no such stage in the team"})
		}
		return err
	}
	return nil
}

// Create implements CreatePool: the team must be ephemeral (409), the name free of agents (409), the
// model enabled for the team, the stage known and the template known, enabled and listing the image
// digest (RA-US-4.3, RA-US-10.3). One transaction writes the row, the audit record and pool.created.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewPoolParams, idem *acting.Idem) (Result, error) {
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixPool)
		if err != nil {
			return err
		}
		p, err := domain.NewPool(id.String(), teamID, in, s.Now())
		if err != nil {
			return err
		}
		if taken, err := s.Repo.AgentNameTaken(ctx, tx, teamID, string(p.Name)); err != nil {
			return err
		} else if taken {
			return domain.Conflict("duplicate", "an agent named "+string(p.Name)+" already exists in this team; agent and pool names share one namespace", nil)
		}
		sets, err := s.Repo.ModelSets(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if p.Models, err = domain.ResolveNewModels(in.Models, in.ModelID, sets); err != nil {
			return err
		}
		p.ModelID = p.Models.DefaultModelID
		if err := s.checkStage(ctx, tx, teamID, p.StageID); err != nil {
			return err
		}
		tplID, err := s.checkTemplate(ctx, tx, p.Template, p.Image)
		if err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, p, tplID); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreatePool", kindPool, p.ID, teamID, nil, auditOf(p))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventPoolCreated, p, map[string]any{"name": string(p.Name), "template": p.Template, "workers": p.Workers}); err != nil {
			return err
		}
		if res.Pool, err = s.get(ctx, tx, teamID, p.ID, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Pool)
	})
	return res, err
}

func changedFields(p domain.PoolPatch) []string {
	var f []string
	add := func(set bool, n string) {
		if set {
			f = append(f, n)
		}
	}
	add(p.Description != nil, "description")
	add(p.StageID.Set, "stageId")
	add(p.Template != nil, "template")
	add(p.Image != nil, "image")
	add(p.Persona != nil, "persona")
	add(p.ModelID != nil, "modelId")
	add(p.Models.Present(), "models")
	add(p.Workers != nil, "workers")
	add(p.Resources != nil, "resources")
	add(p.ScratchSizeGiB != nil, "scratchSizeGiB")
	add(p.SharedVolume != nil, "sharedVolume")
	return f
}

// Update implements UpdatePool (If-Match required). Template, image, model and stage are checked only
// when the request changes them. A change of the worker count also raises pool.scaled.
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version, patch domain.PoolPatch) (domain.Pool, error) {
	var out domain.Pool
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		cur, err := s.get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(patch, s.Now())
		if err != nil {
			return err
		}
		tplChanged := next.Template != cur.Template || next.Image != cur.Image
		tplID := ""
		if tplChanged {
			if tplID, err = s.checkTemplate(ctx, tx, next.Template, next.Image); err != nil {
				return err
			}
		}
		next.Models, next.ModelID = cur.Models, cur.ModelID
		if patch.Models.Present() || patch.ModelID != nil {
			sets, err := s.Repo.ModelSets(ctx, tx, teamID)
			if err != nil {
				return err
			}
			legacy := ""
			if patch.ModelID != nil {
				legacy = *patch.ModelID
			}
			if next.Models, err = cur.Models.ApplyModels(patch.Models, legacy, sets); err != nil {
				return err
			}
			next.ModelID = next.Models.DefaultModelID
		}
		if next.StageID != cur.StageID {
			if err := s.checkStage(ctx, tx, teamID, next.StageID); err != nil {
				return err
			}
		}
		if err := s.Repo.Update(ctx, tx, next, tplID, tplChanged, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdatePool", kindPool, id, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventPoolUpdated, next, map[string]any{"fields": changedFields(patch)}); err != nil {
			return err
		}
		if next.Workers != cur.Workers {
			if err := s.emit(ctx, tx, c, domain.EventPoolScaled, next, map[string]any{"from": cur.Workers, "workers": next.Workers}); err != nil {
				return err
			}
		}
		out, err = s.get(ctx, tx, teamID, id, false)
		return err
	})
	return out, err
}

// Delete implements DeletePool: the pool is marked deleting (status Deleting) and the reconciler runs
// the delete steps and finalizes it. Deleting a deleting pool changes nothing.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) (domain.Pool, error) {
	var out domain.Pool
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		cur, err := s.get(ctx, tx, teamID, id, true)
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
		if err := s.Audit.Write(ctx, tx, c.Record("DeletePool", kindPool, id, teamID, auditOf(cur), auditOf(next))); err != nil {
			return err
		}
		if err := s.emit(ctx, tx, c, domain.EventPoolDeleted, next, map[string]any{"name": string(next.Name)}); err != nil {
			return err
		}
		if out, err = s.get(ctx, tx, teamID, id, false); err != nil {
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
	apply  func(domain.Pool) (domain.Pool, bool, error)
	data   func(cur, next domain.Pool) map[string]any
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
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		cur, err := s.get(ctx, tx, teamID, id, true)
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
			if err := s.Audit.Write(ctx, tx, c.Record(v.action, kindPool, id, teamID,
				map[string]any{"state": cur.State, "workers": cur.Workers, "generation": cur.Generation},
				map[string]any{"state": next.State, "workers": next.Workers, "generation": next.Generation})); err != nil {
				return err
			}
			if v.event != "" {
				if err := s.emit(ctx, tx, c, v.event, next, v.data(cur, next)); err != nil {
					return err
				}
			}
		}
		if res.Pool, err = s.get(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Pool)
	})
	return res, err
}

func stateData(_, next domain.Pool) map[string]any {
	return map[string]any{"fields": []string{"state"}, "state": string(next.State)}
}

// Scale implements ScalePool: workers 0 to 200 (400 otherwise); pool.scaled when the count changes.
func (s *Service) Scale(ctx context.Context, c acting.Caller, teamID, id string, workers int, idem *acting.Idem) (Result, error) {
	if workers < 0 || workers > domain.MaxPoolWorkers {
		return Result{}, domain.Validation(domain.FieldError{Field: "workers", Reason: "must be 0 to 200"})
	}
	return s.transition(ctx, c, teamID, id, verb{"ScalePool", domain.EventPoolScaled,
		func(p domain.Pool) (domain.Pool, bool, error) { return p.Scale(workers, s.Now()) },
		func(cur, next domain.Pool) map[string]any {
			return map[string]any{"from": cur.Workers, "workers": next.Workers}
		}}, idem)
}

// Pause implements PausePool: the Deployment scales to zero, the identity and volumes stay. It raises
// pool.updated (the contract has no pause event).
func (s *Service) Pause(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"PausePool", domain.EventPoolUpdated,
		func(p domain.Pool) (domain.Pool, bool, error) { return p.Pause(s.Now()) }, stateData}, idem)
}

// Resume implements ResumePool: the pool returns to its desired worker count.
func (s *Service) Resume(ctx context.Context, c acting.Caller, teamID, id string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, id, verb{"ResumePool", domain.EventPoolUpdated,
		func(p domain.Pool) (domain.Pool, bool, error) { return p.Resume(s.Now()) }, stateData}, idem)
}
