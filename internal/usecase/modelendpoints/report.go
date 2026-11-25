package modelendpoints

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Inference report dimensions.
var reportDims = []string{"team", "route", "model", "agent", "pool", "hour", "day"}

// RawReport is an inference report request as the query string gave it.
type RawReport struct {
	From, To, Range string
	GroupBy         string
	TeamID          string
	Route           string
	ModelID         string
	AgentID         string
	PoolID          string
}

// ReportQuery is a validated inference report request. Offset and Limit page the rows.
type ReportQuery struct {
	From, To time.Time
	GroupBy  []string
	TeamID   string
	Route    string
	ModelID  string
	AgentID  string
	PoolID   string
	Offset   int
	Limit    int
}

func (q ReportQuery) fingerprint() string {
	s := strings.Join([]string{q.From.Format(time.RFC3339), q.To.Format(time.RFC3339), strings.Join(q.GroupBy, ","), q.TeamID,
		q.Route, q.ModelID, q.AgentID, q.PoolID}, "|")
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// ReportRow is one row of the inference report: the groupBy columns, then the measures.
type ReportRow struct {
	TeamID, Route, Model, AgentID, PoolID, Day string
	Hour                                       *time.Time
	Calls, Errors, Throttled, Refused          int64
	LatencyP50, LatencyP95, LatencyP99         *int64
	TTFTP50, TTFTP95, TTFTP99, GatewayP99      *int64
	InputTokens, OutputTokens                  int64
	CachedTokens, ReasoningTokens              int64
	OutputTokensPerSecond                      *float64
	Estimated                                  int64
	CostUSD                                    domain.Money
	PricingMode                                string
}

// ErrorRatePct is errors over calls, in percent.
func (r ReportRow) ErrorRatePct() float64 { return pct(r.Errors, r.Calls) }

// EstimatedPct is the share of calls settled with usage_source estimated, in percent.
func (r ReportRow) EstimatedPct() float64 { return pct(r.Estimated, r.Calls) }

func pct(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return float64(n*10000/d) / 100
}

// Report is the inference report.
type Report struct {
	From, To time.Time
	GroupBy  []string
	Rows     []ReportRow
	Totals   ReportRow
	Notes    []string
	Next     string
}

func parseReport(raw RawReport, now time.Time) (ReportQuery, error) {
	from, to, err := domain.ParseReportRange(domain.RawReportQuery{From: raw.From, To: raw.To, Range: raw.Range}, now)
	if err != nil {
		return ReportQuery{}, err
	}
	q := ReportQuery{From: from, To: to, TeamID: raw.TeamID, Route: raw.Route, ModelID: raw.ModelID, AgentID: raw.AgentID, PoolID: raw.PoolID}
	if raw.GroupBy == "" {
		q.GroupBy = []string{"route", "model"}
	} else {
		for _, g := range strings.Split(raw.GroupBy, ",") {
			g = strings.TrimSpace(g)
			if !slices.Contains(reportDims, g) {
				return q, refused("groupBy", "each must be one of "+strings.Join(reportDims, ", "), "")
			}
			if slices.Contains(q.GroupBy, g) {
				return q, refused("groupBy", "is listed twice", "")
			}
			q.GroupBy = append(q.GroupBy, g)
		}
	}
	if len(q.GroupBy) > 4 {
		return q, refused("groupBy", "at most 4 dimensions", "")
	}
	if slices.Contains(q.GroupBy, "hour") && to.Sub(from) > 31*24*time.Hour {
		return q, refused("groupBy", "hour needs a range of at most 31 days", "")
	}
	if q.Route != "" && q.Route != domain.RouteBedrock && domain.ValidRouteSlug(q.Route) != nil {
		return q, refused("route", "must be a route name", "")
	}
	return q, nil
}

// InferenceReport implements GetInferenceReport (teamID "", admin:reports) and GetTeamInferenceReport
// (reports:read on the team): calls, errors, latency, time to first token, tokens and cost by route,
// model, agent or hour, Bedrock included as the route bedrock.
func (s *Service) InferenceReport(ctx context.Context, c acting.Caller, teamID string, raw RawReport, p acting.Page) (Report, error) {
	if teamID == "" {
		if err := s.serviceCheck(ctx, c, "GetInferenceReport", authz.AdminReports); err != nil {
			return Report{}, err
		}
	} else {
		if err := s.teamCheck(ctx, c, "GetTeamInferenceReport", teamID, authz.ReportsRead); err != nil {
			return Report{}, err
		}
		if raw.TeamID != "" && raw.TeamID != teamID {
			return Report{}, refused("teamId", "a team report is for the team in the path", "")
		}
		raw.TeamID = teamID
	}
	q, err := parseReport(raw, s.Now())
	if err != nil {
		return Report{}, err
	}
	p = p.Normalized()
	if p.Cursor != "" {
		off, fp, ok := strings.Cut(p.Cursor, ".")
		n, err := strconv.Atoi(off)
		if !ok || err != nil || n < 0 || fp != q.fingerprint() {
			return Report{}, refused("nextToken", "invalid or expired token", "")
		}
		q.Offset = n
	}
	q.Limit = p.Limit + 1
	var out Report
	err = s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.InferenceReport(ctx, tx, q)
		return err
	})
	if err != nil {
		return Report{}, err
	}
	out.From, out.To, out.GroupBy = q.From, q.To, q.GroupBy
	if len(out.Rows) > p.Limit {
		out.Rows = out.Rows[:p.Limit]
		out.Next = fmt.Sprintf("%d.%s", q.Offset+p.Limit, q.fingerprint())
	}
	out.Notes = append(out.Notes, "Costs of capacity routes are allocated by afs-billing and appear in the cost reports, not here.",
		"Percentiles are over calls in the range; estimatedPct is the share settled without usage in the response.")
	return out, nil
}
