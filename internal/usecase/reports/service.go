// Package reports implements GetUsage, GetTeamReport, GetAgentReport and GetServiceReport (API
// contract §4.9, RA-6): usage, costs, activity and traffic at team, agent and service level, grouped
// by dimension or tag. Reports are reads; the database's row-level security limits what the caller's
// session sees, and the use case checks the permission and the team before querying.
package reports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Result is what the repository computed for one page.
type Result struct {
	// Rows are the rows of the page (at most the limit asked for plus one).
	Rows []domain.ReportRow
	// Totals are the measures over every row of the range, in measure order.
	Totals []any
}

// Repository reads the report tables inside the transaction it is given.
type Repository interface {
	// TeamKind returns whether the team is ephemeral; NotFound when the caller sees no such team
	// (a deleted team is still seen by service admins).
	TeamIsEphemeral(ctx context.Context, tx port.Tx, teamID string) (bool, error)
	// AgentExists returns NotFound unless the team has (or had) the agent.
	AgentExists(ctx context.Context, tx port.Tx, teamID, agentID string) error
	// Run computes the report; the rows are those from offset, at most limit.
	Run(ctx context.Context, tx port.Tx, q domain.ReportQuery, offset, limit int) (Result, error)
}

// Service is the report use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Scope is the level of a report.
type Scope struct {
	// TeamID is empty for a service report.
	TeamID string
	// AgentID is set for an agent report.
	AgentID string
}

func (s *Service) check(ctx context.Context, c acting.Caller, perm authz.Permission, action string, sc Scope) error {
	res := authz.Resource{Scope: authz.ScopeService}
	if sc.TeamID != "" {
		res = authz.Resource{Scope: authz.ScopeTeam, TeamID: sc.TeamID}
	}
	switch d := authz.Authorize(c.Principal, perm, res); d.Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, sc.TeamID, perm)
}

func fingerprint(q domain.ReportQuery) string {
	sum := sha256.Sum256([]byte(q.Fingerprint()))
	return hex.EncodeToString(sum[:4])
}

// parseCursor reads "<offset>.<query fingerprint>"; a cursor made for another query is refused.
func parseCursor(cursor string, q domain.ReportQuery) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	off, fp, ok := strings.Cut(cursor, ".")
	n, err := strconv.Atoi(off)
	if !ok || err != nil || n < 0 {
		return 0, domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid or expired token"})
	}
	if fp != fingerprint(q) {
		return 0, domain.Validation(domain.FieldError{Field: "nextToken", Reason: "the token belongs to a different query"})
	}
	return n, nil
}

// run is the common path: check, scope the query, read.
func (s *Service) run(ctx context.Context, c acting.Caller, perm authz.Permission, action string, sc Scope, kind domain.ReportKind,
	raw domain.RawReportQuery, p acting.Page) (domain.Report, error) {
	if err := s.check(ctx, c, perm, action, sc); err != nil {
		return domain.Report{}, err
	}
	now := s.Clock.Now()
	q, err := domain.ParseReportQuery(kind, raw, now)
	if err != nil {
		return domain.Report{}, err
	}
	if sc.TeamID != "" {
		if q.Filters.TeamID != "" && q.Filters.TeamID != sc.TeamID {
			return domain.Report{}, domain.Validation(domain.FieldError{Field: "teamId", Reason: "a team report is for the team in the path"})
		}
		q.Filters.TeamID = sc.TeamID
	}
	if sc.AgentID != "" {
		if q.Filters.AgentID != "" && q.Filters.AgentID != sc.AgentID {
			return domain.Report{}, domain.Validation(domain.FieldError{Field: "agentId", Reason: "an agent report is for the agent in the path"})
		}
		q.Filters.AgentID = sc.AgentID
	}
	p = p.Normalized()
	offset, err := parseCursor(p.Cursor, q)
	if err != nil {
		return domain.Report{}, err
	}
	usesPool := q.Filters.PoolID != ""
	for _, g := range q.GroupBy {
		usesPool = usesPool || g.Dim == domain.DimPool
	}
	var res Result
	err = s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if sc.TeamID != "" {
			eph, err := s.Repo.TeamIsEphemeral(ctx, tx, sc.TeamID)
			if err != nil {
				return err
			}
			if usesPool && !eph {
				return domain.Validation(domain.FieldError{Field: "groupBy", Reason: "pools exist only in ephemeral teams"})
			}
		}
		if sc.AgentID != "" {
			if err := s.Repo.AgentExists(ctx, tx, sc.TeamID, sc.AgentID); err != nil {
				return err
			}
		}
		var err error
		res, err = s.Repo.Run(ctx, tx, q, offset, p.Limit+1)
		return err
	})
	if err != nil {
		return domain.Report{}, err
	}
	out := domain.Report{Kind: kind, Range: domain.ReportRange{From: q.From, To: q.To}, GroupBy: q.Columns(), Totals: res.Totals, Notes: domain.ReportNotes(q)}
	for _, m := range domain.ReportMeasures(kind) {
		out.MeasureCols = append(out.MeasureCols, m.Name)
	}
	rows := res.Rows
	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		out.NextCursor = fmt.Sprintf("%d.%s", offset+p.Limit, fingerprint(q))
	}
	out.Rows = rows
	return out, nil
}

// Usage implements GetUsage: the team's usage grouped by agent, model or day (default agent), from
// the first of the month to now unless the range says otherwise.
func (s *Service) Usage(ctx context.Context, c acting.Caller, teamID string, raw domain.RawReportQuery, p acting.Page) (domain.Report, error) {
	if raw.GroupBy != "" {
		switch raw.GroupBy {
		case "agent", "model", "day":
		default:
			return domain.Report{}, domain.Validation(domain.FieldError{Field: "groupBy", Reason: "must be agent, model or day"})
		}
	} else {
		raw.DefaultGroupBy = []domain.ReportGroup{{Dim: domain.DimAgent}}
	}
	if raw.From == "" && raw.To == "" && raw.Range == "" {
		raw.Range = "mtd"
	}
	return s.run(ctx, c, authz.UsageRead, "GetUsage", Scope{TeamID: teamID}, domain.ReportKindUsage, raw, p)
}

// TeamReport implements GetTeamReport.
func (s *Service) TeamReport(ctx context.Context, c acting.Caller, teamID string, kind domain.ReportKind, raw domain.RawReportQuery, p acting.Page) (domain.Report, error) {
	return s.run(ctx, c, authz.ReportsRead, "GetTeamReport", Scope{TeamID: teamID}, kind, raw, p)
}

// AgentReport implements GetAgentReport.
func (s *Service) AgentReport(ctx context.Context, c acting.Caller, teamID, agentID string, kind domain.ReportKind, raw domain.RawReportQuery, p acting.Page) (domain.Report, error) {
	return s.run(ctx, c, authz.ReportsRead, "GetAgentReport", Scope{TeamID: teamID, AgentID: agentID}, kind, raw, p)
}

// ServiceReport implements GetServiceReport (admin:reports, service admins only).
func (s *Service) ServiceReport(ctx context.Context, c acting.Caller, kind domain.ReportKind, raw domain.RawReportQuery, p acting.Page) (domain.Report, error) {
	return s.run(ctx, c, authz.AdminReports, "GetServiceReport", Scope{}, kind, raw, p)
}
