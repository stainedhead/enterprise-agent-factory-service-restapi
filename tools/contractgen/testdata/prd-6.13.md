### 6.13 Service settings

Every service setting, set by service admins on S10 (`PutSetting`) and stored in `afs.service_settings`.
The database refuses a value outside its range. This table and the database contract are the only lists
(D45); ranges for the 000013 keys are D55; the 000016 keys are seeded by `-db` for remediation teams.

| Key | Default | Allowed values | Used by | Migration |
|---|---|---|---|---|
| `secrets.path_prefix` | `"afs"` | `^[a-z][a-z0-9-]{0,30}$`; applies to new secrets only | SEC-5 | 000003 |
| `pricing.price_list_sync` | `true` | boolean | BUD-9 | 000010 |
| `billing.variance_alert_pct` | `5` | 0 to 100 | BUD-6 | 000010 |
| `api.idempotency_key_ttl_seconds` | `3600` | 60 to 604800 | IF-1 | 000011 |
| `teams.ephemeral_max_days` | `90` | 1 to 365 | TEAM-7 | 000012 |
| `work_orders.sla_warning_hours` | `48` | 1 to 720 | WO-4 | 000012 |
| `billing.tag_activation_acknowledged` | `false` | boolean; setting it to `true` is how S9's banner is acknowledged | TAG-7 | 000012 |
| `work_orders.notification_channel_id` | `null` | null, or the id (`nch_<ulid>`) of a service-level email channel | WO-4 | 000012 |
| `reports.idle_cpu_millicores` | `50` | 1 to 4000 | RPT-5 | 000013 |
| `egress.denied_spike_threshold` | `20` | 1 to 100000 denials per agent or pool in 5 minutes | RPT-6 | 000013 |
| `gateway.reservation_ttl_seconds` | `900` | 60 to 3600 | BUD-2 | 000013 |
| `api.cors_origins` | `[]` | array of `https://host[:port]` origins | IF-1, IF-4 | 000013 |
| `work_orders.cost_center` | unset | string of 1 to 100 characters; cost center of a remediation team whose swarm template names none | WO-1, RC-12 | 000016 |
| `work_orders.owner_email` | unset | string `local@domain`; owner of a remediation team whose swarm template names none | WO-1, RC-12 | 000016 |
