package gateway

import "sync"

// StreamMeter accumulates what a streamed call reports, as the adapter reads its events, so that the
// call can be settled on whatever was seen when the stream ends, is cut or is aborted (GW-6).
//
// Bedrock reports usage in the final event; the Anthropic stream reports input tokens in message_start
// and output tokens in message_delta. Each Observe call overwrites only the fields it carries.
type StreamMeter struct {
	mu       sync.Mutex
	started  bool
	reported bool
	usage    Usage
}

// Started records that the upstream's first event arrived.
func (m *StreamMeter) Started() {
	m.mu.Lock()
	m.started = true
	m.mu.Unlock()
}

// Observe merges usage fields seen in an event. A negative field is not carried by the event.
func (m *StreamMeter) Observe(u Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started, m.reported = true, true
	set := func(dst *int, v int) {
		if v >= 0 {
			*dst = v
		}
	}
	set(&m.usage.Input, u.Input)
	set(&m.usage.Output, u.Output)
	set(&m.usage.CacheRead, u.CacheRead)
	set(&m.usage.CacheWrite, u.CacheWrite)
}

// Result is the Result to return from an InvokeFunc when the stream ends.
func (m *StreamMeter) Result() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Result{Usage: m.usage, UsageReported: m.reported, Started: m.started}
}

// Absent is the value of a usage field an event does not carry, for Observe.
const Absent = -1
