-- Profil batch scope target a atau b, dijalankan melalui psql.
-- Dijalankan setelah 001_receipts.sql dan 002_batch_checkpoints.sql.
\getenv target_slot TARGET_SLOT
SELECT :'target_slot' IN ('a', 'b') AS target_slot_ok \gset
\if :target_slot_ok
\else
\echo 'TARGET_SLOT must be a or b'
\quit 3
\endif

SELECT EXISTS (SELECT FROM pg_catalog.pg_namespace WHERE nspname = 'wbd_auth')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'target_marker')
       AND EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_checkpoints')
       AND NOT EXISTS (SELECT FROM pg_catalog.pg_tables WHERE schemaname = 'wbd_auth' AND tablename = 'batch_scopes') AS profile_003_ready \gset
\if :profile_003_ready
\else
\echo 'Prerequisite profile 001/002 missing or batch_scopes already exists'
SELECT 1 / 0;
\endif

CREATE TABLE wbd_auth.batch_scopes (
  execution_id uuid NOT NULL PRIMARY KEY,
  physical_target_id uuid NOT NULL REFERENCES wbd_auth.target_marker(physical_target_id),
  database_id uuid NOT NULL,
  actor_id uuid NOT NULL,
  total_count bigint NOT NULL CHECK (total_count >= 0),
  ready boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT pg_catalog.clock_timestamp(),
  ready_at timestamptz
);

SELECT pg_catalog.format('ALTER TABLE wbd_auth.batch_scopes OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON TABLE wbd_auth.batch_scopes FROM PUBLIC;
SELECT pg_catalog.format('GRANT SELECT ON TABLE wbd_auth.batch_scopes TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec

CREATE TABLE wbd_auth.batch_scope_items (
  execution_id uuid NOT NULL REFERENCES wbd_auth.batch_scopes(execution_id) ON DELETE CASCADE,
  item_id bigint NOT NULL,
  PRIMARY KEY (execution_id, item_id)
);

SELECT pg_catalog.format('ALTER TABLE wbd_auth.batch_scope_items OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON TABLE wbd_auth.batch_scope_items FROM PUBLIC;
SELECT pg_catalog.format('GRANT SELECT ON TABLE wbd_auth.batch_scope_items TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec

-- The fixture owner provisions access with GRANT SELECT (id, processed) ON managed.batch_items.
SELECT pg_catalog.format('GRANT USAGE ON SCHEMA managed TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec

CREATE FUNCTION wbd_auth.freeze_batch_scope(
  p_execution_id uuid,
  p_physical_target_id uuid,
  p_database_id uuid,
  p_actor_id uuid
)
RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
AS $function$
DECLARE
  marker wbd_auth.target_marker%ROWTYPE;
  existing wbd_auth.batch_scopes%ROWTYPE;
  v_count bigint;
  fixture oid;
BEGIN
  SELECT * INTO STRICT marker FROM wbd_auth.target_marker WHERE singleton;
  IF session_user <> pg_catalog.replace(marker.executor_role::text, '_executor', '_preparer') THEN
    RAISE EXCEPTION 'freeze authorization rejected: must be preparer' USING ERRCODE = '42501';
  END IF;

  IF p_execution_id IS NULL OR p_physical_target_id IS NULL
     OR p_database_id IS NULL OR p_actor_id IS NULL THEN
    RAISE EXCEPTION 'invalid scope parameters' USING ERRCODE = '22023';
  END IF;

  IF p_physical_target_id <> marker.physical_target_id THEN
    RAISE EXCEPTION 'target mismatch' USING ERRCODE = '42501';
  END IF;

  IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'scope capture requires read committed isolation' USING ERRCODE = '25001';
  END IF;
  PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(p_execution_id::text, 74102833912028));

  SELECT * INTO existing FROM wbd_auth.batch_scopes WHERE execution_id = p_execution_id;
  IF FOUND THEN
    IF existing.physical_target_id <> p_physical_target_id
       OR existing.database_id <> p_database_id THEN
      RAISE EXCEPTION 'mismatched batch scope identity' USING ERRCODE = '42501';
    END IF;
    IF NOT existing.ready THEN
      RAISE EXCEPTION 'incomplete batch scope' USING ERRCODE = '55000';
    END IF;
    RETURN existing.total_count;
  END IF;

  -- A column-level SELECT takes an AccessShareLock without granting table-wide SELECT.
  PERFORM id, processed FROM ONLY managed.batch_items WHERE false;
  SELECT c.oid INTO fixture
    FROM pg_catalog.pg_class c
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname = 'managed' AND c.relname = 'batch_items'
     AND c.relkind = 'r' AND NOT c.relispartition AND NOT c.relrowsecurity
     AND pg_catalog.pg_get_userbyid(c.relowner) = marker.executor_role::text
     AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = c.oid OR i.inhparent = c.oid)
     AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'id'
                 AND a.atttypid = 'pg_catalog.int8'::pg_catalog.regtype AND a.attnotnull AND NOT a.attisdropped)
     AND EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'processed'
                 AND a.atttypid = 'pg_catalog.bool'::pg_catalog.regtype AND a.attnotnull AND NOT a.attisdropped)
     AND EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
                 WHERE k.conrelid = c.oid AND k.contype = 'p' AND k.convalidated
                   AND k.conkey = ARRAY[(SELECT a.attnum FROM pg_catalog.pg_attribute a
                                          WHERE a.attrelid = c.oid AND a.attname = 'id')]::smallint[]);
  IF fixture IS NULL THEN
    RAISE EXCEPTION 'unsupported managed.batch_items fixture' USING ERRCODE = '55000';
  END IF;

  INSERT INTO wbd_auth.batch_scopes
    (execution_id, physical_target_id, database_id, actor_id, total_count, ready)
  VALUES
    (p_execution_id, p_physical_target_id, p_database_id, p_actor_id, 0, false);

  INSERT INTO wbd_auth.batch_scope_items (execution_id, item_id)
  SELECT p_execution_id, id FROM ONLY managed.batch_items WHERE processed = false ORDER BY id;
  GET DIAGNOSTICS v_count = ROW_COUNT;

  UPDATE wbd_auth.batch_scopes
  SET ready = true,
      total_count = v_count,
      ready_at = pg_catalog.clock_timestamp()
  WHERE execution_id = p_execution_id;

  RETURN v_count;
END
$function$;

SELECT pg_catalog.format('ALTER FUNCTION wbd_auth.freeze_batch_scope(uuid, uuid, uuid, uuid) OWNER TO %I', 'wbd_target_' || :'target_slot' || '_receipt_owner') \gexec
REVOKE ALL ON FUNCTION wbd_auth.freeze_batch_scope(uuid, uuid, uuid, uuid) FROM PUBLIC;
SELECT pg_catalog.format('GRANT EXECUTE ON FUNCTION wbd_auth.freeze_batch_scope(uuid, uuid, uuid, uuid) TO %I', 'wbd_target_' || :'target_slot' || '_preparer') \gexec
