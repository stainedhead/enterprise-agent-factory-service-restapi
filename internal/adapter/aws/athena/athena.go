// Package athena reads the AWS Cost and Usage Report through Athena for afs-billing (BI-1, BI-7).
//
// One query per UTC day returns the AFS-relevant lines already grouped and classified: Bedrock cost by
// inference profile, EKS split cost per namespace (pods on a dedicated node are left out and the node
// is attributed whole by its afs-team-id tag), and EBS volumes and snapshots. The column names are the
// CUR 2.0 "standard data export" with resource ids and split cost allocation data
// (line_item_*, split_line_item_*, resource_tags) and tag keys carry the export's user tag prefix.
package athena

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awsathena "github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
)

// API is the part of the Athena client the adapter uses.
type API interface {
	StartQueryExecution(ctx context.Context, in *awsathena.StartQueryExecutionInput, opts ...func(*awsathena.Options)) (*awsathena.StartQueryExecutionOutput, error)
	GetQueryExecution(ctx context.Context, in *awsathena.GetQueryExecutionInput, opts ...func(*awsathena.Options)) (*awsathena.GetQueryExecutionOutput, error)
	GetQueryResults(ctx context.Context, in *awsathena.GetQueryResultsInput, opts ...func(*awsathena.Options)) (*awsathena.GetQueryResultsOutput, error)
}

// Config names the CUR table and the Athena workgroup.
type Config struct {
	// Database and Table hold the CUR (AFS_CUR_DATABASE, AFS_CUR_TABLE).
	Database, Table string
	// Workgroup runs the query (AFS_ATHENA_WORKGROUP).
	Workgroup string
	// OutputLocation is the s3:// result location (AFS_ATHENA_OUTPUT); empty uses the workgroup's.
	OutputLocation string
	// TagPrefix is the CUR export's prefix of user tag keys in resource_tags (default "user_").
	TagPrefix string
	// PollInterval and Timeout bound the wait for a query (defaults 2 s and 10 min).
	PollInterval, Timeout time.Duration
}

var identRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)

// Source implements cur.Fetcher on Athena.
type Source struct {
	api   API
	cfg   Config
	sleep func(context.Context, time.Duration) error
}

var _ cur.Fetcher = (*Source)(nil)

// New returns a Source on the SDK configuration. It refuses names that are not plain identifiers,
// because the database and table are placed in the SQL text.
func New(awsCfg awssdk.Config, c Config) (*Source, error) {
	return newSource(awsathena.NewFromConfig(awsCfg), c)
}

func newSource(api API, c Config) (*Source, error) {
	for name, v := range map[string]string{"database": c.Database, "table": c.Table, "workgroup": c.Workgroup} {
		if !identRE.MatchString(strings.ReplaceAll(v, "-", "_")) {
			return nil, fmt.Errorf("athena: %s %q is not a plain identifier", name, v)
		}
	}
	if c.TagPrefix == "" {
		c.TagPrefix = "user_"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	return &Source{api: api, cfg: c, sleep: sleepCtx}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Query returns the SQL of one day. It is exported for the tests that pin its shape.
func (s *Source) Query(day time.Time) string {
	day = day.UTC().Truncate(24 * time.Hour)
	from := day.Format("2006-01-02") + " 00:00:00"
	to := day.AddDate(0, 0, 1).Format("2006-01-02") + " 00:00:00"
	tbl := fmt.Sprintf(`"%s"."%s"`, s.cfg.Database, s.cfg.Table)
	team := fmt.Sprintf(`element_at(resource_tags, '%s%s')`, s.cfg.TagPrefix, cur.TagTeamID)
	window := fmt.Sprintf(`line_item_usage_start_date >= TIMESTAMP '%s' AND line_item_usage_start_date < TIMESTAMP '%s'`, from, to)
	const pod = `(line_item_product_code = 'AmazonEKS' AND line_item_resource_id LIKE 'arn:%:pod/%')`
	node := fmt.Sprintf(`(line_item_product_code = 'AmazonEC2' AND (line_item_usage_type LIKE '%%BoxUsage%%' OR line_item_usage_type LIKE '%%SpotUsage%%') AND %s IS NOT NULL)`, team)
	return fmt.Sprintf(`WITH dedicated AS (
  SELECT DISTINCT line_item_resource_id AS node_id FROM %[1]s WHERE %[2]s AND %[3]s
), lines AS (
  SELECT date(line_item_usage_start_date) AS day,
    CASE
      WHEN line_item_product_code = 'AmazonBedrock' THEN 'bedrock'
      WHEN %[4]s THEN 'pod'
      WHEN %[3]s THEN 'dedicated_node'
      WHEN line_item_product_code = 'AmazonEC2' AND line_item_usage_type LIKE '%%EBS:%%' THEN 'storage'
    END AS kind,
    CASE WHEN line_item_product_code = 'AmazonBedrock' THEN line_item_resource_id
         WHEN %[4]s THEN split_part(line_item_resource_id, '/', 3) ELSE '' END AS resource,
    CASE WHEN %[4]s THEN coalesce(split_line_item_split_cost, 0) + coalesce(split_line_item_unused_cost, 0)
         ELSE line_item_unblended_cost END AS cost,
    split_line_item_parent_resource_id AS parent,
    json_format(CAST(resource_tags AS JSON)) AS tags
  FROM %[1]s
  WHERE %[2]s AND line_item_line_item_type <> 'Tax'
)
SELECT CAST(day AS varchar) AS day, kind, resource, format('%%.6f', sum(cost)) AS cost, tags
FROM lines
WHERE kind IS NOT NULL AND (kind <> 'pod' OR parent IS NULL OR parent NOT IN (SELECT node_id FROM dedicated))
GROUP BY 1, 2, 3, 5
ORDER BY 1, 2, 3, 5`, tbl, window, node, pod)
}

// ErrQueryFailed wraps a query that Athena reported as failed or cancelled; the message is Athena's
// state change reason, which holds no credentials.
var ErrQueryFailed = errors.New("athena: query failed")

// FetchDay implements cur.Fetcher.
func (s *Source) FetchDay(ctx context.Context, day time.Time) ([]cur.Line, error) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	in := &awsathena.StartQueryExecutionInput{
		QueryString: awssdk.String(s.Query(day)),
		WorkGroup:   awssdk.String(s.cfg.Workgroup),
		QueryExecutionContext: &types.QueryExecutionContext{
			Database: awssdk.String(s.cfg.Database),
		},
	}
	if s.cfg.OutputLocation != "" {
		in.ResultConfiguration = &types.ResultConfiguration{OutputLocation: awssdk.String(s.cfg.OutputLocation)}
	}
	started, err := s.api.StartQueryExecution(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("athena: start query: %w", err)
	}
	id := started.QueryExecutionId
	if err := s.wait(ctx, id); err != nil {
		return nil, err
	}
	return s.results(ctx, id, day.UTC().Truncate(24*time.Hour))
}

func (s *Source) wait(ctx context.Context, id *string) error {
	for {
		out, err := s.api.GetQueryExecution(ctx, &awsathena.GetQueryExecutionInput{QueryExecutionId: id})
		if err != nil {
			return fmt.Errorf("athena: get query execution: %w", err)
		}
		if st := out.QueryExecution; st != nil && st.Status != nil {
			switch st.Status.State {
			case types.QueryExecutionStateSucceeded:
				return nil
			case types.QueryExecutionStateFailed, types.QueryExecutionStateCancelled:
				return fmt.Errorf("%w: %s: %s", ErrQueryFailed, st.Status.State, awssdk.ToString(st.Status.StateChangeReason))
			}
		}
		if err := s.sleep(ctx, s.cfg.PollInterval); err != nil {
			return fmt.Errorf("athena: waiting for the query: %w", err)
		}
	}
}

func (s *Source) results(ctx context.Context, id *string, day time.Time) ([]cur.Line, error) {
	var lines []cur.Line
	var token *string
	first := true
	for {
		out, err := s.api.GetQueryResults(ctx, &awsathena.GetQueryResultsInput{QueryExecutionId: id, NextToken: token})
		if err != nil {
			return nil, fmt.Errorf("athena: get query results: %w", err)
		}
		rows := out.ResultSet.Rows
		if first && len(rows) > 0 { // the first row of the first page is the header
			rows = rows[1:]
		}
		first = false
		for _, r := range rows {
			l, err := s.line(r, day)
			if err != nil {
				return nil, err
			}
			lines = append(lines, l)
		}
		if out.NextToken == nil {
			return lines, nil
		}
		token = out.NextToken
	}
}

// line parses a result row: day, kind, resource, cost, tags.
func (s *Source) line(r types.Row, day time.Time) (cur.Line, error) {
	if len(r.Data) != 5 {
		return cur.Line{}, fmt.Errorf("athena: result row has %d columns, want 5", len(r.Data))
	}
	cell := func(i int) string { return awssdk.ToString(r.Data[i].VarCharValue) }
	if got := cell(0); got != day.Format(time.DateOnly) {
		return cur.Line{}, fmt.Errorf("athena: result row for day %q in the query for %s", got, day.Format(time.DateOnly))
	}
	kind := cur.Kind(cell(1))
	switch kind {
	case cur.KindBedrock, cur.KindPod, cur.KindNode, cur.KindStorage:
	default:
		return cur.Line{}, fmt.Errorf("athena: unknown line kind %q", kind)
	}
	cost, err := domain.ParseMoney("cost", cell(3))
	if err != nil {
		// Credits and refunds can sum below zero; a negative figure is parsed too.
		return cur.Line{}, fmt.Errorf("athena: cost %q: %w", cell(3), err)
	}
	tags, err := s.tags(cell(4))
	if err != nil {
		return cur.Line{}, err
	}
	return cur.Line{Day: day, Kind: kind, Resource: cell(2), Cost: cost, Tags: tags}, nil
}

// tags decodes the resource_tags JSON object and strips the export's prefix; keys without the prefix
// (AWS-generated tags) are dropped, and so are empty values.
func (s *Source) tags(raw string) (map[string]string, error) {
	if raw == "" || raw == "null" {
		return nil, nil
	}
	var all map[string]*string
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil, fmt.Errorf("athena: resource tags: %w", err)
	}
	out := map[string]string{}
	for k, v := range all {
		if v == nil || *v == "" || !strings.HasPrefix(k, s.cfg.TagPrefix) {
			continue
		}
		out[strings.TrimPrefix(k, s.cfg.TagPrefix)] = *v
	}
	return out, nil
}
