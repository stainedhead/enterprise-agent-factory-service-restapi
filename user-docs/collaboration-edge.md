# Paperclip collaboration: the edge

`afs-edge` is the only internet-reachable component of AFS. It sits behind an ALB, at
`https://<team-slug>.<collab.base_domain>`, and is the single public way into a team's Paperclip. It signs people in,
decides what they may do, counts access, scans what they send for credentials, and forwards to the team's
`paperclip` Service. It owns no tables: it connects as the `afs_edge` database role, which can run ten database
functions and nothing else.

The API surface that manages collaboration (`GetTeamCollaboration`, `PutTeamCollaboration`, and the rest) is in the
[usage examples](usage-examples.md); this page is for operators.

## What gets deployed

| Piece | Where | Notes |
|---|---|---|
| ALB, certificate, DNS, WAF | `deploy/terraform/modules/collab-edge` | Created only when `collab_base_domain` is set |
| `afs-edge` Deployment, Service, PDB, NetworkPolicy, ServiceMonitor | `deploy/helm/afs` (`values/edge.yaml`) | 3 replicas across zones, `minAvailable: 2`, read-only root filesystem |
| `TargetGroupBinding` | the chart, when `edge.targetGroupArn` is set | Registers the pods in the Terraform target group |
| Log routing | ConfigMap `afs-edge-log-routing` | Rule for the node log collector: lines with `afs.team_id` go to that team's aggregator |
| Pod Identity role `afs-edge` | `deploy/terraform/modules/pod-identity` | Reads the collaboration sign-in client secrets, nothing else |

### Terraform

```hcl
collab_base_domain             = "collab.example.com"
collab_route53_zone_id         = "Z0123456789ABCDEFGHIJ"
collab_public_subnet_ids       = ["subnet-0aaaaaaaaaaaaaaaa", "subnet-0bbbbbbbbbbbbbbbb"]
collab_edge_security_group_ids = ["sg-0123456789abcdef0"]   # the afs-edge pods
```

After `apply`: allow the output `collab_alb_security_group_id` to reach the pods' security group on 8443 and 9090 (the
health check), put `collab_target_group_arn` into `edge.targetGroupArn`, and give the reconciler
`AFS_COLLAB_WAF_IPSET_V4_ARN` and `AFS_COLLAB_WAF_IPSET_V6_ARN` from `collab_waf_ip_set_v4_arn` and `collab_waf_ip_set_v6_arn`.
Set the service setting `collab.base_domain` to the same domain.

The collaboration sign-in client secrets must live under `<secrets_path_prefix>/idp/` (or the pattern you set in
`collab_client_secret_name_pattern`): the edge role can read only that.

### The sealing keys

The sign-in state (PKCE verifier, nonce, team, return path) travels through the identity provider sealed with
AES-256-GCM. Two keys, 32 bytes each, base64, live in the Kubernetes Secret named by `edge.stateKey.secretName`
(`current` and `previous`) and are identical on every replica. Terraform creates the empty Secrets Manager secret
`<name>/edge/state-key` as the place to keep them; it never holds a value, so none reaches Terraform state.

Rotate every 30 days: copy `current` to `previous`, write a new `current`, restart the Deployment. Sign-ins in flight
at the restart still complete because `previous` still opens their state.

### Helm values

```yaml
edge:
  targetGroupArn: arn:aws:elasticloadbalancing:...:targetgroup/...
  tls:
    issuerRef: {name: afs-ca}          # cert-manager issuer for the ALB's HTTPS target group
  network:
    albCidrs: ["10.1.0.0/20"]          # the ALB's subnets: who may reach 8443 and the 9090 health check
    monitoringNamespace: monitoring
```

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `AFS_EDGE_ADDR` | `:8443` | HTTPS listener (ALB target) |
| `AFS_EDGE_TLS_CERT`, `AFS_EDGE_TLS_KEY` | required | Server certificate, reloaded when cert-manager renews it |
| `AFS_EDGE_STATE_KEY`, `AFS_EDGE_STATE_KEY_PREVIOUS` | required, optional | Sealing keys (from the Secret) |
| `AFS_DATABASE_URL`, `AFS_DB_IAM_AUTH` | required | The `afs_edge` login |
| `AFS_EDGE_IDLE_TIMEOUT` | `10m` | A WebSocket or event stream with no traffic is closed |
| `AFS_EDGE_REFRESH_INTERVAL` | `30s` | Route table reload; it also reloads on every database notification |
| `AFS_EDGE_FLUSH_INTERVAL` | `30s` | Five-minute access counts to the database; flushed again on shutdown |
| `AFS_EDGE_DRAIN_TIMEOUT` | `30s` | Time requests get to finish at shutdown |
| `AFS_METRICS_ADDR` | `:9090` | `/metrics`, `/healthz`, `/readyz` |

## Service settings

The edge reads these through `afs.collab_settings()`; change them with `PutServiceSetting`.

| Setting | Default | Effect |
|---|---|---|
| `collab.base_domain` | unset | Apex of the hostnames. Unset: no team has a route |
| `collab.public_endpoints` | `true` | Off: every team host answers `404` |
| `collab.allowed_source_cidrs` | `[]` | Synced by the reconciler into the WAF IP sets within 60 s; empty means everywhere. An IPv4-only list closes IPv6 |
| `collab.session_ttl_minutes` | `480` | Lifetime of a session |
| `collab.content_scan` | `block` | `block`: a body with a credential is refused with `422`. `warn`: forwarded and counted. `off`: not scanned |
| `collab.max_body_mb` | `25` | Larger bodies are `400 body_too_large` |
| `collab.denied_spike_threshold` | `50` | Denied requests in five minutes before `collaboration.denied_spike` |

## How a request is decided

* A request with the cookie `__Host-afs-collab` is web UI traffic; one with `X-Afs-Authorization: Bearer <token>`
  is REST API traffic; both is `400 ambiguous_credentials`. With neither, browser navigation is redirected to sign in
  and anything else is API traffic.
* `X-Afs-Authorization` takes an access token from the team's sign-in provider or a team API key (`afsk_...`).
  The caller's own Paperclip credential stays in `Authorization`.
* Safe methods need `collaboration:access`; every other method and every client WebSocket frame needs
  `collaboration:contribute`; API traffic needs `collaboration:api` too. A caller who holds only access may open a
  WebSocket or event stream to watch; a client frame closes it with code 1008.
* Decisions are cached for 60 seconds (10 seconds when negative), so turning a surface off, revoking a key or removing
  a role takes effect within a minute. Ending the web UI refuses that team's sessions at once.
* Limits: 20 requests per second per caller, burst 40 (`429` with `Retry-After`).

Reserved paths on a team host, never forwarded: `GET /_afs/signin`, `GET /_afs/handoff`, `POST /_afs/signout`. On the
apex only `GET /oauth2/callback` is served (the one redirect URI to register with the provider); every other apex path
is `404`.

## Troubleshooting

| Symptom | Look at |
|---|---|
| `503 starting` | The collaboration agent is not ready: `GetTeamCollaboration` shows the `EndpointReady` condition |
| `503 upstream_unavailable` | The edge cannot reach the team's `paperclip` Service on 3100: the NetworkPolicy, the pod |
| `503` on every host, `Retry-After: 30` | The route table is older than 5 minutes: `afs_edge_route_refresh_failures_total`, database reachability |
| `404` on a team host | `collab.base_domain`, `collab.public_endpoints`, `mayExposeCollaboration`, both switches off, no sign-in client on the provider |
| `401 token_invalid` on sign-in | The collaboration client secret is not readable (path, KMS) or the redirect URI is not registered |
| `403 surface_disabled` | The caller holds a role, but the surface they used (web UI or REST API) is off for the team |
| `422 SecretDetected` | The body matched a credential pattern; store the value as an AFS secret instead |

Every sign-in, sign-out and denied request is one JSON line on stdout with `afs.team_id`, the principal, the method, the
first path segment, the status and the reason. No body, header value, cookie, token or query string is ever written.

Metrics: `afs_edge_requests_total{surface,code}`, `afs_edge_request_duration_seconds`,
`afs_edge_route_refresh_failures_total`, `afs_edge_access_counts_dropped_total`; alerts `AFSEdgeErrorRatioHigh`,
`AFSEdgeRouteRefreshFailing` and `AFSEdgeAccessCountsDropped` ship with the chart.

## Known limits

* A `permission_required` refusal for a WebSocket is decided per frame, not at the handshake, so a viewer can watch.
* Text WebSocket messages over 1 MiB close the connection with 1009; binary frames are not scanned.
* The Paperclip image must honour `AFS_PAPERCLIP_BOOTSTRAP` (`manual` or `managed`); until it does, first-admin setup is
  manual. In managed mode the reconciler registers connected workers in Paperclip (agent create, key to Secrets Manager,
  pause, revoke), guards the roster, probes readiness (including open sign-up) and sends the first-admin invite by email.
  The Paperclip API paths it calls are listed in `docs/technical-details.md`; live Paperclip has not been exercised.
