package collab

// Condition types of the tool change (ORC-11, API contract 4.4d): ToolChanging is true from the snapshot
// request until the new tool serves and its connected workers are registered; SnapshotTaken says whether the
// pre_tool_change snapshot completed; ToolReady says whether the current tool's pod serves.
const (
	ConditionToolChanging  = "ToolChanging"
	ConditionSnapshotTaken = "SnapshotTaken"
	ConditionToolReady     = "ToolReady"
)

// Steps of the tool change, used as the reason of ToolChanging.
const (
	StepIdle           = "Idle"
	StepSnapshotting   = "Snapshotting"
	StepSnapshotFailed = "SnapshotFailed"
	StepStarting       = "Starting"
	StepReregistering  = "Reregistering"
	StepCompleted      = "Completed"
)

// ToolChange is the computed state of a tool change.
type ToolChange struct {
	// Open is true while the change runs.
	Open bool
	// Step is the reason of the ToolChanging condition.
	Step string
	// SnapshotTaken is "" when there is no snapshot, else "True" or "False".
	SnapshotTaken string
	// ToolReady is true when the current tool's pod serves and no change is open.
	ToolReady bool
	// Closed is the id carried in the ToolChanging message once the change is over, so a closed change is not
	// reopened by its (still present) snapshot.
	Closed string
}

// ComputeToolChange decides the tool change's step from the latest pre_tool_change snapshot, the pod and the
// connected workers' links. The snapshot is the change's marker: it is requested in the same transaction that
// switches the tool, and the change is over when the new pod serves and no connected worker waits to be
// registered. The snapshot id recorded at that moment keeps the change closed afterwards.
func ComputeToolChange(s TeamState) ToolChange {
	serving := s.PodReady && !s.PodStale
	c := ToolChange{Step: StepIdle, ToolReady: serving}
	if s.SnapshotID == "" {
		return c
	}
	if s.SnapshotID == s.PrevClosedSnapshot {
		c.SnapshotTaken, c.Closed = snapshotTaken(s.SnapshotState), s.SnapshotID
		return c
	}
	c.Open, c.ToolReady = true, false
	c.SnapshotTaken = snapshotTaken(s.SnapshotState)
	switch s.SnapshotState {
	case "requested", "pending":
		c.Step = StepSnapshotting
	case "failed":
		c.Step = StepSnapshotFailed
	default: // completed, or deleted after its retention
		switch {
		case !serving:
			c.Step = StepStarting
		case s.PendingLinks > 0:
			c.Step = StepReregistering
		default:
			c.Open, c.Step, c.ToolReady, c.Closed = false, StepCompleted, true, s.SnapshotID
		}
	}
	return c
}

func snapshotTaken(state string) string {
	switch state {
	case "completed", "deleted":
		return "True"
	}
	return "False"
}
