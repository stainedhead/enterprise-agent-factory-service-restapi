# Tasks

Status: [x] done, [ ] open (recorded as a known gap in implementation-notes.md).

- [x] T1 Pin `-db` (000022) and regenerate `api/openapi.yaml` v1.4.0 (194 operations)
- [x] T2 Domain: exposure, tool profile, models, settings, reasons
- [x] T3 CreateTeam creates the orchestration agent and boot secrets; limit; model access
- [x] T4 Reserved role and name, delete guard, fixed fields, 23514 mapping by constraint
- [x] T5 GetTeamOrchestration, PutTeamOrchestration, ChangeCollaborationTool, ListOrchestrationEndpoints
- [x] T6 Deprecated 1.0.1 operations mapped, with Deprecation and Sunset headers
- [x] T7 Team, agent and pool models; ListModels callable and priceState; PutServiceModelAccess
- [x] T8 Settings orchestration.* and models.*; 1.0.1 keys read-only aliases
- [x] T9 Agent template capabilities; ListAgentTemplates hides orchestration images
- [x] T10 Swarm template spec.orchestration (spec.collaboration deprecated)
- [x] T11 Export and import carry the orchestration agent and exposures
- [x] T12 `orchestration-backfill` (use case, repository, subcommand, Helm hook)
- [x] T13 Reconciler: render by tool, environment, models delivery
- [x] T14 Reconciler: profiles with grace, `ModelUnavailable`, daily Bedrock sync
- [x] T15 Gateway: admission, reasons, cache, 503 model_provisioning, metric
- [x] T16 Edge: private and public hostnames, wrong-level 404, exposure on records
- [x] T17 Terraform `orchestration-private`; Helm values and hook
- [x] T18 Release 1.1.0 files; documentation
- [ ] T19 Buzz keypair generation job (P2; Buzz is disabled by default)
- [ ] T20 Tool-change state machine conditions beyond `ToolChanging` and `ToolReady` (P2)
