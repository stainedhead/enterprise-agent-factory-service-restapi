# Product summary

The control plane of the Agent Factory Service. See the root repository's `docs/product-summary.md` for the
product; this file covers what this repository delivers.

Four binaries and one contract:

- **`afs-api`** is the REST API every client (the `afs` CLI, the web console, `@afs/sdk`, automation and running
  agents) calls. It enforces role-based access (the only place access is decided), writes desired state to
  PostgreSQL with an audit entry and an outbox event in the same transaction, and serves the 182 operations of
  `api/openapi.yaml`: teams and members, agents and agent pools, secrets, budgets, reports, costs and prices,
  notification channels and log destinations, external systems, identity and AWS account links, export, import and
  restore, swarm templates and work orders.
- **`afs-reconciler`** reads that desired state and applies it: a namespace, policies and IAM roles per team,
  Bedrock inference profiles, agent and pool workloads, egress policies, queues and tags; it reverts drift,
  deletes teams in order, and runs the housekeeping jobs (outbox publisher, partitions, report rollups, orphan
  secret sweep, budget periods, work order intake and tracking).
- **`afs-gateway`** is the only path from agents to Bedrock. It identifies the calling pod, reserves spend before
  each call, refuses with `402` when a budget is spent and fails closed when it cannot reserve.
- **`afs-billing`** runs as CronJobs: CUR ingest and reconciliation against the metered spend, price sync, scheduled
  reports and monthly showback.
- **`api/openapi.yaml`** is the contract that `@afs/sdk` and the `afs` CLI are generated from.

What is not here: the schema (the `-db` repository), the CLI and the console. Checks that need a development
cluster or an AWS account (end-to-end, restore drill, full-scale load) run on the cluster.
