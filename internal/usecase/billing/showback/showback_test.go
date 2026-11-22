package showback_test

import (
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback/showbacktest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

var today = time.Date(2026, 12, 3, 8, 0, 0, 0, time.UTC)

func m(calls, in, out, est, billed int64) showback.Measures {
	return showback.Measures{Calls: calls, InputTokens: in, OutputTokens: out, EstimatedMicros: est, BilledMicros: billed}
}

func fixture() *showbacktest.Source {
	return &showbacktest.Source{
		Total: m(10, 1000, 500, 12_500_000, 13_000_000),
		CostCenters: []showback.Line{
			{Value: "CC200", Measures: m(4, 400, 200, 5_000_000, 5_250_000)},
			{Value: "CC100", Measures: m(6, 600, 300, 7_500_000, 7_750_000)},
		},
		Tags: map[string][]showback.Line{"business-unit": {
			{Value: "retail", Measures: m(7, 700, 350, 8_000_000, 8_500_000)},
			{Value: domain.UntaggedValue, Measures: m(3, 300, 150, 4_500_000, 4_500_000)},
		}},
	}
}

func newGen(src *showbacktest.Source) (*showback.Generator, *fakes.ObjectStore) {
	objs := &fakes.ObjectStore{}
	return &showback.Generator{Tx: &fakes.TxRunner{}, Source: src, Objects: objs, Clock: &fakes.Clock{T: today}}, objs
}

func parse(t *testing.T, body []byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// BI-US-5.1: one row per cost center and per value of the tag, totals equal cost_daily, an (untagged) row.
func TestShowbackCSV(t *testing.T) {
	src := fixture()
	gen, objs := newGen(src)
	res, err := gen.Run(t.Context(), time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC), "business-unit")
	if err != nil {
		t.Fatal(err)
	}
	if res.Key != "showback/2026-11/showback-2026-11-by-business-unit.csv" || res.URI != "s3://example-reports/"+res.Key {
		t.Errorf("result = %+v", res)
	}
	if len(src.Spans) != 1 || !src.Spans[0][0].Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) || !src.Spans[0][1].Equal(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("span = %v: the month is the first to the last UTC day", src.Spans)
	}
	body, err := objs.Body(res.Key)
	if err != nil {
		t.Fatal(err)
	}
	got := parse(t, body)
	want := [][]string{
		{"dimension", "value", "calls", "inputTokens", "outputTokens", "estimatedUsd", "billedUsd"},
		{"costCenter", "CC100", "6", "600", "300", "7.50", "7.75"},
		{"costCenter", "CC200", "4", "400", "200", "5.00", "5.25"},
		{"costCenter", "(total)", "10", "1000", "500", "12.50", "13.00"},
		{"tag:business-unit", "(untagged)", "3", "300", "150", "4.50", "4.50"},
		{"tag:business-unit", "retail", "7", "700", "350", "8.00", "8.50"},
		{"tag:business-unit", "(total)", "10", "1000", "500", "12.50", "13.00"},
	}
	if len(got) != len(want) {
		t.Fatalf("rows:\n%s", body)
	}
	for i := range want {
		if strings.Join(got[i], ",") != strings.Join(want[i], ",") {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
	if objs.Types[res.Key] != "text/csv; charset=utf-8" {
		t.Errorf("content type %q", objs.Types[res.Key])
	}
}

func TestShowbackWithoutATagKeyHasCostCentersOnly(t *testing.T) {
	src := fixture()
	gen, objs := newGen(src)
	res, err := gen.Run(t.Context(), today, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Key != "showback/2026-12/showback-2026-12.csv" || len(src.TagKeys) != 0 {
		t.Errorf("key %s, tag keys %v", res.Key, src.TagKeys)
	}
	body, _ := objs.Body(res.Key)
	if rows := parse(t, body); len(rows) != 4 || rows[0][0] != "dimension" {
		t.Errorf("rows = %v", rows)
	}
}

// A line that does not add up to cost_daily means a bug; the CSV is not written.
func TestShowbackThatDoesNotAddUpIsNotWritten(t *testing.T) {
	cases := map[string]func(*showbacktest.Source){
		"cost centers miss spend": func(s *showbacktest.Source) { s.CostCenters = s.CostCenters[:1] },
		"tags double count":       func(s *showbacktest.Source) { s.Tags["business-unit"][0].BilledMicros += 1 },
		"total changed":           func(s *showbacktest.Source) { s.Total.Calls++ },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			src := fixture()
			mut(src)
			gen, objs := newGen(src)
			if _, err := gen.Run(t.Context(), today, "business-unit"); !errors.Is(err, showback.ErrNotAdditive) {
				t.Fatalf("err = %v", err)
			}
			if objs.Puts != 0 {
				t.Error("a CSV that disagrees with cost_daily was written")
			}
		})
	}
}

func TestShowbackRefusals(t *testing.T) {
	gen, objs := newGen(fixture())
	if _, err := gen.Run(t.Context(), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), ""); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("a month that has not started: %v", err)
	}
	for _, k := range []string{"aws:createdBy", "afs-team-id", "has space", strings.Repeat("k", 200)} {
		if _, err := gen.Run(t.Context(), today, k); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("tag key %q: %v", k, err)
		}
	}
	if objs.Puts != 0 {
		t.Error("a refused request wrote an object")
	}
}

func TestShowbackFailures(t *testing.T) {
	boom := errors.New("boom")
	src := fixture()
	src.Err = boom
	gen, _ := newGen(src)
	if _, err := gen.Run(t.Context(), today, "business-unit"); !errors.Is(err, boom) {
		t.Errorf("source error: %v", err)
	}
	gen, objs := newGen(fixture())
	objs.PutErr = boom
	if _, err := gen.Run(t.Context(), today, "business-unit"); !errors.Is(err, boom) {
		t.Errorf("store error: %v", err)
	}
}

// Tag values are typed by people: a value that a spreadsheet would run as a formula is neutralized.
func TestShowbackCSVNeutralizesFormulas(t *testing.T) {
	rep := showback.Report{Month: today, TagKey: "owner", CostCenters: []showback.Line{{Value: "CC100"}},
		Tags: []showback.Line{{Value: "=HYPERLINK(\"http://evil\")"}, {Value: "-2+3"}, {Value: "-5"}, {Value: "@sum"}, {Value: "+1"}, {Value: "ok"}}}
	body, err := showback.CSV(rep)
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]bool{}
	for _, r := range parse(t, body) {
		vals[r[1]] = true
	}
	for _, v := range []string{"'=HYPERLINK(\"http://evil\")", "'-2+3", "-5", "'@sum", "'+1", "ok"} {
		if !vals[v] {
			t.Errorf("missing %q in %v", v, vals)
		}
	}
}

func TestMonthOfAndObjectKey(t *testing.T) {
	if got := showback.MonthOf(time.Date(2026, 11, 30, 23, 59, 0, 0, time.FixedZone("x", -5*3600))); !got.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("MonthOf = %s (UTC rules)", got)
	}
	if got := showback.ObjectKey(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "env"); got != "showback/2026-03/showback-2026-03-by-env.csv" {
		t.Errorf("key = %s", got)
	}
}
