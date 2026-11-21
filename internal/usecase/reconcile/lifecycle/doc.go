// Package lifecycle holds the ordered delete finalizer and the suspend and resume workflow of the
// reconciler (RC-17, team part). It computes nothing about Kubernetes or AWS: every effect goes
// through a port, so the package runs against fakes. The engine (M1-F) calls Finalizer.Reconcile
// for a resource in state `deleting` and SuspendResume for a team whose state changed; this
// package defines only the minimal ports it needs from the engine (ConditionStore, Completer).
//
// Agent (M2) and pool (M5) streams plug their delete steps and scale hooks in through Hooks and
// ScaleHook, so the team steps stay in one place and run after every workload step.
package lifecycle
