package workorder

import "time"

// AssignInterval is how often open work orders are given their team: a work order gets its team within 2
// minutes (RC-US-10.2) when the reconciler leads, with room for a failed pass.
const AssignInterval = 15 * time.Second

// IntakeInterval is how often the intake queue is drained.
const IntakeInterval = 15 * time.Second

// Jobs are the work order jobs; nil members are not registered.
type Jobs struct {
	Intake   *Intake
	Assigner *Assigner
	Tracker  *Tracker
	Notifier *Notifier
}

// Register adds the jobs to the scheduler.
func Register(s Scheduler, j Jobs) {
	if j.Intake != nil {
		s.Every("work-order-intake", IntakeInterval, j.Intake.Run)
	}
	if j.Assigner != nil {
		s.Every("work-order-assign", AssignInterval, j.Assigner.Run)
	}
	if j.Tracker != nil {
		s.Every("work-order-poll", PollInterval, j.Tracker.Poll)
		s.Every("work-order-settle", SettleInterval, j.Tracker.Settle)
	}
	if j.Notifier != nil {
		s.Every("work-order-notify", NotifyInterval, j.Notifier.Run)
	}
}
