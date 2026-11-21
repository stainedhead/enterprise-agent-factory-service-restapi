# Data dictionary: `-restapi` feature complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Date** | 2026-11-21 |
| **Purpose** | The Go types, ports and enumerations this repository defines, and where each maps to the API contract (§5 wire shapes) and the `-db` schema. Wire shapes are not repeated here: API contract §5 is authoritative, then `api/openapi.yaml` once written. Table columns: `-db/docs/data-model.md` |

## 1. Identifiers and value objects (`internal/domain`)

| Type | File | Rule |
|---|---|---|
| `ID` and constructors per prefix (`TeamID`, `AgentID`, `PoolID`, `StageID`, `SecretID`, `BindingID`, `APIKeyID`, `UserID`, `IdPID`, `ExtSystemID`, `RoleLinkID`, `PoolRoleLinkID`, `AgentIdentityID`, `NamespaceConfigID`, `AgentConfigID`, `PoolConfigID`, `BudgetID`, `ModelPriceID`, `SnapshotID`, `WorkOrderID`, `SwarmTemplateID`, `ExportID`, `ReportScheduleID`, `LogDestinationID`, `ChannelID`, `EgressRuleID`, `ExtSystemOptoutID`, `TemplateID`) | `ids.go` | Prefixes of contract §1 (`team_`, `agt_`, …, `tpl_`); ULID body; `Parse<Kind>ID` refuses a wrong prefix |
| `Name` | `names.go` | `^[a-z][a-z0-9-]{1,38}[a-z0-9]$` for team slug, agent, pool and secret names; immutable |
| `ConfigName` | `names.go` | `UPPER_SNAKE`; credential-like names refused (mirrors the `-db` rule for a clear error) |
| `Money` | `money.go` | Decimal string, ≤ 6 fraction digits, USD; arithmetic on scaled `int64` micro-dollars or `math/big`; never `float64` |
| `ImageRef` | `image.go` | Must contain `@sha256:<64 hex>` |
| `Version`, `Generation` | `concurrency.go` | `ETag: "<version>"`; `If-Match` parsing |
| `Phase`, `Condition`, `Status` | `status.go` | Phases Pending, Reconciling, Ready, Degraded, Failed, Deleting; conditions of contract §1.1 (`NamespaceReady`, `IamReady`, `SecretsSynced`, `VolumeReady`, `EgressPolicyReady`, `TemplateRetired`, `DriftCorrected`, `BudgetExhausted`) plus reconciler step conditions while `Deleting` (`DeleteStep<Name>`) and `DeliveryHealthy` on channels |
| `TagSet`, `Tag`, `TagSource` | `tags.go` | Sources system, service, team, service_default, missing; `EffectiveTags` ≤ 40 keys (D27); `BedrockMetadata()` ≤ 16 pairs in D27 order |
| `Period` | `budget.go` | monthly (calendar month UTC) or lifetime; `PeriodStart(t)` |
| `ReportQuery` | `report.go` | `from`/`to` or `range` (`24h`, `7d`, `30d`, `90d`, `mtd`), `groupBy` (`team`, `agent`, `model`, `system`, `day`, `hour`, `tag:<key>`, `pool`), filters; picks daily views for ranges over 2 days |
| `RepoRef` | `workorder.go` | `host/owner/repo`, case-insensitive compare |
| `RequestID` | `request.go` | ≤ 64 chars when client-sent; generated ULID otherwise |

## 2. Entities (`internal/domain`)

One file per aggregate. Each entity has a constructor that enforces its invariants and returns
`domain.ValidationError{Fields: …}`; use cases never build entities field by field.

| Entity | File | Key invariants (beyond the database's) | Table(s) |
|---|---|---|---|
| `Team` | `team.go` | `kind` immutable; ephemeral needs `expiresAt` ≤ now + `teams.ephemeral_max_days`, `teardownRule`, lifetime budget; guardrail fields listed in `Team.GuardrailFields()`; `EffectiveCeiling(now)`; `state` transitions active↔suspended, →deleting→deleted | `teams`, `team_models` |
| `Member` (role binding) | `member.go` | Team roles only on team bindings; subject user or group | `role_bindings` |
| `APIKey` | `apikey.go` | `afsk_` prefix, 32 random bytes, SHA-256 stored; expiry ≤ 90 days; service keys only `security_operator` | `api_keys` |
| `Agent` | `agent.go` | Image digest; volume grow-only, storage class create-only; no replicas; secret bindings env or file | `agents`, `agent_secret_bindings` |
| `Secret` | `secret.go` | Scope team/agent/pool; no value field; `Value` is a separate type `SecretValue` with `String()` = `[redacted]` and no JSON marshalling | `secrets` |
| `ConfigValue` | `config.go` | source literal/namespace | `namespace_config_values`, `agent_config_values`, `pool_config_values` |
| `ExternalSystem`, `ExternalSystemState` | `extsys.go` | Team-declared needs owner and purpose | `external_systems`, `external_system_optouts` |
| `Budget` | `budget.go` | amount ≤ effective ceiling; `notify` only persistent and only `teams:set-guardrails`; lifetime not removable | `budgets` |
| `RoleLink`, `PoolRoleLink`, `AgentIdentity` | `identity.go` | Dedicated identity for agent links; account allowed; pool one role name | `agent_role_links`, `pool_role_links`, `agent_identities` |
| `Stage`, `Pool`, `Handoff` | `pool.go` | Ephemeral teams only (checked before write → `409`); workers 0–200; scratch 1–500 GiB | `swarm_stages`, `agent_pools`, `pool_handoffs`, `pool_secret_bindings` |
| `Model`, `ModelPrice`, `PriceProposal` | `model.go` | `maxOutputTokens` required on create; effective time `max(effectiveFrom, approvedAt)` | `models`, `model_prices`, `model_price_proposals` |
| `StorageClass`, `AgentTemplate` | `catalog.go` | Template digests well formed; delete in use → `Conflict` | `storage_classes`, `agent_templates` |
| `IdentityProvider` | `idp.go` | One default, enabled; disabling one in use → `Conflict` | `identity_providers` |
| `ServiceTag`, `TeamTags` | `tags.go` | Fixed vs required; team PUT sends team-owned keys only | `service_tags`, `team_tags` |
| `LogDestination`, `NotificationChannel` | `delivery.go` | Credentials only as `secretId`; inline credential → `ValidationError` | `log_destinations`, `notification_channels` |
| `ReportSchedule` | `report.go` | Team's own channel only | `report_schedules` |
| `WorkOrder`, `WorkOrderRepo`, `SwarmTemplate` | `workorder.go` | One open per CVE; `pr_opened` needs PR URL; owner explicit or resolved (D38) | `work_orders`, `work_order_repos`, `swarm_templates` |
| `Export`, `ImportReport`, `Bundle` | `export.go` | Bundle has `schemaVersion`, `sha256`; ≤ 100 MB; no secret value, key hash or client secret | `export_bundles` |
| `Revision`, `RestorePlan` | `revision.go` | Restore diff per resource; skipped bindings reported | `config_revisions` |
| `Setting` | `settings.go` | Typed registry of root PRD §6.13 keys with type, default, range (generated from `api/contract/settings.json`) | `service_settings` |
| `UsageEvent`, `Reservation` | `usage.go` | Outcome ok/error/rejected_budget/rejected_model | `usage_events`, `spend_reservations` |
| `ActivitySample`, `TrafficFlow` | `telemetry.go` | One sample per agent per 5-minute window | `agent_activity`, `external_traffic` |
| `CostDaily`, `CostReconciliation`, `CostTagStatus` | `cost.go` | Variance vs `billing.variance_alert_pct` | `cost_daily`, `cost_reconciliations`, `cost_allocation_tag_status` |
| `Event` | `events.go` | Type constants generated from `api/contract/events.json` (contract §7); `detail` = `{id, occurredAt, teamId, agentId?, poolId?, actor, data}` | `outbox_events` |
| `AuditRecord` | `audit.go` | Actor type user/api_key/system/agent/pool; outcome `allowed`/`denied`; before/after without secret values | `audit_log` |

## 3. Authorization types (`internal/authz`)

| Type | Meaning |
|---|---|
| `Permission` | Constants generated from contract §3 (`catalog_gen.go`); `AllPermissions()` in contract order |
| `Role` | `team_viewer`, `team_admin`, `service_admin`, `security_operator`; plus the workload grant |
| `PrincipalType` | `user`, `api_key`, `workload` |
| `Principal` | `{ID, Type, Email, Name, Bindings[], ServiceAdmin, SecurityOperator, Workload *{TeamID, AgentID or PoolID, WorkerPod}}` |
| `Resource` | `{Scope: team\|service\|public\|any, TeamID, TeamFlags{MayManageAwsAccounts, ViewerKubectl, MayAddExternalSystems}, WorkOrder *{AssignedTeamID, RepoOwnerTeamIDs[]}}` |
| `Decision` | `Allow`, `NotFound` (unbound on a team route), `Deny{RequiredPermission}`; the middleware maps them to 2xx/404/403 per PRD §3 |
| `Authorize(p, perm, res) Decision` | The only decision function |
| `Effective(p, teamID, flags) []Permission` | For `GetMe` (D42) |
| `CanGrant(p, role, teamID) bool` | Team-admins grant team roles on own team; only service admins grant `service_admin`/`security_operator` |

## 4. Ports (`internal/usecase/port` and per-area packages)

Cross-cutting ports are defined once in M0 (`internal/usecase/port`); area ports live in the area's use case
package. Every port has an in-memory fake in `internal/usecase/port/fakes` (cross-cutting) or
`<area>/<area>test` (area), written with the port.

| Port | Package | Methods (summary) | Adapters |
|---|---|---|---|
| `TxRunner` | port | `InTx(ctx, Session, func(Tx) error)` with session settings, retry of `40001`/`40P01` ×3; interim `Lookup(ctx, name, args)` for the registered read-only lookups (M0-F8, removed by M6-X0) | postgres |
| `Session` | port | actor, serviceAdmin, teamIDs, securityOperator | — |
| `AuditWriter`, `OutboxWriter` | port | `Write(ctx, Tx, AuditRecord)`, `Publish(ctx, Tx, Event)`; denied audit in its own transaction | postgres |
| `IdempotencyStore` | port | `Begin`, `Complete`, `Lookup` per database contract §7.1 | postgres |
| `Clock`, `IDGen`, `Random` | port | Injected time and ids | stdlib |
| `SettingsReader` | port | Typed read with cache (30 s) | postgres |
| `<Area>Repository` | each area | CRUD with version checks; returns domain errors | postgres |
| `SecretsWriter` | secrets | `Create(name, value, tags)`, `PutValue`, `Rotate`, `ScheduleDelete(arn, 30d)` | aws/secretsmanager (the only AWS client in `afs-api`) |
| `TokenVerifier` | auth | `VerifyIdP(token)`, `VerifyWorkload(token)` | oidc |
| `PrincipalResolver` | auth | bindings for user/groups/key/workload | postgres |
| `RepoOwnerResolver` | workorders | `afs.resolve_repo_owners(host, repo)` | postgres |
| `ChangeFeed` | reconcile/engine | `Listen(ctx) <-chan Change`, `PollStale`, `ListAll` | postgres |
| `StatusWriter` | reconcile/engine | `resource_status` upsert, events `*.status_changed` | postgres |
| `Cluster` sub-ports | reconcile/* | `TeamApplier`, `AgentApplier`, `PoolApplier`, `AdmissionApplier`, `LogPipelineApplier`, `SnapshotManager`, `Scaler`, `DriftWatcher`, `MetricsReader`, `FlowReader` (Hubble), `PodDirectory` | k8s |
| `IAM` | reconcile | roles, policies, boundary, deny statements, tags | aws/iam |
| `PodIdentity` | reconcile | associations | aws/eks |
| `AccessEntries` | reconcile | EKS access entries | aws/eks |
| `InferenceProfiles` | reconcile | create, tag, delete application inference profiles | aws/bedrock |
| `AssumeRoleProbe` | reconcile/links | `Verify(roleArn, externalID, sessionName)` | aws/sts |
| `Organizations` | reconcile/housekeeping | list accounts | aws/organizations |
| `EventBus` | reconcile/housekeeping | `PutEvents` batch | aws/eventbridge |
| `NotificationInfra` | reconcile/notify | rules, targets, API destinations, connections | aws/eventbridge |
| `Queues` | reconcile/pool, workorder, notify | SQS create, policy, receive, delete | aws/sqs |
| `Mailer` | reconcile/notify | SES send | aws/ses |
| `BundleStore` | reconcile/export, billing | S3 put (KMS), pre-sign | aws/s3 |
| `SecretsReader` | reconcile/notify | Channel credentials only (OQ-S3); `SecretsDeleter` for RC-17 | aws/secretsmanager |
| `GitHub` | reconcile/workorder | PR state with ETag | github |
| `PodIdentityCache` | gateway | IP → pod → team/agent/pool | k8s/podcache |
| `TokenReviewer` | gateway | review with 5 min cache | k8s/tokenreview |
| `Bedrock` | gateway | Converse, ConverseStream, InvokeModel, InvokeModelWithResponseStream on a profile with an assumed role | aws/bedrock |
| `Spend` | gateway | `EnsureCounter`, `Reserve`, `Settle`, `RecordThreshold`, `WriteUsage` (batched) | postgres |
| `CUR`, `PriceList`, `CostAllocationTags` | billing | Athena queries, Pricing API, Cost Explorer tags | aws/athena, aws/pricing, aws/costexplorer |

## 5. Enumerations (closed lists from the contracts)

Error codes (contract §6 and gateway §7): `ValidationError`, `Unauthenticated`, `AccessDenied`, `NotFound`,
`BudgetExceeded`, `Conflict`, `PreconditionFailed`, `PreconditionRequired`, `Throttled`, `InternalError`,
`UpstreamError`, `Unavailable`. Conflict reasons: `idempotency_mismatch`, `idempotency_in_progress`,
`team_suspended`, plus dependents lists. Other enums: as contract §5 (team kind/state/tier, teardown rule,
role, delivery, iam identity, volume onDelete and schedule, role link status, pool state, work order
status/slaState/severity/kind, repo status, ownerSource, report kind/cadence/format, tag source, budget
period and onExhaust, IdP kind, export status, price proposal status, cost tag status). Generated or
hand-written once in `internal/domain/enums.go` (M0-S1) with a test against the OpenAPI enums.

## 6. API request/response types

Wire DTOs live in `internal/adapter/httpapi/dto_<area>.go`, never in `domain`, with explicit mapping
functions to and from domain types. A test per area round-trips each DTO against the OpenAPI schema
examples (`libopenapi-validator`). Secret values exist only in the create/put/rotate request DTOs, as
`domain.SecretValue`.
