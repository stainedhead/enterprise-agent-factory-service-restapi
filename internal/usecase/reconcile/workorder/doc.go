// Package workorder is the work order reconciler (M6-E, RC-12). It runs four leader-only jobs:
//
//   - Intake drains the EventBridge intake queue and creates one work order per source event id.
//   - Assigner creates each open work order's remediation team from its swarm template (the -db function
//     afs.instantiate_work_order_team) and provisions the secrets the template declares, empty, in Secrets
//     Manager (a value is never copied).
//   - Tracker reads the state of recorded pull requests from GitHub, moves repositories to merged, and moves
//     the work order to in_progress, pr_opened, done or failed. A done work order makes its team due for teardown
//     (afs.teams_due_for_teardown, M5-D).
//   - Notifier tells the owners once per event and recipient: AFS teams through their channels (RC-15: an outbox
//     event carrying the owner team's id), outside contacts by email through the service channel named in
//     work_orders.notification_channel_id.
//
// Everything is derived from stored state on each pass, so a missed or repeated pass repairs itself, and every
// write is idempotent. The package imports the domain only; the database, SQS, GitHub, Secrets Manager and SES
// sit behind the ports in this file.
package workorder
