-- Profil batch ticket reservation target a atau b, dijalankan melalui psql.
-- Dijalankan setelah 001_receipts.sql, 002_batch_checkpoints.sql, dan 003_batch_scopes.sql.
\getenv target_slot TARGET_SLOT
SELECT :'target_slot' IN ('a', 'b') AS target_slot_ok \gset
\if :target_slot_ok
\else
\echo 'TARGET_SLOT must be a or b'
\quit 3
\endif

SELECT EXISTS (SELECT FROM pg_catalog.pg_namespace WHERE nspname = 'wbd_auth')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'target_marker')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_tickets')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_checkpoints')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_scopes')
       AND NOT EXISTS (SELECT FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'wbd_auth' AND p.proname = 'reserve_batch_ticket') AS profile_004_ready \gset
\if :profile_004_ready
\else
\echo 'Prerequisite profile 001/002/003 missing or reserve_batch_ticket already exists'
SELECT 1 / 0;
\endif

CREATE FUNCTION wbd_auth.reserve_batch_ticket(
  p_execution_id uuid,
  p_physical_target_id uuid,
  p_target_xid xid8,
  p_batch_index integer,
  p_direction text,
  p_capability bytea
)
RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
DECLARE
  marker wbd_auth.target_marker%ROWTYPE;
  v_existing wbd_auth.batch_tickets%ROWTYPE;
  v_old_status text;
  v_new_status text;
BEGIN
  SELECT * INTO STRICT marker FROM wbd_auth.target_marker WHERE singleton;
  IF session_user <> pg_catalog.replace(marker.executor_role::text, '_executor', '_preparer') THEN
    RAISE EXCEPTION 'reservation authorization rejected: must be preparer' USING ERRCODE = '42501';
  END IF;

  IF p_execution_id IS NULL OR p_physical_target_id IS NULL OR p_target_xid IS NULL
     OR p_batch_index IS NULL OR p_batch_index < 0
     OR p_direction IS NULL OR p_direction NOT IN ('up', 'down')
     OR p_capability IS NULL OR pg_catalog.octet_length(p_capability) <> 32 THEN
    RAISE EXCEPTION 'invalid reservation parameters' USING ERRCODE = '22023';
  END IF;

  IF p_physical_target_id <> marker.physical_target_id THEN
    RAISE EXCEPTION 'target mismatch' USING ERRCODE = '42501';
  END IF;

  -- Refuse checkpointed old work
  IF EXISTS (
    SELECT 1 FROM wbd_auth.batch_checkpoints
    WHERE execution_id = p_execution_id AND direction = p_direction AND batch_index = p_batch_index
  ) THEN
    RAISE EXCEPTION 'cannot reserve ticket: batch % already checkpointed', p_batch_index USING ERRCODE = '55000';
  END IF;

  -- Require a valid new transaction
  v_new_status := pg_catalog.pg_xact_status(p_target_xid);
  IF v_new_status IS NULL OR v_new_status <> 'in progress' THEN
    RAISE EXCEPTION 'invalid new transaction: target xid % status is %',
      p_target_xid, coalesce(v_new_status, 'unknown') USING ERRCODE = '25000';
  END IF;

  -- Row lock existing ticket
  SELECT * INTO v_existing
  FROM wbd_auth.batch_tickets
  WHERE execution_id = p_execution_id AND direction = p_direction AND batch_index = p_batch_index
  FOR UPDATE;

  IF FOUND THEN
    IF v_existing.physical_target_id <> p_physical_target_id THEN
      RAISE EXCEPTION 'target mismatch on existing ticket' USING ERRCODE = '42501';
    END IF;

    -- Refuse committed, in-flight, or unknown; permit replacement only for positively aborted prior XID
    v_old_status := pg_catalog.pg_xact_status(v_existing.target_xid);
    IF v_old_status IS NULL OR v_old_status <> 'aborted' THEN
      RAISE EXCEPTION 'cannot replace ticket: prior transaction % status is % (must be aborted)',
        v_existing.target_xid, coalesce(v_old_status, 'unavailable') USING ERRCODE = '55000';
    END IF;

    UPDATE wbd_auth.batch_tickets
    SET target_xid = p_target_xid,
        physical_target_id = p_physical_target_id,
        capability_sha256 = pg_catalog.sha256(p_capability)
    WHERE execution_id = p_execution_id AND direction = p_direction AND batch_index = p_batch_index;
  ELSE
    BEGIN
      INSERT INTO wbd_auth.batch_tickets
        (execution_id, physical_target_id, target_xid, batch_index, direction, capability_sha256)
      VALUES
        (p_execution_id, p_physical_target_id, p_target_xid, p_batch_index, p_direction, pg_catalog.sha256(p_capability));
    EXCEPTION WHEN unique_violation THEN
      SELECT * INTO STRICT v_existing
      FROM wbd_auth.batch_tickets
      WHERE execution_id = p_execution_id AND direction = p_direction AND batch_index = p_batch_index
      FOR UPDATE;

      IF v_existing.physical_target_id <> p_physical_target_id THEN
        RAISE EXCEPTION 'target mismatch on existing ticket' USING ERRCODE = '42501';
      END IF;

      v_old_status := pg_catalog.pg_xact_status(v_existing.target_xid);
      IF v_old_status IS NULL OR v_old_status <> 'aborted' THEN
        RAISE EXCEPTION 'cannot replace ticket: prior transaction % status is % (must be aborted)',
          v_existing.target_xid, coalesce(v_old_status, 'unavailable') USING ERRCODE = '55000';
      END IF;

      UPDATE wbd_auth.batch_tickets
      SET target_xid = p_target_xid,
          physical_target_id = p_physical_target_id,
          capability_sha256 = pg_catalog.sha256(p_capability)
      WHERE execution_id = p_execution_id AND direction = p_direction AND batch_index = p_batch_index;
    END;
  END IF;
END
$function$;

SELECT pg_catalog.format('ALTER FUNCTION wbd_auth.reserve_batch_ticket(uuid, uuid, xid8, integer, text, bytea) OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON FUNCTION wbd_auth.reserve_batch_ticket(uuid, uuid, xid8, integer, text, bytea) FROM PUBLIC;
SELECT pg_catalog.format('GRANT EXECUTE ON FUNCTION wbd_auth.reserve_batch_ticket(uuid, uuid, xid8, integer, text, bytea) TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
SELECT pg_catalog.format('REVOKE ALL ON TABLE wbd_auth.batch_tickets FROM %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
