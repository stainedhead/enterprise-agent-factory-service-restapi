# Intent

## Purpose
Be the control plane of the Enterprise Agent Factory Service: the one API every client uses, and the
services that turn stored desired state into isolated, budget-capped team environments on EKS.

## Where it fits
One of four children of [enterprise-agent-factory-service](https://github.com/stainedhead/enterprise-agent-factory-service),
which holds the PRD and the cross-repo design.

- Depends on `enterprise-agent-factory-service-db` for the schema and migrations.
- Serves `enterprise-agent-factory-service-cli` (`afs`) and `enterprise-agent-factory-service-website`
  (`@afs/sdk` and the console) through the contract in `api/openapi.yaml`.
- Drives the Agentic Teammates runtime inside each team namespace.

## What it delivers
| Binary | Job |
|---|---|
| `afs-api` | REST API. Authenticates with the enterprise IdP, enforces service admin / team-admin / team viewer on every operation, writes desired state with audit and outbox records in one transaction |
| `afs-reconciler` | Applies desired state to EKS and AWS at startup and continuously: namespaces, network policy, quotas, IAM and Pod Identity, inference profiles, secret delivery, agent workloads. Reverts drift. Publishes events |
| `afs-gateway` | The only path from agents to Bedrock. Reserves worst-case cost per call against the team's budget, invokes the team's inference profile, settles to actual cost, rejects at the hard cap |
| `afs-billing` | Daily reconciliation of gateway estimates against the AWS Cost and Usage Report, including EKS compute by namespace |

## Goals
- Role-based access enforced in one place, here, for every client.
- Spend control that acts per request, with an IAM deny as the backstop.
- An environment that recovers itself from the database and reverts manual changes.
- Lifecycle, budget and event hooks ready for ephemeral teams and CVE work orders.

## Non-goals
- Owning tables (that is `-db`), the CLI (`-cli`) or any UI (`-website`).
- Storing secret values. Only Secrets Manager references.
- The agent runtime images.
