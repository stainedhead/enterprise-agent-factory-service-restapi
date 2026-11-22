package gateway

import (
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// record builds the usage_events row of a call (gateway contract §8). Rejected calls cost nothing.
func record(c Caller, req Request, outcome Outcome, u Usage, cost domain.Money, started time.Time, now time.Time, tagSet string) Record {
	return Record{
		OccurredAt: started.UTC(), TeamID: c.TeamID, AgentID: c.AgentID, PoolID: c.PoolID, WorkerPod: workerPod(c),
		ModelID: req.ModelID, RequestID: req.RequestID, Outcome: outcome, Usage: u, Cost: cost,
		LatencyMS: int(now.Sub(started) / time.Millisecond), TagSetID: tagSet,
	}
}

// workerPod is the pool worker's pod name; an agent's row carries none (usage_events_subject_check).
func workerPod(c Caller) string {
	if c.PoolID != "" {
		return c.PodName
	}
	return ""
}

// UsageDropReason says why a usage row was dropped.
type UsageDropReason string

// Reasons a usage row is dropped (afs_gateway_usage_rows_dropped_total{reason}).
const (
	// UsageQueueFull: the writer's queue was full when the row arrived.
	UsageQueueFull UsageDropReason = "queue_full"
	// UsageWriteFailed: every attempt to write the row's batch failed.
	UsageWriteFailed UsageDropReason = "write_failed"
)

// UsageDropRecorder is told of every usage row the writer drops, so the loss is exported and alertable
// (FR-R08, RD-3, RD-4). The composition root binds it to a metric.
type UsageDropRecorder interface {
	UsageRowsDropped(reason UsageDropReason, rows int)
}
