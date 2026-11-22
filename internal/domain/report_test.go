package domain

import (
	"errors"
	"testing"
	"time"
)

var reportNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func TestParseReportQuery(t *testing.T) {
	tests := []struct {
		name    string
		kind    ReportKind
		raw     RawReportQuery
		bad     string
		gran    Granularity
		columns string
	}{
		{name: "default is 7 days daily by day", kind: ReportKindUsage, gran: Daily, columns: "day"},
		{name: "24h reads 5-minute tables", kind: ReportKindUsage, raw: RawReportQuery{Range: "24h"}, gran: FiveMinute, columns: "day"},
		{name: "exactly two days is 5-minute", kind: ReportKindUsage, raw: RawReportQuery{From: "2026-11-20T12:00:00Z", To: "2026-11-22T12:00:00Z"}, gran: FiveMinute, columns: "day"},
		{name: "just over two days is daily", kind: ReportKindUsage, raw: RawReportQuery{From: "2026-11-20T11:59:00Z", To: "2026-11-22T12:00:00Z"}, gran: Daily, columns: "day"},
		{name: "mtd", kind: ReportKindCosts, raw: RawReportQuery{Range: "mtd"}, gran: Daily, columns: "day"},
		{name: "from after to", kind: ReportKindUsage, raw: RawReportQuery{From: "2026-11-22T00:00:00Z", To: "2026-11-21T00:00:00Z"}, bad: "from"},
		{name: "over 400 days", kind: ReportKindUsage, raw: RawReportQuery{From: "2025-01-01T00:00:00Z", To: "2026-11-01T00:00:00Z"}, bad: "from"},
		{name: "exactly 400 days", kind: ReportKindUsage, raw: RawReportQuery{From: "2025-10-18T12:00:00Z", To: "2026-11-22T12:00:00Z"}, gran: Daily, columns: "day"},
		{name: "range and from", kind: ReportKindUsage, raw: RawReportQuery{Range: "7d", From: "2026-11-01T00:00:00Z"}, bad: "range"},
		{name: "bad range", kind: ReportKindUsage, raw: RawReportQuery{Range: "1y"}, bad: "range"},
		{name: "bad from", kind: ReportKindUsage, raw: RawReportQuery{From: "yesterday"}, bad: "from"},
		{name: "bad to", kind: ReportKindUsage, raw: RawReportQuery{To: "x"}, bad: "to"},
		{name: "unknown groupBy", kind: ReportKindUsage, raw: RawReportQuery{GroupBy: "planet"}, bad: "groupBy"},
		{name: "model on traffic", kind: ReportKindTraffic, raw: RawReportQuery{GroupBy: "model"}, bad: "groupBy"},
		{name: "system on usage", kind: ReportKindUsage, raw: RawReportQuery{GroupBy: "system"}, bad: "groupBy"},
		{name: "bare tag", kind: ReportKindUsage, raw: RawReportQuery{GroupBy: "tag"}, bad: "groupBy"},
		{name: "bad tag key", kind: ReportKindUsage, raw: RawReportQuery{GroupBy: "tag:aws:x"}, bad: "groupBy"},
		{name: "duplicate", kind: ReportKindUsage, raw: RawReportQuery{GroupBy: "agent,agent"}, bad: "groupBy"},
		{name: "too many", kind: ReportKindUsage, raw: RawReportQuery{GroupBy: "team,agent,model,day,pool"}, bad: "groupBy"},
		{name: "hour on a long range", kind: ReportKindUsage, raw: RawReportQuery{Range: "30d", GroupBy: "hour"}, bad: "groupBy"},
		{name: "hour on a short range", kind: ReportKindUsage, raw: RawReportQuery{Range: "24h", GroupBy: "hour"}, gran: FiveMinute, columns: "hour"},
		{name: "tag and agent", kind: ReportKindActivity, raw: RawReportQuery{GroupBy: "agent,tag:business-unit"}, gran: Daily, columns: "agent,tag:business-unit"},
		{name: "bad team filter", kind: ReportKindUsage, raw: RawReportQuery{TeamID: "x"}, bad: "teamId"},
		{name: "bad agent filter", kind: ReportKindUsage, raw: RawReportQuery{AgentID: "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"}, bad: "agentId"},
		{name: "bad pool filter", kind: ReportKindUsage, raw: RawReportQuery{PoolID: "x"}, bad: "poolId"},
		{name: "bad system filter", kind: ReportKindTraffic, raw: RawReportQuery{SystemID: "x"}, bad: "systemId"},
		{name: "system filter on usage", kind: ReportKindUsage, raw: RawReportQuery{SystemID: "ext_01JD2Q3R4S5T6V7W8X9Y0Z0001"}, bad: "systemId"},
		{name: "model filter on traffic", kind: ReportKindTraffic, raw: RawReportQuery{ModelID: "m"}, bad: "modelId"},
		{name: "bad tag filter key", kind: ReportKindUsage, raw: RawReportQuery{Tags: map[string]string{"aws:x": "v"}}, bad: "tag.aws:x"},
		{name: "bad tag filter value", kind: ReportKindUsage, raw: RawReportQuery{Tags: map[string]string{"env": "a|b"}}, bad: "tag.env"},
		{name: "untagged filter", kind: ReportKindUsage, raw: RawReportQuery{Tags: map[string]string{"env": UntaggedValue}}, gran: Daily, columns: "day"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := ParseReportQuery(tc.kind, tc.raw, reportNow)
			if tc.bad != "" {
				var e *Error
				if !errors.As(err, &e) || KindOf(err) != KindValidation || len(e.Fields) == 0 || e.Fields[0].Field != tc.bad {
					t.Fatalf("err = %v, want validation on %s", err, tc.bad)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cols := ""
			for i, c := range q.Columns() {
				if i > 0 {
					cols += ","
				}
				cols += c
			}
			if q.Granularity != tc.gran || cols != tc.columns {
				t.Errorf("granularity %s columns %s", q.Granularity, cols)
			}
		})
	}
}

func TestReportDaysAndKind(t *testing.T) {
	q := ReportQuery{From: time.Date(2026, 11, 1, 13, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)}
	if q.FirstDay().Day() != 1 || q.LastDay().Day() != 3 {
		t.Errorf("days %v %v", q.FirstDay(), q.LastDay())
	}
	q.To = q.From
	if q.LastDay().Day() != 1 {
		t.Errorf("empty range last day %v", q.LastDay())
	}
	if _, err := ParseReportKind("usage"); err != nil {
		t.Error(err)
	}
	if _, err := ParseReportKind("zzz"); KindOf(err) != KindNotFound {
		t.Errorf("err = %v", err)
	}
	a := ReportQuery{Kind: ReportKindUsage, From: q.From, To: q.To, Filters: ReportFilters{Tags: map[string]string{"a": "1", "b": "2"}}}
	b := a
	b.Filters.AgentID = "agt_x"
	c := a
	if a.Fingerprint() == b.Fingerprint() || a.Fingerprint() != c.Fingerprint() {
		t.Error("fingerprint")
	}
}
