package postgres

// ProposedRecordWorkOrderSecretSQL is the -db change M6-E needs to give remediation teams the secrets their swarm
// template declares (RC-12 known gap; database contract "Still open after 000016").
//
// afs.instantiate_work_order_team does not create template secrets: a secrets row needs the ARN Secrets Manager
// assigns, and afs_reconciler may not insert secrets or pool_secret_bindings. WorkOrderStore therefore creates each
// declared secret empty in Secrets Manager (the value is never copied; the team puts it with PutSecretValue) and
// records it through this function, which writes only what the template declared for a live work order's own
// team: one managed secret scoped to one of the team's pools, its binding, the audit record and secret.created.
// The function is idempotent on (team, name).
//
// Until -db ships it (as migration 000019 or later, GRANT to afs_reconciler), WorkOrderStore.SecretsSupported
// reports false, nothing is created in Secrets Manager, and the reconciler logs one warning. The integration test
// installs this SQL in its own database to prove the store against it.
const ProposedRecordWorkOrderSecretSQL = `
CREATE FUNCTION afs.record_work_order_secret(
    p_work_order_id text, p_pool_id text, p_name text, p_description text, p_arn text, p_delivery text, p_target text)
  RETURNS text
  LANGUAGE plpgsql VOLATILE SECURITY DEFINER
  SET search_path = pg_catalog, afs
AS $$
DECLARE
  v_team   text;
  v_status text;
  v_secret text;
BEGIN
  SELECT w.team_id, w.status INTO v_team, v_status FROM afs.work_orders w WHERE w.id = p_work_order_id FOR SHARE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'no work order %', p_work_order_id USING ERRCODE = 'no_data_found';
  END IF;
  IF v_team IS NULL OR v_status NOT IN ('assigned', 'in_progress', 'pr_opened') THEN
    RAISE EXCEPTION 'work order % has no live remediation team', p_work_order_id
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM afs.agent_pools p WHERE p.id = p_pool_id AND p.team_id = v_team AND p.state IN ('enabled', 'paused')) THEN
    RAISE EXCEPTION 'pool % is not a live pool of team %', p_pool_id, v_team USING ERRCODE = 'foreign_key_violation';
  END IF;
  SELECT s.id INTO v_secret FROM afs.secrets s WHERE s.team_id = v_team AND s.name = p_name;
  IF FOUND THEN
    RETURN v_secret;
  END IF;
  v_secret := afs.new_id('sec');
  INSERT INTO afs.secrets (id, team_id, name, description, secrets_manager_arn, origin, pool_id)
  VALUES (v_secret, v_team, p_name, coalesce(p_description, ''), p_arn, 'managed', p_pool_id);
  INSERT INTO afs.pool_secret_bindings (team_id, pool_id, secret_id, delivery, target)
  VALUES (v_team, p_pool_id, v_secret, p_delivery, p_target);
  INSERT INTO afs.audit_log (actor_type, actor_id, action, outcome, resource_kind, resource_id, team_id, pool_id, after)
  VALUES ('system', afs.current_actor(), 'RecordWorkOrderSecret', 'allowed', 'secrets', v_secret, v_team, p_pool_id,
          jsonb_build_object('workOrderId', p_work_order_id, 'name', p_name, 'delivery', p_delivery, 'target', p_target));
  INSERT INTO afs.outbox_events (event_type, aggregate_kind, aggregate_id, team_id, payload)
  VALUES ('secret.created', 'secrets', v_secret, v_team,
          jsonb_build_object('id', 'evt_' || substr(v_secret, 5), 'occurredAt', to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
                             'teamId', v_team, 'actor', afs.current_actor(),
                             'data', jsonb_build_object('secretId', v_secret, 'name', p_name, 'scope', 'pool')));
  RETURN v_secret;
END $$;
REVOKE ALL ON FUNCTION afs.record_work_order_secret(text, text, text, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION afs.record_work_order_secret(text, text, text, text, text, text, text) TO afs_reconciler;
`
