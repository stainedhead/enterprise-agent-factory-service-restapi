package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ParseReportKind validates the {kind} path value of a report operation.
func ParseReportKind(s string) (ReportKind, error) {
	switch k := ReportKind(s); k {
	case ReportKindUsage, ReportKindCosts, ReportKindActivity, ReportKindTraffic:
		return k, nil
	}
	return "", NotFound("report")
}

// Report limits.
const (
	// MaxReportRange is the longest range a report may cover (RA-US-7.2, F-8).
	MaxReportRange = 400 * 24 * time.Hour
	// DailyAbove is the range above which daily views are read; a shorter range reads the 5-minute
	// tables (RA-US-7.2).
	DailyAbove = 48 * time.Hour
	// DefaultReportRange is the range of a report that names none.
	DefaultReportRange = 7 * 24 * time.Hour
	// MaxReportGroups bounds the dimensions of one report.
	MaxReportGroups = 4
	// UntaggedValue is the value of a tag group for a record that did not carry the key (TAG-6).
	UntaggedValue = "(untagged)"
)

// Granularity says which tables a report reads.
type Granularity string

// Granularities.
const (
	// FiveMinute reads the 5-minute and per-call tables.
	FiveMinute Granularity = "5m"
	// Daily reads the daily views and rollups.
	Daily Granularity = "daily"
)

// ReportDim is one dimension a report can be grouped by.
type ReportDim string

// Report dimensions.
const (
	DimTeam   ReportDim = "team"
	DimAgent  ReportDim = "agent"
	DimModel  ReportDim = "model"
	DimSystem ReportDim = "system"
	DimDay    ReportDim = "day"
	DimHour   ReportDim = "hour"
	DimPool   ReportDim = "pool"
	DimTag    ReportDim = "tag"
)

// ReportGroup is one groupBy element: a dimension and, for a tag, its key.
type ReportGroup struct {
	Dim ReportDim
	Key string
}

// String is the groupBy spelling and the name of the row column ("agent", "tag:business-unit").
func (g ReportGroup) String() string {
	if g.Dim == DimTag {
		return "tag:" + g.Key
	}
	return string(g.Dim)
}

// dimsOf lists the dimensions that make sense for a kind: a usage report has models, a traffic report
// has external systems, an activity report has neither.
func dimsOf(k ReportKind) map[ReportDim]bool {
	d := map[ReportDim]bool{DimTeam: true, DimAgent: true, DimDay: true, DimHour: true, DimPool: true, DimTag: true}
	switch k {
	case ReportKindUsage, ReportKindCosts:
		d[DimModel] = true
	case ReportKindTraffic:
		d[DimSystem] = true
	case ReportKindActivity:
	}
	return d
}

// RawReportQuery is a report request as the query string gave it. Empty means absent.
type RawReportQuery struct {
	From, To, Range string
	// GroupBy is the comma-separated groupBy value.
	GroupBy string
	TeamID  string
	AgentID string
	ModelID string
	// SystemID, PoolID are the ext_ and pool_ identifiers.
	SystemID string
	PoolID   string
	// Tags are the tag.<key>=<value> filters.
	Tags map[string]string
	// DefaultGroupBy is used when GroupBy is empty.
	DefaultGroupBy []ReportGroup
}

// ReportFilters narrow a report.
type ReportFilters struct {
	TeamID, AgentID, ModelID, SystemID, PoolID string
	// Tags maps a tag key to the value a record must have; UntaggedValue selects records without it.
	Tags map[string]string
}

// ReportQuery is a validated report request.
type ReportQuery struct {
	Kind ReportKind
	// From is inclusive and To exclusive. A daily report covers every UTC day the range touches.
	From, To    time.Time
	Granularity Granularity
	GroupBy     []ReportGroup
	Filters     ReportFilters
}

// Columns are the names of the groupBy columns, in order.
func (q ReportQuery) Columns() []string {
	out := make([]string, len(q.GroupBy))
	for i, g := range q.GroupBy {
		out[i] = g.String()
	}
	return out
}

// FirstDay and LastDay are the UTC days a daily report covers (both inclusive).
func (q ReportQuery) FirstDay() time.Time { return q.From.UTC().Truncate(24 * time.Hour) }

// LastDay is the last UTC day the range touches; an end exactly at midnight does not touch its day.
func (q ReportQuery) LastDay() time.Time {
	end := q.To.UTC()
	if end.After(q.From) {
		end = end.Add(-time.Nanosecond)
	}
	return end.Truncate(24 * time.Hour)
}

// Fingerprint identifies the query without its page, so a page token cannot be replayed on another
// query.
func (q ReportQuery) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%d|%d|%s|", q.Kind, q.From.Unix(), q.To.Unix(), strings.Join(q.Columns(), ","))
	f := q.Filters
	fmt.Fprintf(&b, "%s|%s|%s|%s|%s|", f.TeamID, f.AgentID, f.ModelID, f.SystemID, f.PoolID)
	keys := make([]string, 0, len(f.Tags))
	for k := range f.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s;", k, f.Tags[k])
	}
	return b.String()
}

func reportBad(field, reason string) error {
	return Validation(FieldError{Field: field, Reason: reason})
}

// ParseReportGroupBy parses a comma-separated groupBy for kind.
func ParseReportGroupBy(kind ReportKind, s string) ([]ReportGroup, error) {
	allowed := dimsOf(kind)
	var out []ReportGroup
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		var g ReportGroup
		if key, ok := strings.CutPrefix(part, "tag:"); ok {
			if !ValidTagKey(key) || strings.Contains(key, " ") {
				return nil, reportBad("groupBy", fmt.Sprintf("%q is not a valid tag key", key))
			}
			g = ReportGroup{Dim: DimTag, Key: key}
		} else {
			d := ReportDim(part)
			if d == DimTag || !allowed[d] {
				if d != DimTag && isKnownDim(d) {
					return nil, reportBad("groupBy", fmt.Sprintf("%s reports cannot be grouped by %s", kind, d))
				}
				return nil, reportBad("groupBy", fmt.Sprintf("unknown dimension %q", part))
			}
			g = ReportGroup{Dim: d}
		}
		if seen[g.String()] {
			return nil, reportBad("groupBy", fmt.Sprintf("%s is listed twice", g))
		}
		seen[g.String()] = true
		out = append(out, g)
	}
	if len(out) > MaxReportGroups {
		return nil, reportBad("groupBy", fmt.Sprintf("at most %d dimensions", MaxReportGroups))
	}
	return out, nil
}

func isKnownDim(d ReportDim) bool {
	switch d {
	case DimTeam, DimAgent, DimModel, DimSystem, DimDay, DimHour, DimPool:
		return true
	}
	return false
}

// ParseReportRange resolves from, to and range against now.
func ParseReportRange(raw RawReportQuery, now time.Time) (from, to time.Time, err error) {
	now = now.UTC()
	if raw.Range != "" {
		if raw.From != "" || raw.To != "" {
			return from, to, reportBad("range", "use either range or from and to")
		}
		switch raw.Range {
		case "24h":
			return now.Add(-24 * time.Hour), now, nil
		case "7d":
			return now.Add(-7 * 24 * time.Hour), now, nil
		case "30d":
			return now.Add(-30 * 24 * time.Hour), now, nil
		case "90d":
			return now.Add(-90 * 24 * time.Hour), now, nil
		case "mtd":
			return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), now, nil
		}
		return from, to, reportBad("range", "must be one of 24h, 7d, 30d, 90d, mtd")
	}
	parse := func(field, s string) (time.Time, error) {
		t, perr := time.Parse(time.RFC3339, s)
		if perr != nil {
			return time.Time{}, reportBad(field, "must be an RFC 3339 timestamp")
		}
		return t.UTC(), nil
	}
	to = now
	if raw.To != "" {
		if to, err = parse("to", raw.To); err != nil {
			return from, to, err
		}
	}
	switch {
	case raw.From != "":
		if from, err = parse("from", raw.From); err != nil {
			return from, to, err
		}
	default:
		from = to.Add(-DefaultReportRange)
	}
	if from.After(to) {
		return from, to, reportBad("from", "must not be after to")
	}
	if to.Sub(from) > MaxReportRange {
		return from, to, reportBad("from", "the range must not exceed 400 days")
	}
	return from, to, nil
}

// ParseReportQuery validates a report request. Ranges over two days read the daily views and
// shorter ones the 5-minute tables; grouping by hour needs the 5-minute tables.
func ParseReportQuery(kind ReportKind, raw RawReportQuery, now time.Time) (ReportQuery, error) {
	q := ReportQuery{Kind: kind}
	var err error
	if q.From, q.To, err = ParseReportRange(raw, now); err != nil {
		return q, err
	}
	q.Granularity = FiveMinute
	if q.To.Sub(q.From) > DailyAbove {
		q.Granularity = Daily
	}
	if raw.GroupBy != "" {
		if q.GroupBy, err = ParseReportGroupBy(kind, raw.GroupBy); err != nil {
			return q, err
		}
	} else {
		q.GroupBy = raw.DefaultGroupBy
		if len(q.GroupBy) == 0 {
			q.GroupBy = []ReportGroup{{Dim: DimDay}}
		}
	}
	for _, g := range q.GroupBy {
		if g.Dim == DimHour && q.Granularity == Daily {
			return q, reportBad("groupBy", "hour needs a range of two days or less")
		}
	}
	q.Filters = ReportFilters{TeamID: raw.TeamID, AgentID: raw.AgentID, ModelID: raw.ModelID, SystemID: raw.SystemID, PoolID: raw.PoolID}
	if raw.TeamID != "" {
		if _, err := ParseID(PrefixTeam, raw.TeamID); err != nil {
			return q, reportBad("teamId", "must be a team_ identifier")
		}
	}
	if raw.AgentID != "" {
		if _, err := ParseID(PrefixAgent, raw.AgentID); err != nil {
			return q, reportBad("agentId", "must be an agt_ identifier")
		}
	}
	if raw.PoolID != "" {
		if _, err := ParseID(PrefixPool, raw.PoolID); err != nil {
			return q, reportBad("poolId", "must be a pool_ identifier")
		}
	}
	if raw.SystemID != "" {
		if _, err := ParseID(PrefixExtSystem, raw.SystemID); err != nil {
			return q, reportBad("systemId", "must be an ext_ identifier")
		}
	}
	if raw.ModelID != "" && kind != ReportKindUsage && kind != ReportKindCosts {
		return q, reportBad("modelId", fmt.Sprintf("%s reports have no model", kind))
	}
	if raw.SystemID != "" && kind != ReportKindTraffic {
		return q, reportBad("systemId", fmt.Sprintf("%s reports have no external system", kind))
	}
	if len(raw.Tags) > 0 {
		q.Filters.Tags = map[string]string{}
		for k, v := range raw.Tags {
			if !ValidTagKey(k) || strings.Contains(k, " ") {
				return q, reportBad("tag."+k, "not a valid tag key")
			}
			if v != UntaggedValue && !ValidTagValue(v) {
				return q, reportBad("tag."+k, "not a valid tag value")
			}
			q.Filters.Tags[k] = v
		}
	}
	return q, nil
}

// ReportRange is the range a report answers for.
type ReportRange struct {
	From, To time.Time
}

// ReportRow is one row: the groupBy values by column name, then the measures. A measure is nil when
// the source has no value (HTTP requests of a traffic row, OQ-8).
type ReportRow struct {
	Group    []string
	Measures []any
}

// Report is a computed report: groupBy columns, measure columns, one row per group and the totals
// over every row in the range (not only the returned page).
type Report struct {
	Kind        ReportKind
	Range       ReportRange
	GroupBy     []string
	MeasureCols []string
	Rows        []ReportRow
	Totals      []any
	Notes       []string
	// NextCursor is "" on the last page.
	NextCursor string
}

// BedrockOnlyNote is the contract's note on what spend a report leaves out.
const BedrockOnlyNote = "Spend with providers other than Bedrock is not included"

// MeasureType is the JSON type of a measure.
type MeasureType int

// Measure types.
const (
	// MeasureInt is an integer; it is nil when the source has no value.
	MeasureInt MeasureType = iota
	// MeasureMoney is a decimal USD string.
	MeasureMoney
	// MeasureFloat is a number such as a percentage; nil when undefined.
	MeasureFloat
)

// Measure names a measure column of a report.
type Measure struct {
	Name string
	Type MeasureType
}

// ReportMeasures lists the measure columns of a report kind, in order. Traffic carries counts and
// bytes, never a cost.
func ReportMeasures(k ReportKind) []Measure {
	switch k {
	case ReportKindUsage:
		return []Measure{{"calls", MeasureInt}, {"inputTokens", MeasureInt}, {"outputTokens", MeasureInt}, {"costUsd", MeasureMoney}}
	case ReportKindCosts:
		return []Measure{{"estimatedUsd", MeasureMoney}, {"billedUsd", MeasureMoney}}
	case ReportKindActivity:
		return []Measure{{"workingSeconds", MeasureInt}, {"idleSeconds", MeasureInt}, {"offSeconds", MeasureInt},
			{"impairedSeconds", MeasureInt}, {"modelCalls", MeasureInt}, {"utilizationPct", MeasureFloat}}
	case ReportKindTraffic:
		return []Measure{{"connections", MeasureInt}, {"deniedConnections", MeasureInt}, {"requests", MeasureInt},
			{"errors", MeasureInt}, {"bytesOut", MeasureInt}, {"bytesIn", MeasureInt}}
	}
	return nil
}

// ReportNotes returns the notes of a report: what it leaves out and where its numbers come from.
func ReportNotes(q ReportQuery) []string {
	var n []string
	switch q.Kind {
	case ReportKindUsage:
		n = append(n, BedrockOnlyNote)
	case ReportKindCosts:
		n = append(n, BedrockOnlyNote, "estimatedUsd is the gateway's metering; billedUsd comes from the AWS Cost and Usage Report and is present for daily ranges once reconciled")
	case ReportKindTraffic:
		n = append(n, "Traffic reports carry counts and bytes, never a cost", "requests and errors are empty where no egress proxy sees HTTP")
	case ReportKindActivity:
		n = append(n, "utilizationPct is the working share of working and idle time")
	}
	if q.Granularity == Daily {
		n = append(n, "A range over two days covers every UTC day it touches")
	}
	return n
}
