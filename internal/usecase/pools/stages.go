package pools

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// StageResult is a stage with the replayed response of an idempotent create.
type StageResult struct {
	Stage  domain.Stage
	Replay *port.StoredResponse
}

func stageAudit(s domain.Stage) map[string]any {
	return map[string]any{"name": string(s.Name), "position": s.Position, "description": s.Description}
}

// ListStages implements ListStages, in position order of the id cursor.
func (s *Service) ListStages(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.Stage, string, error) {
	p = p.Normalized()
	var rows []domain.Stage
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListStages(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.Stage) string { return x.ID })
	return page, next, nil
}

// CreateStage implements CreateStage. Stages are a swarm feature: a persistent team is a 409. Contract
// §7 has no stage event, so the write is the row and an audit record (CD-7).
func (s *Service) CreateStage(ctx context.Context, c acting.Caller, teamID string, in domain.NewStageParams, idem *acting.Idem) (StageResult, error) {
	var res StageResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = StageResult{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixStage)
		if err != nil {
			return err
		}
		st, err := domain.NewStage(id.String(), teamID, in, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.InsertStage(ctx, tx, st); err != nil {
			return err
		}
		if res.Stage, err = s.Repo.GetStage(ctx, tx, teamID, st.ID, false); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateStage", kindStage, st.ID, teamID, nil, stageAudit(res.Stage))); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Stage)
	})
	return res, err
}

// UpdateStage implements UpdateStage (If-Match required).
func (s *Service) UpdateStage(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version, patch domain.StagePatch) (domain.Stage, error) {
	var out domain.Stage
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		cur, err := s.Repo.GetStage(ctx, tx, teamID, id, true)
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
		if err := s.Repo.UpdateStage(ctx, tx, next, expect); err != nil {
			return err
		}
		if out, err = s.Repo.GetStage(ctx, tx, teamID, id, false); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateStage", kindStage, id, teamID, stageAudit(cur), stageAudit(out)))
	})
	return out, err
}

// DeleteStage implements DeleteStage (If-Match required). A stage that pools still name is a Conflict.
func (s *Service) DeleteStage(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) error {
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireEphemeral(ctx, tx, teamID); err != nil {
			return err
		}
		cur, err := s.Repo.GetStage(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.DeleteStage(ctx, tx, teamID, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteStage", kindStage, id, teamID, stageAudit(cur), nil))
	})
}
