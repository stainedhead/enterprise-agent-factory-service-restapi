package athena

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/awstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
)

var day = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func row(cells ...string) map[string]any {
	data := make([]map[string]string, len(cells))
	for i, c := range cells {
		data[i] = map[string]string{"VarCharValue": c}
	}
	return map[string]any{"Data": data}
}

var header = row("day", "kind", "resource", "cost", "tags")

func source(t *testing.T, srv *awstest.Server) *Source {
	t.Helper()
	s, err := New(srv.Config(), Config{Database: "afs_cur", Table: "cur2", Workgroup: "afs-billing",
		OutputLocation: "s3://example-athena-results/afs/", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func succeeded() awstest.Response {
	return awstest.JSONOK(map[string]any{"QueryExecution": map[string]any{"Status": map[string]any{"State": "SUCCEEDED"}}})
}

// A fixture CUR as Athena returns it: two pages, a header row, prefixed tags.
func TestFetchDayParsesTheFixtureCUR(t *testing.T) {
	srv := awstest.New(t)
	srv.OnAction("StartQueryExecution", awstest.JSONOK(map[string]string{"QueryExecutionId": "q-1"}))
	srv.OnAction("GetQueryExecution", succeeded())
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Action != "GetQueryResults" {
			return nil
		}
		if strings.Contains(string(r.Body), `"NextToken"`) {
			resp := awstest.JSONOK(map[string]any{"ResultSet": map[string]any{"Rows": []any{
				row("2026-11-20", "storage", "", "0.750000", `{"user_afs-team-id":"team_1","user_afs-agent-id":"agt_1"}`)}}})
			return &resp
		}
		resp := awstest.JSONOK(map[string]any{"NextToken": "page-2", "ResultSet": map[string]any{"Rows": []any{
			header,
			row("2026-11-20", "bedrock", "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc", "25.410000",
				`{"user_afs-team-id":"team_1","user_business-unit":"payments","aws_createdBy":"x","user_empty":""}`),
			row("2026-11-20", "pod", "afs-t-payments", "3.000000", `{"user_afs-pool-id":"pool_1"}`),
			row("2026-11-20", "dedicated_node", "", "20.000000", `null`),
		}}})
		return &resp
	})

	lines, err := source(t, srv).FetchDay(t.Context(), day)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 {
		t.Fatalf("lines = %+v", lines)
	}
	b := lines[0]
	if b.Kind != cur.KindBedrock || b.Cost.String() != "25.41" || b.Tags["business-unit"] != "payments" || b.Tags[cur.TagTeamID] != "team_1" {
		t.Fatalf("bedrock line = %+v", b)
	}
	if _, ok := b.Tags["createdBy"]; ok || len(b.Tags) != 2 {
		t.Fatalf("only prefixed non-empty tags are kept: %v", b.Tags)
	}
	if lines[1].Kind != cur.KindPod || lines[1].Resource != "afs-t-payments" || lines[1].Tags[cur.TagPoolID] != "pool_1" {
		t.Fatalf("pod line = %+v", lines[1])
	}
	if lines[2].Kind != cur.KindNode || lines[2].Tags != nil || lines[3].Kind != cur.KindStorage {
		t.Fatalf("lines = %+v", lines[2:])
	}

	start, _ := srv.Find("StartQueryExecution")
	var in struct {
		QueryString string
		WorkGroup   string
	}
	if err := start.JSON(&in); err != nil {
		t.Fatal(err)
	}
	if in.WorkGroup != "afs-billing" {
		t.Fatalf("workgroup = %q", in.WorkGroup)
	}
	for _, want := range []string{
		`"afs_cur"."cur2"`, `TIMESTAMP '2026-11-20 00:00:00'`, `TIMESTAMP '2026-11-21 00:00:00'`,
		`element_at(resource_tags, 'user_afs-team-id')`, `'AmazonBedrock'`, `'AmazonEKS'`, `EBS:`,
		`split_line_item_split_cost`, `NOT IN (SELECT node_id FROM dedicated)`,
	} {
		if !strings.Contains(in.QueryString, want) {
			t.Errorf("query lacks %q:\n%s", want, in.QueryString)
		}
	}
}

func TestPollsUntilTheQuerySucceeds(t *testing.T) {
	srv := awstest.New(t)
	srv.OnAction("StartQueryExecution", awstest.JSONOK(map[string]string{"QueryExecutionId": "q-1"}))
	polls := 0
	srv.On(func(r awstest.Request) *awstest.Response {
		if r.Action != "GetQueryExecution" {
			return nil
		}
		polls++
		state := "RUNNING"
		if polls >= 3 {
			state = "SUCCEEDED"
		}
		resp := awstest.JSONOK(map[string]any{"QueryExecution": map[string]any{"Status": map[string]any{"State": state}}})
		return &resp
	})
	srv.OnAction("GetQueryResults", awstest.JSONOK(map[string]any{"ResultSet": map[string]any{"Rows": []any{header}}}))
	lines, err := source(t, srv).FetchDay(t.Context(), day)
	if err != nil || len(lines) != 0 || polls != 3 {
		t.Fatalf("lines=%v err=%v polls=%d", lines, err, polls)
	}
}

func TestFailedQueryIsAnError(t *testing.T) {
	srv := awstest.New(t)
	srv.OnAction("StartQueryExecution", awstest.JSONOK(map[string]string{"QueryExecutionId": "q-1"}))
	srv.OnAction("GetQueryExecution", awstest.JSONOK(map[string]any{"QueryExecution": map[string]any{
		"Status": map[string]any{"State": "FAILED", "StateChangeReason": "COLUMN_NOT_FOUND: resource_tags"}}}))
	_, err := source(t, srv).FetchDay(t.Context(), day)
	if !errors.Is(err, ErrQueryFailed) || !strings.Contains(err.Error(), "COLUMN_NOT_FOUND") {
		t.Fatalf("err = %v", err)
	}
}

func TestThrottledStartIsAnError(t *testing.T) {
	srv := awstest.New(t)
	srv.OnAction("StartQueryExecution", awstest.JSONError(400, "TooManyRequestsException", "slow down"))
	if _, err := source(t, srv).FetchDay(t.Context(), day); err == nil || !strings.Contains(err.Error(), "TooManyRequests") {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusesMalformedResults(t *testing.T) {
	for name, rows := range map[string][]any{
		"short row":    {header, row("2026-11-20", "bedrock")},
		"wrong day":    {header, row("2026-11-19", "bedrock", "x", "1.000000", "{}")},
		"unknown kind": {header, row("2026-11-20", "lambda", "x", "1.000000", "{}")},
		"bad cost":     {header, row("2026-11-20", "bedrock", "x", "1.0E-6", "{}")},
		"bad tags":     {header, row("2026-11-20", "bedrock", "x", "1.000000", "{")},
	} {
		t.Run(name, func(t *testing.T) {
			srv := awstest.New(t)
			srv.OnAction("StartQueryExecution", awstest.JSONOK(map[string]string{"QueryExecutionId": "q"}))
			srv.OnAction("GetQueryExecution", succeeded())
			srv.OnAction("GetQueryResults", awstest.JSONOK(map[string]any{"ResultSet": map[string]any{"Rows": rows}}))
			if _, err := source(t, srv).FetchDay(t.Context(), day); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestContextCancelStopsPolling(t *testing.T) {
	srv := awstest.New(t)
	srv.OnAction("StartQueryExecution", awstest.JSONOK(map[string]string{"QueryExecutionId": "q"}))
	srv.OnAction("GetQueryExecution", awstest.JSONOK(map[string]any{"QueryExecution": map[string]any{"Status": map[string]any{"State": "QUEUED"}}}))
	s := source(t, srv)
	ctx, cancel := context.WithCancel(t.Context())
	s.sleep = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	if _, err := s.FetchDay(ctx, day); err == nil {
		t.Fatal("want an error")
	}
}

func TestIdentifiersAreChecked(t *testing.T) {
	for _, c := range []Config{
		{Database: `a"; DROP`, Table: "t", Workgroup: "w"},
		{Database: "d", Table: "t t", Workgroup: "w"},
		{Database: "d", Table: "t", Workgroup: ""},
	} {
		if _, err := newSource(nil, c); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if _, err := newSource(nil, Config{Database: "d", Table: "t", Workgroup: "afs-billing"}); err != nil {
		t.Fatal(err)
	}
}
