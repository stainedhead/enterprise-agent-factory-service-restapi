# Data dictionary (as used by `-restapi`; the schema is `-db`'s `docs/data-model.md`)

| Name | Meaning |
|---|---|
| `agents.role = 'orchestration'` | The team's orchestration agent; one live per team; created by `afs.create_orchestration_agent` |
| `team_collaboration.tool` | `paperclip` or `buzz` |
| `team_collaboration.web_ui_exposure`, `web_api_exposure` | `off`, `private`, `public`; the legacy booleans follow (public is true) until 2.0.0 |
| `teams.max_collaboration_exposure` | Limit of the two levels; `teams.may_expose_collaboration` is the deprecated boolean |
| `teams.orchestration_required` | True for teams that must have a live orchestration agent at commit |
| `teams.model_access`, `agents.model_access`, `agent_pools.model_access` | `all` or `selected` |
| `team_models`, `agent_models`, `pool_models` | The selected sets |
| `agents.default_model_id` | Default model (`model_id` is its deprecated alias) |
| `team_inference_profiles` | One row per team and callable model: ARN, state `creating`, `active`, `retiring`, `retire_after` |
| `models.origin`, `models.synced_at` | `admin` or `bedrock_sync`; last seen in the Bedrock catalogue |
| `afs.orchestration_routes()` | Two rows per team (web_ui, web_api): effective level, hostnames, readiness |
| `afs.orchestration_settings()` | `orchestration.public_base_domain`, `private_base_domain`, `public_endpoints`, `private_endpoints` |
| `afs.model_admission(kind, id)` | First failing level or null |
| `afs.effective_agent_models` | The models an agent may call: agent set within team set within service set, priced |
