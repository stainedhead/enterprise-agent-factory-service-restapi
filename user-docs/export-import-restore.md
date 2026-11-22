# Export, import and restore

These calls are for operators. Export and import need the service admin role. Restore needs team admin or service admin.

## Export

`POST /v1/exports` with `{"teamIds": ["team_..."]}` returns `202` and an export in status `pending`. The reconciler builds the bundle, stores it in the export bucket encrypted with KMS, and moves the export to `succeeded` with a `downloadUrl` that is valid for 7 days. A failed build is `failed` with a summary. Poll with `GET /v1/exports/{exportId}`; list with `GET /v1/exports`.

A bundle never holds a secret value, an API key hash or a client secret. Secrets appear as metadata only.

## Import

`POST /v1/imports` takes `{"bundle": {...}, "accountMap": {"111122223333": "444455556666"}}`. By default it is a dry run (`dryRun=true`) and writes nothing; the report lists each team as created, updated or skipped, and the secrets that still need a value. Add `?dryRun=false` to apply. Each team is written in its own transaction, so a team that fails is reported and leaves nothing behind while the other teams apply. A bundle whose checksum does not match, or that is larger than 100 MB, is refused with `400`. Role links come in as `pending_trust`.

## Restore

`POST /v1/teams/{teamId}/revisions/{rev}:restore` with `{"dryRun": true}` shows what would change. With `{"dryRun": false}` the team goes back to its state at that revision in one transaction: settings, agents, config values, tags and budget. The team's `generation` rises by one and one audit record names the revision. Send `If-Match` to refuse the restore when the team changed since you read it (`412`). Resources the database refuses to restore, such as a model that is no longer enabled, are skipped and listed in the response. Secret bindings are not restored.
