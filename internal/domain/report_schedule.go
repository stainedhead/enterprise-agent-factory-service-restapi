package domain

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// ScheduleRunHour is the UTC hour a schedule runs at.
const ScheduleRunHour = 6

// MaxScheduleNameLength is the longest schedule name (the API's limit; the column allows 120).
const MaxScheduleNameLength = 100

// Schedule outcomes as the database stores them (report_schedules.last_status).
const (
	ScheduleSent   = "sent"
	ScheduleFailed = "failed"
)

// NextRun returns the first run of a schedule with cadence c strictly after t: every day at 06:00 UTC,
// every Monday at 06:00 UTC, or the first of the month at 06:00 UTC.
func NextRun(c Cadence, t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), ScheduleRunHour, 0, 0, 0, time.UTC)
	switch c {
	case CadenceWeekly:
		for day.Weekday() != time.Monday || !day.After(t) {
			day = day.AddDate(0, 0, 1)
		}
		return day
	case CadenceMonthly:
		first := time.Date(t.Year(), t.Month(), 1, ScheduleRunHour, 0, 0, 0, time.UTC)
		if !first.After(t) {
			first = first.AddDate(0, 1, 0)
		}
		return first
	}
	if !day.After(t) {
		day = day.AddDate(0, 0, 1)
	}
	return day
}

// DefaultScheduleRange is the range a schedule's query gets when it names none: the period the
// cadence repeats.
func DefaultScheduleRange(c Cadence) string {
	switch c {
	case CadenceWeekly:
		return "7d"
	case CadenceMonthly:
		return "30d"
	}
	return "24h"
}

// ScheduleQuery is the stored report query of a schedule, in the API's shape (ReportQuery): a range or
// from and to, groupBy as a list and filters keyed teamId, agentId, modelId, systemId, poolId and
// tag.<key>.
type ScheduleQuery struct {
	From    string            `json:"from,omitempty"`
	To      string            `json:"to,omitempty"`
	Range   string            `json:"range,omitempty"`
	GroupBy []string          `json:"groupBy,omitempty"`
	Filters map[string]string `json:"filters,omitempty"`
}

// Raw converts the stored query into a report request for teamID: a schedule reports on its own team
// only, so the teamId filter is the team (a filter naming another team is refused by Validate).
func (q ScheduleQuery) Raw(teamID string) RawReportQuery {
	raw := RawReportQuery{From: q.From, To: q.To, Range: q.Range, GroupBy: strings.Join(q.GroupBy, ","), TeamID: teamID}
	for k, v := range q.Filters {
		switch {
		case k == "agentId":
			raw.AgentID = v
		case k == "modelId":
			raw.ModelID = v
		case k == "systemId":
			raw.SystemID = v
		case k == "poolId":
			raw.PoolID = v
		case strings.HasPrefix(k, "tag."):
			if raw.Tags == nil {
				raw.Tags = map[string]string{}
			}
			raw.Tags[strings.TrimPrefix(k, "tag.")] = v
		}
	}
	return raw
}

// WithDefaults returns the query with the cadence's range when it says no period.
func (q ScheduleQuery) WithDefaults(c Cadence) ScheduleQuery {
	if q.Range == "" && q.From == "" && q.To == "" {
		q.Range = DefaultScheduleRange(c)
	}
	return q
}

// Validate checks the query for a report of kind kind in team teamID against the report rules, and
// refuses filters that name another team or are not report filters.
func (q ScheduleQuery) Validate(kind ReportKind, teamID string, now time.Time) error {
	keys := make([]string, 0, len(q.Filters))
	for k := range q.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case k == "teamId":
			if q.Filters[k] != teamID {
				return reportBad("query.filters.teamId", "a schedule reports on its own team")
			}
		case k == "agentId", k == "modelId", k == "systemId", k == "poolId", strings.HasPrefix(k, "tag."):
		default:
			return reportBad("query.filters."+k, "not a report filter")
		}
	}
	raw := q.Raw(teamID)
	if _, err := ParseReportQuery(kind, raw, now); err != nil {
		var e *Error
		if errors.As(err, &e) && e.Kind == KindValidation {
			return prefixFields(e, "query.")
		}
		return err
	}
	return nil
}

func prefixFields(e *Error, prefix string) error {
	fields := make([]FieldError, 0, len(e.Fields))
	for _, f := range e.Fields {
		f.Field = prefix + f.Field
		fields = append(fields, f)
	}
	return Validation(fields...)
}

// ReportSchedule is a stored recurring report of a team.
type ReportSchedule struct {
	ID        string
	TeamID    string
	Name      string
	Report    ReportKind
	Query     ScheduleQuery
	Cadence   Cadence
	ChannelID string
	Format    string
	Enabled   bool
	NextRunAt time.Time
	LastRunAt *time.Time
	// LastStatus is ScheduleSent or ScheduleFailed, "" before the first run.
	LastStatus string
	LastError  string
	Version    Version
	CreatedAt  time.Time
	CreatedBy  string
	UpdatedAt  time.Time
}

// NewScheduleParams are the fields of CreateReportSchedule.
type NewScheduleParams struct {
	Name      string
	Report    ReportKind
	Query     ScheduleQuery
	Cadence   Cadence
	ChannelID string
	// Format must be "csv" or empty (the default).
	Format string
	// Enabled defaults to true.
	Enabled *bool
}

func (p NewScheduleParams) checkShape() error {
	var fs []FieldError
	if n := strings.TrimSpace(p.Name); n == "" || len(p.Name) > MaxScheduleNameLength {
		fs = append(fs, FieldError{Field: "name", Reason: "must be 1 to 100 characters"})
	}
	if !p.Report.Valid() {
		fs = append(fs, FieldError{Field: "report", Reason: "must be usage, costs, activity or traffic"})
	}
	if !p.Cadence.Valid() {
		fs = append(fs, FieldError{Field: "cadence", Reason: "must be daily, weekly or monthly"})
	}
	if _, err := ParseID(PrefixChannel, p.ChannelID); err != nil {
		fs = append(fs, FieldError{Field: "channelId", Reason: "must be an nch_ identifier"})
	}
	if p.Format != "" && p.Format != "csv" {
		fs = append(fs, FieldError{Field: "format", Reason: "must be csv"})
	}
	if len(fs) > 0 {
		return Validation(fs...)
	}
	return nil
}

// New validates the parameters and builds the schedule, due at the first run after now.
func (p NewScheduleParams) New(id, teamID string, now time.Time) (ReportSchedule, error) {
	if err := p.checkShape(); err != nil {
		return ReportSchedule{}, err
	}
	q := p.Query.WithDefaults(p.Cadence)
	if err := q.Validate(p.Report, teamID, now); err != nil {
		return ReportSchedule{}, err
	}
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	return ReportSchedule{ID: id, TeamID: teamID, Name: p.Name, Report: p.Report, Query: q, Cadence: p.Cadence, ChannelID: p.ChannelID,
		Format: "csv", Enabled: enabled, NextRunAt: NextRun(p.Cadence, now), Version: 1, CreatedAt: now, UpdatedAt: now}, nil
}

// SchedulePatch is the fields of UpdateReportSchedule; nil leaves a field as it is. The report kind
// and format cannot change.
type SchedulePatch struct {
	Name      *string
	Query     *ScheduleQuery
	Cadence   *Cadence
	ChannelID *string
	Enabled   *bool
}

// Apply returns the schedule with the patch applied and re-validated. A changed cadence or a schedule
// switched back on is due at the first run after now.
func (s ReportSchedule) Apply(p SchedulePatch, now time.Time) (ReportSchedule, error) {
	out := s
	if p.Name != nil {
		out.Name = *p.Name
	}
	if p.Query != nil {
		out.Query = *p.Query
	}
	if p.Cadence != nil {
		out.Cadence = *p.Cadence
	}
	if p.ChannelID != nil {
		out.ChannelID = *p.ChannelID
	}
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	np := NewScheduleParams{Name: out.Name, Report: out.Report, Query: out.Query, Cadence: out.Cadence, ChannelID: out.ChannelID}
	if err := np.checkShape(); err != nil {
		return s, err
	}
	if p.Query != nil || p.Cadence != nil {
		out.Query = out.Query.WithDefaults(out.Cadence)
		if err := out.Query.Validate(out.Report, out.TeamID, now); err != nil {
			return s, err
		}
	}
	if (p.Cadence != nil && *p.Cadence != s.Cadence) || (p.Enabled != nil && *p.Enabled && !s.Enabled) {
		out.NextRunAt = NextRun(out.Cadence, now)
	}
	out.UpdatedAt = now
	return out, nil
}

// Due reports whether the schedule should run at now.
func (s ReportSchedule) Due(now time.Time) bool { return s.Enabled && !s.NextRunAt.After(now) }
