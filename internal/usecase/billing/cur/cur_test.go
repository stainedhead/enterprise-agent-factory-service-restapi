package cur_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur/curtest"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	arnA  = "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc"
)

func usd(s string) domain.Money { return domain.MustMoney(s) }

func directory() cur.Directory {
	return cur.Directory{
		Namespaces: map[string]string{"afs-t-payments": teamA, "afs-t-search": teamB},
		Teams:      map[string]bool{teamA: true, teamB: true},
		Profiles:   map[string]cur.ProfileRef{arnA: {TeamID: teamA, ModelID: "anthropic.claude-sonnet-4-5"}},
		TagKeys:    map[string]bool{"business-unit": true, "cost-center": true},
	}
}

var monday = time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

func TestAttribute(t *testing.T) {
	lines := []cur.Line{
		{Day: monday, Kind: cur.KindBedrock, Resource: arnA, Cost: usd("10.50"), Tags: map[string]string{cur.TagTeamID: teamA}},
		// The profile alone attributes a line AWS billed without the tag.
		{Day: monday, Kind: cur.KindBedrock, Resource: arnA, Cost: usd("1.25")},
		{Day: monday, Kind: cur.KindPod, Resource: "afs-t-payments", Cost: usd("3"), Tags: map[string]string{cur.TagAgentID: "agt_1"}},
		{Day: monday, Kind: cur.KindPod, Resource: "afs-t-payments", Cost: usd("2"), Tags: map[string]string{cur.TagAgentID: "agt_1"}},
		// A pool worker carries the pool and never an agent.
		{Day: monday, Kind: cur.KindPod, Resource: "afs-t-search", Cost: usd("4"), Tags: map[string]string{cur.TagPoolID: "pool_1", cur.TagAgentID: "agt_9"}},
		{Day: monday, Kind: cur.KindNode, Cost: usd("20"), Tags: map[string]string{cur.TagTeamID: teamB}},
		{Day: monday, Kind: cur.KindStorage, Cost: usd("0.75"), Tags: map[string]string{cur.TagTeamID: teamA, cur.TagAgentID: "agt_1"}},
		// No team can be found: reported, not written.
		{Day: monday, Kind: cur.KindPod, Resource: "kube-system", Cost: usd("7")},
		{Day: monday, Kind: cur.KindStorage, Cost: usd("0.50"), Tags: map[string]string{cur.TagTeamID: "team_01JD2Q3R4S5T6V7W8X9Y0Z0009"}},
	}
	got := cur.Attribute(lines, directory())
	if got.UnattributedLines != 2 || got.Unattributed.String() != "7.50" {
		t.Fatalf("unattributed = %d lines, %s", got.UnattributedLines, got.Unattributed)
	}
	type k struct{ team, source, agent, pool, model string }
	want := map[k]string{
		{teamA, cur.SourceBedrock, "", "", "anthropic.claude-sonnet-4-5"}: "11.75",
		{teamA, cur.SourceEKS, "agt_1", "", ""}:                           "5.00",
		{teamB, cur.SourceEKS, "", "pool_1", ""}:                          "4.00",
		{teamB, cur.SourceEKS, "", "", ""}:                                "20.00",
		{teamA, cur.SourceOther, "agt_1", "", ""}:                         "0.75",
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("rows = %+v", got.Rows)
	}
	for _, r := range got.Rows {
		w, ok := want[k{r.TeamID, r.Source, r.AgentID, r.PoolID, r.ModelID}]
		if !ok || r.Cost.String() != w {
			t.Errorf("row %+v: want cost %q (known=%v)", r, w, ok)
		}
	}
}

// BI-US-6.1: a team tagged payments until Tuesday 12:00 and platform after. Monday's cost is under
// payments, Wednesday's under platform, and Tuesday's is split by the tags of its line items.
func TestTagChangeSplitsTheDay(t *testing.T) {
	tag := func(v string) map[string]string {
		return map[string]string{cur.TagTeamID: teamA, "business-unit": v, "unmanaged": "x"}
	}
	byDay := map[string][]cur.Line{
		"2026-11-16": {{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("10"), Tags: tag("payments")}},
		"2026-11-17": {
			{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("6"), Tags: tag("payments")},
			{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("5"), Tags: tag("platform")},
		},
		"2026-11-18": {{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("9"), Tags: tag("platform")}},
	}
	for day, want := range map[string]map[string]string{
		"2026-11-16": {"payments": "10.00"},
		"2026-11-17": {"payments": "6.00", "platform": "5.00"},
		"2026-11-18": {"platform": "9.00"},
	} {
		att := cur.Attribute(byDay[day], directory())
		got := map[string]string{}
		for _, r := range att.Rows {
			if len(r.Tags) != 1 {
				t.Fatalf("%s: tags %v: want only the managed key", day, r.Tags)
			}
			got[r.Tags["business-unit"]] = r.Cost.String()
		}
		if len(got) != len(want) {
			t.Fatalf("%s: rows %v, want %v", day, got, want)
		}
		for v, c := range want {
			if got[v] != c {
				t.Errorf("%s %s = %s, want %s", day, v, got[v], c)
			}
		}
	}
}

func TestSystemTagsAreNotATagSet(t *testing.T) {
	dir := directory()
	dir.TagKeys["afs-team-id"] = true // even if listed, afs-* keys are system tags
	att := cur.Attribute([]cur.Line{{Kind: cur.KindStorage, Cost: usd("1"), Tags: map[string]string{cur.TagTeamID: teamA, "cost-center": ""}}}, dir)
	if len(att.Rows) != 1 || att.Rows[0].Tags != nil {
		t.Fatalf("rows = %+v", att.Rows)
	}
}

func ingestor(f *curtest.Fetcher, s *curtest.Store) (*cur.Ingestor, *[]time.Duration, *[]cur.Alert) {
	var sleeps []time.Duration
	var alerts []cur.Alert
	return &cur.Ingestor{Fetch: f, Store: s,
		Retry:   cur.Backoff{Attempts: 4, Base: time.Second, Max: 3 * time.Second},
		Sleep:   func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
		OnAlert: func(a cur.Alert) { alerts = append(alerts, a) }}, &sleeps, &alerts
}

func TestRerunReplacesTheDay(t *testing.T) {
	f := &curtest.Fetcher{Lines: map[string][]cur.Line{"2026-11-16": {{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("10")}}}}
	s := &curtest.Store{Dir: directory()}
	in, _, _ := ingestor(f, s)
	for range 2 {
		res := in.Run(t.Context(), []time.Time{monday})
		if res.Err() != nil || res.Days[0].Rows != 1 || res.Days[0].Cost.String() != "10.00" {
			t.Fatalf("result = %+v", res)
		}
	}
	// A restated day replaces the rows, it does not add to them.
	f.Lines["2026-11-16"] = []cur.Line{{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("12")}}
	in.Run(t.Context(), []time.Time{monday})
	if rows := s.Days["2026-11-16"]; len(rows) != 1 || rows[0].Cost.String() != "12.00" || s.Writes["2026-11-16"] != 3 {
		t.Fatalf("rows = %+v writes=%d", rows, s.Writes["2026-11-16"])
	}
}

func TestNoLinesLeavesTheDay(t *testing.T) {
	s := &curtest.Store{Dir: directory(), Days: map[string][]cur.Row{"2026-11-16": {{TeamID: teamA}}}}
	in, _, _ := ingestor(&curtest.Fetcher{}, s)
	res := in.Run(t.Context(), []time.Time{monday})
	if !res.Days[0].NoData || res.Err() != nil || len(s.Days["2026-11-16"]) != 1 {
		t.Fatalf("result = %+v rows=%v", res, s.Days)
	}
}

func TestAthenaFailureIsRetriedWithBackoff(t *testing.T) {
	boom := errors.New("athena: throttled")
	f := &curtest.Fetcher{
		Lines: map[string][]cur.Line{"2026-11-16": {{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("1")}}},
		Errs:  map[string][]error{"2026-11-16": {boom, boom, boom}},
	}
	in, sleeps, alerts := ingestor(f, &curtest.Store{Dir: directory()})
	res := in.Run(t.Context(), []time.Time{monday})
	if res.Err() != nil || f.Calls["2026-11-16"] != 4 {
		t.Fatalf("err=%v calls=%d", res.Err(), f.Calls["2026-11-16"])
	}
	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second} // doubled, then capped
	if len(*sleeps) != 3 || (*sleeps)[0] != want[0] || (*sleeps)[1] != want[1] || (*sleeps)[2] != want[2] {
		t.Fatalf("sleeps = %v", *sleeps)
	}
	if len(*alerts) != 0 {
		t.Fatalf("a recovered day must not alert: %v", *alerts)
	}
}

func TestAlertAfterThreeFailedDays(t *testing.T) {
	boom := errors.New("athena: down")
	days := []time.Time{monday, monday.AddDate(0, 0, 1), monday.AddDate(0, 0, 2), monday.AddDate(0, 0, 3)}
	forever := func() []error {
		e := make([]error, 10)
		for i := range e {
			e[i] = boom
		}
		return e
	}
	for _, tc := range []struct {
		name   string
		failed []string
		alert  bool
	}{
		{"two failed days", []string{"2026-11-16", "2026-11-17"}, false},
		{"three failed days", []string{"2026-11-16", "2026-11-17", "2026-11-18"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &curtest.Fetcher{Errs: map[string][]error{}, Lines: map[string][]cur.Line{}}
			for _, d := range days {
				f.Lines[d.Format(time.DateOnly)] = []cur.Line{{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("1")}}
			}
			for _, d := range tc.failed {
				f.Errs[d] = forever()
			}
			in, _, alerts := ingestor(f, &curtest.Store{Dir: directory()})
			res := in.Run(t.Context(), days)
			if res.Failed != len(tc.failed) || res.Err() == nil {
				t.Fatalf("failed = %d, err = %v", res.Failed, res.Err())
			}
			if tc.alert != (len(*alerts) == 1) || (tc.alert && len((*alerts)[0].FailedDays) != 3) {
				t.Fatalf("alerts = %+v", *alerts)
			}
			// The day after the failures is still ingested.
			if res.Days[3].Err != nil {
				t.Fatalf("a failed day stopped the run: %v", res.Days[3].Err)
			}
		})
	}
}

func TestStoreFailureIsReported(t *testing.T) {
	f := &curtest.Fetcher{Lines: map[string][]cur.Line{"2026-11-16": {{Kind: cur.KindBedrock, Resource: arnA, Cost: usd("1")}}}}
	s := &curtest.Store{Dir: directory(), ReplaceErr: errors.New("db down")}
	in, _, _ := ingestor(f, s)
	if res := in.Run(t.Context(), []time.Time{monday}); res.Failed != 1 || res.Err() == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestDays(t *testing.T) {
	now := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	got := cur.Days(now, 3)
	if len(got) != 3 || got[0].Format(time.DateOnly) != "2026-11-19" || got[2].Format(time.DateOnly) != "2026-11-21" {
		t.Fatalf("days = %v", got)
	}
	if len(cur.Days(now, 0)) != 1 {
		t.Fatal("lookback below 1 means yesterday")
	}
}
