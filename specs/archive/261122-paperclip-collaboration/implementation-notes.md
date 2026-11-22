# Implementation notes

## Decisions
- Edge serves HTTPS :8443 with a cert-manager certificate; health `/readyz` on ops port 9090.
- Sealing keys come from a K8s Secret; Terraform creates only an empty Secrets Manager container (no value in state). 30-day rotation is an operator procedure.
- Collaboration client secrets live under `<prefix>/idp/*` (or `collab_client_secret_name_pattern`).
- X-Forwarded-For is set from the peer address; compressed bodies refused; WebSocket frame policing in-house.
- Fluent Bit routing shipped as a ConfigMap rule; ALB subnet health-check rule in the NetworkPolicy.
- EndpointReady derives from pod readiness plus route checks; unlink detection is in memory.
- PAPERCLIP_HOME=/data/paperclip; KMS statement only when AFS_KMS_KEY_ARN is set; board key policy allows PutSecretValue only; connected-worker ingress uses a pod label.
- Admission policy allows Services with no selector.

## C5 decisions (parked in-scope items)
- Paperclip API surface is not specified by the PRD: the client and the `papertest` fake define it (bearer board key; paths in docs/technical-details.md). Errors carry operation and status only, never a body.
- Registration order: create the Paperclip agent, record its id, write the key to Secrets Manager, then `afs.record_collab_agent_key`. A failure after create revokes the Paperclip agent and the link is `failed` with the id cleared. Teardown skips the Paperclip revoke when the collaboration agent is gone (Paperclip died with the pod).
- Keys, the board key and invite URLs are `collab.Key` (redacting); a log-capture test proves none is printed.
- Guard: first scan looks back two intervals; items are deduped by team, id and update time; events carry pattern, count, item id and author, never text.
- Member sync wants role-binding users plus edge sign-ins of the last 30 days; admin-role invites are never touched. The 30-day window is a choice, not in the PRD.
- `collaboration` is a report kind outside the schedulable `ReportKind` enum; it always reads 5-minute rows of `afs.collaboration_access`.
- Export carries `collaboration` (team settings and the collaboration agent's name) and each worker's `connect`; secrets and bindings with `managed_by <> 'user'` are left out; import creates the collaboration agent from the template of the same name and the link starts pending (or disabled).
- `CollaborationKeyMissing`: a `PAPERCLIP_AGENT_API_KEY` binding whose secret has no version is not delivered (render, ExternalSecret and hash), and the agent controller reports the condition for connected workers. Creating the key changes the hash and rolls the agent.
- `TestStreamFailsPartWay` flake: the SDK client raced with the fake closing the connection after the exception frame; the test now reads frames from a raw HTTP response and the SDK test tolerates the closed connection. Fixed with `-count=50 -race`.

## Security review fixes
Independent review of `afs-edge`; each fix has a test written for the attack. Decisions RD-24 to RD-27.

| # | Fix | Where | Test |
|---|---|---|---|
| 1 | `crossOrigin`: cookie traffic with a non-safe method, upgrade or event stream needs `Origin == https://<host>`, else `Sec-Fetch-Site: same-origin` when no Origin and no upgrade; 403 `cross_origin`; sign-out too; API header traffic exempt | usecase/edge `proxy`, `handle` | `TestCrossOriginRefused` (team A to team B POST and WebSocket), `TestCrossOriginExemptions` |
| 2 | `Service.Recheck` (no cache, no limit, no count); `edgehttp.watch` every 60 s (injectable `NewTicker`); WebSocket closed with 1008, event stream body closed; also closed when the right to send or the scan mode changed | usecase/edge, edgehttp `ws.go`, `server.go` | `TestRecheck`, `TestWebSocketReauthorized`, `TestEventStreamReauthorized` |
| 3 | `Sec-WebSocket-Extensions` stripped and any extension in the answer refused; RSV bits, reserved opcodes and unmasked client frames close 1002; binary messages scanned when valid UTF-8; fragments scanned on reassembly; JSON escapes decoded in messages | `ws.go`, `collab.ScanMessage` | `TestRelayScanBypasses`, `TestWebSocketExtensionsStripped` |
| 4 | `collab.ScanBody` decodes JSON escapes, form `+`/`%XX` (two passes), multipart parts by Content-Transfer-Encoding, UTF-8 BOM, UTF-16 by BOM or charset; unknown charset or broken multipart is unreadable (415); `ClassifyBody` | `domain/collab/scan_decode.go` | `TestScanBodyBypasses` (one row per bypass), `TestBodyKinds`, `TestUnsupportedMediaType`, `TestBodyScanDecoding` |
| 5 | The tunnel reads the upstream response, requires 101 with `Sec-WebSocket-Accept` for the client's key, Upgrade/Connection tokens and no extension; a non-101 answer is relayed as a plain response (cookies scrubbed) and the connection closes; a bad client key is 400 | `ws.go` `tunnel`, `validHandshake` | `TestWebSocketHandshakeValidation`, `TestWebSocketRejectsBadClientKey` |
| 6 | Bearer audience = provider client id or collab client id (either, non-empty); the code-flow ID token stays bound to the collab client; the REST API surface needs no collab client | `oidc/codeflow.go`, `Route.ProviderClientID` | `TestCodeFlowVerifyBearerAudience` |
| 7 | `lru` of `CacheSize` (10000) for people, sessions, keys and unknown-source windows; TTL stays at most 60 s, negatives 10 s; 20 unknown-credential lookups per source per minute, then 429 without a store call | `cache.go` | `TestCachesStayBounded`, `TestUnknownCredentialsRateLimited` |
| 8 | `/_afs/signin` sets `__Host-afs-signin` (Secure, HttpOnly, Lax, host-only, 600 s) with a random value; its hash is in the sealed state and then in the sealed handoff envelope (`code` is the sealed envelope holding the DB code); handoff requires the cookie, then deletes it | `signin.go`, `edgehttp` | `TestLoginCSRFBinding`, `TestSigninCookieRedirects` |
| 9 | `safeReturn`: one leading `/` then not `/` or `\\`, no control character or backslash raw or percent-decoded once, in path and query | `signin.go` | `TestSafeReturnControlCharacters` (`/%09/evil.com`) |
| 10 | `scrubSetCookies`: drop `Domain`, drop `__Host-afs*` cookies, on proxied responses, SSE and relayed refusals | `server.go` | `TestSetCookieAndOverridesThroughProxy` |
| 11 | `X-HTTP-Method-Override`, `X-HTTP-Method`, `X-Method-Override` stripped; `_method` query refused for callers without contribute, and for any caller on a safe method | `server.go` `sanitize`, usecase `methodOverride` | `TestWebSocketExtensionsStripped`, `TestMethodOverrideQuery` |
| 12 | RD-24 below; groups hash in the people cache key | `service.go` | `TestGroupOnlySessionReauthenticates`, `TestBearerCacheKeyIncludesGroups` |
| 13 | `NormalizeHost` once in `Handle`; used by routing, sign-in, callback and the origin check | `cache.go` | `TestHostNormalization` |
| 14 | RD-26 below | `service.go` | `TestSigninRedirectNotCounted` |

- **RD-24 Session groups.** A session whose bindings all come from IdP groups (`afs.collab_principal.subject_type = 'group'`, carried as `Binding.Group`) is treated as ended 60 minutes after it was created (`Session.CreatedAt`, else expiry minus `collab.session_ttl_minutes`). Navigation is redirected to `/_afs/signin?silent=1`, which asks the provider with `prompt=none`; on `login_required`, `interaction_required`, `consent_required` or `account_selection_required` the callback sends the browser to an interactive sign-in. A new session replaces the cookie, with fresh groups. Sessions with any person-level binding are unaffected (bindings re-read every 60 s). Non-navigation requests get `401 session_expired`.
- **RD-25 Unreadable bodies.** In `block` mode a non-safe request with a body whose media type the scan cannot read is `415 unsupported_media_type` (outcome `denied_bad_request`). Allowed without a scan: `image/*`, `audio/*`, `video/*`, `application/pdf`, because Paperclip attachments are those; inside multipart, parts of any other type are scanned as raw bytes. `application/octet-stream` and archives are refused at top level (an easy place to hide a credential); revisit if Paperclip needs them. `warn` and `off` never refuse.
- **RD-26 Sign-in redirects.** `afs.collaboration_access.outcome` has no value for them and `denied_unauthenticated` feeds the denied-spike alert, which people opening a link must not raise. They are not counted.
- **RD-27 Provider client id.** `afs.collab_routes()` returns only the collaboration client id; `afs_edge` cannot read `identity_providers.client_id`. The edge and `Route.ProviderClientID` accept it, but it stays empty until `-db` returns it (new column `provider_client_id` in `collab_routes()`): `-db` follow-up, then set it in `edgedb.Routes`. Until then a CLI SSO token for a team without that column is refused.

## Root contract follow-ups
- -db: default-partition blocking; reconciler ensures partitions before edge writes.
- Switch go.mod to -db v1.0.1 when J pushes that tag; keep the pseudo-version meanwhile.
- Root docs should link https://github.com/stainedhead/software-factory-manager and https://github.com/stainedhead/agentic-teams (root repo not edited here).
- Board key CreateSecret question; DP-2/DP-3 HTTPS 8443 followed; PRD state key in Secrets Manager with rotation implemented as container only; /readyz on 9090.

- `-db`: `collab_routes()` to return the provider's `client_id` (RD-27).
- J: register `collab.base_domain` on the Public Suffix List or use a dedicated registrable domain per environment (see `user-docs/collaboration-edge.md`).

## Parked
| Item | Reason |
|---|---|
| Paperclip API paths against a live Paperclip | real Paperclip only; the surface is defined by the fake |
| Managed registration, guard, probe, scan, first admin and member sync against a real cluster | real AWS/EKS only (Secrets Manager, SES, pods) |
| Sealing key rotation automation | operator procedure for now |
| Real AWS/EKS validation | needs cluster |

## Release
Version 1.0.1 in lockstep with -db; J must push tag v1.0.1.
