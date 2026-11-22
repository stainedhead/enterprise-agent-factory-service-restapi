package postgres_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

type dropRecorder struct {
	mu   sync.Mutex
	rows map[gateway.UsageDropReason]int
}

func (d *dropRecorder) UsageRowsDropped(r gateway.UsageDropReason, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rows == nil {
		d.rows = map[gateway.UsageDropReason]int{}
	}
	d.rows[r] += n
}

func (d *dropRecorder) get(r gateway.UsageDropReason) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rows[r]
}

func TestUsageWriterReportsQueueFullDrops(t *testing.T) {
	rec := &dropRecorder{}
	uw := postgres.NewUsageWriter(nil, nil, postgres.UsageWriterConfig{Queue: 1, Recorder: rec})
	for range 100 {
		uw.Record(gateway.Record{OccurredAt: time.Now(), TeamID: "t", ModelID: "m", RequestID: "x", Outcome: gateway.OutcomeOK})
	}
	if got := rec.get(gateway.UsageQueueFull); got != 99 || uw.Dropped() != 99 {
		t.Fatalf("recorder saw %d queue_full drops, Dropped() = %d, want 99", got, uw.Dropped())
	}
	if rec.get(gateway.UsageWriteFailed) != 0 {
		t.Fatal("write_failed reported without a write")
	}
}

func TestUsageWriterReportsWriteFailedDrops(t *testing.T) {
	w := seedGateway(t, "10")
	rec := &dropRecorder{}
	uw := postgres.NewUsageWriter(w.pool, nil, postgres.UsageWriterConfig{FlushEvery: 20 * time.Millisecond, Attempts: 2, Recorder: rec})
	uw.Run(t.Context())
	// An outcome the table's CHECK refuses fails every attempt.
	for range 3 {
		uw.Record(gateway.Record{OccurredAt: time.Now(), TeamID: w.team, ModelID: w.model, RequestID: "r", Outcome: "bogus",
			Cost: domain.MustMoney("0.1")})
	}
	uw.Close()
	if got := rec.get(gateway.UsageWriteFailed); got != 3 || uw.Failed() != 3 {
		t.Fatalf("recorder saw %d write_failed rows, Failed() = %d, want 3", got, uw.Failed())
	}
	if rec.get(gateway.UsageQueueFull) != 0 {
		t.Fatal("queue_full reported with room in the queue")
	}
}

// TestUsageWriterShutdownFlushSurvivesACancelledParent: Close after the parent context is gone still
// writes what is queued (the flush derives from the parent with WithoutCancel).
func TestUsageWriterShutdownFlushSurvivesACancelledParent(t *testing.T) {
	w := seedGateway(t, "10")
	uw := postgres.NewUsageWriter(w.pool, nil, postgres.UsageWriterConfig{FlushEvery: time.Hour})
	pctx, stop := context.WithCancel(t.Context())
	uw.Run(pctx)
	uw.Record(gateway.Record{OccurredAt: time.Now().UTC(), TeamID: w.team, ModelID: w.model, RequestID: "shutdown-1", Outcome: gateway.OutcomeOK, Cost: domain.MustMoney("0.1")})
	stop()
	uw.Close()
	var n int
	if err := w.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FROM afs.usage_events WHERE team_id = $1 AND request_id = 'shutdown-1'`, w.team).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || uw.Written() != 1 {
		t.Fatalf("rows %d written %d: the shutdown flush must outlive a cancelled parent", n, uw.Written())
	}
}

// TestUsageWriterHasNoBackgroundContext: the adapter derives every context from the one it is given
// (AGENTS.md: no context.Background() below cmd/).
func TestUsageWriterHasNoBackgroundContext(t *testing.T) {
	src, err := os.ReadFile("usage_writer.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "context.Background()") || strings.Contains(string(src), "context.TODO()") {
		t.Fatal("usage_writer.go creates a root context")
	}
}
