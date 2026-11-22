// Package collab is the reconciler's side of Paperclip team collaboration (RC-21 to RC-30): the status
// of a team's collaboration endpoint and its events (RC-24), the link state of connected workers
// (RC-22), the denied-spike alert and the housekeeping of the edge's tables (RC-27), the roster guard
// and credential scan of Paperclip content (RC-26) and the bootstrap of the first admin (RC-29).
// It computes from rows and calls ports; the postgres and Kubernetes adapters implement them. Nothing here
// holds a secret value: only names, ARNs and counts.
package collab
