# Architecture

```
afs-api  --desired state (tx: row + audit + outbox)-->  PostgreSQL (-db 000022)
                                                          |  change feed
              +-------------------------------------------+------------------+
              v                       v                                      v
        afs-reconciler           afs-gateway                              afs-edge
  render by tool, models,    model admission (60 s cache,           orchestration_routes():
  profiles, Bedrock sync     invalidated on the change feed)        private ALB / public ALB
```

- Dependency rule unchanged: `adapter`, `cmd` -> `usecase` -> `domain`. New domain types (`Exposure`, `CollabTool`,
  `ToolProfile`, `AgentModels`, `ModelSets`) hold the rules; use cases own the checks; adapters call the `-db`
  functions.
- The edge keeps one `collab.Route` per team and hostname level. A surface is served only on the hostname of its
  effective level; the other level answers `404` (CD-38).
- The gateway asks `Store.ModelAdmission` per request (cached), maps the reason to `details.reason`, records the
  denial metric, and answers `503 model_provisioning` while a team's inference profile is being created.
- The reconciler's model state (`modelstate.go`) computes `agent.Desired.Models`, the reason a model is unavailable
  and the previous condition, so `ModelUnavailable` and its event fire on change only.
- Deploy: the internal ALB (`modules/orchestration-private`) and the public ALB (`modules/collab-edge`) share the
  afs-edge pods through two `TargetGroupBinding`s.
