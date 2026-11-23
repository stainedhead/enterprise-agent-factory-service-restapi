package collab

import (
	"testing"
	"time"
)

// ORC-11: the tool change is a state machine over a snapshot, a pod on the new tool and the re-registration of
// connected workers. Each step has its conditions; a closed change stays closed.
func TestComputeToolChange(t *testing.T) {
	tests := []struct {
		name                        string
		mod                         func(*TeamState)
		open                        bool
		step, snapshotTaken, toolOK string // snapshotTaken: "", "True" or "False"; toolOK: True or False
	}{
		{"no snapshot, no change", func(*TeamState) {}, false, StepIdle, "", "True"},
		{"snapshot requested", func(s *TeamState) { s.SnapshotID, s.SnapshotState = "snap_1", "requested" }, true, StepSnapshotting, "False", "False"},
		{"snapshot pending", func(s *TeamState) { s.SnapshotID, s.SnapshotState = "snap_1", "pending" }, true, StepSnapshotting, "False", "False"},
		{"snapshot failed", func(s *TeamState) { s.SnapshotID, s.SnapshotState = "snap_1", "failed" }, true, StepSnapshotFailed, "False", "False"},
		{"completed, pod not ready", func(s *TeamState) { s.SnapshotID, s.SnapshotState, s.PodReady = "snap_1", "completed", false }, true, StepStarting, "True", "False"},
		{"completed, pod still the old one", func(s *TeamState) { s.SnapshotID, s.SnapshotState, s.PodStale = "snap_1", "completed", true }, true, StepStarting, "True", "False"},
		{"completed, workers re-registering", func(s *TeamState) { s.SnapshotID, s.SnapshotState, s.PendingLinks = "snap_1", "completed", 2 }, true, StepReregistering, "True", "False"},
		{"completed and settled closes the change", func(s *TeamState) { s.SnapshotID, s.SnapshotState = "snap_1", "completed" }, false, StepCompleted, "True", "True"},
		{"closed change stays closed", func(s *TeamState) {
			s.SnapshotID, s.SnapshotState, s.PrevClosedSnapshot, s.PodReady = "snap_1", "completed", "snap_1", false
		}, false, StepIdle, "True", "False"},
		{"a newer snapshot reopens", func(s *TeamState) {
			s.SnapshotID, s.SnapshotState, s.PrevClosedSnapshot = "snap_2", "requested", "snap_1"
		}, true, StepSnapshotting, "False", "False"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := up()
			tt.mod(&s)
			c := ComputeToolChange(s)
			if c.Open != tt.open || c.Step != tt.step || c.SnapshotTaken != tt.snapshotTaken || c.ToolReady != (tt.toolOK == "True") {
				t.Errorf("%+v", c)
			}
		})
	}
}

func TestToolChangeHoldsTheEndpointStarting(t *testing.T) {
	s := up()
	s.SnapshotID, s.SnapshotState = "snap_1", "pending"
	if e := ComputeEndpoint(s); e.Ready || e.Reason != ReasonStarting {
		t.Errorf("%+v", e)
	}
}

func TestSyncWritesToolChangeConditions(t *testing.T) {
	ev, st := &fakeEvents{}, &fakeStatus{}
	s := up()
	s.SnapshotID, s.SnapshotState, s.PrevToolVersion = "snap_1", "completed", "version 1.2.3"
	c := &Controller{Source: fakeSource{states: []TeamState{s}}, Status: st, Events: ev, Now: func() time.Time { return t0 }}
	if err := c.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	msg := map[string]string{}
	for _, cd := range st.reports[0].Conditions {
		got[cd.Type], msg[cd.Type] = string(cd.Status), cd.Message
	}
	if got[ConditionToolChanging] != "False" || msg[ConditionToolChanging] != "snap_1" || got[ConditionSnapshotTaken] != "True" ||
		got[ConditionToolReady] != "True" || msg[ConditionToolReady] != "version 1.2.3" {
		t.Errorf("conditions %v messages %v", got, msg)
	}
}
