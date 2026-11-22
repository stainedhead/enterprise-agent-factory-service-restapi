package domain

import (
	"strings"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNextRun(t *testing.T) {
	cases := []struct {
		name    string
		cadence Cadence
		now     string
		want    string
	}{
		{"daily before the hour", CadenceDaily, "2026-11-22T05:59:59Z", "2026-11-22T06:00:00Z"},
		{"daily at the hour is strictly after", CadenceDaily, "2026-11-22T06:00:00Z", "2026-11-23T06:00:00Z"},
		{"daily after the hour", CadenceDaily, "2026-11-22T18:00:00Z", "2026-11-23T06:00:00Z"},
		{"weekly from a Sunday", CadenceWeekly, "2026-11-22T10:00:00Z", "2026-11-23T06:00:00Z"},
		{"weekly from Monday before the hour", CadenceWeekly, "2026-11-23T01:00:00Z", "2026-11-23T06:00:00Z"},
		{"weekly from Monday after the hour", CadenceWeekly, "2026-11-23T07:00:00Z", "2026-11-30T06:00:00Z"},
		{"monthly mid month", CadenceMonthly, "2026-11-22T10:00:00Z", "2026-12-01T06:00:00Z"},
		{"monthly on the first before the hour", CadenceMonthly, "2026-12-01T05:00:00Z", "2026-12-01T06:00:00Z"},
		{"monthly across the year", CadenceMonthly, "2026-12-15T05:00:00Z", "2027-01-01T06:00:00Z"},
		{"non UTC input", CadenceDaily, "2026-11-22T00:30:00-05:00", "2026-11-22T06:00:00Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NextRun(c.cadence, ts(c.now)); !got.Equal(ts(c.want)) {
				t.Errorf("NextRun = %s, want %s", got.Format(time.RFC3339), c.want)
			}
		})
	}
}

const chanID = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"
const teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
const teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"

func TestNewScheduleParams(t *testing.T) {
	now := ts("2026-11-22T10:00:00Z")
	off := false
	good := NewScheduleParams{Name: "weekly", Report: ReportKindUsage, Cadence: CadenceWeekly, ChannelID: chanID}
	cases := []struct {
		name    string
		mut     func(*NewScheduleParams)
		wantErr string
	}{
		{"ok with defaults", func(*NewScheduleParams) {}, ""},
		{"disabled", func(p *NewScheduleParams) { p.Enabled = &off }, ""},
		{"grouped by tag", func(p *NewScheduleParams) {
			p.Query = ScheduleQuery{Range: "30d", GroupBy: []string{"tag:business-unit"}, Filters: map[string]string{"tag.env": "prod"}}
		}, ""},
		{"own team filter", func(p *NewScheduleParams) { p.Query = ScheduleQuery{Filters: map[string]string{"teamId": teamA}} }, ""},
		{"other team filter", func(p *NewScheduleParams) { p.Query = ScheduleQuery{Filters: map[string]string{"teamId": teamB}} }, "query.filters.teamId"},
		{"unknown filter", func(p *NewScheduleParams) { p.Query = ScheduleQuery{Filters: map[string]string{"color": "red"}} }, "query.filters.color"},
		{"bad groupBy", func(p *NewScheduleParams) { p.Query = ScheduleQuery{GroupBy: []string{"model", "bogus"}} }, "query.groupBy"},
		{"model group of a traffic report", func(p *NewScheduleParams) {
			p.Report = ReportKindTraffic
			p.Query = ScheduleQuery{GroupBy: []string{"model"}}
		}, "query.groupBy"},
		{"bad range", func(p *NewScheduleParams) { p.Query = ScheduleQuery{Range: "1y"} }, "query.range"},
		{"empty name", func(p *NewScheduleParams) { p.Name = " " }, "name"},
		{"long name", func(p *NewScheduleParams) { p.Name = strings.Repeat("n", 101) }, "name"},
		{"bad report", func(p *NewScheduleParams) { p.Report = "bills" }, "report"},
		{"bad cadence", func(p *NewScheduleParams) { p.Cadence = "hourly" }, "cadence"},
		{"bad channel", func(p *NewScheduleParams) { p.ChannelID = "chan_1" }, "channelId"},
		{"bad format", func(p *NewScheduleParams) { p.Format = "pdf" }, "format"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := good
			c.mut(&p)
			s, err := p.New("rsc_01JD2Q3R4S5T6V7W8X9Y0Z0001", teamA, now)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want a field error naming %s", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if s.Format != "csv" || s.Version != 1 || !s.NextRunAt.After(now) {
				t.Errorf("schedule = %+v", s)
			}
			if s.Query.Range == "" && s.Query.From == "" {
				t.Errorf("the query should default its range from the cadence: %+v", s.Query)
			}
			if p.Enabled != nil && s.Enabled != *p.Enabled || p.Enabled == nil && !s.Enabled {
				t.Errorf("enabled = %v", s.Enabled)
			}
		})
	}
}

func TestScheduleDefaultRanges(t *testing.T) {
	for c, want := range map[Cadence]string{CadenceDaily: "24h", CadenceWeekly: "7d", CadenceMonthly: "30d"} {
		if got := (ScheduleQuery{}).WithDefaults(c).Range; got != want {
			t.Errorf("%s: range %q, want %q", c, got, want)
		}
	}
	if got := (ScheduleQuery{Range: "90d"}).WithDefaults(CadenceDaily).Range; got != "90d" {
		t.Errorf("an explicit range was replaced: %q", got)
	}
}

func TestScheduleQueryRaw(t *testing.T) {
	raw := ScheduleQuery{Range: "7d", GroupBy: []string{"agent", "tag:env"},
		Filters: map[string]string{"agentId": "agt_x", "modelId": "m", "systemId": "ext_x", "poolId": "pool_x", "tag.env": "prod", "teamId": teamB}}.Raw(teamA)
	if raw.TeamID != teamA || raw.GroupBy != "agent,tag:env" || raw.Tags["env"] != "prod" || raw.AgentID != "agt_x" || raw.ModelID != "m" || raw.SystemID != "ext_x" || raw.PoolID != "pool_x" {
		t.Errorf("raw = %+v", raw)
	}
}

func TestSchedulePatch(t *testing.T) {
	now := ts("2026-11-22T10:00:00Z")
	base, err := NewScheduleParams{Name: "w", Report: ReportKindCosts, Cadence: CadenceWeekly, ChannelID: chanID}.New("rsc_01JD2Q3R4S5T6V7W8X9Y0Z0001", teamA, now)
	if err != nil {
		t.Fatal(err)
	}
	str := func(s string) *string { return &s }
	cad := func(c Cadence) *Cadence { return &c }
	yes, no := true, false
	later := now.Add(48 * time.Hour)
	cases := []struct {
		name    string
		base    ReportSchedule
		patch   SchedulePatch
		wantErr string
		check   func(t *testing.T, s ReportSchedule)
	}{
		{"rename keeps the run time", base, SchedulePatch{Name: str("renamed")}, "", func(t *testing.T, s ReportSchedule) {
			if s.Name != "renamed" || !s.NextRunAt.Equal(base.NextRunAt) {
				t.Errorf("%+v", s)
			}
		}},
		{"cadence change reschedules", base, SchedulePatch{Cadence: cad(CadenceMonthly)}, "", func(t *testing.T, s ReportSchedule) {
			if s.Cadence != CadenceMonthly || !s.NextRunAt.Equal(ts("2026-12-01T06:00:00Z")) {
				t.Errorf("%+v", s)
			}
		}},
		{"same cadence keeps the run time", base, SchedulePatch{Cadence: cad(CadenceWeekly)}, "", func(t *testing.T, s ReportSchedule) {
			if !s.NextRunAt.Equal(base.NextRunAt) {
				t.Errorf("%+v", s)
			}
		}},
		{"disable then enable reschedules", func() ReportSchedule { b := base; b.Enabled = false; return b }(), SchedulePatch{Enabled: &yes}, "", func(t *testing.T, s ReportSchedule) {
			if !s.Enabled || !s.NextRunAt.After(now) {
				t.Errorf("%+v", s)
			}
		}},
		{"disable", base, SchedulePatch{Enabled: &no}, "", func(t *testing.T, s ReportSchedule) {
			if s.Enabled {
				t.Errorf("%+v", s)
			}
		}},
		{"query is re-validated", base, SchedulePatch{Query: &ScheduleQuery{GroupBy: []string{"nope"}}}, "query.groupBy", nil},
		{"channel shape", base, SchedulePatch{ChannelID: str("x")}, "channelId", nil},
		{"empty name", base, SchedulePatch{Name: str("")}, "name", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.base.Apply(c.patch, later)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, got)
		})
	}
}

func TestScheduleDue(t *testing.T) {
	now := ts("2026-11-22T10:00:00Z")
	cases := []struct {
		name string
		s    ReportSchedule
		want bool
	}{
		{"past", ReportSchedule{Enabled: true, NextRunAt: now.Add(-time.Minute)}, true},
		{"exactly now", ReportSchedule{Enabled: true, NextRunAt: now}, true},
		{"future", ReportSchedule{Enabled: true, NextRunAt: now.Add(time.Minute)}, false},
		{"disabled", ReportSchedule{Enabled: false, NextRunAt: now.Add(-time.Hour)}, false},
	}
	for _, c := range cases {
		if got := c.s.Due(now); got != c.want {
			t.Errorf("%s: Due = %v", c.name, got)
		}
	}
}
