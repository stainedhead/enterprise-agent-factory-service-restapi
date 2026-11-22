// Package reportschedules implements the report schedule operations (API contract §4.9, RPT-8):
// ListReportSchedules, CreateReportSchedule, UpdateReportSchedule and DeleteReportSchedule. A schedule
// is desired state that afs-billing runs (internal/usecase/billing/schedules); the API stores it, points
// it at one of the team's own notification channels and never runs a report itself. Contract §7 has no
// schedule event, so a change writes the row and an audit record (CD-7).
package reportschedules

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "report_schedules"

// Repository is the report_schedules store. Every method runs in the transaction it is given, so
// row-level security applies with the caller's session.
type Repository interface {
	// TeamExists reports whether the caller sees the team.
	TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error)
	// List returns the team's schedules with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.ReportSchedule, error)
	// Get returns one schedule of the team (NotFound otherwise); forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.ReportSchedule, error)
	// ChannelOwnedByTeam reports whether channelID is a notification channel of that team. A
	// service-level channel, another team's channel and an unknown one all answer false.
	ChannelOwnedByTeam(ctx context.Context, tx port.Tx, teamID, channelID string) (bool, error)
	// Insert stores a schedule; a duplicate name is a Conflict.
	Insert(ctx context.Context, tx port.Tx, s domain.ReportSchedule) error
	// Update stores the changed schedule; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, s domain.ReportSchedule, expect domain.Version) (domain.ReportSchedule, error)
	// Delete removes the schedule; PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error
}

// Service is the report schedule use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is a schedule with the replayed response of an idempotent create.
type Result struct {
	Schedule domain.ReportSchedule
	Replay   *port.StoredResponse
}

// can authorizes the call; a denied write is audited.
func (s *Service) can(ctx context.Context, c acting.Caller, action, teamID string, write bool) error {
	perm := authz.ReportsRead
	if write {
		perm = authz.ReportsSchedule
	}
	switch d := authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}); d.Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	if !write {
		return domain.AccessDenied(string(perm))
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

func auditOf(r domain.ReportSchedule) map[string]any {
	return map[string]any{"name": r.Name, "report": r.Report, "query": r.Query, "cadence": r.Cadence, "channelId": r.ChannelID, "enabled": r.Enabled}
}

// checkChannel refuses a channel that is not one of the team's own (RA-US-7a.1, RA-US-15.6).
func (s *Service) checkChannel(ctx context.Context, tx port.Tx, teamID, channelID string) error {
	ok, err := s.Repo.ChannelOwnedByTeam(ctx, tx, teamID, channelID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.Validation(domain.FieldError{Field: "channelId", Reason: "must be one of the team's own notification channels (a service-level channel or another team's channel cannot be used)"})
	}
	return nil
}

// List implements ListReportSchedules.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.ReportSchedule, string, error) {
	if err := s.can(ctx, c, "ListReportSchedules", teamID, false); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.ReportSchedule
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		ok, err := s.Repo.TeamExists(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !ok {
			return domain.NotFound("team")
		}
		rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(d domain.ReportSchedule) string { return d.ID })
	return page, next, nil
}

// Create implements CreateReportSchedule.
func (s *Service) Create(ctx context.Context, c acting.Caller, teamID string, in domain.NewScheduleParams, idem *acting.Idem) (Result, error) {
	const action = "CreateReportSchedule"
	if err := s.can(ctx, c, action, teamID, true); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		ok, err := s.Repo.TeamExists(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !ok {
			return domain.NotFound("team")
		}
		id, err := s.IDs.NewID(domain.PrefixReportSchedule)
		if err != nil {
			return err
		}
		sch, err := in.New(id.String(), teamID, s.Now())
		if err != nil {
			return err
		}
		if err := s.checkChannel(ctx, tx, teamID, sch.ChannelID); err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, sch); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kind, sch.ID, teamID, nil, auditOf(sch))); err != nil {
			return err
		}
		if sch, err = s.Repo.Get(ctx, tx, teamID, sch.ID, false); err != nil {
			return err
		}
		res.Schedule = sch
		return idem.Complete(ctx, s.Idem, tx, sch)
	})
	return res, err
}

// Update implements UpdateReportSchedule.
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, p domain.SchedulePatch, expect domain.Version) (domain.ReportSchedule, error) {
	const action = "UpdateReportSchedule"
	if err := s.can(ctx, c, action, teamID, true); err != nil {
		return domain.ReportSchedule{}, err
	}
	var out domain.ReportSchedule
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
		if next.ChannelID != cur.ChannelID {
			if err := s.checkChannel(ctx, tx, teamID, next.ChannelID); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record(action, kind, id, teamID, auditOf(cur), auditOf(out)))
	})
	return out, err
}

// Delete implements DeleteReportSchedule.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) error {
	const action = "DeleteReportSchedule"
	if err := s.can(ctx, c, action, teamID, true); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, teamID, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record(action, kind, id, teamID, auditOf(cur), nil))
	})
}
