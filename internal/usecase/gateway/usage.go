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
