# User docs

How to deploy, configure and call the Agent Factory API.

Start here:

| Document | For |
|---|---|
| [Getting started](getting-started.md) | Everyone: build, run `afs-api` locally, run the checks |
| [Configuration reference](configuration.md) | Operators: every environment variable per binary, service settings |
| [Usage examples](usage-examples.md) | Callers: create a team, an agent, a secret, a budget; read a report |

Guides:

| Document | For |
|---|---|
| [Authentication, limits and CORS](authentication-and-limits.md) | Callers and operators |
| [Deployment](deployment.md) | Operators |
| [Observability](observability.md) | Operators |
| [Running afs-reconciler](reconciler.md) | Operators: what it applies, tag sync, configuration, probes |
| [Reports and usage](reports.md) | Callers: report kinds, grouping, CSV, activity samples |
| [Costs, reconciliation and prices](billing.md) | Callers and operators: cost reads, billing jobs, price proposals |
| [Work orders and swarm templates](work-orders.md) | Callers and operators: swarm templates, work orders, security operators, service keys, worker tokens |
| [Paperclip collaboration: the edge](collaboration-edge.md) | Operators: deploying `afs-edge`, the ALB and WAF, settings, sealing keys, troubleshooting |
| [Model routes and the team gateway](model-routes.md) | Callers and operators: routes, team gateways, agent variables, health, provider switches (1.2.0) |
| [Governed MCP servers](mcp-servers.md) | Callers and operators: the catalog, review and approval, team switches, agent access, hosted servers, the `/mcp` endpoints (1.3.0) |
| [Upgrading to 1.3.0](upgrading-to-1.3.0.md) | Operators: migrations 000024–000026, new reconciler, gateway and API settings, nothing changes until a server is approved and switched on |
| [Upgrading to 1.2.0](upgrading-to-1.2.0.md) | Operators: migration 000023, new reconciler and billing settings, nothing changes for 1.1.0 agents |
| [Upgrading to 1.1.0](upgrading-to-1.1.0.md) | Operators: the one-way database upgrade, the backfill, deprecations, new settings |
| [Export, import and restore](export-import-restore.md) | Operators: bundles, dry-run imports with an account map, restoring a team revision |

## The rest of the product

These guides cover this repository only. For the service as a whole, start with the
[service overview](https://github.com/stainedhead/enterprise-agent-factory-service#readme) and the
[adoption guide](https://github.com/stainedhead/enterprise-agent-factory-service/blob/main/user-docs/adoption-guide.md).
