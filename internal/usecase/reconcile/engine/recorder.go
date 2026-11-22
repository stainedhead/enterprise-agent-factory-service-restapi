package engine

import "time"

// Recorder receives the reconcile metrics. cmd/afs-reconciler binds it to the Prometheus collectors;
// the engine never imports them (RD-4).
type Recorder interface {
	// ReconcileDone is called once per finished pass with its duration and error (nil on success).
	ReconcileDone(kind Kind, d time.Duration, err error)
	// ReconcileLag is called when a pass observed a desired-state change: d is the time from the
	// change being queued to the pass that applied it.
	ReconcileLag(kind Kind, d time.Duration)
	// DriftCorrected is called once for every object a pass reverted after a drift notice.
	DriftCorrected(kind Kind)
}
