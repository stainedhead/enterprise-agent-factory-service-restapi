package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget/budgettest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

type jobRecorder struct{ jobs []engine.Job }

func (j *jobRecorder) AddJob(x engine.Job) { j.jobs = append(j.jobs, x) }

func TestWireBudgetRegistersJobs(t *testing.T) {
	rec := &jobRecorder{}
	wireBudget(rec, nil, nil, nil, nil, &budgettest.IAM{}, nil, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(rec.jobs) == 0 {
		t.Fatal("no budget jobs registered")
	}
	seen := map[string]bool{}
	for _, j := range rec.jobs {
		if j.Name == "" || j.Fn == nil || seen[j.Name] {
			t.Fatalf("bad or duplicate job %+v", j)
		}
		seen[j.Name] = true
	}
}
