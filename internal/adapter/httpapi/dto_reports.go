package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// reportRangeBody is the range a report answers for.
type reportRangeBody struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// reportBody is the wire shape of Report: groupBy columns, then measures, per row.
type reportBody struct {
	Kind      string           `json:"kind"`
	Range     reportRangeBody  `json:"range"`
	GroupBy   []string         `json:"groupBy"`
	Rows      []map[string]any `json:"rows"`
	Totals    map[string]any   `json:"totals"`
	Notes     []string         `json:"notes"`
	NextToken string           `json:"nextToken,omitempty"`
}

// usageRowBody is the wire shape of UsageRow: the groupBy values under key.
type usageRowBody struct {
	Key          map[string]string `json:"key"`
	Calls        any               `json:"calls"`
	InputTokens  any               `json:"inputTokens"`
	OutputTokens any               `json:"outputTokens"`
	CostUsd      any               `json:"costUsd"`
}

// usageBody is the wire shape of Usage.
type usageBody struct {
	Range     reportRangeBody `json:"range"`
	GroupBy   []string        `json:"groupBy"`
	Rows      []usageRowBody  `json:"rows"`
	Totals    usageRowBody    `json:"totals"`
	NextToken string          `json:"nextToken,omitempty"`
}

func toRange(r domain.ReportRange) reportRangeBody {
	return reportRangeBody{From: w2Time(r.From), To: w2Time(r.To)}
}

func measureMap(cols []string, vals []any) map[string]any {
	m := make(map[string]any, len(cols))
	for i, c := range cols {
		if i < len(vals) {
			m[c] = vals[i]
		}
	}
	return m
}

func toReportBody(r domain.Report, next string) reportBody {
	rows := make([]map[string]any, 0, len(r.Rows))
	for _, row := range r.Rows {
		m := measureMap(r.MeasureCols, row.Measures)
		for i, g := range r.GroupBy {
			m[g] = row.Group[i]
			if g == string(domain.DimPerson) && r.Kind == domain.ReportKindCollaboration {
				m["principal"] = principalOf(row.Group[i])
			}
		}
		if row.ServiceAdminEntry {
			m["serviceAdminEntry"] = true
		}
		rows = append(rows, m)
	}
	notes := r.Notes
	if notes == nil {
		notes = []string{}
	}
	return reportBody{Kind: string(r.Kind), Range: toRange(r.Range), GroupBy: r.GroupBy, Rows: rows,
		Totals: measureMap(r.MeasureCols, r.Totals), Notes: notes, NextToken: next}
}

func toUsageBody(r domain.Report, next string) usageBody {
	row := func(key map[string]string, vals []any) usageRowBody {
		m := measureMap(r.MeasureCols, vals)
		return usageRowBody{Key: key, Calls: m["calls"], InputTokens: m["inputTokens"], OutputTokens: m["outputTokens"], CostUsd: m["costUsd"]}
	}
	out := usageBody{Range: toRange(r.Range), GroupBy: r.GroupBy, Rows: make([]usageRowBody, 0, len(r.Rows)), NextToken: next}
	for _, rw := range r.Rows {
		key := make(map[string]string, len(r.GroupBy))
		for i, g := range r.GroupBy {
			key[g] = rw.Group[i]
		}
		out.Rows = append(out.Rows, row(key, rw.Measures))
	}
	out.Totals = row(map[string]string{}, r.Totals)
	return out
}

// reportCells renders the rows of a report as CSV cells, in header order (RA-US-7.3).
func reportCells(r domain.Report) (header []string, cells [][]string) {
	header = append(append([]string{}, r.GroupBy...), r.MeasureCols...)
	for _, row := range r.Rows {
		line := append([]string{}, row.Group...)
		for _, v := range row.Measures {
			line = append(line, cell(v))
		}
		cells = append(cells, line)
	}
	return header, cells
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// rawReportQuery reads the report query parameters; tag.<key>=<value> are the tag filters.
func rawReportQuery(r *http.Request) domain.RawReportQuery {
	q := r.URL.Query()
	raw := domain.RawReportQuery{From: q.Get("from"), To: q.Get("to"), Range: q.Get("range"), GroupBy: q.Get("groupBy"),
		TeamID: q.Get("teamId"), AgentID: q.Get("agentId"), ModelID: q.Get("modelId"), SystemID: q.Get("systemId"), PoolID: q.Get("poolId")}
	for name, vals := range q {
		if key, ok := strings.CutPrefix(name, "tag."); ok && len(vals) > 0 {
			if raw.Tags == nil {
				raw.Tags = map[string]string{}
			}
			raw.Tags[key] = vals[0]
		}
	}
	return raw
}

// principalOf is the principal object of a collaboration row grouped by person: a user (usr_ id) or an API key
// (its prefix). The anonymous group has none.
func principalOf(id string) map[string]string {
	switch {
	case id == "(anonymous)":
		return map[string]string{"id": "", "type": "anonymous"}
	case strings.HasPrefix(id, "usr_"):
		return map[string]string{"id": id, "type": "user"}
	}
	return map[string]string{"id": id, "type": "api_key"}
}
