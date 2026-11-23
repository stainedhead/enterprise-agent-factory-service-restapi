package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// BackfillBatch is how many teams one orchestration-backfill batch creates agents for (RA-US-21).
const BackfillBatch = 20

// BackfillActor is the actor of the backfill's database session and audit records.
const BackfillActor = "system:migration"

// BackfillResult is what a backfill run did.
type BackfillResult struct {
	Created   int
	Failed    []string // team ids whose agent could not be created; a rerun retries them
	Remaining int      // teams still without an orchestration agent after the run
}

// BackfillCaller is the caller of the backfill: a service-level system principal.
func BackfillCaller() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: BackfillActor, Type: authz.PrincipalUser, ServiceAdmin: true}, RequestID: "orchestration-backfill"}
}

// Backfill creates the orchestration agent of every live team that has none, in batches of BackfillBatch,
// one transaction per team, as system:migration (RA-US-21). It is idempotent and resumable: a team that
// already has an agent is not listed, so a rerun picks up where an interrupted run stopped. A failure of one
// team is recorded and the run continues; the caller decides from Failed whether to retry. The team-admins
// are told by the orchestration.created event the creation emits.
func (s *Service) Backfill(ctx context.Context, log *slog.Logger) (BackfillResult, error) {
	if log == nil {
		log = slog.Default()
	}
	c := BackfillCaller()
	var res BackfillResult
	failed := map[string]bool{}
	for {
		var ids []string
		err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
			var err error
			ids, err = s.Repo.WithoutOrchestration(ctx, tx, len(failed)+BackfillBatch)
			return err
		})
		if err != nil {
			return res, fmt.Errorf("teams: list teams without an orchestration agent: %w", err)
		}
		todo := ids[:0:0]
		for _, id := range ids {
			if !failed[id] {
				todo = append(todo, id)
			}
		}
		if len(todo) == 0 {
			break
		}
		if len(todo) > BackfillBatch {
			todo = todo[:BackfillBatch]
		}
		for _, id := range todo {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := s.backfillTeam(ctx, c, id); err != nil {
				log.Error("orchestration backfill: team skipped", "team", id, "err", err)
				failed[id] = true
				res.Failed = append(res.Failed, id)
				continue
			}
			res.Created++
		}
		log.Info("orchestration backfill: batch done", "created", res.Created, "failed", len(res.Failed))
	}
	res.Remaining = len(failed)
	if len(res.Failed) > 0 {
		return res, errors.New("teams: orchestration backfill left teams without an agent")
	}
	return res, nil
}

func (s *Service) backfillTeam(ctx context.Context, c acting.Caller, teamID string) error {
	var created []string
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		t, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if t.State == domain.TeamStateDeleting || t.State == domain.TeamStateDeleted {
			return nil
		}
		has, err := s.Repo.HasOrchestration(ctx, tx, teamID)
		if err != nil || has {
			return err
		}
		tool, err := s.resolveTool(ctx, "")
		if err != nil {
			return err
		}
		return s.createOrchestration(ctx, tx, c, t, tool, nil, nil, &created)
	})
	if err != nil && s.Boot != nil {
		s.Boot.Compensate(ctx, created)
	}
	return err
}
