// Package housekeeping holds the reconciler's periodic jobs that need no cluster (RC-8): the outbox
// publisher, partition management and the idempotency-key purge. The jobs are plain functions
// scheduled by the engine's leader-only scheduler (M1-F4); this package defines the scheduler port
// it needs (Scheduler) and nothing else from the engine.
//
// Events are delivered at least once: a crash between PutEvents and marking the row published sends
// the event again, so consumers dedupe on detail.id. Events of one aggregate are delivered in id
// order; a failing event holds back the later events of its aggregate (not of other aggregates).
package housekeeping
