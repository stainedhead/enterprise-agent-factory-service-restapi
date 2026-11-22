// Package costs implements ListCosts (a team's gateway and CUR cost by day, usage:read) and
// ListCostReconciliations (gateway against billed per team and day, admin:reports) (API contract §4.9,
// M4-C5). Both are reads; the database's row-level security limits what the caller's session sees, and
// a deleted team's rows stay readable to service admins.
package costs

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Row is one cost_daily figure of a team: per day, source, subject and model, summed over the tag sets
// the day was split into.
type Row struct {
	Day time.Time
	// Source is gateway, cur_bedrock, cur_eks or cur_other.
	Source  string
	AgentID *string
	PoolID  *string
	ModelID *string
	CostUSD domain.Money
	// Calls is the number of model calls (gateway rows; 0 for CUR rows).
	Calls int64
}

// Reconciliation is a cost_reconciliations row.
type Reconciliation struct {
	TeamID     string
	Day        time.Time
	GatewayUSD domain.Money
	BilledUSD  domain.Money
	// VariancePct is the database's figure, nil when nothing was billed.
	VariancePct  *string
	Status       string
	ReconciledAt time.Time
}

// ReconCursor is the keyset position of the reconciliation list (day and team, both descending).
type ReconCursor struct {
	Day    time.Time
	TeamID string
}

// Repository reads the cost tables inside the transaction it is given.
type Repository interface {
	// TeamExists returns NotFound when the caller sees no such team (a deleted team is still seen by
	// service admins).
	TeamExists(ctx context.Context, tx port.Tx, teamID string) error
	// ListCosts returns the team's rows with a day from fromDay to toDay inclusive, in day, source,
	// subject, model order, from offset, at most limit.
	ListCosts(ctx context.Context, tx port.Tx, teamID string, fromDay, toDay time.Time, offset, limit int) ([]Row, error)
	// ListReconciliations returns reconciliations (of status when not empty), newest day first, after
	// the cursor, at most limit.
	ListReconciliations(ctx context.Context, tx port.Tx, status string, after *ReconCursor, limit int) ([]Reconciliation, error)
}

// Service is the cost use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

func invalidToken() error {
	return domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid or expired token"})
}

// ListCosts implements ListCosts. from and to are RFC 3339 timestamps; rows of every UTC day they touch
// are returned. The default range is the last 7 days, as for reports.
func (s *Service) ListCosts(ctx context.Context, c acting.Caller, teamID, from, to string, p acting.Page) ([]Row, string, error) {
	switch d := authz.Authorize(c.Principal, authz.UsageRead, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}); d.Outcome {
	case authz.Allow:
	case authz.NotFound:
		return nil, "", domain.NotFound("team")
	default:
		return nil, "", s.Denied(ctx, c, "ListCosts", teamID, authz.UsageRead)
	}
	f, t, err := domain.ParseReportRange(domain.RawReportQuery{From: from, To: to}, s.Clock.Now())
	if err != nil {
		return nil, "", err
	}
	fromDay, toDay := f.UTC().Truncate(24*time.Hour), t.UTC().Truncate(24*time.Hour)
	p = p.Normalized()
	fp := fromDay.Format(time.DateOnly) + ":" + toDay.Format(time.DateOnly)
	offset := 0
	if p.Cursor != "" {
		off, got, ok := strings.Cut(p.Cursor, ":")
		n, err := strconv.Atoi(off)
		if !ok || err != nil || n < 0 {
			return nil, "", invalidToken()
		}
		if got != fp {
			return nil, "", domain.Validation(domain.FieldError{Field: "nextToken", Reason: "the token belongs to a different range"})
		}
		offset = n
	}
	var rows []Row
	err = s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.Repo.TeamExists(ctx, tx, teamID); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.ListCosts(ctx, tx, teamID, fromDay, toDay, offset, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	if len(rows) <= p.Limit {
		return rows, "", nil
	}
	return rows[:p.Limit], fmt.Sprintf("%d:%s", offset+p.Limit, fp), nil
}

// ListReconciliations implements ListCostReconciliations (service admins). status filters by
// ok, over_threshold or missing_billing.
func (s *Service) ListReconciliations(ctx context.Context, c acting.Caller, status string, p acting.Page) ([]Reconciliation, string, error) {
	if !authz.Authorize(c.Principal, authz.AdminReports, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil, "", s.Denied(ctx, c, "ListCostReconciliations", "", authz.AdminReports)
	}
	switch status {
	case "", "ok", "over_threshold", "missing_billing":
	default:
		return nil, "", domain.Validation(domain.FieldError{Field: "status", Reason: "must be one of ok, over_threshold, missing_billing"})
	}
	p = p.Normalized()
	var after *ReconCursor
	if p.Cursor != "" {
		day, team, ok := strings.Cut(p.Cursor, "|")
		d, err := time.Parse(time.DateOnly, day)
		if !ok || err != nil || team == "" {
			return nil, "", invalidToken()
		}
		after = &ReconCursor{Day: d, TeamID: team}
	}
	var rows []Reconciliation
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListReconciliations(ctx, tx, status, after, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r Reconciliation) string { return r.Day.Format(time.DateOnly) + "|" + r.TeamID })
	return page, next, nil
}
