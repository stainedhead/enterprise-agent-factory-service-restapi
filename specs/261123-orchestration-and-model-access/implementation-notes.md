# Implementation notes

## Deviations from the contract and PRD (the merged `-db` schema wins)

- **OM-D1..14 of `-db`** are adopted as written (see the `-db` spec). The ones that shape this repository: the legacy
  booleans follow the exposure columns and exposure wins on INSERT; `afs_app` cannot insert `team_collaboration` or an
  agent with role `orchestration`; `models.service_access = 'all'` passes disabled models; `team_models` is not
  required when the team's access is `all`.
- `ServiceModelAccess` carries counts as integers (`enabledCount`, `totalCount`), not strings.
- The deprecated `ListCollaborationEndpoints` folds the two surfaces of a team into one row (web UI level wins; the
  row reads `ready` only when both configured surfaces are ready).
- `toolVersion` is read from the `ToolReady` condition message ("version ...") because no column holds it.
- The `models_enabled` guard (a model cannot be disabled while a team's selection needs it) is enforced in the use
  case as well as by the database, so the error names the teams.
- `CreateTeam` writes more than one audit record (team, agent from `create_orchestration_agent`, boot secrets).
- Import creates the orchestration agent with `afs.create_orchestration_agent` (a bundle cannot INSERT the role) with
  the default model from the bundle; collaboration settings of the bundle update the exposures and are noted, not
  inserted, when the team has no row.
- Inference profiles follow `effective_agent_models` (one per callable model of the team), not `team_models` alone.
- A written template capability `paperclip` is accepted until 2.0.0 and stored as `orchestration` plus
  `tool:paperclip` (the database rewrites it).
- Notification of team-admins by the backfill is the `orchestration.created` event the creation emits; no separate
  message is sent.
- The orchestration agent is deleted only with its team: the database allows it once the team is `deleting`, and the
  team reconciler removes the agent with the namespace. `DeleteAgent` refuses it (`orchestration_agent_required`).
- Bedrock sync lists on-demand text models; the SDK in use has no inference-profile type to filter on.

## Tool change and Buzz registration (T19, T20)

- Tool change is derived, not stored. `collab.ComputeToolChange` reads the latest `pre_tool_change` snapshot, the
  agent's stale pod (`observed_generation < generation`) and the links still pending. Steps: snapshotting,
  snapshot failed, starting, re-registering, completed. The controller writes `ToolChanging`, `SnapshotTaken` and
  `ToolReady` on the collaboration status row; the endpoint is Starting while a change is open. When a change
  closes, its snapshot id is kept in the `ToolChanging` message so a closed change is not reopened; `ToolReady`
  carries the tool version.
- Buzz registration is `collab.BuzzRegistrar` (job `collab-buzz-register`, every minute, managed teams running
  Buzz only). Per connected worker it generates an Ed25519 keypair, records the public key (hex) as the link's
  remote id (`paperclip_agent_id`), writes the private key to Secrets Manager, records the key
  (`afs.record_collab_agent_key`) and adds the public key at the relay. A half-registered link is restarted,
  because the private key is not recoverable. Teardown removes the member (unless the orchestration agent is
  gone), drops the key and disables the link.
- Assumption (upstream open question OR-OQ-4): the relay's admin API is `PUT /admin/members/{publicKey}` and
  `DELETE /admin/members/{publicKey}` (404 is success) with the admin key as bearer. The key comes from the file
  named by `AFS_BUZZ_RELAY_ADMIN_KEY_FILE`; without it the job is off with a warning. Only the `adapter/buzz`
  client changes if the upstream surface differs.

## Gotchas

- Tests need `AFS_TEST_DATABASE_URL`; without it the integration tests skip silently.
- `make openapi` needs a generous timeout.
- A test that drives the gateway against the real database needs a live agent row, because admission reads the agent.
