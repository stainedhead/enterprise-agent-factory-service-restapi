// Package schedules is the schedule runner of afs-billing (BI-5, RPT-8, OQ-S2): it finds the report
// schedules that are due, computes each report with the same code and query rules as the on-demand
// report, stores the CSV in the reports bucket, announces it with a report.delivered event (channel id
// and a pre-signed download URL) and moves next_run_at forward. A failure to compute, render, store or
// sign records the schedule failed with the error and a report.failed event, and leaves next_run_at
// where it is so the next cycle retries.
package schedules

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

// Defaults of a Runner.
const (
	// DefaultURLExpiry is the lifetime of the download URL in report.delivered (OQ-S2: 7 days).
	DefaultURLExpiry = 7 * 24 * time.Hour
	// DefaultMaxRows bounds one scheduled report; a larger one fails with a message to narrow the query.
	DefaultMaxRows = 100000
	// DefaultBatch is how many due schedules one cycle runs.
	DefaultBatch = 200
	// MaxErrorLength is the longest error text recorded (the column allows 500).
	MaxErrorLength = 500
)

// Outcome is what a run records on its schedule.
type Outcome struct {
	// Status is domain.ScheduleSent or domain.ScheduleFailed.
	Status string
	// Error is the failure text; empty when sent.
	Error string
	// RunAt is when the run happened.
	RunAt time.Time
	// NextRunAt is the new next_run_at after a success; nil leaves it so the next cycle retries.
	NextRunAt *time.Time
}

// Store is the schedule queue of afs-billing, on the database role afs_billing.
type Store interface {
	// DueIDs lists the ids of enabled schedules due at now, oldest first.
	DueIDs(ctx context.Context, tx port.Tx, now time.Time, limit int) ([]string, error)
	// Claim locks one schedule for this run. It returns false when the schedule is gone, disabled, not
	// due or locked by another runner.
	Claim(ctx context.Context, tx port.Tx, id string, now time.Time) (domain.ReportSchedule, bool, error)
	// Record stores the outcome of a run on the claimed schedule.
	Record(ctx context.Context, tx port.Tx, id string, o Outcome) error
}

// ReportSource reads the report tables inside the transaction it is given; the report repository of the
// on-demand reports is one.
type ReportSource interface {
	Run(ctx context.Context, tx port.Tx, q domain.ReportQuery, offset, limit int) (reports.Result, error)
}

// Runner runs due report schedules.
type Runner struct {
	Tx      port.TxRunner
	Store   Store
	Reports ReportSource
	Objects port.ObjectStore
	Outbox  port.OutboxWriter
	Clock   port.Clock
	IDs     port.IDGen
	// Render turns a report into the CSV that the on-demand report returns for the same query.
	Render func(domain.Report) ([]byte, error)
	Log    *slog.Logger
	// URLExpiry, MaxRows and Batch default to the constants above.
	URLExpiry time.Duration
	MaxRows   int
	Batch     int
}

// Summary counts the outcomes of one cycle.
type Summary struct {
	Due, Delivered, Failed, Skipped int
}

func (r *Runner) session() port.Session { return port.SystemSession("billing") }

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// RunDue runs every schedule that is due now. A failed schedule does not stop the others; the error is
// returned only for trouble that stops the cycle (listing, the context).
func (r *Runner) RunDue(ctx context.Context) (Summary, error) {
	var sum Summary
	batch := r.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	var ids []string
	err := r.Tx.ReadTx(ctx, r.session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		ids, err = r.Store.DueIDs(ctx, tx, r.Clock.Now(), batch)
		return err
	})
	if err != nil {
		return sum, fmt.Errorf("schedules: list due schedules: %w", err)
	}
	sum.Due = len(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		switch res, err := r.runOne(ctx, id); {
		case err != nil:
			// The outcome could not be recorded; the schedule stays due and is retried next cycle.
			r.log().Error("schedule run not recorded", "schedule", id, "error", err)
			sum.Failed++
		case res == resultSkipped:
			sum.Skipped++
		case res == resultDelivered:
			sum.Delivered++
		default:
			sum.Failed++
		}
	}
	return sum, ctx.Err()
}

type result int

const (
	resultSkipped result = iota
	resultDelivered
	resultFailed
)

// runOne claims one schedule and runs it in one transaction: the report, the upload, the event and the
// recorded outcome commit together, so a crash leaves the schedule due and no event behind. A retried
// transaction writes the same object key again, which replaces the object.
func (r *Runner) runOne(ctx context.Context, id string) (result, error) {
	res := resultSkipped
	err := r.Tx.InTx(ctx, r.session(), func(ctx context.Context, tx port.Tx) error {
		res = resultSkipped
		now := r.Clock.Now().UTC()
		sch, ok, err := r.Store.Claim(ctx, tx, id, now)
		if err != nil || !ok {
			return err
		}
		ev, derr := r.deliver(ctx, tx, sch, now)
		if derr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.log().Warn("scheduled report failed", "schedule", sch.ID, "team", sch.TeamID, "report", string(sch.Report), "error", Truncate(derr.Error()))
			res = resultFailed
			if err := r.Store.Record(ctx, tx, sch.ID, Outcome{Status: domain.ScheduleFailed, Error: Truncate(derr.Error()), RunAt: now}); err != nil {
				return err
			}
			return r.publish(ctx, tx, domain.EventReportFailed, sch, now, map[string]any{
				"scheduleId": sch.ID, "channelId": sch.ChannelID, "report": string(sch.Report), "error": Truncate(derr.Error())})
		}
		res = resultDelivered
		next := domain.NextRun(sch.Cadence, now)
		if err := r.Store.Record(ctx, tx, sch.ID, Outcome{Status: domain.ScheduleSent, RunAt: now, NextRunAt: &next}); err != nil {
			return err
		}
		return r.publish(ctx, tx, domain.EventReportDelivered, sch, now, ev)
	})
	return res, err
}

// Compute builds the report of a schedule at now: the stored query for the schedule's team, read the way
// the on-demand report reads it, as one page. It fails when the report has more than MaxRows rows.
func (r *Runner) Compute(ctx context.Context, tx port.Tx, sch domain.ReportSchedule, now time.Time) (domain.Report, error) {
	maxRows := r.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	q, err := domain.ParseReportQuery(sch.Report, sch.Query.WithDefaults(sch.Cadence).Raw(sch.TeamID), now)
	if err != nil {
		return domain.Report{}, fmt.Errorf("the stored query is no longer valid: %w", err)
	}
	q.Filters.TeamID = sch.TeamID
	res, err := r.Reports.Run(ctx, tx, q, 0, maxRows+1)
	if err != nil {
		return domain.Report{}, fmt.Errorf("compute the report: %w", err)
	}
	if len(res.Rows) > maxRows {
		return domain.Report{}, fmt.Errorf("the report has more than %d rows; narrow the query or group it more coarsely", maxRows)
	}
	rep := domain.Report{Kind: sch.Report, Range: domain.ReportRange{From: q.From, To: q.To}, GroupBy: q.Columns(), Rows: res.Rows,
		Totals: res.Totals, Notes: domain.ReportNotes(q)}
	for _, m := range domain.ReportMeasures(sch.Report) {
		rep.MeasureCols = append(rep.MeasureCols, m.Name)
	}
	return rep, nil
}

// ObjectKey is where a schedule's CSV for the run due at due is stored (OQ-S2). It depends on the
// scheduled instant, not on when the run happens, so a retry replaces the object it wrote before.
func ObjectKey(sch domain.ReportSchedule, due time.Time) string {
	return fmt.Sprintf("reports/%s/%s/%s-%s.csv", sch.TeamID, sch.ID, sch.Report, due.UTC().Format("20060102T150405Z"))
}

// deliver computes, renders, stores and signs; it returns the data of the report.delivered event.
func (r *Runner) deliver(ctx context.Context, tx port.Tx, sch domain.ReportSchedule, now time.Time) (map[string]any, error) {
	rep, err := r.Compute(ctx, tx, sch, now)
	if err != nil {
		return nil, err
	}
	body, err := r.Render(rep)
	if err != nil {
		return nil, fmt.Errorf("render the CSV: %w", err)
	}
	key := ObjectKey(sch, sch.NextRunAt)
	put, err := r.Objects.Put(ctx, port.PutObject{Key: key, Body: body, ContentType: "text/csv; charset=utf-8"})
	if err != nil {
		return nil, fmt.Errorf("store the CSV: %w", err)
	}
	expiry := r.URLExpiry
	if expiry <= 0 {
		expiry = DefaultURLExpiry
	}
	url, err := r.Objects.PresignGet(ctx, key, expiry)
	if err != nil {
		return nil, fmt.Errorf("sign the download URL: %w", err)
	}
	return map[string]any{
		"scheduleId": sch.ID, "channelId": sch.ChannelID, "report": string(sch.Report), "rows": len(rep.Rows),
		"range":     map[string]any{"from": rep.Range.From.UTC().Format(time.RFC3339), "to": rep.Range.To.UTC().Format(time.RFC3339)},
		"s3Uri":     put.URI,
		"sizeBytes": put.Size, "downloadUrl": url.URL, "downloadExpiresAt": url.ExpiresAt.UTC().Format(time.RFC3339),
	}, nil
}

func (r *Runner) publish(ctx context.Context, tx port.Tx, typ domain.EventType, sch domain.ReportSchedule, now time.Time, data map[string]any) error {
	eid, err := r.IDs.NewID("evt")
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, eid.String(), now, r.session().Actor, sch.TeamID, "report_schedule", sch.ID, data)
	if err != nil {
		return err
	}
	return r.Outbox.Publish(ctx, tx, ev)
}

// Truncate cuts an error text to what the schedule's last_error column holds.
func Truncate(s string) string {
	if len(s) <= MaxErrorLength {
		return s
	}
	return strings.ToValidUTF8(s[:MaxErrorLength-3], "") + "..."
}
