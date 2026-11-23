# Upgrading to 1.1.0

1.1.0 gives every team an orchestration agent and adds model access. It needs `-db` migration 000022. Read this page
before upgrading a service that has teams.

## Before you start

- **The database upgrade is one-way.** Migration 000022 has a `.down.sql` for local use only; in production, restore
  from a backup to go back. Take a snapshot first.
- **Lock window.** 000022 takes `ACCESS EXCLUSIVE` locks on `agents`, `teams`, `team_collaboration` and
  `agent_templates` for the time of its backfills (hundreds of rows, with triggers disabled during the rewrite). Writes
  to those tables wait for that time; reads of other tables are not affected. Schedule the upgrade in a quiet window and
  expect seconds, not minutes.
- **Set up the runtime.** In `orchestration.tools` give Paperclip an enabled agent template (capabilities
  `orchestration` and `tool:paperclip`, a digest) and an image repository. Without it `CreateTeam` and the backfill
  answer `orchestration_runtime_unset`. A template that still has the capability `paperclip` is rewritten to
  `orchestration` plus `tool:paperclip`.
- **Set a default model.** `models.default_model_id` (or a model on the orchestration agent) is required: the
  orchestration agent must have a default model (`no_model_available`).

## What the upgrade does

1. The Helm chart (1.1.0) starts new `afs-api` pods, which run the migration under the advisory lock.
2. A post-install and post-upgrade hook runs `afs-api orchestration-backfill`: it creates the orchestration agent of
   each team that has none, in batches of 20, as actor `system:migration`, and sets `orchestration_required`.
   It is idempotent and resumable: a team that already has an agent is skipped, so after a failure rerun it
   (run `helm upgrade` again, or `afs-api orchestration-backfill` with the API environment). Exit code 1 means some team could not be done; the log
   names it. Team-admins are told by the `orchestration.created` event.
3. The legacy collaboration agent of a 1.0.1 team (role `collaboration`) becomes the orchestration agent; its
   exposure levels are `public` where the 1.0.1 booleans were true and `off` otherwise. The team's limit
   `maxCollaborationExposure` is `public` where `mayExposeCollaboration` was true.
4. The reconciler restarts pods only where the rendered desired state changed (the tool-neutral variables and the
   models delivery are new, so orchestration pods roll once).

## Deprecations (until 2.0.0)

These keep working and answer with `Deprecation` and `Sunset` headers (`AFS_DEPRECATION_SINCE`,
`AFS_DEPRECATION_SUNSET`): `GetTeamCollaboration`, `PutTeamCollaboration`, `ListCollaborationEndpoints`,
`Team.mayExposeCollaboration`, `Agent.modelId`, `Pool.modelId`, `SwarmTemplate.spec.collaboration`, the settings
`collab.*` that moved to `orchestration.*`, and the events `collaboration.updated` and
`collaboration.endpoint_state_changed`. `CreateAgent` can no longer create a collaboration agent and `DeleteAgent`
refuses the orchestration agent (1.0.1 has no production users, so this is a minor change).

## New deployment pieces (optional)

- **Private entry.** Terraform module `orchestration-private` (internal ALB, private zone, WAF) and the chart value
  `edge.privateTargetGroupArn`. Set `orchestration.private_base_domain` and `orchestration.private_endpoints`.
  A surface exposed at one level answers `404` on the hostname of the other.
- **Bedrock model sync.** The reconciler lists the region's text models daily; new ones arrive disabled. Enable and price
  them, then allow them to teams. `AFS_MODEL_SYNC=off` turns it off.

## Checks after the upgrade

- `GET /v1/teams/{teamId}` shows `orchestrationAgentId` for every team.
- `afs-gateway` metric `afs_gateway_model_denials_total` stays flat for models that were callable before; a rise by
  reason tells which level refuses.
- `ListOrchestrationEndpoints` shows each team's levels and readiness.
