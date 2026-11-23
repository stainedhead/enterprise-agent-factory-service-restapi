# Research notes

- `-db` 000022 is one-way in production (down is for local use). It takes `ACCESS EXCLUSIVE` locks for the time of
  its backfills (agents, teams, `team_collaboration`: hundreds of rows, triggers disabled during the rewrite), so a
  short write pause on those tables is expected; schedule the upgrade in a quiet window.
- `create_orchestration_agent` is the only way to create an agent with role `orchestration`; `afs_app` has no INSERT
  for that role and no INSERT on `team_collaboration`. It is service-admin only, so the backfill runs as a service
  admin session with actor `system:migration`.
- `model_admission(kind, id)` returns the first failing level; it is not `SECURITY DEFINER`, so the gateway role
  reads it under its own grants.
- The Bedrock SDK in use lists inference types ON_DEMAND and PROVISIONED only; a model is callable when it is
  ON_DEMAND. Models reached only through a system inference profile are added by an admin.
- ALB host-header conditions treat `*` as any run of characters, so `*.<domain>` covers `api.<team>.<domain>`.
- A private-only name cannot be validated by public ACM; the private module takes a certificate ARN.
