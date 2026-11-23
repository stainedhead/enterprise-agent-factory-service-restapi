# Plan

Order of work (each step: red test, green change, refactor, status update).

1. Pin `-db` at the 000022 commit; regenerate `api/openapi.yaml` from `api/src` (tools/contractgen keeps it in step
   with the root contract). 194 operations.
2. Domain: exposure, tool and profile, `AgentModels`/`ModelSets`, `OrchestrationInput` rules, settings keys.
3. Use cases: teams (create, limit, models, backfill), agents (reserved role and name, delete guard, fixed fields,
   models), collab (orchestration read/write, tool change), pools (models), models (callable, price state, service
   access), settings (orchestration.*, models.*), swarm and agent templates.
4. Adapters: postgres repositories on the 000022 functions and views; httpapi handlers and the permission matrix;
   deprecation headers.
5. Reconciler: render by tool, model delivery, profiles, Bedrock sync, conditions and events.
6. Gateway: admission, cache invalidation, metric.
7. Edge: levels and routes.
8. Deploy: Terraform module, Helm values and hook.
9. Release: Makefile, chart, CHANGELOG, AGENTS.md; docs; user-docs upgrade notes.
10. Gate, test-db, PR, CI, merge.
