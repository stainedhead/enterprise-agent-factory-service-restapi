# Spec: team orchestration agent and model access (`-restapi` 1.1.0)

Source: root repository `team-orchestration-agent-PRD.md` (ORC-1 to ORC-32, MOD-1 to MOD-8, OR-D1 to OR-D17, section 8
migration), `docs/contracts/api-contract.md` v1.4.0 (section 4.4d), `gateway-contract.md`, `database-contract.md`, and
`-db` migration 000022 (`-db` specs `261123-orchestration-and-model-access`). Where the merged `-db` schema and the
contract differ, the schema wins and the difference is recorded in `implementation-notes.md`.

## Goals

1. Every team has exactly one orchestration agent from `CreateTeam` on (role `orchestration`, name `orchestrator`),
   running the chosen collaboration tool (Paperclip, or Buzz where enabled).
2. The tool's web UI and web API are offered at a level per surface: `off`, `private` (internal ALB) or `public`,
   within the team's limit `maxCollaborationExposure`.
3. Model access is a four-level intersection (service, team, agent and pool) plus a price, enforced by the gateway and
   delivered to the agent (`AFS_DEFAULT_MODEL`, `AFS_MODELS`, `/etc/afs/models.json`).
4. Existing 1.0.1 teams get their agent through `afs-api orchestration-backfill`.
5. The 1.0.1 surface keeps working until 2.0.0, with `Deprecation` and `Sunset` headers.

## Requirements by component

| Component | Requirement | PRD |
|---|---|---|
| `api/openapi.yaml` | v1.4.0: five operations, changed bodies, six events, reasons | ORC-25..27, MOD-7 |
| `afs-api` | CreateTeam creates the agent and boot secrets in one transaction; `role_reserved`, `name_reserved`, delete guard, `orchestration_field_fixed`; deferred 23514 mapped by constraint name; exposure checks against the limit and the tool profile; model access and `Agent.models`/`Pool.models`; backfill subcommand; permissions of PRD section 4 | ORC-1..5, 25..27, MOD-1..7 |
| `afs-reconciler` | render by tool; tool-neutral environment; tool change; model delivery; daily Bedrock sync; inference profile per callable model with 7-day grace; `ModelUnavailable` | ORC-8..12, MOD-4..6 |
| `afs-gateway` | model admission and its four reasons; fail closed; 60 s cache refreshed on the change feed; `503 model_provisioning`; denial metric | MOD-5, gateway contract |
| `afs-edge` | private and public hostnames from `orchestration_routes()`; wrong-level `404`; `exposure` on access records | ORC-13..15, CD-38 |
| Deploy | Terraform `orchestration-private` module; Helm private target group, deprecation window, backfill hook | ORC-13, RA-US-21 |

## Non-goals

Buzz keypair generation and the per-step conditions of the tool-change state machine beyond `ToolChanging` and
`ToolReady` are P2 (see implementation notes). No schema change here (`-db` owns it). No console or CLI change here.

## Acceptance

`make gate` and `make test-db` green; `make cover` at 90% for domain, usecase and authz; every new route has allowed
and denied tests per role; the contract replay test covers the new operations; the Terraform and Helm tests cover the
new module and hook.
