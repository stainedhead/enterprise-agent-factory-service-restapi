package schedules_test

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules/schedtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

const (
	team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	ch   = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	sid  = "rsc_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	sid2 = "rsc_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

var now = time.Date(2026, 11, 23, 6, 5, 0, 0, time.UTC)

type rig struct {
	run    *schedules.Runner
	store  *schedtest.Store
	rep    *schedtest.Reports
	objs   *fakes.ObjectStore
	outbox *fakes.Outbox
	txr    *fakes.TxRunner
	clock  *fakes.Clock
}

// renderCSV is the CSV of a report as the API writes it: the groupBy columns and the measures, then
// one line per row.
func renderCSV(r domain.Report) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(append(append([]string{}, r.GroupBy...), r.MeasureCols...))
	for _, row := range r.Rows {
		line := append([]string{}, row.Group...)
		for _, m := range row.Measures {
			switch v := m.(type) {
			case int64:
				line = append(line, strconv.FormatInt(v, 10))
			case string:
				line = append(line, v)
			default:
				line = append(line, "")
			}
		}
		_ = w.Write(line)
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

func newRig() *rig {
	r := &rig{store: schedtest.NewStore(), rep: &schedtest.Reports{}, outbox: &fakes.Outbox{}, txr: &fakes.TxRunner{}, clock: &fakes.Clock{T: now}}
	r.objs = &fakes.ObjectStore{Clock: r.clock}
	r.rep.Rows = []domain.ReportRow{
		{Group: []string{"agt_1"}, Measures: []any{int64(3), int64(100), int64(50), "1.25"}},
		{Group: []string{"agt_2"}, Measures: []any{int64(1), int64(10), int64(5), "0.10"}},
	}
	r.rep.Totals = []any{int64(4), int64(110), int64(55), "1.35"}
	r.run = &schedules.Runner{Tx: r.txr, Store: r.store, Reports: r.rep, Objects: r.objs, Outbox: r.outbox, Clock: r.clock, IDs: &fakes.IDGen{}, Render: renderCSV,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return r
}

func (r *rig) add(id string, mut func(*domain.ReportSchedule)) domain.ReportSchedule {
	s := domain.ReportSchedule{ID: id, TeamID: team, Name: "n-" + id, Report: domain.ReportKindUsage, Cadence: domain.CadenceWeekly, ChannelID: ch,
		Query: domain.ScheduleQuery{Range: "7d", GroupBy: []string{"agent"}}, Format: "csv", Enabled: true,
		NextRunAt: time.Date(2026, 11, 23, 6, 0, 0, 0, time.UTC), Version: 1}
	if mut != nil {
		mut(&s)
	}
	r.store.Rows[id] = s
	return s
}

func TestDueScheduleIsDeliveredEqualToTheOnDemandReport(t *testing.T) {
	r := newRig()
	sch := r.add(sid, nil)
	sum, err := r.run.RunDue(t.Context())
	if err != nil || sum != (schedules.Summary{Due: 1, Delivered: 1}) {
		t.Fatalf("%+v %v", sum, err)
	}

	// The on-demand report for the same query, through the real report service on the same rows.
	svc := &reports.Service{Deps: acting.Deps{Tx: r.txr, Audit: &fakes.Audit{}, Outbox: r.outbox, Idem: &fakes.Idempotency{}, Clock: r.clock, IDs: &fakes.IDGen{}}, Repo: r.rep}
	admin := acting.Caller{Principal: authz.Principal{ID: "usr_sa", Type: authz.PrincipalUser, ServiceAdmin: true}}
	onDemand, err := svc.TeamReport(t.Context(), admin, team, domain.ReportKindUsage, sch.Query.Raw(team), acting.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	want, err := renderCSV(onDemand)
	if err != nil {
		t.Fatal(err)
	}
	key := "reports/" + team + "/" + sid + "/usage-20261123T060000Z.csv"
	got, err := r.objs.Body(key)
	if err != nil {
		t.Fatalf("object %s: %v (have %v)", key, err, r.objs.Objects)
	}
	if string(got) != string(want) {
		t.Errorf("scheduled CSV differs from the on-demand report:\n%s\nwant:\n%s", got, want)
	}
	if r.objs.Types[key] != "text/csv; charset=utf-8" {
		t.Errorf("content type %q", r.objs.Types[key])
	}

	// The outcome: sent, next run advanced, no error.
	row := r.store.Rows[sid]
	if row.LastStatus != domain.ScheduleSent || row.LastError != "" || row.LastRunAt == nil || !row.LastRunAt.Equal(now) {
		t.Errorf("row = %+v", row)
	}
	if !row.NextRunAt.Equal(time.Date(2026, 11, 30, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("next run = %s, want the next Monday", row.NextRunAt)
	}

	// The announcement: report.delivered for the team's channel with a 7-day download URL.
	if len(r.outbox.Events) != 1 {
		t.Fatalf("%d events", len(r.outbox.Events))
	}
	ev := r.outbox.Events[0]
	d := ev.Detail.Data
	if ev.Type != domain.EventReportDelivered || ev.Detail.TeamID != team || ev.AggregateID != sid || ev.Detail.Actor != "system:billing" {
		t.Errorf("event = %+v", ev)
	}
	if d["channelId"] != ch || d["scheduleId"] != sid || d["report"] != "usage" || d["rows"] != 2 {
		t.Errorf("data = %v", d)
	}
	if u, _ := d["downloadUrl"].(string); !strings.Contains(u, key) || !strings.Contains(u, "X-Amz-Expires=604800") {
		t.Errorf("downloadUrl = %v", d["downloadUrl"])
	}
	if d["downloadExpiresAt"] != now.Add(7*24*time.Hour).Format(time.RFC3339) || d["s3Uri"] != "s3://example-reports/"+key {
		t.Errorf("data = %v", d)
	}
}

func TestTheScheduleIsRunForItsOwnTeamOnly(t *testing.T) {
	r := newRig()
	r.add(sid, func(s *domain.ReportSchedule) { s.Query.Filters = map[string]string{"teamId": team, "tag.env": "prod"} })
	if _, err := r.run.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.rep.Queries) != 1 {
		t.Fatalf("%d queries", len(r.rep.Queries))
	}
	q := r.rep.Queries[0]
	if q.Filters.TeamID != team || q.Filters.Tags["env"] != "prod" || q.Kind != domain.ReportKindUsage || !q.To.Equal(now) || !q.From.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("query = %+v", q)
	}
}

func TestSchedulesThatAreNotDueAreLeftAlone(t *testing.T) {
	r := newRig()
	r.add(sid, func(s *domain.ReportSchedule) { s.NextRunAt = now.Add(time.Minute) })
	r.add(sid2, func(s *domain.ReportSchedule) { s.Enabled = false })
	sum, err := r.run.RunDue(t.Context())
	if err != nil || sum != (schedules.Summary{}) || r.objs.Puts != 0 || len(r.outbox.Events) != 0 || len(r.store.Recorded) != 0 {
		t.Fatalf("%+v %v puts=%d events=%d recorded=%d", sum, err, r.objs.Puts, len(r.outbox.Events), len(r.store.Recorded))
	}
}

func TestAScheduleLockedByAnotherRunnerIsSkipped(t *testing.T) {
	r := newRig()
	r.add(sid, nil)
	r.store.Locked[sid] = true
	sum, err := r.run.RunDue(t.Context())
	if err != nil || sum != (schedules.Summary{Due: 1, Skipped: 1}) || r.objs.Puts != 0 {
		t.Fatalf("%+v %v", sum, err)
	}
}

// BI-US-3.1: a failure records failed with the error and retries the next cycle.
func TestFailureIsRecordedAndRetriedNextCycle(t *testing.T) {
	boom := errors.New("AccessDenied: s3:PutObject")
	cases := []struct {
		name    string
		break_  func(r *rig)
		mend    func(r *rig)
		wantErr string
	}{
		{"store", func(r *rig) { r.objs.PutErr = boom }, func(r *rig) { r.objs.PutErr = nil }, "store the CSV"},
		{"sign", func(r *rig) { r.objs.PresignErr = boom }, func(r *rig) { r.objs.PresignErr = nil }, "sign the download URL"},
		{"compute", func(r *rig) { r.rep.Err = boom }, func(r *rig) { r.rep.Err = nil }, "compute the report"},
		{"render", func(r *rig) {
			r.run.Render = func(domain.Report) ([]byte, error) { return nil, boom }
		}, func(r *rig) { r.run.Render = renderCSV }, "render the CSV"},
		{"too many rows", func(r *rig) { r.run.MaxRows = 1 }, func(r *rig) { r.run.MaxRows = 0 }, "more than 1 rows"},
		{"invalid stored query", func(r *rig) {
			s := r.store.Rows[sid]
			s.Query.GroupBy = []string{"bogus"}
			r.store.Rows[sid] = s
		}, func(r *rig) {
			s := r.store.Rows[sid]
			s.Query.GroupBy = []string{"agent"}
			r.store.Rows[sid] = s
		}, "no longer valid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig()
			r.add(sid, nil)
			dueAt := r.store.Rows[sid].NextRunAt
			c.break_(r)
			sum, err := r.run.RunDue(t.Context())
			if err != nil || sum != (schedules.Summary{Due: 1, Failed: 1}) {
				t.Fatalf("%+v %v", sum, err)
			}
			row := r.store.Rows[sid]
			if row.LastStatus != domain.ScheduleFailed || !strings.Contains(row.LastError, c.wantErr) {
				t.Errorf("row = %+v, want an error naming %q", row, c.wantErr)
			}
			if !row.NextRunAt.Equal(dueAt) {
				t.Errorf("a failure moved next_run_at to %s", row.NextRunAt)
			}
			if len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventReportFailed {
				t.Fatalf("events = %+v", r.outbox.Events)
			}
			d := r.outbox.Events[0].Detail.Data
			if d["channelId"] != ch || d["scheduleId"] != sid || !strings.Contains(d["error"].(string), c.wantErr) {
				t.Errorf("data = %v", d)
			}

			// Next cycle, with the cause gone, the same schedule runs and succeeds.
			c.mend(r)
			r.clock.Advance(time.Hour)
			sum, err = r.run.RunDue(t.Context())
			if err != nil || sum != (schedules.Summary{Due: 1, Delivered: 1}) {
				t.Fatalf("retry: %+v %v", sum, err)
			}
			row = r.store.Rows[sid]
			if row.LastStatus != domain.ScheduleSent || row.LastError != "" || !row.NextRunAt.After(r.clock.Now()) {
				t.Errorf("after retry: %+v", row)
			}
			if len(r.outbox.Events) != 2 || r.outbox.Events[1].Type != domain.EventReportDelivered {
				t.Errorf("events = %+v", r.outbox.Events)
			}
			// The retry writes the object of the scheduled instant, not a second one.
			if len(r.objs.Objects) != 1 && c.name != "store" && c.name != "compute" && c.name != "render" && c.name != "too many rows" && c.name != "invalid stored query" {
				t.Errorf("objects = %v", r.objs.Objects)
			}
		})
	}
}

func TestARetryReplacesTheObjectOfTheScheduledInstant(t *testing.T) {
	r := newRig()
	r.add(sid, nil)
	r.objs.PresignErr = errors.New("kms: unavailable")
	if _, err := r.run.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.objs.PresignErr = nil
	r.clock.Advance(3 * time.Hour)
	if _, err := r.run.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(r.objs.Objects) != 1 || r.objs.Puts != 2 {
		t.Errorf("objects=%d puts=%d: the retry must overwrite the first attempt's object", len(r.objs.Objects), r.objs.Puts)
	}
}

func TestLongErrorsAreTruncated(t *testing.T) {
	r := newRig()
	r.add(sid, nil)
	r.objs.PutErr = errors.New(strings.Repeat("é", 400))
	if _, err := r.run.RunDue(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := r.store.Rows[sid].LastError
	if len(e) > schedules.MaxErrorLength || !strings.HasSuffix(e, "...") {
		t.Errorf("last error is %d bytes: %.30q", len(e), e)
	}
	if got := schedules.Truncate("short"); got != "short" {
		t.Errorf("Truncate(short) = %q", got)
	}
}

func TestOneFailureDoesNotStopTheOthers(t *testing.T) {
	r := newRig()
	r.add(sid, func(s *domain.ReportSchedule) { s.Query.GroupBy = []string{"bogus"} })
	r.add(sid2, func(s *domain.ReportSchedule) { s.NextRunAt = s.NextRunAt.Add(time.Minute) })
	sum, err := r.run.RunDue(t.Context())
	if err != nil || sum != (schedules.Summary{Due: 2, Delivered: 1, Failed: 1}) {
		t.Fatalf("%+v %v", sum, err)
	}
	if r.store.Rows[sid2].LastStatus != domain.ScheduleSent || r.store.Rows[sid].LastStatus != domain.ScheduleFailed {
		t.Errorf("rows = %+v", r.store.Rows)
	}
}

func TestBatchLimitsACycle(t *testing.T) {
	r := newRig()
	r.run.Batch = 1
	r.add(sid, nil)
	r.add(sid2, func(s *domain.ReportSchedule) { s.NextRunAt = s.NextRunAt.Add(time.Minute) })
	sum, err := r.run.RunDue(t.Context())
	if err != nil || sum.Due != 1 || sum.Delivered != 1 {
		t.Fatalf("%+v %v", sum, err)
	}
	if sum, _ = r.run.RunDue(t.Context()); sum.Delivered != 1 {
		t.Errorf("second cycle: %+v", sum)
	}
}

func TestStoreTroubleIsReported(t *testing.T) {
	boom := errors.New("db down")
	r := newRig()
	r.add(sid, nil)
	r.store.DueErr = boom
	if _, err := r.run.RunDue(t.Context()); !errors.Is(err, boom) {
		t.Errorf("listing: %v", err)
	}
	r.store.DueErr = nil

	// An outcome that cannot be recorded leaves the schedule due and publishes nothing.
	r.store.RecordErr = boom
	sum, err := r.run.RunDue(t.Context())
	if err != nil || sum.Failed != 1 || len(r.outbox.Events) != 0 || r.store.Rows[sid].LastStatus != "" {
		t.Errorf("record failure: %+v %v events=%d row=%+v", sum, err, len(r.outbox.Events), r.store.Rows[sid])
	}
	r.store.RecordErr = nil
	r.store.ClaimErr = boom
	if sum, err = r.run.RunDue(t.Context()); err != nil || sum.Failed != 1 {
		t.Errorf("claim failure: %+v %v", sum, err)
	}
	r.store.ClaimErr = nil
	if sum, err = r.run.RunDue(t.Context()); err != nil || sum.Delivered != 1 {
		t.Errorf("recovery: %+v %v", sum, err)
	}
}

func TestACancelledCycleRecordsNothing(t *testing.T) {
	r := newRig()
	r.add(sid, nil)
	cctx, stop := contextWithCancel(t.Context())
	r.objs.PutErr = errors.New("cancelled mid-upload")
	r.run.Objects = cancellingStore{ObjectStore: r.objs, cancel: stop}
	if _, err := r.run.RunDue(cctx); err == nil {
		t.Fatal("a cancelled cycle returned no error")
	}
	if len(r.store.Recorded) != 0 || len(r.outbox.Events) != 0 {
		t.Errorf("recorded=%d events=%d: a cancelled run must not record a failure", len(r.store.Recorded), len(r.outbox.Events))
	}
}

func TestObjectKeyDependsOnTheScheduledInstant(t *testing.T) {
	s := domain.ReportSchedule{ID: sid, TeamID: team, Report: domain.ReportKindCosts}
	got := schedules.ObjectKey(s, time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC))
	if got != "reports/"+team+"/"+sid+"/costs-20261201T060000Z.csv" {
		t.Errorf("key = %s", got)
	}
}
