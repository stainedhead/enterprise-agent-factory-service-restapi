package rollup_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/rollup"
)

type store struct {
	days []int
	err  error
}

func (s *store) Recent(_ context.Context, d int) (int64, error) {
	s.days = append(s.days, d)
	return 5, s.err
}

type rec struct {
	jobs []string
	errs []error
}

func (r *rec) RollupRun(j string, _ int64, err error) {
	r.jobs = append(r.jobs, j)
	r.errs = append(r.errs, err)
}

type sched map[string]time.Duration

func (s sched) Every(n string, d time.Duration, _ func(context.Context) error) { s[n] = d }

func TestJobsRegisterWithTheDocumentedCadence(t *testing.T) {
	s := sched{}
	(&rollup.Runner{Store: &store{}}).Register(s)
	if s["report-rollup-recent"] != 10*time.Minute || s["report-rollup-nightly"] != 24*time.Hour || len(s) != 2 {
		t.Fatalf("registered = %v", s)
	}
}

func TestRunCallsRecentWithTheJobsDaysAndRecords(t *testing.T) {
	st, r := &store{}, &rec{}
	run := &rollup.Runner{Store: st, Recorder: r, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, j := range rollup.Jobs() {
		if err := run.Run(t.Context(), j); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.days) != 2 || st.days[0] != 2 || st.days[1] != 7 || len(r.jobs) != 2 {
		t.Fatalf("days=%v recorded=%v", st.days, r.jobs)
	}
}

func TestRunErrorIsWrappedAndRecorded(t *testing.T) {
	boom := errors.New("boom")
	r := &rec{}
	err := (&rollup.Runner{Store: &store{err: boom}, Recorder: r}).Run(t.Context(), rollup.Jobs()[0])
	if !errors.Is(err, boom) || r.errs[0] == nil {
		t.Fatalf("err=%v recorded=%v", err, r.errs)
	}
}
