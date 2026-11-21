# Research: `-restapi` feature complete

| | |
|---|---|
| **Feature** | restapi-feature-complete |
| **Date** | 2026-11-21 |
| **Source PRD** | [enterprise-agent-factory-service-restapi-PRD.md](enterprise-agent-factory-service-restapi-PRD.md) |

## 1. Research questions

Seeded from the PRD's open questions (§9) and dependencies (§7). Each has the milestone that needs the
answer and the default the tasks build to.

| # | Question | Needed by | Default built to | Task |
|---|---|---|---|---|
| RQ-1 (OQ-1) | Which session tags does EKS Pod Identity set in the target EKS version, and do they survive role chaining? | M0 | IAM conditions on `aws:PrincipalTag/kubernetes-namespace`; AFS ids as role tags (D26); `role_session_name` = agent or pool id for links (D51) | M0-E1 |
| RQ-2 (OQ-2) | AWS tag limit after the EBS CSI driver's tags; Bedrock `requestMetadata` pair limit | M0 | 40 effective keys; 16 pairs in D27 order | M0-E2 |
| RQ-3 (OQ-7) | Agentic Teammates CRD group/version and status endpoint | M2 | Pin the version current at M2 behind `internal/adapter/k8s/crd/agentictm`; activity falls back to model calls and CPU | M2-F1 |
| RQ-4 (OQ-5) | GitHub App per organization, rate budget | M6 | One App per organization (GHES: one per host), conditional requests, 5,000 req/h per installation | M6-E3 |
| RQ-5 (OQ-6) | Admission engine | M1 | ValidatingAdmissionPolicy | M1-H1 |
| RQ-6 (OQ-9) | IdP federated role for viewer kubectl | M5 | No access entry without a configured role | M5-E2 |
| RQ-7 (OQ-8) | Egress proxy for HTTP counts | M4 | Hubble connections and bytes only | M2-H3 |
| RQ-8 | How is `api/openapi.yaml` produced from the contract and kept equal to it? | M0 | §3.1 below | M0-A, M0-B |
| RQ-9 | How does `TestNotificationChannel` return a delivery result when `afs-api` cannot read channel credentials? | M3 | OQ-S1 | M3-G4 |

## 2. Industry standards and protocols

| Topic | Standard | Use |
|---|---|---|
| API description | OpenAPI 3.1 (JSON Schema 2020-12) | `api/openapi.yaml` |
| Tokens | OIDC Core, RFC 7517 (JWK), RFC 7519 (JWT), RFC 8628 (device grant, clients only) | RA-2, RA-2b |
| Kubernetes tokens | Bound ServiceAccount tokens (projected, audience-scoped); TokenReview API | RA-2b (offline JWKS), GW-1 (TokenReview) |
| Bedrock wire | Bedrock Runtime `Converse`, `ConverseStream`, `InvokeModel`, `InvokeModelWithResponseStream`; AWS event stream encoding (`application/vnd.amazon.eventstream`) | GW-1, GW-6 |
| Anthropic wire | Messages API, SSE (`message_start`, `content_block_*`, `message_delta.usage`, `message_stop`, `error`) | GW-1, GW-6 |
| Security | OWASP ASVS 4.0 L2 | NFR, M6-F |
| Rate limiting | Token bucket (`golang.org/x/time/rate`) | RA-15 |
| CSV | RFC 4180 | Reports, list endpoints marked *CSV* |

## 3. Decisions from research

### 3.1 OpenAPI production (RQ-8)

1. **Extract.** `tools/contractgen` parses the root API contract v1.1.0 markdown tables (§3 permissions,
   §4 operations, §6 errors, §7 events) and root PRD §6.13 settings into `api/contract/*.json`
   (committed, with `api/contract/SOURCE` holding the contract version and the SHA-256 of each source).
   It reads the root checkout from `AFS_ROOT_DIR` (default `../enterprise-agent-factory-service`). It also
   generates `internal/authz/catalog_gen.go` and `internal/adapter/httpapi/routes_gen.go`. `make contract`
   runs it; CI runs it when the root checkout is present and fails on a diff.
2. **Author.** The paths and schemas are written by hand as YAML fragments, one file per contract section,
   under `api/src/` (shapes from contract §5, errors from §6, conventions from §1). Each operation carries
   `operationId` and extensions `x-afs-permission`, `x-afs-scope` (`team` \| `service` \| `public` \| `any`),
   `x-afs-cli`, `x-afs-sdk`, `x-afs-screen`, `x-afs-priority`, `x-afs-csv`, `x-afs-events`. Every operation
   has a request and response example.
3. **Bundle.** `tools/openapibundle` merges the fragments into `api/openapi.yaml` (sorted, stable output).
   `make openapi` runs it; `make gate` fails if the committed bundle differs.
4. **Check.** `api/contract_test.go` (package `api_test`) asserts: the bundle validates as OpenAPI 3.1
   (`pb33f/libopenapi`); the set of operationIds, methods, paths, permissions and scopes equals
   `operations.json`; every documented error code is in §6; no schema has a replica count (RA-US-4.6);
   every operation has examples. The contract test (`test/contract`) replays every example against the
   server with `libopenapi-validator` (requests and responses), as operations get implemented.
5. **Serve.** `api/embed.go` embeds the bundle; `afs-api` serves it at `GET /v1/openapi.yaml` (public, not
   a contract operation, declared `authz.Public` in the route table).
6. **Where the contract is silent** (for example the `RestoreRevision` dry-run diff shape, the
   `TestNotificationChannel` response, the CSV column order) the OpenAPI document decides in M0 and the
   M0 exit task opens a root PR that updates the contract to match (contract header: "once an operation is
   in the YAML, the YAML wins").

### 3.2 Libraries

| # | Need | Choice | Why |
|---|---|---|---|
| R-1 | HTTP routing | `net/http` `ServeMux` (Go 1.22+ patterns) and our own middleware chain | ADR-002; route table is ours |
| R-2 | PostgreSQL | `jackc/pgx/v5` v5.13.0, `pgxpool`; `LISTEN` on a dedicated connection | Same as `-db`; `-db/migrate` uses it |
| R-3 | JWT/JWKS | `go-jose/go-jose/v4` plus our own JWKS cache (10 min, single-flight refetch on unknown `kid`) | Exact cache semantics are a requirement (RA-US-2.3) |
| R-4 | Kubernetes | `client-go`, `controller-runtime` client and envtest; server-side apply with field manager `afs-reconciler`; third-party CRDs (Cilium, Karpenter, ESO, Agentic Teammates, VolumeSnapshot) as `unstructured.Unstructured` built by typed render functions | Avoids heavy dependency trees (Cilium) and version lock-in |
| R-5 | Leader election | `client-go/tools/leaderelection` with a `Lease` | Standard; < 30 s failover with 15 s lease, 10 s renew |
| R-6 | AWS | AWS SDK for Go v2, one module per service; adapters behind ports; tests use an `httptest` server as the SDK endpoint (`awstest` recorder) or port fakes | No test touches real AWS |
| R-7 | OpenAPI validation | `pb33f/libopenapi`, `libopenapi-validator` (tests only) | OpenAPI 3.1 support |
| R-8 | Observability | OpenTelemetry Go SDK (OTLP exporter), `prometheus/client_golang`, `log/slog` JSON | NFR |
| R-9 | GitHub | Thin client over `net/http` (App JWT → installation token, `GET /repos/{o}/{r}/pulls/{n}` with `If-None-Match`) in `internal/adapter/github` | Few endpoints; ETag control |
| R-10 | Rate limiting | `golang.org/x/time/rate`, per-principal limiter map with idle eviction | RA-15 |
| R-11 | Gateway conformance | AWS SDK `bedrockruntime` and `anthropic-sdk-go` in `test/conformance` (build tag `conformance`) against the gateway with a fake Bedrock | Gateway contract §10 |
| R-12 | IDs | ULID (`oklog/ulid/v2`) with the contract prefixes | Database `afs.ulid_ok` check |
| R-13 | Event stream | `aws-sdk-go-v2/aws/protocol/eventstream` encoder/decoder | Bedrock streaming paths |

### 3.3 Spikes OQ-1 and OQ-2 (M0)

No AWS account is available to agents, so the spikes are desk research recorded in this file by M0-E,
plus an `[e2e]` probe (`test/e2e/spike_test.go`) that a person with the dev account runs: it creates a
Pod Identity association and prints the session tags seen by `sts:GetCallerIdentity` from a test pod and
from a chained role, and creates an EBS volume through the CSI driver and counts its tags. Until the probe
runs, the defaults in §1 are built to.

## 4. Existing implementations and API documentation

- `-db/migrate`: `Load`, `Up`, `Down`, `Version`, advisory lock, checksum guard. `afs-api` calls `Up`
  at startup on an `afs_owner` connection; the other binaries call `Version` and refuse to start when it
  is older than the version embedded at build time.
- `-db/bootstrap`: `RolesSQL` for test databases.
- `-db` functions used: see data-dictionary.md §4.
- Scaffold: `internal/authz` (replaced), `internal/adapter/httpapi.NewHandler` (health routes kept as
  `GetHealth`/`GetReadiness`), `cmd/*/main.go` (flag parsing and graceful shutdown kept).

## 5. Spec-level open questions

Found while writing and reviewing the spec. Each has a default the tasks build to; J decides.

| Id | Question | Default built to | Affects |
|---|---|---|---|
| OQ-S1 | `TestNotificationChannel` must "return the failure" (RA-US-17.3), but `afs-api` cannot read channel credentials or call EventBridge. | `afs-api` writes the test request (audit and an outbox event `notification.test_requested` with `data.channelId` and a test id) and waits up to 10 s for the reconciler's result on the channel's `resource_status` (condition `DeliveryHealthy` with `lastTestId`), returning `200 {delivered, error?}`, or `202 {pending: true, testId}` when no result arrives in time. The new event type is an additive contract change (root PR from M0 exit). | M3-G, contract §7 |
| OQ-S2 | Report schedules (BI-5) must "send CSV to the team's channel", but channels are EventBridge rules and API destinations, which cannot carry file attachments. | `afs-billing` writes the CSV to the reports bucket (KMS, prefix `reports/<team>/<schedule>/`), and emits `report.delivered` with `data.channelId` and a 7-day pre-signed `downloadUrl`; each channel's rule also matches `report.*` events whose `data.channelId` is that channel. A failure to render, store or publish records `failed` and retries next cycle; downstream channel failures show on the channel status (`notification.delivery_failed`). | M4-E, RC-15 |
| OQ-S3 | EventBridge connections need the credential value at `CreateConnection`; RC-15 says credentials live in a connection "backed by Secrets Manager". | The reconciler reads the channel's secret (only secrets referenced by `notification_channels.secret_id`, IAM-scoped to that path) in memory to create or update the connection and never logs or stores it. Slack webhook URLs are treated as credentials: the API destination uses the connection's API key or the webhook path held in the connection, never a URL stored in the database. | M3-G, IAM policy of the reconciler |
| OQ-S4 | Where the GitHub App private keys come from | Mounted into the reconciler from a Kubernetes Secret that ESO syncs from Secrets Manager (`deploy/helm` value `github.apps[]` with host, org, app id, secret name); never in the database | M6-E |
| OQ-S5 | `GetTeamKubeconfig` needs the cluster endpoint and CA, but `afs-api` never calls Kubernetes or EKS | Helm passes `AFS_CLUSTER_NAME`, `AFS_CLUSTER_ENDPOINT`, `AFS_CLUSTER_CA_DATA`, `AFS_AWS_REGION`; the kubeconfig uses `aws eks get-token` as its exec plugin | M5-E1 |
| OQ-S6 | PRD §8 places no milestone for RC-8 (outbox publisher, partitions, retention, org mirror, purge) | Outbox, partitions, retention and purge in M1 (every later milestone emits events); the org account mirror in M4 with role links | M1-J, M4-A |
| OQ-S7 | May `afs-api` run a read-only lookup with `afs.service_admin = 'on'` until `-db` provides the X-DB-1..3 functions (§6)? | Yes, as an interim only: one registered, named, read-only query per lookup (API key by hash, workload by namespace and ServiceAccount, team directory), actor `system:afs-api-lookup`, a test that fails on any other use; removed by M6-X0. If J says no, M1-A4 waits for X-DB-1 and M6-A3/M6-D2 wait for X-DB-2/3 | M0-F8 |
| OQ-S8 | Six schema gaps block parts of the PRD (§6) | Requested from `-db` at M0 exit; tasks that need them depend on the adoption tasks in tasks.md *External dependencies* | tasks.md |

## 6. Schema access gaps (found in the spec review)

Checked against the RLS policies and grants of migrations 000001–000014 (and the unmerged 000015 on
`-db` branch `feat/db-feature-complete`, which does not address them). Each is a `-db` request with the
task that adopts it in tasks.md *External dependencies*.

| Id | Gap | Evidence | Request |
|---|---|---|---|
| X-DB-1 | `afs-api` cannot find an API key by hash before it knows the caller's team | `api_keys` has only `app_team_scope` for `afs_app` (000002); service keys have `team_id` NULL, and `caller_sees_team(NULL)` is false for non-admins | SECURITY DEFINER `afs.authenticate_api_key(key_hash)` |
| X-DB-2 | `afs-api` cannot map a workload's namespace and ServiceAccount to its team, agent or pool before scope exists | `teams`, `agents`, `agent_pools` policies for `afs_app` are team-scoped | SECURITY DEFINER `afs.resolve_workload(namespace, service_account)` |
| X-DB-3 | Security operators see no team rows, so `ListTeamDirectory` is empty for them | `caller_sees_team` is service admin or `afs.team_ids` only (000002); no security-operator policy on `teams` | SECURITY DEFINER `afs.team_directory()` for service admins and security operators |
| X-DB-4 | The reconciler cannot create a remediation team (RC-12) | `afs_reconciler` has `SELECT` on teams, pools and budgets and only column updates (000002, 000012, 000013) | SECURITY DEFINER `afs.instantiate_work_order_team(work_order_id)` writing rows, audit, outbox and revisions |
| X-DB-5 | The reconciler cannot mark a pool `deleted` (RC-17) | 000002 grants `UPDATE (state, deleted_at)` on `teams` and `agents` only; 000012 grants pools `SELECT` | `GRANT UPDATE (state, deleted_at) ON afs.agent_pools TO afs_reconciler` |
| X-DB-6 | Denied-request audit on service routes and every security operator mutation cannot be written | `audit_log` and `outbox_events` have only `app_team_scope` WITH CHECK `caller_sees_team(team_id)` for `afs_app` (000002) | `FOR INSERT` policies for rows whose `actor = afs.current_actor()` (audit) and for security operator work order events (outbox) |
