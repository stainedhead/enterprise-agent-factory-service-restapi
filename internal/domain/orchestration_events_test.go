package domain

import (
	"testing"
	"time"
)

// The v1.4.0 event types build valid envelopes (API contract §4.12).
func TestOrchestrationEventTypes(t *testing.T) {
	for _, et := range []EventType{EventOrchestrationCreated, EventOrchestrationUpdated, EventOrchestrationToolChanged,
		EventOrchestrationEndpointStateChanged, EventTeamModelsChanged, EventAgentModelUnavailable} {
		if !et.Known() {
			t.Errorf("%s is not known", et)
		}
		if _, err := NewEvent(et, "evt_1", time.Now(), "usr_1", "team_1", "team", "team_1", nil); err != nil {
			t.Errorf("%s: %v", et, err)
		}
	}
}
