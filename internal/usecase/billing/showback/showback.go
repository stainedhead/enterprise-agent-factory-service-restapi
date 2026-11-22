// Package showback builds the monthly showback CSV of afs-billing (BI-4, BUD-7, TAG-6): one row per cost
// center and one per value of a chosen tag key, each with the month's calls, tokens, estimated (gateway)
// and billed (CUR) cost, a total row per dimension that equals cost_daily for the month, and an
// "(untagged)" row for spend that carries no value for the key. Tags are the tags in effect when each
// cost happened (cost_daily.tag_set_id), not today's. The CSV is written to the reports bucket.
package showback

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Dimensions of a showback CSV (the first column).
const (
	// DimCostCenter groups by the team's cost center.
	DimCostCenter = "costCenter"
	// TotalValue is the value column of a dimension's total row.
	TotalValue = "(total)"
)

// Measures are the sums of cost_daily rows. Calls and tokens come from the gateway rows only (the CUR
// rows carry cost, not calls); EstimatedMicros is the gateway's metering and BilledMicros the CUR
// costs, both in millionths of a USD.
type Measures struct {
	Calls, InputTokens, OutputTokens int64
	EstimatedMicros, BilledMicros    int64
}

// Add returns the sum of m and o.
func (m Measures) Add(o Measures) Measures {
	return Measures{m.Calls + o.Calls, m.InputTokens + o.InputTokens, m.OutputTokens + o.OutputTokens,
		m.EstimatedMicros + o.EstimatedMicros, m.BilledMicros + o.BilledMicros}
}

// Line is the measures of one value of a dimension.
type Line struct {
	Value string
	Measures
}

// Source reads cost_daily for a span of UTC days (both ends inclusive) in the transaction it is given.
type Source interface {
	// Totals sums every cost_daily row of the span.
	Totals(ctx context.Context, tx port.Tx, first, last time.Time) (Measures, error)
	// ByCostCenter sums the span per cost center of the team that incurred the cost.
	ByCostCenter(ctx context.Context, tx port.Tx, first, last time.Time) ([]Line, error)
	// ByTag sums the span per value of tag key in the tags in effect when the cost happened;
	// domain.UntaggedValue holds spend with no value for the key.
	ByTag(ctx context.Context, tx port.Tx, first, last time.Time, key string) ([]Line, error)
}

// Report is one month's showback.
type Report struct {
	Month  time.Time
	TagKey string
	// Total is the sum of cost_daily for the month.
	Total Measures
	// CostCenters and Tags are the per-value lines, in value order; Tags is empty without a TagKey.
	CostCenters, Tags []Line
}

// Generator builds and stores showback CSVs.
type Generator struct {
	Tx      port.TxRunner
	Source  Source
	Objects port.ObjectStore
	Clock   port.Clock
}

// Result says where a showback CSV was stored.
type Result struct {
	Key, URI string
	Report   Report
}

// ErrNotAdditive is returned when a dimension's lines do not add up to the month's total: the CSV is
// not written, because a showback that disagrees with cost_daily must not reach finance.
var ErrNotAdditive = errors.New("showback: the lines of a dimension do not add up to the month's total")

// MonthOf returns the first day (UTC) of the month t falls in.
func MonthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// ObjectKey is where the showback of month with tagKey is stored.
func ObjectKey(month time.Time, tagKey string) string {
	m := month.UTC().Format("2006-01")
	if tagKey == "" {
		return fmt.Sprintf("showback/%s/showback-%s.csv", m, m)
	}
	return fmt.Sprintf("showback/%s/showback-%s-by-%s.csv", m, m, tagKey)
}

// Build reads the month's showback. month is any instant in the month; a month that has not started is
// refused, and the current month is month to date. tagKey may be empty for cost centers only.
func (g *Generator) Build(ctx context.Context, month time.Time, tagKey string) (Report, error) {
	first := MonthOf(month)
	if first.After(g.Clock.Now().UTC()) {
		return Report{}, domain.Validation(domain.FieldError{Field: "month", Reason: "the month has not started"})
	}
	if tagKey != "" && (!domain.ValidTagKey(tagKey) || containsSpace(tagKey)) {
		return Report{}, domain.Validation(domain.FieldError{Field: "tagKey", Reason: "not a valid tag key"})
	}
	last := first.AddDate(0, 1, -1)
	rep := Report{Month: first, TagKey: tagKey}
	err := g.Tx.ReadTx(ctx, port.SystemSession("billing"), func(ctx context.Context, tx port.Tx) error {
		var err error
		if rep.Total, err = g.Source.Totals(ctx, tx, first, last); err != nil {
			return err
		}
		if rep.CostCenters, err = g.Source.ByCostCenter(ctx, tx, first, last); err != nil {
			return err
		}
		if tagKey != "" {
			rep.Tags, err = g.Source.ByTag(ctx, tx, first, last, tagKey)
		}
		return err
	})
	if err != nil {
		return Report{}, fmt.Errorf("showback: read cost_daily: %w", err)
	}
	sortLines(rep.CostCenters)
	sortLines(rep.Tags)
	dims := map[string][]Line{DimCostCenter: rep.CostCenters}
	if tagKey != "" {
		dims["tag:"+tagKey] = rep.Tags
	}
	for _, name := range []string{DimCostCenter, "tag:" + tagKey} {
		lines, ok := dims[name]
		if !ok {
			continue
		}
		var sum Measures
		for _, l := range lines {
			sum = sum.Add(l.Measures)
		}
		if sum != rep.Total {
			return Report{}, fmt.Errorf("%w: %s sums to %+v, cost_daily to %+v", ErrNotAdditive, name, sum, rep.Total)
		}
	}
	return rep, nil
}

// Run builds the showback of month and stores its CSV in the reports bucket, replacing the file of the
// same month and tag key.
func (g *Generator) Run(ctx context.Context, month time.Time, tagKey string) (Result, error) {
	rep, err := g.Build(ctx, month, tagKey)
	if err != nil {
		return Result{}, err
	}
	body, err := CSV(rep)
	if err != nil {
		return Result{}, err
	}
	key := ObjectKey(rep.Month, tagKey)
	put, err := g.Objects.Put(ctx, port.PutObject{Key: key, Body: body, ContentType: "text/csv; charset=utf-8"})
	if err != nil {
		return Result{}, fmt.Errorf("showback: store the CSV: %w", err)
	}
	return Result{Key: key, URI: put.URI, Report: rep}, nil
}

func containsSpace(s string) bool {
	for _, r := range s {
		if r == ' ' {
			return true
		}
	}
	return false
}

func sortLines(l []Line) {
	sort.Slice(l, func(i, j int) bool { return l[i].Value < l[j].Value })
}

// Header is the first line of a showback CSV.
var Header = []string{"dimension", "value", "calls", "inputTokens", "outputTokens", "estimatedUsd", "billedUsd"}

// CSV renders a report: for each dimension its value rows in value order, then its total row.
// Cells that a spreadsheet would run as a formula are prefixed with a quote, because tag values are
// typed by people.
func CSV(rep Report) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	write := func(dim, value string, m Measures) error {
		return w.Write([]string{dim, safe(value), fmt.Sprint(m.Calls), fmt.Sprint(m.InputTokens), fmt.Sprint(m.OutputTokens),
			domain.MoneyFromMicros(m.EstimatedMicros).String(), domain.MoneyFromMicros(m.BilledMicros).String()})
	}
	if err := w.Write(Header); err != nil {
		return nil, err
	}
	emit := func(dim string, lines []Line) error {
		for _, l := range lines {
			if err := write(dim, l.Value, l.Measures); err != nil {
				return err
			}
		}
		return write(dim, TotalValue, rep.Total)
	}
	if err := emit(DimCostCenter, rep.CostCenters); err != nil {
		return nil, err
	}
	if rep.TagKey != "" {
		if err := emit("tag:"+rep.TagKey, rep.Tags); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

func safe(c string) string {
	if c == "" {
		return c
	}
	switch c[0] {
	case '=', '+', '@', '\t', '\r':
		return "'" + c
	case '-':
		if _, err := strconv.ParseFloat(c, 64); err != nil {
			return "'" + c
		}
	}
	return c
}
